package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrClosed        = errors.New("lsp client is closed")
	ErrTooManyPending = errors.New("lsp pending request limit exceeded")
	ErrMessageTooLarge = errors.New("lsp message exceeds runtime limit")
	ErrDocumentTooLarge = errors.New("lsp document exceeds runtime limit")
)

type Client struct {
	cfg Config
	cmd *exec.Cmd
	stdin io.WriteCloser
	stdout *bufio.Reader
	cancel context.CancelFunc

	writeMu sync.Mutex
	pendingMu sync.Mutex
	pending map[int64]chan response
	nextID atomic.Int64

	diagMu sync.RWMutex
	diagnostics map[string][]Diagnostic

	docMu sync.Mutex
	documents map[string]documentState

	statusMu sync.RWMutex
	lastError string
	closed atomic.Bool
	closing atomic.Bool
	closeOnce sync.Once
	done chan struct{}
}

func Start(ctx context.Context, cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("lsp command is required")
	}
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, errors.New("lsp workspace is required")
	}
	workspace, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(workspace); err != nil {
		return nil, err
	}
	cfg.Workspace = workspace

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, cfg.Command, cfg.Args...)
	cmd.Dir = workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr := &boundedBuffer{max: 128 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	c := &Client{
		cfg: cfg,
		cmd: cmd,
		stdin: stdin,
		stdout: bufio.NewReaderSize(stdoutPipe, 64<<10),
		cancel: cancel,
		pending: make(map[int64]chan response),
		diagnostics: make(map[string][]Diagnostic),
		documents: make(map[string]documentState),
		done: make(chan struct{}),
	}
	go c.readLoop(stderr)

	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	defer initCancel()
	var initializeResult json.RawMessage
	if err := c.Request(initCtx, "initialize", c.initializeParams(), &initializeResult); err != nil {
		c.Close()
		return nil, fmt.Errorf("lsp initialize: %w", err)
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) initializeParams() map[string]any {
	rootURI := fileURI(c.cfg.Workspace)
	return map[string]any{
		"processId": os.Getpid(),
		"rootUri": rootURI,
		"workspaceFolders": []map[string]any{{"uri": rootURI, "name": filepath.Base(c.cfg.Workspace)}},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"hover": map[string]any{},
				"definition": map[string]any{},
				"references": map[string]any{},
				"documentSymbol": map[string]any{},
				"rename": map[string]any{},
				"publishDiagnostics": map[string]any{},
				"synchronization": map[string]any{"didSave": true},
			},
			"workspace": map[string]any{
				"symbol": map[string]any{},
				"workspaceFolders": true,
			},
		},
	}
}

func (c *Client) Request(ctx context.Context, method string, params any, result any) error {
	return c.request(ctx, method, params, result, false)
}

