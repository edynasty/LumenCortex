package mcp

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edynasty/LumenCortex/internal/shell"
)

var (
	ErrClosed             = errors.New("mcp client is closed")
	ErrTooManyPending     = errors.New("mcp pending request limit exceeded")
	ErrMessageTooLarge    = errors.New("mcp message exceeds runtime limit")
	ErrToolResultTooLarge = errors.New("mcp tool result exceeds runtime limit")
)

// closeGrace bounds how long Close waits for a server to exit on its own after its stdin closes: a
// server that stops what it started (playwright-mcp closes its Chromium) does so on exit, and only a
// server that does not is signalled.
const closeGrace = 2 * time.Second

type Client struct {
	cfg    Config
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	cancel context.CancelFunc

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[int64]chan response
	nextID    atomic.Int64

	toolsMu sync.RWMutex
	tools   []Tool

	statusMu        sync.RWMutex
	lastError       string
	protocolVersion string

	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
}

func Start(ctx context.Context, cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, errors.New("mcp server id is required")
	}
	if cfg.transport() != TransportStdio {
		return nil, fmt.Errorf("mcp server %q: the stdio client cannot start the %s transport", cfg.ID, cfg.transport())
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("mcp command is required")
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = ModeLegacy
	}
	if cfg.ProtocolMode != ModeLegacy && cfg.ProtocolMode != ModeModern {
		return nil, errors.New("unsupported mcp protocol mode")
	}
	workspace := cfg.Workspace
	if workspace == "" {
		workspace = "."
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	cfg.Workspace = workspace

	env, err := resolvedEnv(cfg.Env)
	if err != nil {
		return nil, err
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, cfg.Command, cfg.Args...)
	cmd.Dir = workspace
	// A server inherits the runtime's environment; an explicit Env entry replaces one variable and is
	// resolved from "{env:NAME}" references only at start, so no credential is persisted.
	cmd.Env = append(os.Environ(), env...)
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
	// A server leads its own process group, the same way a shell command does: cancellation kills the
	// group rather than the server alone, and whatever the server leaves in that group dies with it.
	shell.StartGroup(cmd)
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	client := &Client{
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdoutPipe, 64<<10),
		cancel:  cancel,
		pending: make(map[int64]chan response),
		done:    make(chan struct{}),
	}
	go client.readLoop(stderr)

	if err := client.negotiate(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	client.toolsMu.Lock()
	client.tools = tools
	client.toolsMu.Unlock()
	return client, nil
}

func (c *Client) negotiate(ctx context.Context) error {
	if c.cfg.ProtocolMode == ModeModern {
		var discover map[string]any
		if err := c.Request(ctx, "server/discover", map[string]any{}, &discover); err != nil {
			return fmt.Errorf("mcp modern discover: %w", err)
		}
		c.statusMu.Lock()
		c.protocolVersion = ProtocolModern
		c.statusMu.Unlock()
		return nil
	}

	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolLegacy,
		"capabilities": map[string]any{
			"roots": map[string]any{"listChanged": false},
		},
		"clientInfo": map[string]any{"name": "LumenCortex", "version": "0.1.0"},
	}, &result, true)
	if err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	if result.ProtocolVersion == "" {
		result.ProtocolVersion = ProtocolLegacy
	}
	c.statusMu.Lock()
	c.protocolVersion = result.ProtocolVersion
	c.statusMu.Unlock()
	return c.Notify("notifications/initialized", map[string]any{})
}

func (c *Client) Request(ctx context.Context, method string, params map[string]any, result any) error {
	return c.request(ctx, method, params, result, false)
}

func (c *Client) request(ctx context.Context, method string, params map[string]any, result any, skipModernMeta bool) error {
	if c.closed.Load() {
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

	if !skipModernMeta && c.cfg.ProtocolMode == ModeModern {
		params = c.withModernMeta(params)
	}
	if params == nil {
		params = map[string]any{}
	}
	if err := c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		c.removePending(id)
		return err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if result != nil && len(res.result) > 0 && string(res.result) != "null" {
			return json.Unmarshal(res.result, result)
		}
		return nil
	case <-ctx.Done():
		if c.removePending(id) {
			_ = c.Notify("notifications/cancelled", map[string]any{
				"requestId": id,
				"reason":    ctx.Err().Error(),
			})
		}
		return ctx.Err()
	case <-c.done:
		c.removePending(id)
		return ErrClosed
	}
}

func (c *Client) Notify(method string, params map[string]any) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if c.cfg.ProtocolMode == ModeModern {
		params = c.withModernMeta(params)
	}
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < MaxToolPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor,omitempty"`
		}
		if err := c.Request(ctx, "tools/list", params, &result); err != nil {
			return nil, err
		}
		for _, tool := range result.Tools {
			if len(all) >= MaxTools {
				return all, nil
			}
			if tool.InputSchema == nil {
				tool.InputSchema = map[string]any{"type": "object"}
			}
			all = append(all, tool)
		}
		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}
	return all, nil
}

func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (CallToolResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CallToolResult{}, errors.New("mcp tool name is required")
	}
	var result CallToolResult
	if err := c.Request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	}, &result); err != nil {
		return CallToolResult{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return CallToolResult{}, err
	}
	if len(raw) > MaxToolResultBytes {
		return CallToolResult{}, ErrToolResultTooLarge
	}
	return result, nil
}

