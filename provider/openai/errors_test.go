package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

func request() protocol.ProviderRequest {
	return protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}
}

func providerError(t *testing.T, err error) *protocol.ProviderError {
	t.Helper()
	var pe *protocol.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error %T %v is not a *protocol.ProviderError", err, err)
	}
	return pe
}

// The adapter table of design §4.5: every failed Complete is a ProviderError whose class follows the status.
func TestAdapterClassifiesHTTPStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   protocol.ErrorClass
	}{
		{"413", 413, "too big", protocol.ClassCapacity},
		{"400 context marker", 400, `{"error":{"code":"context_length_exceeded"}}`, protocol.ClassCapacity},
		{"400 maximum context", 400, "This model's Maximum Context Length is 8192 tokens", protocol.ClassCapacity},
		{"400 gateway content too long", 400, `{"error":{"message":"Content too long. Please shorten it or start a new task","type":"invalid_request_error"}}`, protocol.ClassCapacity},
		{"400 plain", 400, `{"error":"unknown parameter"}`, protocol.ClassBadRequest},
		{"401", 401, "no", protocol.ClassAuth},
		{"403", 403, "no", protocol.ClassAuth},
		{"404", 404, "no", protocol.ClassBadRequest},
		{"422", 422, "no", protocol.ClassBadRequest},
		{"408", 408, "slow", protocol.ClassAvailability},
		{"409", 409, "busy", protocol.ClassAvailability},
		{"429", 429, "rate", protocol.ClassAvailability},
		{"500", 500, "boom", protocol.ClassAvailability},
		{"503", 503, "down", protocol.ClassAvailability},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true, MaxRetries: 2, RetryBaseDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Complete(context.Background(), request())
			pe := providerError(t, err)
			if pe.Class != c.want {
				t.Fatalf("class=%s, want %s (%v)", pe.Class, c.want, err)
			}
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != c.status {
				t.Fatalf("the HTTP error is not reachable through the ProviderError: %v", err)
			}
			wantHits := int32(1)
			if c.want == protocol.ClassAvailability {
				wantHits = 3 // availability errors are retried: 1 + MaxRetries
			}
			if got := hits.Load(); got != wantHits {
				t.Fatalf("requests=%d, want %d", got, wantHits)
			}
		})
	}
}

// Retry-After above the in-adapter cap is reported at once instead of slept through (F8); the class
// carries the delay so the health registry can record it.
func TestRetryAfterBeyondCapReturnsImmediately(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.Complete(context.Background(), request())
	pe := providerError(t, err)
	if pe.Class != protocol.ClassAvailability || pe.RetryAfter != time.Hour {
		t.Fatalf("class=%s retryAfter=%v", pe.Class, pe.RetryAfter)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second || hits.Load() != 1 {
		t.Fatalf("elapsed=%v requests=%d: the adapter waited or retried", elapsed, hits.Load())
	}
}

// A Retry-After inside the cap is honoured before the retry.
func TestRetryAfterWithinCapIsHonoured(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true, RetryBaseDelay: time.Millisecond})
	start := time.Now()
	if _, err := client.Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || hits.Load() != 2 {
		t.Fatalf("elapsed=%v requests=%d, want the 1 s Retry-After before the second request", elapsed, hits.Load())
	}
}

func TestTransportErrorIsAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	client, _ := New(Config{BaseURL: url, Model: "m", DisableStreaming: true, DisableRetries: true})
	_, err := client.Complete(context.Background(), request())
	if pe := providerError(t, err); pe.Class != protocol.ClassAvailability {
		t.Fatalf("class=%s", pe.Class)
	}
}

func TestMalformedStreamIsAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {not json\n\n")
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true})
	_, err := client.Complete(context.Background(), request())
	if pe := providerError(t, err); pe.Class != protocol.ClassAvailability {
		t.Fatalf("class=%s", pe.Class)
	}
}

// A stream the server ends early is not an answer: an in-band error event, a stream that stops with
// neither a finish_reason nor [DONE], or a finish_reason by which the provider says it interrupted the
// generation (DeepSeek: insufficient_system_resource, aborted) is an availability error and is retried,
// never returned as a response with the tool calls received so far (TB2 W13: a gateway
// "upstream_error" cut a response after its first complete tool call, and the call ran).
func TestTruncatedStreamIsAvailability(t *testing.T) {
	call := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"rm -rf /tmp/x\"}"}}]}}]}` + "\n\n"
	cut := `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"apply_patch","arguments":"{\"patches\": [{\"pa"}}]}}]}` + "\n\n"
	cases := []struct {
		name, tail, wantErr string
	}{
		{"error event", `data: {"error":{"message":"Upstream request failed","type":"upstream_error"}}` + "\n\n", "Upstream request failed"},
		{"error event without a message", `data: {"error":{}}` + "\n\n", "stream error"},
		{"connection closed mid-response", "", "ended before"},
		{"insufficient_system_resource", `data: {"choices":[{"delta":{},"finish_reason":"insufficient_system_resource"}]}` + "\n\ndata: [DONE]\n\n", "insufficient_system_resource"},
		{"aborted", `data: {"choices":[{"delta":{},"finish_reason":"aborted"}]}` + "\n\ndata: [DONE]\n\n", "aborted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, call+cut+c.tail)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, Model: "m", MaxRetries: 2, RetryBaseDelay: time.Millisecond})
			resp, err := client.Complete(context.Background(), request())
			if err == nil {
				t.Fatalf("a cut stream was returned as a response: %+v", resp.Message.ToolCalls)
			}
			pe := providerError(t, err)
			if pe.Class != protocol.ClassAvailability || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("class=%s err=%v", pe.Class, err)
			}
			if hits.Load() != 3 {
				t.Fatalf("attempts=%d, want 3 (retried as availability)", hits.Load())
			}
		})
	}
}

// The same interrupted finish in a non-streaming response is an availability error, not a response.
func TestInterruptedFinishWithoutStreamingIsAvailability(t *testing.T) {
	for _, reason := range []string{"insufficient_system_resource", "aborted"} {
		t.Run(reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"The answer is","tool_calls":[{"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"rm -rf /tmp/x\"}"}}]},"finish_reason":%q}]}`, reason)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true, DisableRetries: true})
			resp, err := client.Complete(context.Background(), request())
			if err == nil {
				t.Fatalf("an interrupted generation was returned as a response: %+v", resp.Message)
			}
			if pe := providerError(t, err); pe.Class != protocol.ClassAvailability || !strings.Contains(err.Error(), reason) {
				t.Fatalf("class=%s err=%v", pe.Class, err)
			}
		})
	}
}

