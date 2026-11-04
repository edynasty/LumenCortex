package anthropic

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

	"github.com/edynasty/LumenCortex/protocol"
)

func providerError(t *testing.T, err error) *protocol.ProviderError {
	t.Helper()
	var pe *protocol.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *protocol.ProviderError", err)
	}
	return pe
}

// The adapter table of design §4.5 with the Messages API's status codes and error bodies: every
// failed Complete is a ProviderError whose class and reason follow the status, 529 (overloaded)
// included. Complete is one attempt: the agent loop owns retrying.
func TestAdapterClassifiesHTTPStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   protocol.ErrorClass
		reason protocol.Reason
	}{
		{"413", 413, "payload too large", protocol.ClassCapacity, ""},
		{"400 prompt too long", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210744 tokens > 200000 maximum"}}`, protocol.ClassCapacity, ""},
		{"400 input and max_tokens exceed limit", 400, "input length and `max_tokens` exceed context limit", protocol.ClassCapacity, ""},
		{"400 plain invalid_request", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: field required"}}`, protocol.ClassBadRequest, ""},
		{"401 authentication", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, protocol.ClassAuth, ""},
		{"403 permission", 403, `{"type":"error","error":{"type":"permission_error","message":"not allowed"}}`, protocol.ClassAuth, ""},
		{"404 not found", 404, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`, protocol.ClassBadRequest, ""},
		{"422 unprocessable", 422, "no", protocol.ClassBadRequest, ""},
		{"408 timeout", 408, "slow", protocol.ClassAvailability, protocol.ReasonTimeout},
		{"409 conflict", 409, "busy", protocol.ClassAvailability, protocol.ReasonConflict},
		{"429 rate limit", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}`, protocol.ClassAvailability, protocol.ReasonRateLimited},
		{"499 client closed", 499, "gone", protocol.ClassBadRequest, ""},
		{"500 api error", 500, `{"type":"error","error":{"type":"api_error","message":"internal"}}`, protocol.ClassAvailability, protocol.ReasonServerError},
		{"502 bad gateway", 502, "bad gateway", protocol.ClassAvailability, protocol.ReasonServerError},
		{"503 unavailable", 503, "down", protocol.ClassAvailability, protocol.ReasonOverloaded},
		{"529 overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, protocol.ClassAvailability, protocol.ReasonOverloaded},
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
			client, err := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Complete(context.Background(), request())
			pe := providerError(t, err)
			if pe.Class != c.want || pe.Reason != c.reason || pe.Status != c.status {
				t.Fatalf("class=%s reason=%q status=%d, want %s %q %d (%v)", pe.Class, pe.Reason, pe.Status, c.want, c.reason, c.status, err)
			}
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != c.status {
				t.Fatalf("the HTTP error is not reachable through the ProviderError: %v", err)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("requests=%d, want 1: the adapter never retries", got)
			}
		})
	}
}

// An error event the API sends mid-stream classifies like the same error over HTTP.
func TestStreamErrorEventsClassify(t *testing.T) {
	cases := []struct {
		payload string
		want    protocol.ErrorClass
		reason  protocol.Reason
	}{
		{`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, protocol.ClassAvailability, protocol.ReasonOverloaded},
		{`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, protocol.ClassAvailability, protocol.ReasonRateLimited},
		{`{"type":"error","error":{"type":"api_error","message":"upstream"}}`, protocol.ClassAvailability, protocol.ReasonServerError},
		{`{"type":"error","error":{"type":"timeout_error","message":"slow"}}`, protocol.ClassAvailability, protocol.ReasonTimeout},
		{`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, protocol.ClassAuth, ""},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"bad shape"}}`, protocol.ClassBadRequest, ""},
		{`{"type":"error"}`, protocol.ClassBadRequest, ""},
	}
	for _, c := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+c.payload+"\n\n")
		}))
		client, _ := New(Config{BaseURL: server.URL, Model: "m"})
		_, err := client.Complete(context.Background(), request())
		server.Close()
		pe := providerError(t, err)
		if pe.Class != c.want || pe.Reason != c.reason {
			t.Fatalf("payload %s: class=%s reason=%q want %s %q (%v)", c.payload, pe.Class, pe.Reason, c.want, c.reason, err)
		}
	}
}

// Retry-After is reported, never slept through: the loop waits it out under the run's deadline.
func TestRetryAfterIsReportedNotSlept(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.Header().Set("retry-after-ms", "1500")
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
	if pe.Class != protocol.ClassAvailability || pe.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("class=%s retryAfter=%v, want retry-after-ms to win", pe.Class, pe.RetryAfter)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second || hits.Load() != 1 {
		t.Fatalf("elapsed=%v requests=%d: the adapter waited or retried", elapsed, hits.Load())
	}
}

func TestTransportErrorIsAvailability(t *testing.T) {
	client, _ := New(Config{BaseURL: "http://127.0.0.1:1", Model: "m"})
	_, err := client.Complete(context.Background(), request())
	if pe := providerError(t, err); pe.Class != protocol.ClassAvailability || pe.Reason != protocol.ReasonTransport {
		t.Fatalf("class=%s reason=%q", pe.Class, pe.Reason)
	}
}

// A body that is not the Messages JSON shape is a malformed stream: availability, so the next
// candidate may answer.
func TestMalformedStreamIsAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json at all")
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true})
	_, err := client.Complete(context.Background(), request())
	if pe := providerError(t, err); pe.Class != protocol.ClassAvailability || pe.Reason != protocol.ReasonMalformedResponse {
		t.Fatalf("class=%s reason=%q (%v)", pe.Class, pe.Reason, err)
	}
}

