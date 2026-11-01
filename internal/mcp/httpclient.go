package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPClient is an MCP Streamable HTTP client. Each request is one HTTP POST whose body is a single
// JSON-RPC message; the response is either application/json or a text/event-stream carrying the
// matching response. A stateful server's Mcp-Session-Id is echoed on later requests.
//
// The client never retries a request on its own: a tool call has a caller-supplied deadline, and a
// silent retry could run a side-effecting tool twice.
type HTTPClient struct {
	cfg      Config
	endpoint *url.URL
	socket   string
	client   *http.Client

	sessionMu sync.Mutex
	sessionID string

	statusMu        sync.RWMutex
	lastError       string
	protocolVersion string

	nextID atomic.Int64

	toolsMu sync.RWMutex
	tools   []Tool

	closed atomic.Bool
}

// startHTTPClient connects to a Streamable HTTP endpoint and performs the MCP handshake. ctx bounds
// the handshake only; later requests carry their own deadline.
func startHTTPClient(ctx context.Context, cfg Config) (*HTTPClient, error) {
	if cfg.transport() != TransportHTTP {
		return nil, fmt.Errorf("mcp server %q: the http client cannot start the %s transport", cfg.ID, cfg.transport())
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = ModeLegacy
	}
	rawURL, err := expandEnv(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", cfg.ID, err)
	}
	endpoint, socket, err := parseEndpoint(rawURL)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", cfg.ID, err)
	}
	client := &HTTPClient{cfg: cfg, endpoint: endpoint, socket: socket}
	client.client = &http.Client{
		Transport: client.transport(),
		// The request context is the only deadline; a global client timeout would cut a long tool call
		// that its own configuration allows.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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

// parseEndpoint turns a configured URL into the HTTP endpoint and, for unix://, the socket path. A
// Unix socket endpoint is served at path /mcp with Host localhost, the form CodeBridge's local daemon
// accepts.
func parseEndpoint(raw string) (*url.URL, string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "unix://") {
		path := strings.TrimPrefix(raw, "unix://")
		if path == "" || !strings.HasPrefix(path, "/") {
			return nil, "", errors.New("a unix url must be unix:///absolute/path")
		}
		return &url.URL{Scheme: "http", Host: "localhost", Path: "/mcp"}, path, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", errors.New("url must be http, https or unix")
	}
	if parsed.Host == "" {
		return nil, "", errors.New("url must name a host")
	}
	return parsed, "", nil
}

func (c *HTTPClient) transport() http.RoundTripper {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 2
	base.IdleConnTimeout = 90 * time.Second
	if c.socket != "" {
		socket := c.socket
		base.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		}
	}
	return base
}

func (c *HTTPClient) negotiate(ctx context.Context) error {
	if c.cfg.ProtocolMode == ModeModern {
		var discover map[string]any
		if err := c.Request(ctx, "server/discover", map[string]any{}, &discover); err != nil {
			return fmt.Errorf("mcp modern discover: %w", err)
		}
		c.setProtocolVersion(ProtocolModern)
		return nil
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	headers, err := c.post(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID.Add(1),
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": ProtocolLegacy,
			"capabilities": map[string]any{
				"roots": map[string]any{"listChanged": false},
			},
			"clientInfo": map[string]any{"name": "LumenCortex", "version": "0.1.0"},
		},
	}, &result)
	if err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	if id := headers.Get("Mcp-Session-Id"); id != "" {
		c.sessionMu.Lock()
		c.sessionID = id
		c.sessionMu.Unlock()
	}
	if result.ProtocolVersion == "" {
		result.ProtocolVersion = ProtocolLegacy
	}
	c.setProtocolVersion(result.ProtocolVersion)
	return c.Notify("notifications/initialized", map[string]any{})
}

func (c *HTTPClient) setProtocolVersion(version string) {
	c.statusMu.Lock()
	c.protocolVersion = version
	c.statusMu.Unlock()
}

func (c *HTTPClient) Request(ctx context.Context, method string, params map[string]any, result any) error {
	params = c.requestParams(params)
	_, err := c.post(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID.Add(1),
		"method":  method,
		"params":  params,
	}, result)
	return err
}

func (c *HTTPClient) Notify(method string, params map[string]any) error {
	// A notification has no response to wait for. It is sent under a short bound so a server that
	// never answers cannot hold a run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.post(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  c.requestParams(params),
	}, nil)
	return err
}

func (c *HTTPClient) requestParams(params map[string]any) map[string]any {
	if c.cfg.ProtocolMode == ModeModern {
		return withModernMeta(params)
	}
	if params == nil {
		return map[string]any{}
	}
	return params
}

