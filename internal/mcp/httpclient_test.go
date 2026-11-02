package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// httpTestServer is a minimal MCP Streamable HTTP endpoint: one JSON-RPC message per POST, answered
// with application/json, plus a stateful session header on initialize.
type httpTestServer struct {
	requests atomic.Int32
	calls    atomic.Int32
	hang     atomic.Bool
	sse      bool
}

func (s *httpTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if msg.Method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "session-http-test")
		result = map[string]any{
			"protocolVersion": ProtocolLegacy,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "http-helper", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{
			{"name": "echo", "description": "Echo text.", "inputSchema": map[string]any{"type": "object"},
				"annotations": map[string]any{"readOnlyHint": true}},
			{"name": "hang", "description": "Never answers.", "inputSchema": map[string]any{"type": "object"}},
		}}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		s.calls.Add(1)
		if params.Name == "hang" || s.hang.Load() {
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		text, _ := params.Arguments["text"].(string)
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	default:
		result = map[string]any{}
	}
	payload := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "result": result}
	raw, _ := json.Marshal(payload)
	if !s.sse {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// A server notification first, then the response: the client must skip to the matching id.
	_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
}

func startHTTPTestServer(t *testing.T, sse bool) (*httpTestServer, *httptest.Server) {
	t.Helper()
	handler := &httpTestServer{sse: sse}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return handler, server
}

