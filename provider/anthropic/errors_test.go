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
// failed Complete is a ProviderError whose class follows the status, 529 (overloaded) included.
func TestAdapterClassifiesHTTPStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   protocol.ErrorClass
	}{
		{"413", 413, "payload too large", protocol.ClassCapacity},
		{"400 prompt too long", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210744 tokens > 200000 maximum"}}`, protocol.ClassCapacity},
		{"400 input and max_tokens exceed limit", 400, "input length and `max_tokens` exceed context limit", protocol.ClassCapacity},
		{"400 plain invalid_request", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: field required"}}`, protocol.ClassBadRequest},
		{"401 authentication", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, protocol.ClassAuth},
		{"403 permission", 403, `{"type":"error","error":{"type":"permission_error","message":"not allowed"}}`, protocol.ClassAuth},
		{"404 not found", 404, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`, protocol.ClassBadRequest},
		{"422 unprocessable", 422, "no", protocol.ClassBadRequest},
		{"408 timeout", 408, "slow", protocol.ClassAvailability},
		{"409 conflict", 409, "busy", protocol.ClassAvailability},
		{"429 rate limit", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}`, protocol.ClassAvailability},
		{"499 client closed", 499, "gone", protocol.ClassBadRequest},
		{"500 api error", 500, `{"type":"error","error":{"type":"api_error","message":"internal"}}`, protocol.ClassAvailability},
		{"503 unavailable", 503, "down", protocol.ClassAvailability},
		{"529 overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, protocol.ClassAvailability},
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

// An error event the API sends mid-stream classifies like the same error over HTTP.
func TestStreamErrorEventsClassify(t *testing.T) {
	cases := []struct {
		payload string
		want    protocol.ErrorClass
	}{
		{`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, protocol.ClassAvailability},
		{`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, protocol.ClassAvailability},
		{`{"type":"error","error":{"type":"api_error","message":"upstream"}}`, protocol.ClassAvailability},
		{`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, protocol.ClassAuth},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"bad shape"}}`, protocol.ClassBadRequest},
		{`{"type":"error"}`, protocol.ClassBadRequest},
	}
	for _, c := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+c.payload+"\n\n")
		}))
		client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true})
		_, err := client.Complete(context.Background(), request())
		server.Close()
		pe := providerError(t, err)
		if pe.Class != c.want {
			t.Fatalf("payload %s: class=%s want %s (%v)", c.payload, pe.Class, c.want, err)
		}
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
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true})
	start := time.Now()
	response, err := client.Complete(context.Background(), request())
	if err != nil || response.Message.Content != "ok" {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("retry after %v: the Retry-After was not honoured", elapsed)
	}
}

func TestTransportErrorIsAvailability(t *testing.T) {
	client, _ := New(Config{BaseURL: "http://127.0.0.1:1", Model: "m", DisableRetries: true})
	_, err := client.Complete(context.Background(), request())
	if pe := providerError(t, err); pe.Class != protocol.ClassAvailability {
		t.Fatalf("class=%s", pe.Class)
	}
}

// A body that is not the Messages JSON shape is a malformed stream: availability, so the next
// candidate may answer.
func TestMalformedStreamIsAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json at all")
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableStreaming: true, DisableRetries: true})
	_, err := client.Complete(context.Background(), request())
	if pe := providerError(t, err); pe.Class != protocol.ClassAvailability {
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
	client, _ := New(Config{BaseURL: server.URL, Model: "m", DisableRetries: true, IdleTimeout: time.Minute, RequestTimeout: time.Minute})

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
	client, _ := New(Config{BaseURL: server.URL, Model: "m", APIKey: key, DisableStreaming: true, DisableRetries: true})
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