// post sends one JSON-RPC message and decodes the response into result when result is not nil. A
// notification gets a body of no interest: the call returns as soon as the server answered.
func (c *HTTPClient) post(ctx context.Context, payload map[string]any, result any) (http.Header, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	c.sessionMu.Lock()
	if c.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.sessionMu.Unlock()
	c.statusMu.RLock()
	if c.protocolVersion != "" {
		request.Header.Set("MCP-Protocol-Version", c.protocolVersion)
	}
	c.statusMu.RUnlock()
	for name, value := range c.cfg.Headers {
		resolved, err := resolveEnvReference(value)
		if err != nil {
			return nil, err
		}
		request.Header.Set(name, resolved)
	}

	response, err := c.client.Do(request)
	if err != nil {
		c.setLastError(err.Error())
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		statusErr := fmt.Errorf("mcp http %s: %d %s", c.cfg.ID, response.StatusCode, strings.TrimSpace(string(body)))
		c.setLastError(statusErr.Error())
		return response.Header, statusErr
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxMessageBytes))
		return response.Header, nil
	}
	expected, _ := payload["id"].(int64)
	message, err := readHTTPMessage(response, expected)
	if err != nil {
		c.setLastError(err.Error())
		return response.Header, err
	}
	var reply envelope
	if err := json.Unmarshal(message, &reply); err != nil {
		c.setLastError(err.Error())
		return response.Header, err
	}
	if reply.Error != nil {
		return response.Header, reply.Error
	}
	if len(reply.Result) == 0 || string(reply.Result) == "null" {
		return response.Header, nil
	}
	if err := json.Unmarshal(reply.Result, result); err != nil {
		c.setLastError(err.Error())
		return response.Header, err
	}
	return response.Header, nil
}

// readHTTPMessage returns the JSON-RPC message carrying the wanted id from a JSON or SSE response.
// Server requests inside the stream (ping, roots/list) are answered out of band and skipped.
func readHTTPMessage(response *http.Response, expected int64) ([]byte, error) {
	contentType := response.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		raw, err := io.ReadAll(io.LimitReader(response.Body, MaxMessageBytes+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > MaxMessageBytes {
			return nil, ErrMessageTooLarge
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil, errors.New("mcp http response carried no message")
		}
		return raw, nil
	}
	reader := bufio.NewReaderSize(response.Body, 64<<10)
	var data bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			if data.Len() == 0 {
				if errors.Is(err, io.EOF) {
					return nil, errors.New("mcp http event stream ended without a response")
				}
				continue
			}
			payload := append([]byte(nil), data.Bytes()...)
			data.Reset()
			var msg envelope
			if json.Unmarshal(payload, &msg) == nil && msg.Method == "" && len(msg.ID) > 0 {
				if id, idErr := parseID(msg.ID); idErr == nil && id == expected {
					return payload, nil
				}
			}
		case strings.HasPrefix(trimmed, "data:"):
			value := strings.TrimPrefix(trimmed, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			if data.Len() > MaxMessageBytes {
				return nil, ErrMessageTooLarge
			}
		}
		if errors.Is(err, io.EOF) {
			return nil, errors.New("mcp http event stream ended without a response")
		}
	}
}

func (c *HTTPClient) ListTools(ctx context.Context) ([]Tool, error) {
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

func (c *HTTPClient) CallTool(ctx context.Context, name string, arguments map[string]any) (CallToolResult, error) {
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
	if len(raw) > c.cfg.maxResultBytes() {
		return CallToolResult{}, ErrToolResultTooLarge
	}
	return result, nil
}

func (c *HTTPClient) Tools() []Tool {
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

func (c *HTTPClient) RefreshTools(ctx context.Context) ([]Tool, error) {
	tools, err := c.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	c.toolsMu.Lock()
	c.tools = tools
	c.toolsMu.Unlock()
	return tools, nil
}

func (c *HTTPClient) Status() Status {
	c.toolsMu.RLock()
	toolCount := len(c.tools)
	c.toolsMu.RUnlock()
	c.statusMu.RLock()
	lastError := c.lastError
	version := c.protocolVersion
	c.statusMu.RUnlock()
	return Status{
		ID:              c.cfg.ID,
		Name:            c.cfg.Name,
		URL:             c.cfg.URL,
		Transport:       TransportHTTP,
		Workspace:       c.cfg.Workspace,
		ProtocolMode:    c.cfg.ProtocolMode,
		ProtocolVersion: version,
		Running:         !c.closed.Load(),
		Tools:           toolCount,
		LastError:       lastError,
	}
}

func (c *HTTPClient) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	if c.client != nil {
		c.client.CloseIdleConnections()
	}
	return nil
}

func (c *HTTPClient) setLastError(value string) {
	c.statusMu.Lock()
	c.lastError = value
	c.statusMu.Unlock()
}

// clearLastError records that a call succeeded, so a server that recovered stops reporting its old
// failure.
func (c *HTTPClient) clearLastError() {
	c.statusMu.Lock()
	c.lastError = ""
	c.statusMu.Unlock()
}

var _ = strconv.Itoa