func TestHTTPClientHandshakeAndCall(t *testing.T) {
	handler, server := startHTTPTestServer(t, false)
	client, err := startHTTPClient(context.Background(), Config{
		ID:           "http-helper",
		Transport:    TransportHTTP,
		URL:          server.URL + "/mcp",
		ProtocolMode: ModeLegacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if status := client.Status(); !status.Running || status.Transport != TransportHTTP || status.Tools != 2 {
		t.Fatalf("status = %#v", status)
	}
	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hi" {
		t.Fatalf("result = %#v", result)
	}
	if handler.calls.Load() != 1 {
		t.Fatalf("server saw %d calls", handler.calls.Load())
	}
}

func TestHTTPClientReadsAnEventStream(t *testing.T) {
	_, server := startHTTPTestServer(t, true)
	client, err := startHTTPClient(context.Background(), Config{
		ID:           "http-sse",
		Transport:    TransportHTTP,
		URL:          server.URL + "/mcp",
		ProtocolMode: ModeLegacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "streamed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "streamed" {
		t.Fatalf("result = %#v", result)
	}
}

// A credential header is an {env:NAME} reference resolved per request, and the value is never part of
// the configuration the runtime holds.
func TestHTTPClientResolvesHeaderReference(t *testing.T) {
	t.Setenv("LCX_TEST_MCP_TOKEN", "secret-value")
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		handler := &httpTestServer{}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := startHTTPClient(context.Background(), Config{
		ID:           "http-auth",
		Transport:    TransportHTTP,
		URL:          server.URL,
		ProtocolMode: ModeLegacy,
		Headers:      map[string]string{"Authorization": "Bearer {env:LCX_TEST_MCP_TOKEN}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got, _ := seen.Load().(string); got != "Bearer secret-value" {
		t.Fatalf("authorization header = %q", got)
	}
}

// An unset reference fails the start with the variable's name and never with a value.
func TestHTTPClientMissingEnvReference(t *testing.T) {
	_, server := startHTTPTestServer(t, false)
	_, err := startHTTPClient(context.Background(), Config{
		ID:           "http-auth",
		Transport:    TransportHTTP,
		URL:          server.URL,
		ProtocolMode: ModeLegacy,
		Headers:      map[string]string{"Authorization": "Bearer {env:LCX_TEST_MCP_UNSET_TOKEN}"},
	})
	if err == nil {
		t.Fatal("a missing referenced variable must fail the start")
	}
	if !strings.Contains(err.Error(), "LCX_TEST_MCP_UNSET_TOKEN") {
		t.Fatalf("error = %v, want the variable name", err)
	}
}

// A hung HTTP call is cut by the server's call timeout through the lifecycle, not only by the client.
func TestHTTPClientHungCallIsCutByTheServerTimeout(t *testing.T) {
	_, server := startHTTPTestServer(t, false)
	root := t.TempDir()
	registry, err := NewRegistry(root, filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	registry.SetProjectTrust(func() bool { return true })
	if err := registry.Upsert(Config{
		ID:                 "http-helper",
		Transport:          TransportHTTP,
		URL:                server.URL,
		ProtocolMode:       ModeLegacy,
		CallTimeoutSeconds: 1,
	}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = registry.CallTool(context.Background(), root, "http-helper", "hang", map[string]any{})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a hung HTTP call must time out")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the call timeout did not cut the call: %s", elapsed)
	}
}

// The Unix socket form reaches an HTTP endpoint with no TCP listener, the shape a local daemon uses.
func TestHTTPClientUnixSocketEndpoint(t *testing.T) {
	dir, err := os.MkdirTemp("", "lcx-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "mcp.sock")
	server := httptest.NewUnstartedServer(&httpTestServer{})
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()

	client, err := startHTTPClient(context.Background(), Config{
		ID:           "unix-helper",
		Transport:    TransportHTTP,
		URL:          "unix://" + socket,
		ProtocolMode: ModeLegacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if status := client.Status(); !status.Running || status.Tools != 2 {
		t.Fatalf("status = %#v", status)
	}
	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "socket"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "socket" {
		t.Fatalf("result = %#v", result)
	}
}

// httpRegistry is a registry with one trusted project server, cfg, over HTTP.
func httpRegistry(t *testing.T, cfg Config) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	registry, err := NewRegistry(root, filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	trustedProject(registry, true)
	cfg.Transport = TransportHTTP
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = ModeLegacy
	}
	if err := registry.Upsert(cfg); err != nil {
		t.Fatal(err)
	}
	return registry, root
}

// ADR-0021 D4: a credential in a URL is an {env:NAME} reference, and its value is reported nowhere. A
// refused request names the configured URL — in the call error, the start error, the registry's
// statuses and states — never the expanded one.
func TestHTTPClientURLReferenceNeverInErrors(t *testing.T) {
	const secret = "sk-test-url-7f3a9c"
	t.Setenv("LCX_TEST_MCP_URL_KEY", secret)
	_, server := startHTTPTestServer(t, false)
	registry, root := httpRegistry(t, Config{ID: "remote", URL: server.URL + "/mcp?key={env:LCX_TEST_MCP_URL_KEY}"})
	ctx := context.Background()
	if _, err := registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "up"}); err != nil {
		t.Fatal(err)
	}
	server.Close()

	reported := func(stage string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: a closed endpoint must fail", stage)
		}
		texts := []string{err.Error()}
		statuses, statusErr := registry.Statuses(root)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		for _, status := range statuses {
			texts = append(texts, status.LastError, status.URL)
		}
		states, stateErr := registry.States(root)
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		for _, state := range states {
			texts = append(texts, state.LastError)
		}
		for _, text := range texts {
			if strings.Contains(text, secret) {
				t.Fatalf("%s: the expanded URL is reported: %s", stage, text)
			}
		}
		if !strings.Contains(err.Error(), "{env:LCX_TEST_MCP_URL_KEY}") {
			t.Fatalf("%s: error = %v, want the configured URL", stage, err)
		}
	}
	_, err := registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "down"})
	reported("call on the running client", err)
	if err := registry.Stop("remote", root); err != nil {
		t.Fatal(err)
	}
	_, err = registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "down"})
	reported("start", err)

	// A value that makes the expanded URL unparsable: the parse error names the configured URL, never
	// the expanded one or the part of it the parser quotes (a userinfo, a port).
	for name, tc := range map[string]struct{ url, value string }{
		"invalid userinfo": {"http://bot:{env:LCX_TEST_MCP_URL_PART}@127.0.0.1:1/mcp", "sk^live|url-7f3a9c"},
		"invalid port":     {"http://127.0.0.1:{env:LCX_TEST_MCP_URL_PART}/mcp", "1sk-url-7f3a9c"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LCX_TEST_MCP_URL_PART", tc.value)
			registry, root := httpRegistry(t, Config{ID: "remote", URL: tc.url})
			_, err := registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "x"})
			if err == nil {
				t.Fatal("an unparsable expanded URL must fail")
			}
			texts := []string{err.Error()}
			states, stateErr := registry.States(root)
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			for _, state := range states {
				texts = append(texts, state.LastError)
			}
			statuses, statusErr := registry.Statuses(root)
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			for _, status := range statuses {
				texts = append(texts, status.LastError)
			}
			for _, text := range texts {
				if strings.Contains(text, "url-7f3a9c") {
					t.Fatalf("the expanded value is reported: %s", text)
				}
			}
			if !strings.Contains(err.Error(), tc.url) {
				t.Fatalf("error = %v, want the configured URL %s", err, tc.url)
			}
		})
	}
}

