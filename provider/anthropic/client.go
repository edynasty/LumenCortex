package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

const (
	defaultMaxResponseBytes = int64(8 << 20)
	defaultRetryBaseDelay   = 500 * time.Millisecond
)

type Config struct {
	Endpoint string
	BaseURL  string
	APIKey   string
	Model    string
	// HTTPClient replaces the default client. Its Timeout is the caller's business: the factory
	// sets none, because the request and idle timeouts live in the adapter and classify as
	// availability.
	HTTPClient       *http.Client
	Headers          map[string]string
	DisableStreaming bool
	DisableRetries   bool
	MaxRetries       int
	RetryBaseDelay   time.Duration
	MaxResponseBytes int64
	// MaxTokensDefault is the max_tokens sent when a request carries none (the API requires the
	// field): the model's resolved output limit. 0 means config.DefaultOutput.
	MaxTokensDefault int
	// RequestTimeout caps each HTTP attempt from send to the last byte; 0 means no cap.
	RequestTimeout time.Duration
	// IdleTimeout ends an attempt that receives nothing for this long: before the headers and
	// between body reads. 0 means no idle cap.
	IdleTimeout time.Duration
}

type Client struct {
	endpoint         string
	apiKey           string
	model            string
	httpClient       *http.Client
	headers          map[string]string
	stream           bool
	maxRetries       int
	retryBaseDelay   time.Duration
	maxResponseBytes int64
	maxTokensDefault int
	requestTimeout   time.Duration
	idleTimeout      time.Duration
}

// TimeoutError reports an attempt that outlived its request or idle timeout. It deliberately has
// no Unwrap: it must never be taken for the run's own cancellation, which is what a derived
// context's context.Canceled would look like (design F2).
type TimeoutError struct {
	// Kind is "idle" or "request".
	Kind  string
	After time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("provider %s timeout after %s", e.Kind, e.After)
}

// ErrNoEndpoint is returned by New when neither Endpoint nor BaseURL is set.
var ErrNoEndpoint = errors.New("provider has no endpoint or baseURL: set providers.<id>.settings.baseURL or endpoint")

type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("provider HTTP %d: %s", e.StatusCode, e.Status)
	}
	return fmt.Sprintf("provider HTTP %d: %s: %s", e.StatusCode, e.Status, e.Body)
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("model is required")
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if base == "" {
			// There is no default URL: a client without a destination would otherwise send its
			// credential and the transcript to a host the operator never named.
			return nil, ErrNoEndpoint
		}
		endpoint = base + "/v1/messages"
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, errors.New("endpoint must use http or https")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 0}
	}
	maxResponseBytes := cfg.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	retryBaseDelay := cfg.RetryBaseDelay
	if retryBaseDelay <= 0 {
		retryBaseDelay = defaultRetryBaseDelay
	}
	maxRetries := cfg.MaxRetries
	if !cfg.DisableRetries && maxRetries <= 0 {
		maxRetries = 2
	}
	if cfg.DisableRetries {
		maxRetries = 0
	}
	maxTokensDefault := cfg.MaxTokensDefault
	headers := map[string]string{}
	for key, value := range cfg.Headers {
		headers[key] = value
	}
	return &Client{
		endpoint:         endpoint,
		apiKey:           cfg.APIKey,
		model:            cfg.Model,
		httpClient:       httpClient,
		headers:          headers,
		stream:           !cfg.DisableStreaming,
		maxRetries:       maxRetries,
		retryBaseDelay:   retryBaseDelay,
		maxResponseBytes: maxResponseBytes,
		maxTokensDefault: maxTokensDefault,
		requestTimeout:   cfg.RequestTimeout,
		idleTimeout:      cfg.IdleTimeout,
	}, nil
}

func (c *Client) Model() string { return c.model }

// maxInAdapterRetryAfter is the longest Retry-After the adapter sleeps through. A provider that
// asks for more is reported at once so the runtime can fail over; the health registry records the
// time.
const maxInAdapterRetryAfter = 5 * time.Second