// A stream that ends before message_stop is cut, not finished: it is an availability error, never a
// response carrying the blocks received so far (a complete tool_use among them would otherwise run).
func TestStreamWithoutMessageStopIsAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"shell","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"rm -rf /tmp/x\"}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t2","name":"apply_patch","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"patches\": [{\"pa"}}`,
		} {
			fmt.Fprint(w, "data: "+event+"\n\n")
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m"})
	resp, err := client.Complete(context.Background(), request())
	if err == nil {
		t.Fatalf("a cut stream was returned as a response: %+v", resp.Message.ToolCalls)
	}
	if pe := providerError(t, err); pe.Class != protocol.ClassAvailability || !strings.Contains(err.Error(), "ended before") {
		t.Fatalf("class=%s (%v)", pe.Class, err)
	}
}

// Cancelling the run is cancelled, never a timeout, aborts the in-flight stream at once, and
// leaves no goroutine behind.
func TestCancellationMidStream(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only after the body is read
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`+"\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", IdleTimeout: time.Minute, RequestTimeout: time.Minute})

	before := goroutines()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	start := time.Now()
	_, err := client.Complete(ctx, request())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatal("cancellation was not prompt")
	}
	pe := providerError(t, err)
	if pe.Class != protocol.ClassCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("class=%s err=%v", pe.Class, err)
	}
	var timeout *TimeoutError
	if errors.As(err, &timeout) {
		t.Fatalf("cancellation reported as a timeout: %v", err)
	}
	// The aborted request is fully unwound: no goroutine of the adapter outlives it.
	if leak := waitForGoroutines(before, 2*time.Second); leak > 0 {
		t.Fatalf("%d goroutines leaked after the cancelled stream", leak)
	}

	// Cancelled before the call: no request leaves.
	dead, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := client.Complete(dead, request()); providerError(t, err).Class != protocol.ClassCancelled {
		t.Fatalf("pre-cancelled: %v", err)
	}
}

// The error text never contains the API key: not the construction errors, and not an error body
// a hostile or logging gateway echoes the key back in.
func TestErrorsNeverContainTheKey(t *testing.T) {
	const key = "sk-ant-9f86d081- SECRET"
	_, err := New(Config{Model: "m", APIKey: key})
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("no-endpoint error: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":"saw key %s and header %s"}}`, key, r.Header.Get("anthropic-version"))
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", APIKey: key, DisableStreaming: true})
	_, err = client.Complete(context.Background(), request())
	if err == nil {
		t.Fatal("the hostile response was accepted")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the error leaks the key: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("the echoed key was not redacted: %v", err)
	}
}

// stop_reason "refusal" is a safety classifier that declined the reply, possibly after part of it
// streamed: the reply is not one, streaming or not, so nothing it holds is returned, a tool call
// included. A retry of the same request would meet it again, so it is a bad request and not retried.
func TestWithheldReplyIsNotAResponse(t *testing.T) {
	cases := map[string]struct {
		stream bool
		body   string
	}{
		"stream": {true, sseEvents(
			`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Sure, "}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"shell","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"x\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`)},
		"non-stream": {false, `{"content":[{"type":"text","text":"Sure, "},{"type":"tool_use","id":"t1","name":"shell","input":{"command":"x"}}],"stop_reason":"refusal","usage":{"input_tokens":3,"output_tokens":5}}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if c.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				fmt.Fprint(w, c.body)
			}))
			defer server.Close()
			client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: !c.stream})
			resp, err := client.Complete(context.Background(), request())
			if err == nil {
				t.Fatalf("a refused reply was returned as a response: %+v", resp.Message)
			}
			if pe := providerError(t, err); pe.Class != protocol.ClassBadRequest || !strings.Contains(err.Error(), "refusal") {
				t.Fatalf("class=%s err=%v", pe.Class, err)
			}
			if hits.Load() != 1 {
				t.Fatalf("attempts=%d, want 1 (a refused reply is not retried)", hits.Load())
			}
		})
	}
}

// A tool_use block with no input is the empty object only after a reply the model ended itself
// (end_turn, tool_use, stop_sequence); after max_tokens, a cut at the context window or a stop reason
// the adapter does not know, it is an empty JSON string, which the loop never runs.
func TestMissingInputIsAnObjectOnlyAfterANormalStop(t *testing.T) {
	cases := map[string]string{
		"tool_use":                      `{}`,
		"end_turn":                      `{}`,
		"max_tokens":                    `""`,
		"model_context_window_exceeded": `""`,
		"brand_new_reason":              `""`,
	}
	for reason, want := range cases {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%v", reason, stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, sseEvents(
							`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
							`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"shell","input":{}}}`,
							`{"type":"content_block_stop","index":0}`,
							`{"type":"message_delta","delta":{"stop_reason":"`+reason+`"},"usage":{"output_tokens":5}}`,
							`{"type":"message_stop"}`))
						return
					}
					fmt.Fprintf(w, `{"content":[{"type":"tool_use","id":"t1","name":"shell"}],"stop_reason":%q,"usage":{"input_tokens":3,"output_tokens":5}}`, reason)
				}))
				defer server.Close()
				client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: !stream})
				resp, err := client.Complete(context.Background(), request())
				if err != nil {
					t.Fatal(err)
				}
				if len(resp.Message.ToolCalls) != 1 || string(resp.Message.ToolCalls[0].Arguments) != want {
					t.Fatalf("tool calls = %+v, want arguments %s", resp.Message.ToolCalls, want)
				}
			})
		}
	}
}

// sseEvents frames each event as one SSE data line.
func sseEvents(events ...string) string {
	var out strings.Builder
	for _, event := range events {
		out.WriteString("data: " + event + "\n\n")
	}
	return out.String()
}
