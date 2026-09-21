package openai

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
	defaultBaseURL          = "https://api.openai.com/v1"
	defaultMaxResponseBytes = int64(8 << 20)
	defaultRetryBaseDelay   = 500 * time.Millisecond
)

type Config struct {
	Endpoint         string
	BaseURL          string
	APIKey           string
	Model            string
	HTTPClient       *http.Client
	Headers          map[string]string
	DisableStreaming bool
	DisableRetries   bool
	MaxRetries       int
	RetryBaseDelay   time.Duration
	MaxResponseBytes int64
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
}

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
			base = defaultBaseURL
		}
		endpoint = base + "/chat/completions"
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
	}, nil
}

func (c *Client) Model() string { return c.model }

func (c *Client) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return protocol.ProviderResponse{}, err
		}
		response, err := c.completeOnce(ctx, req)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if attempt == c.maxRetries || !retryable(err) {
			break
		}
		delay := c.retryBaseDelay << attempt
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.RetryAfter > delay {
			delay = httpErr.RetryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return protocol.ProviderResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return protocol.ProviderResponse{}, lastErr
}

func (c *Client) completeOnce(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	wireReq := requestFromProtocol(c.model, req, c.stream)
	body, err := json.Marshal(wireReq)
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for key, value := range c.headers {
		httpReq.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return protocol.ProviderResponse{}, readHTTPError(resp)
	}
	if c.stream {
		return c.readStream(resp.Body)
	}
	return c.readJSON(resp.Body)
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
	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return protocol.ProviderResponse{}, err
	}
	return decoded.toProtocol()
}

func (c *Client) readStream(body io.Reader) (protocol.ProviderResponse, error) {
	reader := bufio.NewReader(io.LimitReader(body, c.maxResponseBytes+1))
	builder := streamBuilder{maxBytes: c.maxResponseBytes}
	var consumed int64
	for {
		line, err := reader.ReadString('
')
		consumed += int64(len(line))
		if consumed > c.maxResponseBytes {
			return protocol.ProviderResponse{}, fmt.Errorf("provider stream exceeded %d bytes", c.maxResponseBytes)
		}
		if len(line) > 0 {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload == "[DONE]" {
					break
				}
				if payload != "" {
					var event streamResponse
					if decodeErr := json.Unmarshal([]byte(payload), &event); decodeErr != nil {
						return protocol.ProviderResponse{}, decodeErr
					}
					if applyErr := builder.apply(event); applyErr != nil {
						return protocol.ProviderResponse{}, applyErr
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return protocol.ProviderResponse{}, err
		}
	}
	return builder.result(), nil
}

func readHTTPError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       strings.TrimSpace(string(raw)),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusRequestTimeout ||
			httpErr.StatusCode == http.StatusConflict ||
			httpErr.StatusCode == http.StatusTooManyRequests ||
			httpErr.StatusCode >= 500
	}
	return true
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