// Complete sends one request, retrying availability errors with backoff. Every error it returns
// is a *protocol.ProviderError: the adapter classifies, the loop only reads the class.
func (c *Client) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassCancelled, Err: err}
		}
		response, err := c.completeOnce(ctx, req)
		if err == nil {
			return response, nil
		}
		lastErr = err
		class, retryAfter := classify(ctx, err)
		if class != protocol.ClassAvailability {
			return protocol.ProviderResponse{}, &protocol.ProviderError{Class: class, RetryAfter: retryAfter, Err: err}
		}
		if attempt == c.maxRetries || retryAfter > maxInAdapterRetryAfter {
			break
		}
		delay := c.retryBaseDelay << attempt
		if delay > maxInAdapterRetryAfter {
			delay = maxInAdapterRetryAfter
		}
		if retryAfter > delay {
			delay = retryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassCancelled, Err: ctx.Err()}
		case <-timer.C:
		}
	}
	class, retryAfter := classify(ctx, lastErr)
	return protocol.ProviderResponse{}, &protocol.ProviderError{Class: class, RetryAfter: retryAfter, Err: lastErr}
}

// capacityMarkers are the fragments of a 400 body that say the request exceeded the model's
// context: the phrases the API uses, and the generic ones a gateway in front of it may use.
var capacityMarkers = []string{
	"prompt is too long",
	"input length and `max_tokens` exceed context limit",
	"context_length_exceeded", "maximum context length", "context length exceeded", "too many tokens",
}

// classify maps one failed attempt to a class (design §4.5, in this order):
//
//  1. the run context is done → cancelled;
//  2. a stream error the API sent mid-stream: overloaded, rate_limit, api and timeout errors are
//     availability; authentication and permission are auth; anything else is bad_request;
//  3. a TimeoutError, a transport error or a malformed stream → availability;
//  4. HTTP 413, or 400 whose body names a capacity marker → capacity;
//  5. HTTP 401, 403 → auth;
//  6. HTTP 408, 409, 429, 5xx (529 overloaded included) → availability;
//  7. any other status → bad_request.
//
// It also returns the provider's Retry-After, 0 when there is none.
func classify(ctx context.Context, err error) (protocol.ErrorClass, time.Duration) {
	if ctx.Err() != nil {
		return protocol.ClassCancelled, 0
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		code, body := httpErr.StatusCode, strings.ToLower(httpErr.Body)
		switch {
		case code == http.StatusRequestEntityTooLarge:
			return protocol.ClassCapacity, httpErr.RetryAfter
		case code == http.StatusBadRequest && containsAny(body, capacityMarkers):
			return protocol.ClassCapacity, httpErr.RetryAfter
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			return protocol.ClassAuth, httpErr.RetryAfter
		case code == http.StatusRequestTimeout || code == http.StatusConflict || code == http.StatusTooManyRequests || code >= 500:
			return protocol.ClassAvailability, httpErr.RetryAfter
		default:
			return protocol.ClassBadRequest, httpErr.RetryAfter
		}
	}
	var streamErr *streamError
	if errors.As(err, &streamErr) {
		switch streamErr.errType {
		case "overloaded_error", "rate_limit_error", "api_error", "timeout_error":
			return protocol.ClassAvailability, 0
		case "authentication_error", "permission_error":
			return protocol.ClassAuth, 0
		default:
			return protocol.ClassBadRequest, 0
		}
	}
	var bad *badRequestError
	if errors.As(err, &bad) {
		return protocol.ClassBadRequest, 0
	}
	return protocol.ClassAvailability, 0
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// badRequestError is a request the adapter could not even build; retrying or failing over cannot
// help.
type badRequestError struct{ err error }

func (e *badRequestError) Error() string { return e.err.Error() }
func (e *badRequestError) Unwrap() error { return e.err }

// activityReader reports every read that returned bytes, which resets the idle timer.
type activityReader struct {
	r     io.Reader
	touch func()
}

func (a activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.touch()
	}
	return n, err
}

