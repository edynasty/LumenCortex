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
	"sort"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/diag"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider/internal/retryafter"
)

const defaultMaxResponseBytes = int64(8 << 20)

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
	MaxResponseBytes int64
	// MaxTokensDefault is the max_tokens sent when a request carries none (the API requires the
	// field): the model's resolved output limit. 0 means config.DefaultOutput.
	MaxTokensDefault int
	// ReasoningFormat is the model's reasoning.format: "" (the package default) is the budget
	// encoding, config.ReasoningAdaptive is adaptive thinking. Any other value is refused when a
	// request is built.
	ReasoningFormat string
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
	maxResponseBytes int64
	maxTokensDefault int
	reasoningFormat  string
	requestTimeout   time.Duration
	idleTimeout      time.Duration
	// secrets are the credential values this client sends, redacted from every error text built
	// from what the server returned.
	secrets []string
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
		maxResponseBytes: maxResponseBytes,
		maxTokensDefault: maxTokensDefault,
		reasoningFormat:  strings.TrimSpace(cfg.ReasoningFormat),
		requestTimeout:   cfg.RequestTimeout,
		idleTimeout:      cfg.IdleTimeout,
		secrets:          errorSecrets(cfg.APIKey, headers),
	}, nil
}

// errorSecrets lists the credentials a client sends, longest first: the API key and the value of
// every configured header whose name is secret-shaped (diag.IsSecretKey: Authorization, x-api-key,
// …), with the credential after its scheme ("Bearer <token>"). anthropic-version, content-type and
// other headers are not credentials and are left alone.
func errorSecrets(apiKey string, headers map[string]string) []string {
	var secrets []string
	add := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			secrets = append(secrets, value)
		}
	}
	add(apiKey)
	for name, value := range headers {
		if !diag.IsSecretKey(name) {
			continue
		}
		add(value)
		if _, credential, ok := strings.Cut(strings.TrimSpace(value), " "); ok {
			add(credential)
		}
	}
	// A whole value goes before the token inside it, so "Bearer <token>" is redacted in one piece.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return secrets
}

// redact replaces every credential the client sends in text a server returned: a gateway or a
// logging proxy may echo the request's key back in an error body or a stream error event.
func (c *Client) redact(text string) string {
	for _, secret := range c.secrets {
		text = strings.ReplaceAll(text, secret, diag.Redacted)
	}
	return text
}

func (c *Client) Model() string { return c.model }

// Complete sends one request: one HTTP attempt. It never retries; the agent loop owns retrying, so
// every attempt is admitted, recorded and logged there. Every error it returns is a
// *protocol.ProviderError: the adapter classifies, the loop only reads the class.
func (c *Client) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassCancelled, Err: err}
	}
	response, err := c.completeOnce(ctx, req)
	if err == nil {
		return response, nil
	}
	return protocol.ProviderResponse{}, classify(ctx, err)
}

// capacityMarkers are the fragments of a 400 body that say the request exceeded the model's
// context: the phrases the API uses, and the generic ones a gateway in front of it may use.
var capacityMarkers = []string{
	"prompt is too long",
	"input length and `max_tokens` exceed context limit",
	"context_length_exceeded", "maximum context length", "context length exceeded", "too many tokens",
}

