package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