func (c *Client) request(ctx context.Context, method string, params any, result any, allowClosing bool) error {
	if c.closed.Load() || (c.closing.Load() && !allowClosing) {
		return ErrClosed
	}
	id := c.nextID.Add(1)
	ch := make(chan response, 1)

	c.pendingMu.Lock()
	if len(c.pending) >= MaxPendingRequests {
		c.pendingMu.Unlock()
		return ErrTooManyPending
	}
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.writeMessage(map[string]any{
		"jsonrpc": "2.0",
		"id": id,
		"method": method,
		"params": params,
	}, allowClosing); err != nil {
		c.removePending(id)
		return err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if result != nil && len(res.result) > 0 && string(res.result) != "null" {
			if err := json.Unmarshal(res.result, result); err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		if c.removePending(id) {
			_ = c.Notify("$/cancelRequest", map[string]any{"id": id})
		}
		return ctx.Err()
	case <-c.done:
		c.removePending(id)
		return ErrClosed
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.notify(method, params, false)
}

func (c *Client) notify(method string, params any, allowClosing bool) error {
	if c.closed.Load() || (c.closing.Load() && !allowClosing) {
		return ErrClosed
	}
	return c.writeMessage(map[string]any{
		"jsonrpc": "2.0",
		"method": method,
		"params": params,
	}, allowClosing)
}

func (c *Client) writeMessage(value any, allowClosing bool) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > MaxMessageBytes {
		return ErrMessageTooLarge
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() || (c.closing.Load() && !allowClosing) {
		return ErrClosed
	}
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(raw)); err != nil {
		return err
	}
	_, err = c.stdin.Write(raw)
	return err
}

func (c *Client) readLoop(stderr *boundedBuffer) {
	defer close(c.done)
	for {
		raw, err := readFrame(c.stdout)
		if err != nil {
			if !c.closed.Load() && !errors.Is(err, io.EOF) {
				c.setLastError(err.Error())
			}
			c.failAllPending(err)
			break
		}
		var msg envelope
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.setLastError(err.Error())
			continue
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 && string(msg.ID) != "null" {
				c.handleServerRequest(msg)
			} else {
				c.handleNotification(msg)
			}
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		id, err := parseID(msg.ID)
		if err != nil {
			continue
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		if !ok {
			continue
		}
		if msg.Error != nil {
			ch <- response{err: msg.Error}
		} else {
			ch <- response{result: msg.Result}
		}
	}
	if text := strings.TrimSpace(stderr.String()); text != "" && c.LastError() == "" {
		c.setLastError(text)
	}
}

func (c *Client) handleNotification(msg envelope) {
	switch msg.Method {
	case "textDocument/publishDiagnostics":
		var params PublishDiagnosticsParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return
		}
		if len(params.Diagnostics) > MaxDiagnostics {
			params.Diagnostics = params.Diagnostics[:MaxDiagnostics]
		}
		c.diagMu.Lock()
		c.diagnostics[params.URI] = append([]Diagnostic(nil), params.Diagnostics...)
		c.diagMu.Unlock()
	case "window/logMessage", "window/showMessage":
		var payload struct { Message string `json:"message"` }
		if json.Unmarshal(msg.Params, &payload) == nil && strings.TrimSpace(payload.Message) != "" {
			c.setLastError("")
		}
	}
}

func (c *Client) handleServerRequest(msg envelope) {
	var id any
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		return
	}
	var result any
	var rpcErr *RPCError
	switch msg.Method {
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create":
		result = nil
	case "workspace/configuration":
		var params struct { Items []json.RawMessage `json:"items"` }
		_ = json.Unmarshal(msg.Params, &params)
		result = make([]any, len(params.Items))
	case "workspace/workspaceFolders":
		uri := fileURI(c.cfg.Workspace)
		result = []map[string]any{{"uri": uri, "name": filepath.Base(c.cfg.Workspace)}}
	default:
		rpcErr = &RPCError{Code: -32601, Message: "method not supported by LumenCortex client"}
	}
	payload := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		payload["error"] = rpcErr
	} else {
		payload["result"] = result
	}
	_ = c.writeMessage(payload, true)
}

func (c *Client) Status() Status {
	c.pendingMu.Lock()
	pending := len(c.pending)
	c.pendingMu.Unlock()
	c.diagMu.RLock()
	diagnostics := 0
	for _, items := range c.diagnostics {
		diagnostics += len(items)
	}
	c.diagMu.RUnlock()
	c.statusMu.RLock()
	lastError := c.lastError
	c.statusMu.RUnlock()

	status := Status{
		Running: !c.closed.Load(),
		Name: c.cfg.Name,
		Command: c.cfg.Command,
		PendingRequests: pending,
		Diagnostics: diagnostics,
		LastError: lastError,
	}
	if c.cmd != nil && c.cmd.Process != nil {
		status.PID = c.cmd.Process.Pid
	}
	return status
}

func (c *Client) Diagnostics(path string) []Diagnostic {
	uri := path
	if !strings.HasPrefix(uri, "file://") {
		if abs, err := c.resolvePath(path); err == nil {
			uri = fileURI(abs)
		}
	}
	c.diagMu.RLock()
	defer c.diagMu.RUnlock()
	return append([]Diagnostic(nil), c.diagnostics[uri]...)
}

func (c *Client) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		if !c.closed.Load() {
			c.closing.Store(true)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			var ignored json.RawMessage
			_ = c.request(ctx, "shutdown", nil, &ignored, true)
			cancel()
			_ = c.notify("exit", nil, true)
			c.closed.Store(true)
		}
		_ = c.stdin.Close()
		c.cancel()
		if c.cmd != nil {
			closeErr = c.cmd.Wait()
		}
		c.failAllPending(ErrClosed)
	})
	return closeErr
}

func (c *Client) removePending(id int64) bool {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if _, ok := c.pending[id]; !ok {
		return false
	}
	delete(c.pending, id)
	return true
}

func (c *Client) failAllPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan response)
	c.pendingMu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- response{err: err}:
		default:
		}
	}
}

func (c *Client) setLastError(value string) {
	c.statusMu.Lock()
	c.lastError = value
	c.statusMu.Unlock()
}

func (c *Client) LastError() string {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.lastError
}

func parseID(raw json.RawMessage) (int64, error) {
	var id int64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, err
	}
	return strconv.ParseInt(text, 10, 64)
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, errors.New("lsp frame missing Content-Length")
	}
	if contentLength > MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	raw := make([]byte, contentLength)
	_, err := io.ReadFull(reader, raw)
	return raw, err
}

func fileURI(path string) string {
	abs, _ := filepath.Abs(path)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

type boundedBuffer struct {
	mu sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return original, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