// classify maps one failed attempt to a class and a reason (design §4.5, in this order):
//
//  1. the run context is done → cancelled;
//  2. a stream error the API sent mid-stream: overloaded (overloaded), rate_limit (rate_limited), api
//     (server_error) and timeout (timeout) errors are availability; authentication and permission are
//     auth; anything else is bad_request;
//  3. a request the adapter could not build, or a reply the provider withheld → bad_request;
//  4. a TimeoutError → availability/timeout; a cut stream → availability/stream_incomplete; an
//     undecodable 2xx body → availability/malformed_response; any other error → availability/transport;
//  5. HTTP 413, or 400 whose body names a capacity marker → capacity;
//  6. HTTP 401, 403 → auth;
//  7. HTTP 408 (timeout), 409 (conflict), 429 (rate_limited), 503 and 529 (overloaded), other 5xx
//     (server_error) → availability;
//  8. any other status → bad_request.
//
// The error carries the provider's Retry-After, 0 when there is none, and the HTTP status.
func classify(ctx context.Context, err error) *protocol.ProviderError {
	if ctx.Err() != nil {
		return &protocol.ProviderError{Class: protocol.ClassCancelled, Err: err}
	}
	out := &protocol.ProviderError{Err: err}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		out.Status, out.RetryAfter = httpErr.StatusCode, httpErr.RetryAfter
		code, body := httpErr.StatusCode, strings.ToLower(httpErr.Body)
		switch {
		case code == http.StatusRequestEntityTooLarge:
			out.Class = protocol.ClassCapacity
		case code == http.StatusBadRequest && containsAny(body, capacityMarkers):
			out.Class = protocol.ClassCapacity
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			out.Class = protocol.ClassAuth
		case code == http.StatusRequestTimeout:
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonTimeout
		case code == http.StatusConflict:
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonConflict
		case code == http.StatusTooManyRequests:
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonRateLimited
		case code == http.StatusServiceUnavailable || code == statusOverloaded:
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonOverloaded
		case code >= 500:
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonServerError
		default:
			out.Class = protocol.ClassBadRequest
		}
		return out
	}
	var (
		streamErr *streamError
		bad       *badRequestError
		timeout   *TimeoutError
		malformed *malformedError
	)
	switch {
	case errors.As(err, &streamErr):
		switch streamErr.errType {
		case "overloaded_error":
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonOverloaded
		case "rate_limit_error":
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonRateLimited
		case "api_error":
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonServerError
		case "timeout_error":
			out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonTimeout
		case "authentication_error", "permission_error":
			out.Class = protocol.ClassAuth
		default:
			out.Class = protocol.ClassBadRequest
		}
	case errors.As(err, &bad):
		out.Class = protocol.ClassBadRequest
	case errors.As(err, &timeout):
		out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonTimeout
	case errors.Is(err, errStreamCut):
		out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonStreamIncomplete
	case errors.As(err, &malformed):
		out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonMalformedResponse
	default:
		out.Class, out.Reason = protocol.ClassAvailability, protocol.ReasonTransport
	}
	return out
}

// statusOverloaded is the API's "overloaded" status.
const statusOverloaded = 529

// malformedError is a 2xx body that is not a response: undecodable JSON, a body past the byte limit,
// a nameless tool_use block.
type malformedError struct{ err error }

func (e *malformedError) Error() string { return e.err.Error() }
func (e *malformedError) Unwrap() error { return e.err }

// malformed marks err as a malformed body unless it already says more: a withheld reply stays
// bad_request and a stream error event keeps its own class.
func malformed(err error) error {
	var (
		bad    *badRequestError
		stream *streamError
	)
	if errors.As(err, &bad) || errors.As(err, &stream) {
		return err
	}
	return &malformedError{err}
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
	wireReq, err := requestFromProtocol(c.model, req, c.stream, c.maxTokensDefault, c.reasoningFormat)
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
		return protocol.ProviderResponse{}, malformed(fmt.Errorf("provider response exceeded %d bytes", c.maxResponseBytes))
	}
	var decoded messagesResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return protocol.ProviderResponse{}, malformed(err)
	}
	out, err := responseToProtocol(decoded)
	if err != nil {
		return protocol.ProviderResponse{}, malformed(err)
	}
	return out, nil
}

// readStream reads the SSE stream. An event's payload may arrive as several data: lines and in
// pieces; lines accumulate until the blank line that ends the event. message_stop ends the stream
// even if the connection stays open; a stream that ends without it was cut and is an error
// (availability), so the blocks received so far never reach the loop as a response.
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
			return false, malformed(err)
		}
		if event.Error != nil {
			event.Error.Type = c.redact(event.Error.Type)
			event.Error.Message = c.redact(event.Error.Message)
		}
		if err := builder.apply(event); err != nil {
			return false, malformed(err)
		}
		return event.Type == "message_stop", nil
	}
	for !stopped {
		line, err := reader.ReadString(10)
		consumed += int64(len(line))
		if consumed > c.maxResponseBytes {
			return protocol.ProviderResponse{}, malformed(fmt.Errorf("provider stream exceeded %d bytes", c.maxResponseBytes))
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
	if !stopped {
		return protocol.ProviderResponse{}, errStreamCut
	}
	out, err := builder.result()
	if err != nil {
		return protocol.ProviderResponse{}, malformed(err)
	}
	return out, nil
}

// readHTTPError reads the API's {type:"error",error:{type,message}} body. No credential the client
// sent reaches the error: every occurrence of one in the status or the body is redacted before the
// error is built.
func (c *Client) readHTTPError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     c.redact(resp.Status),
		Body:       c.redact(strings.TrimSpace(string(raw))),
		RetryAfter: retryafter.Parse(resp.Header, time.Now()),
	}
}
