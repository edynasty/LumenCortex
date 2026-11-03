package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// A gateway that echoes the request's credentials back in its reply never gets them into an error:
// the API key and every secret-named configured header are redacted from the HTTP error body and
// from a mid-stream error event, the whole header value and the token after its scheme alike.
// anthropic-version is not a credential and stays readable, and the error keeps its type, class and
// Retry-After.
func TestErrorsRedactSentCredentials(t *testing.T) {
	const (
		apiKey = "astra-anthropic-fixture-key-5e92"
		bearer = "astra-anthropic-fixture-bearer-0d4c"
		header = "astra-anthropic-fixture-header-a7b3"
	)
	credentials := []struct {
		name    string
		apiKey  string
		headers map[string]string
		secrets []string
	}{
		{"api key", apiKey, nil, []string{apiKey}},
		{"authorization header", "", map[string]string{"Authorization": "Bearer " + bearer}, []string{"Bearer " + bearer, bearer}},
		{"api key overridden by header", apiKey, map[string]string{"X-Api-Key": header}, []string{apiKey, header}},
	}
	// echoed is what a hostile or logging gateway reflects: every credential-carrying header and the
	// innocuous version header.
	echoed := func(r *http.Request) string {
		return fmt.Sprintf("x-api-key=%s auth=%s version=%s",
			r.Header.Get("x-api-key"), r.Header.Get("Authorization"), r.Header.Get("anthropic-version"))
	}
	for _, cred := range credentials {
		for _, stream := range []bool{false, true} {
			name := cred.name + "/http"
			if stream {
				name = cred.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !stream {
						w.Header().Set("Retry-After", "3")
						w.WriteHeader(http.StatusUnauthorized)
						fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":"rejected %s"}}`, echoed(r))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":%q}}\n\n", "rejected "+echoed(r))
				}))
				defer server.Close()
				client, err := New(Config{
					BaseURL: server.URL, Model: "fixture", APIKey: cred.apiKey, Headers: cred.headers, DisableStreaming: !stream,
				})
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Complete(context.Background(), request())
				if err == nil {
					t.Fatal("the rejection was accepted")
				}
				pe := providerError(t, err)
				if pe.Class != protocol.ClassAuth {
					t.Fatalf("class=%s, want auth", pe.Class)
				}
				texts := []string{err.Error(), pe.Err.Error()}
				if stream {
					var se *streamError
					if !errors.As(err, &se) {
						t.Fatalf("error %v is not a *streamError", err)
					}
					texts = append(texts, se.errType, se.message)
				} else {
					var httpErr *HTTPError
					if !errors.As(err, &httpErr) {
						t.Fatalf("error %v is not an *HTTPError", err)
					}
					if httpErr.StatusCode != http.StatusUnauthorized || pe.RetryAfter != 3*time.Second {
						t.Fatalf("status=%d retryAfter=%s, want 401 and 3s", httpErr.StatusCode, pe.RetryAfter)
					}
					texts = append(texts, httpErr.Status, httpErr.Body)
				}
				for _, text := range texts {
					for _, secret := range cred.secrets {
						if strings.Contains(text, secret) {
							t.Fatalf("an error text exposes a sent credential: %q", text)
						}
					}
				}
				if !strings.Contains(err.Error(), "[redacted]") {
					t.Fatalf("the echoed credential was not redacted: %v", err)
				}
				if !strings.Contains(err.Error(), "version="+defaultAnthropicVersion) {
					t.Fatalf("anthropic-version, not a credential, was redacted: %v", err)
				}
			})
		}
	}
}
