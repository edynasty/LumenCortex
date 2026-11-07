package evalruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// maxFrame bounds one JSON-RPC frame in either direction. A worker that writes
// more is failed, never buffered without limit.
const maxFrame = 4 << 20

var (
	errFrameTooLarge = errors.New("eval: control frame exceeds limit")
	errConnClosed    = errors.New("eval: control channel closed")
)

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// rpcMessage is one frame. ID is kept raw so a host integer id and a worker
// string id are both represented without ambiguity.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func idKey(id json.RawMessage) string {
	if len(id) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(id, &s); err == nil {
		return "s:" + s
	}
	var n json.Number
	if err := json.Unmarshal(id, &n); err == nil {
		return "n:" + n.String()
	}
	return "r:" + string(id)
}

// rpcConn is a JSON-RPC 2.0 peer over a byte stream: newline-delimited frames,
// correlated responses, inbound requests handled off the read loop, and
// notifications.
type rpcConn struct {
	r io.Reader
	w io.Writer

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan *rpcMessage
	nextID  atomic.Int64

	// handler answers inbound requests (worker -> host). It must return either a
	// result or an error; both are marshalled back to the worker.
	handler func(ctx context.Context, method string, params json.RawMessage) (any, error)
	// notify receives inbound notifications.
	notify func(method string, params json.RawMessage)

	done      chan struct{}
	closeOnce sync.Once
	err       error
	limit     int
}

func newRPCConn(r io.Reader, w io.Writer, handler func(context.Context, string, json.RawMessage) (any, error), notify func(string, json.RawMessage)) *rpcConn {
	return &rpcConn{
		r:       r,
		w:       w,
		pending: make(map[string]chan *rpcMessage),
		handler: handler,
		notify:  notify,
		done:    make(chan struct{}),
		limit:   maxFrame,
	}
}

// start runs the read loop until the stream ends or the connection closes.
func (c *rpcConn) start() {
	go c.readLoop()
}

func (c *rpcConn) readLoop() {
	reader := bufio.NewReaderSize(c.r, 64<<10)
	for {
		line, err := readBoundedLine(reader, c.limit)
		if err != nil {
			if errors.Is(err, errFrameTooLarge) {
				c.close(err)
				return
			}
			c.close(errConnClosed)
			return
		}
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			// A malformed frame is a protocol failure, not a result.
			c.close(fmt.Errorf("eval: invalid control frame: %w", err))
			return
		}
		c.dispatch(msg)
	}
}

func (c *rpcConn) dispatch(msg rpcMessage) {
	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		go c.serveRequest(msg)
	case msg.Method != "":
		if c.notify != nil {
			c.notify(msg.Method, msg.Params)
		}
	default:
		key := idKey(msg.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		if ok {
			delete(c.pending, key)
		}
		c.mu.Unlock()
		if ok {
			select {
			case ch <- &msg:
			default:
			}
		}
	}
}

func (c *rpcConn) serveRequest(msg rpcMessage) {
	var result any
	var err error
	if c.handler == nil {
		err = &rpcError{Code: -32601, Message: "method not supported by host"}
	} else {
		result, err = c.handler(context.Background(), msg.Method, msg.Params)
	}
	response := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
	if err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			response.Error = rpcErr
		} else {
			response.Error = &rpcError{Code: -32603, Message: err.Error()}
		}
	} else {
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			response.Error = &rpcError{Code: -32603, Message: marshalErr.Error()}
		} else {
			response.Result = data
		}
	}
	_ = c.write(response)
}

// call sends a request and waits for its correlated response.
func (c *rpcConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	key := "n:" + strconv.FormatInt(id, 10)
	ch := make(chan *rpcMessage, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return nil, c.connErr()
	default:
	}
	c.pending[key] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(params)
	if err != nil {
		c.removePending(key)
		return nil, err
	}
	request := rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: payload}
	if err := c.write(request); err != nil {
		c.removePending(key)
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-c.done:
		return nil, c.connErr()
	case <-ctx.Done():
		c.removePending(key)
		return nil, ctx.Err()
	}
}

func (c *rpcConn) removePending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

// notify sends a notification (no id, no response).
func (c *rpcConn) notifyPeer(method string, params any) error {
	payload, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: payload})
}

func (c *rpcConn) write(msg rpcMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data) > c.limit {
		return errFrameTooLarge
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.connErr()
	default:
	}
	if _, err := c.w.Write(data); err != nil {
		return err
	}
	return nil
}

func (c *rpcConn) close(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		pending := c.pending
		c.pending = make(map[string]chan *rpcMessage)
		c.mu.Unlock()
		close(c.done)
		for key, ch := range pending {
			select {
			case ch <- &rpcMessage{Error: &rpcError{Code: -32000, Message: err.Error()}}:
			default:
			}
			delete(pending, key)
		}
	})
}

func (c *rpcConn) connErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return errConnClosed
}

func readBoundedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > limit {
			return nil, errFrameTooLarge
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}