func (c *Client) completeOnce(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	wireReq, err := requestFromProtocol(c.model, req, c.stream, c.maxTokensDefault)
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	body, err := json.Marshal(wireReq)
	if err != nil {
		return protocol.ProviderResponse{}, &badRequestError{err}
	}
	// The attempt gets its own cancellable context so a timeout can cancel it with a cause. The
	// cause is read back below; the run context itself is never cancelled from here.
	attemptCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if c.requestTimeout > 0 {
		timer := time.AfterFunc(c.requestTimeout, func() {
			cancel(&TimeoutError{Kind: "request", After: c.requestTimeout})
		})
		defer timer.Stop()
	}
	touch := func() {}
	if c.idleTimeout > 0 {
		idle := time.AfterFunc(c.idleTimeout, func() {
			cancel(&TimeoutError{Kind: "idle", After: c.idleTimeout})
		})
		defer idle.Stop()
		touch = func() { idle.Reset(c.idleTimeout) }
	}
	// timeoutOr turns the error of a cancelled attempt into the timeout that cancelled it.
	timeoutOr := func(err error) error {
		var timeout *TimeoutError
		if ctx.Err() == nil && errors.As(context.Cause(attemptCtx), &timeout) {
			return timeout
		}
		return err
	}

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return protocol.ProviderResponse{}, &badRequestError{err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	// The Messages API authenticates with x-api-key, never a Bearer token; configured headers are
	// applied last so a gateway in front can override any of the defaults, the version included.
	if c.apiKey != "" {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}
	httpReq.Header.Set("anthropic-version", defaultAnthropicVersion)
	for key, value := range c.headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return protocol.ProviderResponse{}, timeoutOr(err)
	}
	defer resp.Body.Close()
	touch()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return protocol.ProviderResponse{}, timeoutOr(c.readHTTPError(resp))
	}
	reader := activityReader{r: resp.Body, touch: touch}
	var out protocol.ProviderResponse
	if c.stream {
		out, err = c.readStream(reader)
	} else {
		out, err = c.readJSON(reader)
	}
	if err != nil {
		return protocol.ProviderResponse{}, timeoutOr(err)
	}
	return out, nil
}

func (c *Client) readJSON(body io.Reader) (protocol.ProviderResponse, error) {
	limited := io.LimitReader(body, c.maxResponseBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	if int64(len(raw)) > c.maxResponseBytes {
		return protocol.ProviderResponse{}, fmt.Errorf("provider response exceeded %d bytes", c.maxResponseBytes)
	}
	var decoded messagesResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return protocol.ProviderResponse{}, err
	}
	return responseToProtocol(decoded)
}

// readStream reads the SSE stream. An event's payload may arrive as several data: lines and in
// pieces; lines accumulate until the blank line that ends the event. message_stop ends the stream
// even if the connection stays open.
func (c *Client) readStream(body io.Reader) (protocol.ProviderResponse, error) {
	reader := bufio.NewReader(io.LimitReader(body, c.maxResponseBytes+1))
	builder := streamBuilder{maxBytes: c.maxResponseBytes}
	var consumed int64
	var dataLines []string
	stopped := false
	// process folds one event payload; it reports message_stop so the reader can return without
	// waiting for the connection to close.
	process := func(payload string) (bool, error) {
		payload = strings.TrimSpace(payload)
		if payload == "" {
			return false, nil
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return false, err
		}
		if err := builder.apply(event); err != nil {
			return false, err
		}
		return event.Type == "message_stop", nil
	}
	for !stopped {
		line, err := reader.ReadString(10)
		consumed += int64(len(line))
		if consumed > c.maxResponseBytes {
			return protocol.ProviderResponse{}, fmt.Errorf("provider stream exceeded %d bytes", c.maxResponseBytes)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			// The blank line ends the event the collected data: lines carry.
			if len(dataLines) > 0 {
				var stopErr error
				stopped, stopErr = process(strings.Join(dataLines, "\n"))
				if stopErr != nil {
					return protocol.ProviderResponse{}, stopErr
				}
				dataLines = dataLines[:0]
			}
		case strings.HasPrefix(trimmed, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		case strings.HasPrefix(trimmed, ":"):
			// SSE comment, e.g. a keepalive.
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return protocol.ProviderResponse{}, err
		}
	}
	if len(dataLines) > 0 {
		var stopErr error
		stopped, stopErr = process(strings.Join(dataLines, "\n"))
		if stopErr != nil {
			return protocol.ProviderResponse{}, stopErr
		}
	}
	return builder.result()
}

// readHTTPError reads the API's {type:"error",error:{type,message}} body. The key never reaches
// the message: any occurrence of it in the body is redacted before the error is built.
func (c *Client) readHTTPError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	body := strings.TrimSpace(string(raw))
	if c.apiKey != "" {
		body = strings.ReplaceAll(body, c.apiKey, "[redacted]")
	}
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       body,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := time.Until(at)
		if delay > 0 {
			return delay
		}
	}
	return 0
}
