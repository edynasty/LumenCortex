package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrClosed        = errors.New("DAP transport closed")
	ErrBusy          = errors.New("DAP pending request bound reached")
	ErrEventOverflow = errors.New("DAP critical event queue overflow")
)

// ResponseError retains bounded adapter details for authorized inspection, not logs.
type ResponseError struct {
	Command, Message string
	Body             json.RawMessage
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("DAP %s rejected: %s", e.Command, e.Message)
}

type Connection interface {
	io.ReadWriteCloser
	SetWriteDeadline(time.Time) error
}

type pending struct {
	command string
	result  chan Message
}

type Client struct {
	conn           Connection
	writer         chan struct{}
	mu             sync.Mutex
	pending        map[int64]pending
	err            error
	done           chan struct{}
	readerDone     chan struct{}
	events         chan Message
	reverse        chan Message
	seq            atomic.Int64
	dropped        atomic.Uint64
	reverseHandler func(context.Context, Message) error
}

func New(conn Connection) *Client {
	c := &Client{conn: conn, writer: make(chan struct{}, 1), pending: make(map[int64]pending), done: make(chan struct{}), readerDone: make(chan struct{}), events: make(chan Message, 256), reverse: make(chan Message, 16)}
	go c.readLoop()
	go c.rejectReverse()
	return c
}
func (c *Client) Done() <-chan struct{}  { return c.done }
func (c *Client) Events() <-chan Message { return c.events }
func (c *Client) DroppedOutput() uint64  { return c.dropped.Load() }
func (c *Client) Err() error             { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

// SetReverseHandler is host-only. Without it every reverse request is refused.
func (c *Client) SetReverseHandler(handler func(context.Context, Message) error) {
	c.mu.Lock()
	c.reverseHandler = handler
	c.mu.Unlock()
}
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
		close(c.done)
		_ = c.conn.Close()
	}
	c.mu.Unlock()
}
func (c *Client) Close() error { c.fail(ErrClosed); <-c.readerDone; return nil }

func (c *Client) send(ctx context.Context, m Message) error {
	select {
	case c.writer <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
	defer func() { <-c.writer }()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetWriteDeadline(time.Now()); close(cancelled) })
	err := writeMessage(c.conn, m)
	if !stop() {
		<-cancelled
	}
	_ = c.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *Client) Request(ctx context.Context, command string, args any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if len(raw) > MaxMessage-1024 {
		return ErrProtocol
	}
	seq := c.seq.Add(1)
	p := pending{command: command, result: make(chan Message, 1)}
	c.mu.Lock()
	if c.err != nil {
		err = c.err
		c.mu.Unlock()
		return err
	}
	if len(c.pending) >= 64 {
		c.mu.Unlock()
		return ErrBusy
	}
	c.pending[seq] = p
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, seq); c.mu.Unlock() }()
	if err = c.send(ctx, Message{Seq: seq, Type: "request", Command: command, Arguments: raw}); err != nil {
		return err
	}
	select {
	case result := <-p.result:
		if !result.Success {
			return &ResponseError{Command: command, Message: result.Message, Body: result.Body}
		}
		if out != nil && len(result.Body) > 0 {
			return json.Unmarshal(result.Body, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}
func (c *Client) readLoop() {
	defer close(c.readerDone)
	defer close(c.events)
	r := bufio.NewReaderSize(c.conn, maxHeaders)
	for {
		m, err := readMessage(r)
		if err != nil {
			c.fail(err)
			return
		}
		switch m.Type {
		case "response":
			c.mu.Lock()
			p, ok := c.pending[m.RequestSeq]
			if ok {
				delete(c.pending, m.RequestSeq)
			}
			c.mu.Unlock()
			if ok {
				if p.command != m.Command {
					c.fail(ErrProtocol)
					return
				}
				p.result <- m
			}
		case "event":
			select {
			case c.events <- m:
			default:
				if m.Event == "output" {
					c.dropped.Add(1)
				} else {
					c.fail(ErrEventOverflow)
					return
				}
			}
		case "request":
			select {
			case c.reverse <- m:
			default:
				c.fail(ErrBusy)
				return
			}
		}
	}
}

// Adapters cannot use reverse requests to spawn terminals or unregistered targets.
func (c *Client) rejectReverse() {
	for {
		select {
		case <-c.done:
			return
		case m := <-c.reverse:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			c.mu.Lock()
			handler := c.reverseHandler
			c.mu.Unlock()
			failure := errors.New("reverse request not authorized by host")
			if handler != nil {
				failure = handler(ctx, m)
			}
			message := ""
			if failure != nil {
				message = failure.Error()
			}
			err := c.send(ctx, Message{Seq: c.seq.Add(1), Type: "response", RequestSeq: m.Seq, Command: m.Command, Success: failure == nil, Message: message})
			cancel()
			if err != nil {
				c.fail(err)
				return
			}
		}
	}
}