// resetServer is httpTestServer with two calls that hang up the connection: "reset" at once and
// "slow_reset" when the test releases it, so a transport failure arrives exactly when a test wants it.
type resetServer struct {
	httpTestServer
	inits       atomic.Int32
	slowSeen    chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (s *resetServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &msg)
	if msg.Method == "initialize" {
		s.inits.Add(1)
	}
	if msg.Method == "tools/call" {
		switch msg.Params.Name {
		case "slow_reset":
			close(s.slowSeen)
			<-s.release
			hangUp(w)
			return
		case "reset":
			hangUp(w)
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.httpTestServer.ServeHTTP(w, r)
}

func (s *resetServer) releaseSlow() { s.releaseOnce.Do(func() { close(s.release) }) }

// hangUp closes the connection without a response.
func hangUp(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// ADR-0021 D3: a transport failure drops the client the failed call used, and only that one. A call
// that was in flight on a crashed client and fails after the server was restarted leaves the healthy
// restarted client running.
func TestMCPLateTransportFailureKeepsRestartedClient(t *testing.T) {
	handler := &resetServer{slowSeen: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// Cleanups run last-in first-out: the held call is released before the server waits for it.
	t.Cleanup(handler.releaseSlow)
	registry, root := httpRegistry(t, Config{ID: "remote", URL: server.URL})
	ctx := context.Background()
	if _, err := registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "c1"}); err != nil {
		t.Fatal(err)
	}

	late := make(chan error, 1)
	go func() {
		_, err := registry.CallTool(ctx, root, "remote", "slow_reset", map[string]any{})
		late <- err
	}()
	<-handler.slowSeen
	if _, err := registry.CallTool(ctx, root, "remote", "reset", map[string]any{}); err == nil {
		t.Fatal("a hung-up call must fail")
	}
	if statuses, err := registry.Statuses(root); err != nil || len(statuses) != 0 {
		t.Fatalf("after the first failure statuses=%#v err=%v, want the client dropped", statuses, err)
	}
	if _, err := registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "c2"}); err != nil {
		t.Fatal(err)
	}
	if inits := handler.inits.Load(); inits != 2 {
		t.Fatalf("initialize count after the restart = %d, want 2", inits)
	}

	handler.releaseSlow()
	if err := <-late; err == nil {
		t.Fatal("the call on the crashed client must fail")
	}
	statuses, err := registry.Statuses(root)
	if err != nil || len(statuses) != 1 || !statuses[0].Running {
		t.Fatalf("after the late failure statuses=%#v err=%v, want the restarted client running", statuses, err)
	}
	if _, err := registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "c2 again"}); err != nil {
		t.Fatal(err)
	}
	if inits := handler.inits.Load(); inits != 2 {
		t.Fatalf("initialize count after the late failure = %d, want 2 (the restarted client was dropped)", inits)
	}
}

// ADR-0021 §3: a tool outside the server's include list is refused by the call itself, before the
// server is reached, whatever list a run was built from.
func TestMCPCallRefusesExcludedTool(t *testing.T) {
	handler, server := startHTTPTestServer(t, false)
	registry, root := httpRegistry(t, Config{ID: "remote", URL: server.URL, Include: []string{"echo"}, CallTimeoutSeconds: 1})
	ctx := context.Background()
	_, err := registry.CallTool(ctx, root, "remote", "hang", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "include") {
		t.Fatalf("excluded tool error = %v, want a refusal naming the include list", err)
	}
	if calls := handler.calls.Load(); calls != 0 {
		t.Fatalf("the excluded tool reached the server (%d calls)", calls)
	}
	if _, err := registry.CallTool(ctx, root, "remote", "echo", map[string]any{"text": "in"}); err != nil {
		t.Fatal(err)
	}
}