// The end of a stream is its finish_reason or [DONE]; either one alone completes it.
func TestStreamEndsAtFinishReasonOrDone(t *testing.T) {
	cases := map[string]string{
		"finish_reason, connection closed without [DONE]": `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n",
		"[DONE] without finish_reason":                    `data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n" + "data: [DONE]\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true})
			resp, err := client.Complete(context.Background(), request())
			if err != nil || resp.Message.Content != "ok" {
				t.Fatalf("resp=%+v err=%v", resp.Message, err)
			}
		})
	}
}

func timeoutOf(t *testing.T, err error) *TimeoutError {
	t.Helper()
	var timeout *TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error %v does not carry a *TimeoutError", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a TimeoutError must not unwrap to a context error (F2): %v", err)
	}
	return timeout
}

// I6: each timeout is an availability error that fails over while the run context is alive.
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
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
				flushing(w).Flush()
				<-r.Context().Done()
			}, "idle"},
		{"request cap while chunks keep arriving", Config{RequestTimeout: 250 * time.Millisecond, IdleTimeout: 100 * time.Millisecond},
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for r.Context().Err() == nil {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
					flushing(w).Flush()
					time.Sleep(20 * time.Millisecond)
				}
			}, "request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
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
			pe := providerError(t, err)
			if pe.Class != protocol.ClassAvailability {
				t.Fatalf("class=%s, want availability: %v", pe.Class, err)
			}
			if got := timeoutOf(t, err); got.Kind != c.kind {
				t.Fatalf("timeout kind=%s, want %s", got.Kind, c.kind)
			}
		})
	}
}

// A steady stream never trips the idle timeout: every read that returns bytes resets it.
func TestIdleTimeoutResetsOnEveryRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 8; i++ {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(40 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true, IdleTimeout: 150 * time.Millisecond})
	resp, err := client.Complete(context.Background(), request())
	if err != nil || resp.Message.Content != "aaaaaaaa" {
		t.Fatalf("resp=%+v err=%v", resp.Message, err)
	}
}

// Timeouts are retried like other availability errors, with the retry budget of the client.
func TestTimeoutsAreRetried(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true, IdleTimeout: 40 * time.Millisecond, MaxRetries: 2, RetryBaseDelay: time.Millisecond})
	_, err := client.Complete(context.Background(), request())
	timeoutOf(t, err)
	if hits.Load() != 3 {
		t.Fatalf("requests=%d, want 3", hits.Load())
	}
}

// Cancelling the run is cancelled, never a timeout, and aborts at once even with timeouts armed.
func TestCancellationIsNotATimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only after the body is read
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true, IdleTimeout: time.Minute, RequestTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	start := time.Now()
	_, err := client.Complete(ctx, request())
	pe := providerError(t, err)
	if pe.Class != protocol.ClassCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("class=%s err=%v", pe.Class, err)
	}
	var timeout *TimeoutError
	if errors.As(err, &timeout) {
		t.Fatalf("cancellation reported as a timeout: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation was not prompt")
	}

	// Cancelled before the call: no request leaves.
	dead, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := client.Complete(dead, request()); providerError(t, err).Class != protocol.ClassCancelled {
		t.Fatalf("pre-cancelled: %v", err)
	}
}

func TestFromModelUsesResolvedSettings(t *testing.T) {
	var gotAuth, gotHeader, gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotHeader = r.Header.Get("Authorization"), r.Header.Get("X-Org")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	t.Setenv("FROM_MODEL_KEY", "k-123")
	m := config.ResolvedModel{
		Ref: "p/m", ProviderID: "p", ModelID: "upstream", BaseURL: server.URL, APIKeyRef: "{env:FROM_MODEL_KEY}",
		Headers: map[string]string{"X-Org": "acme"}, ReasoningFormat: config.ReasoningNone, IdleTimeout: time.Minute,
	}
	client, err := FromModel(m, Options{DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	gotModel = client.Model()
	if _, err := client.Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k-123" || gotHeader != "acme" || gotModel != "upstream" {
		t.Fatalf("auth=%q header=%q model=%q", gotAuth, gotHeader, gotModel)
	}
	if client.httpClient.Timeout != 0 {
		t.Fatalf("the factory set http.Client.Timeout=%v; timeouts belong to the adapter", client.httpClient.Timeout)
	}

	// A missing key variable fails before anything is built, naming only the variable.
	t.Setenv("FROM_MODEL_KEY", "")
	if _, err := FromModel(m, Options{}); err == nil || !strings.Contains(err.Error(), "FROM_MODEL_KEY") || strings.Contains(err.Error(), "k-123") {
		t.Fatalf("missing key accepted: %v", err)
	}
	m.BaseURL = ""
	t.Setenv("FROM_MODEL_KEY", "k-123")
	if _, err := FromModel(m, Options{}); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("no endpoint: %v", err)
	}
	_ = gotModel
}
