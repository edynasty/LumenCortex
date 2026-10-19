package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

func request() protocol.ProviderRequest {
	return protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}
}

// There is no default URL: a client without an endpoint or baseURL is refused at construction, so
// its key and the transcript can never reach a host nobody configured.
func TestNewRequiresEndpointOrBaseURL(t *testing.T) {
	for name, cfg := range map[string]Config{
		"empty":      {Model: "m", APIKey: "sk-secret"},
		"blank":      {Model: "m", APIKey: "sk-secret", BaseURL: "  /", Endpoint: " "},
		"only-trail": {Model: "m", APIKey: "sk-secret", BaseURL: "///"},
	} {
		client, err := New(cfg)
		if !errors.Is(err, ErrNoEndpoint) || client != nil {
			t.Fatalf("%s: client=%v err=%v", name, client, err)
		}
		if strings.Contains(err.Error(), "sk-secret") {
			t.Fatalf("%s: error leaks the key: %v", name, err)
		}
	}
	for name, cfg := range map[string]Config{
		"baseURL":  {Model: "m", BaseURL: "http://127.0.0.1:1/v1"},
		"endpoint": {Model: "m", Endpoint: "http://127.0.0.1:1/x"},
	} {
		if _, err := New(cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The Messages API authenticates with x-api-key and the version header, never a Bearer token;
// configured headers override the defaults, the version included.
func TestHeadersAuthAndOverride(t *testing.T) {
	var gotKey, gotVersion, gotAuth, gotPath, gotExtra string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Org")
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL: server.URL, APIKey: "sk-ant-secret", Model: "m",
		Headers: map[string]string{"X-Org": "acme"}, DisableStreaming: true, DisableRetries: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path=%q, want the baseURL + /v1/messages", gotPath)
	}
	if gotKey != "sk-ant-secret" || gotAuth != "" {
		t.Fatalf("x-api-key=%q Authorization=%q", gotKey, gotAuth)
	}
	if gotVersion != defaultAnthropicVersion {
		t.Fatalf("anthropic-version=%q", gotVersion)
	}
	if gotExtra != "acme" {
		t.Fatalf("configured header=%q", gotExtra)
	}

	// The configured headers win over the defaults, the version included.
	client, err = New(Config{
		BaseURL: server.URL, APIKey: "sk-ant-secret", Model: "m",
		Headers: map[string]string{"anthropic-version": "2023-01-01"}, DisableStreaming: true, DisableRetries: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if gotVersion != "2023-01-01" {
		t.Fatalf("overridden anthropic-version=%q", gotVersion)
	}
}

// A streamed tool call assembles from interleaved blocks: thinking with signature deltas, text
// split across chunks, and a tool_use input assembled from input_json_delta fragments. The usage
// of message_start and message_delta combine, ping is ignored, and message_stop ends the stream.
func TestStreamingAssembly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream not requested: %v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		write := func(s string) { fmt.Fprint(w, s) }
		write("event: message_start\n")
		write(`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"cache_read_input_tokens":4}}}` + "\n\n")
		write("event: ping\n")
		write(`data: {"type":"ping"}` + "\n\n")
		write(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}` + "\n\n")
		write(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}` + "\n\n")
		write(`data: {"type":"content_block_stop","index":0}` + "\n\n")
		write(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n")
		write(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the answer"}}` + "\n\n")
		write(`data: {"type":"content_block_stop","index":1}` + "\n\n")
		write(`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_1","name":"shell"}}` + "\n\n")
		write(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"go"}}` + "\n\n")
		write(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":" test ./...\"}"}}` + "\n\n")
		write(`data: {"type":"content_block_stop","index":2}` + "\n\n")
		write(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":13}}` + "\n\n")
		write(`data: {"type":"message_stop"}` + "\n\n")
		// The connection stays open after message_stop: the reader must not wait for it.
		flusher.Flush()
		time.Sleep(250 * time.Millisecond)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var response protocol.ProviderResponse
	var completeErr error
	go func() {
		defer close(done)
		response, completeErr = client.Complete(context.Background(), request())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader waited for the connection to close after message_stop")
	}
	if completeErr != nil {
		t.Fatal(completeErr)
	}
	if response.Message.Content != "the answer" {
		t.Fatalf("content=%q", response.Message.Content)
	}
	if len(response.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls=%v", response.Message.ToolCalls)
	}
	call := response.Message.ToolCalls[0]
	if call.ID != "tool_1" || call.Name != "shell" || string(call.Arguments) != `{"command":"go test ./..."}` {
		t.Fatalf("call=%v", call)
	}
	if response.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", response.FinishReason)
	}
	if response.Usage.PromptTokens != 25 || response.Usage.CompletionTokens != 13 || response.Usage.TotalTokens != 38 {
		t.Fatalf("usage=%+v", response.Usage)
	}
	if response.Echo.Kind() != echoThinking {
		t.Fatalf("echo=%v", response.Echo)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(response.Echo.Data(), &blocks); err != nil || len(blocks) != 1 {
		t.Fatalf("echo data=%s", response.Echo.Data())
	}
	if blocks[0]["type"] != "thinking" || blocks[0]["thinking"] != "pondering" || blocks[0]["signature"] != "sig-abc" {
		t.Fatalf("reassembled thinking block=%v", blocks[0])
	}
}

// The stream reader tolerates SSE comments and an event whose bytes arrive in fragments.
func TestStreamingSplitChunksAndComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		event := `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"assembled"}}` + "\n\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
			`data: {"type":"message_stop"}` + "\n\n"
		fmt.Fprint(w, ": keepalive\n\n")
		w.(http.Flusher).Flush()
		// The events arrive in small pieces, as a slow connection would.
		for len(event) > 0 {
			n := 7
			if n > len(event) {
				n = len(event)
			}
			fmt.Fprint(w, event[:n])
			w.(http.Flusher).Flush()
			event = event[n:]
		}
	}))
	defer server.Close()

	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true})
	response, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "assembled" || response.FinishReason != "stop" {
		t.Fatalf("response=%+v", response)
	}
}