func (c *Client) Tools() []Tool {
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

func (c *Client) RefreshTools(ctx context.Context) ([]Tool, error) {
	tools, err := c.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	c.toolsMu.Lock()
	c.tools = tools
	c.toolsMu.Unlock()
	return tools, nil
}

func (c *Client) Status() Status {
	c.pendingMu.Lock()
	pending := len(c.pending)
	c.pendingMu.Unlock()
	c.toolsMu.RLock()
	toolCount := len(c.tools)
	c.toolsMu.RUnlock()
	c.statusMu.RLock()
	lastError := c.lastError
	version := c.protocolVersion
	c.statusMu.RUnlock()
	status := Status{
		ID:              c.cfg.ID,
		Name:            c.cfg.Name,
		Command:         c.cfg.Command,
		Transport:       TransportStdio,
		Workspace:       c.cfg.Workspace,
		ProtocolMode:    c.cfg.ProtocolMode,
		ProtocolVersion: version,
		Running:         !c.closed.Load(),
		PendingRequests: pending,
		Tools:           toolCount,
		LastError:       lastError,
	}
	if c.cmd != nil && c.cmd.Process != nil {
		status.PID = c.cmd.Process.Pid
	}
	return status
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		_ = c.stdin.Close()
		if c.cmd != nil {
			// A server that stops what it started does it on exit, which stdin EOF begins: give it
			// closeGrace to end before its group is killed, so a browser it owns is closed rather than
			// signalled.
			select {
			case <-c.done:
			case <-time.After(closeGrace):
			}
		}
		c.cancel()
		if c.cmd != nil {
			// The process is owned by this client. Cancellation/termination caused
			// by an explicit Close is expected and must not surface as a Stop error.
			_ = c.cmd.Wait()
			// A server that was signalled had no chance to stop its children: the group takes them.
			_ = shell.KillGroup(c.cmd)
		}
		c.failAllPending(ErrClosed)
	})
	return nil
}

// killGroup kills the server's process group: the processes it started are not this client's to leak.
func (c *Client) killGroup() {
	if c.cmd != nil {
		_ = shell.KillGroup(c.cmd)
	}
}

func (c *Client) withModernMeta(params map[string]any) map[string]any {
	return withModernMeta(params)
}

// withModernMeta adds the stateless protocol metadata the modern mode requires.
func withModernMeta(params map[string]any) map[string]any {
	out := make(map[string]any, len(params)+1)
	for key, value := range params {
		out[key] = value
	}
	out["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion": ProtocolModern,
		"io.modelcontextprotocol/clientInfo": map[string]any{
			"name":    "LumenCortex",
			"version": "0.1.0",
		},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{
			"roots": map[string]any{},
		},
	}
	return out
}

func (c *Client) readLoop(stderr *boundedBuffer) {
	defer close(c.done)
	defer c.closed.Store(true)
	// The server is gone (it exited, crashed or was killed): whatever it left in its process group is
	// not this client's to leak, so the group is killed here and not only on Close.
	defer c.killGroup()
	for {
		raw, err := readJSONLine(c.stdout)
		if err != nil {
			if !c.closed.Load() && !errors.Is(err, io.EOF) {
				c.setLastError(err.Error())
			}
			c.failAllPending(err)
			return
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var msg envelope
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 && string(msg.ID) != "null" {
				c.handleServerRequest(msg)
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
}

func (c *Client) handleServerRequest(msg envelope) {
	var id any
	if json.Unmarshal(msg.ID, &id) != nil {
		return
	}
	var result any
	var rpcErr *rpcError
	switch msg.Method {
	case "ping":
		result = map[string]any{}
	case "roots/list":
		result = map[string]any{
			"roots": []map[string]any{{
				"uri":  fileURI(c.cfg.Workspace),
				"name": filepath.Base(c.cfg.Workspace),
			}},
		}
	default:
		rpcErr = &rpcError{Code: -32601, Message: "client request not supported by LumenCortex"}
	}
	payload := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		payload["error"] = rpcErr
	} else {
		payload["result"] = result
	}
	_ = c.write(payload)
}

func (c *Client) write(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > MaxMessageBytes {
		return ErrMessageTooLarge
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	_, err = c.stdin.Write(append(raw, '\n'))
	return err
}

func readJSONLine(reader *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		part, isPrefix, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		if out.Len()+len(part) > MaxMessageBytes {
			return nil, ErrMessageTooLarge
		}
		_, _ = out.Write(part)
		if !isPrefix {
			return out.Bytes(), nil
		}
	}
}

func parseID(raw json.RawMessage) (int64, error) {
	var id int64
	if json.Unmarshal(raw, &id) == nil {
		return id, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, err
	}
	return strconv.ParseInt(text, 10, 64)
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

// resolvedEnv resolves the "{env:NAME}" references of a server's environment. A missing variable is
// an *ErrMissingEnv naming it; no value is ever put in an error or a log line.
func resolvedEnv(env map[string]string) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		value, err := resolveEnvReference(env[name])
		if err != nil {
			return nil, err
		}
		out = append(out, name+"="+value)
	}
	return out, nil
}

func (c *Client) setLastError(value string) {
	c.statusMu.Lock()
	c.lastError = value
	c.statusMu.Unlock()
}

func fileURI(path string) string {
	abs, _ := filepath.Abs(path)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

type boundedBuffer struct {
	mu  sync.Mutex
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