// A redacted_thinking block streams its opaque data in the block start and is echoed back in the
// same shape the API would return in a non-streamed response.
func TestStreamingRedactedThinking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"encrypted-blob"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"visible"}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer server.Close()

	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true})
	response, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(response.Echo.Data(), &blocks); err != nil || len(blocks) != 1 {
		t.Fatalf("echo data=%s", response.Echo.Data())
	}
	if blocks[0]["type"] != "redacted_thinking" || blocks[0]["data"] != "encrypted-blob" {
		t.Fatalf("redacted block=%v", blocks[0])
	}
}

func TestNonStreamingResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test", DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "done" || response.Usage.TotalTokens != 4 {
		t.Fatalf("response=%+v", response)
	}
}

func TestRetries429AndHonorsBoundedAttempts(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := attempts.Add(1)
		if current < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"type":"error","error":{"type":"rate_limit_error","message":"busy"}}`, http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Model: "test", DisableStreaming: true, MaxRetries: 2, RetryBaseDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" || attempts.Load() != 3 {
		t.Fatalf("response=%+v attempts=%d", response, attempts.Load())
	}
}

func TestResponseBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 2048))
	}))
	defer server.Close()

	client, _ := New(Config{BaseURL: server.URL, Model: "test", DisableStreaming: true, DisableRetries: true, MaxResponseBytes: 1024})
	if _, err := client.Complete(context.Background(), request()); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err=%v", err)
	}
}

// The idle and request timeouts are availability errors, and a stream that keeps arriving stops
// at the request cap.
func TestIdleAndRequestTimeouts(t *testing.T) {
	flushing := func(w http.ResponseWriter) http.Flusher { return w.(http.Flusher) }
	cases := []struct {
		name    string
		cfg     Config
		handler http.HandlerFunc
		kind    string
	}{
		{"idle before headers", Config{IdleTimeout: 80 * time.Millisecond, DisableStreaming: true},
			func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, "idle"},
		{"idle between stream chunks", Config{IdleTimeout: 120 * time.Millisecond},
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
				flushing(w).Flush()
				<-r.Context().Done()
			}, "idle"},
		{"request cap while events keep arriving", Config{RequestTimeout: 250 * time.Millisecond, IdleTimeout: 100 * time.Millisecond},
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for r.Context().Err() == nil {
					fmt.Fprint(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
					flushing(w).Flush()
					time.Sleep(20 * time.Millisecond)
				}
			}, "request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only after the body is read
				c.handler(w, r)
			}))
			defer server.Close()
			cfg := c.cfg
			cfg.BaseURL, cfg.Model, cfg.DisableRetries = server.URL, "m", true
			client, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, err = client.Complete(context.Background(), request())
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("timeout took %v", elapsed)
			}
			var pe *protocol.ProviderError
			if !errors.As(err, &pe) || pe.Class != protocol.ClassAvailability {
				t.Fatalf("class of %v", err)
			}
			var timeout *TimeoutError
			if !errors.As(err, &timeout) || timeout.Kind != c.kind {
				t.Fatalf("timeout of %v", err)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a TimeoutError must not unwrap to a context error: %v", err)
			}
		})
	}
}
