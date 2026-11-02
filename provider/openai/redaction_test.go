package openai

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
// from a stream error event, the whole header value and the token after its scheme alike. A header
// that is not a credential stays readable, and the error keeps its type, class and Retry-After.
func TestErrorsRedactSentCredentials(t *testing.T) {
	const (
		apiKey  = "astra-openai-fixture-key-3b1f"
		bearer  = "astra-openai-fixture-bearer-77d0"
		header  = "astra-openai-fixture-header-c41a"
		version = "astra-fixture-version-2026"
	)
	credentials := []struct {
		name    string
		apiKey  string
		headers map[string]string
		secrets []string
	}{
		{"api key", apiKey, map[string]string{"X-Client-Version": version}, []string{apiKey}},
		{"authorization header", "", map[string]string{"Authorization": "Bearer " + bearer, "X-Client-Version": version}, []string{"Bearer " + bearer, bearer}},
		{"api key and key header", apiKey, map[string]string{"X-Goog-Api-Key": header, "X-Client-Version": version}, []string{apiKey, header}},
	}
	// echoed is what a hostile or logging gateway reflects: every credential-carrying header and the
	// innocuous version header.
	echoed := func(r *http.Request) string {
		return fmt.Sprintf("auth=%s goog=%s version=%s",
			r.Header.Get("Authorization"), r.Header.Get("X-Goog-Api-Key"), r.Header.Get("X-Client-Version"))
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
						fmt.Fprintf(w, `{"error":{"message":"rejected %s"}}`, echoed(r))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"error\":{\"type\":%q,\"message\":%q}}\n\n",
						r.Header.Get("X-Goog-Api-Key"), "rejected "+echoed(r))
				}))
				defer server.Close()
				client, err := New(Config{
					Endpoint: server.URL, Model: "fixture", APIKey: cred.apiKey, Headers: cred.headers,
					DisableRetries: true, DisableStreaming: !stream,
				})
				if err != nil {
					t.Fatal(err)
				}
				_, err = client.Complete(context.Background(), request())
				if err == nil {
					t.Fatal("the rejection was accepted")
				}
				pe := providerError(t, err)
				texts := []string{err.Error(), pe.Err.Error()}
				if stream {
					var se *streamError
					if !errors.As(err, &se) {
						t.Fatalf("error %v is not a *streamError", err)
					}
					if pe.Class != protocol.ClassAvailability {
						t.Fatalf("class=%s, want availability", pe.Class)
					}
					texts = append(texts, se.Type, se.Message)
				} else {
					var httpErr *HTTPError
					if !errors.As(err, &httpErr) {
						t.Fatalf("error %v is not an *HTTPError", err)
					}
					if httpErr.StatusCode != http.StatusUnauthorized || pe.Class != protocol.ClassAuth || pe.RetryAfter != 3*time.Second {
						t.Fatalf("status=%d class=%s retryAfter=%s, want 401 auth 3s", httpErr.StatusCode, pe.Class, pe.RetryAfter)
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
				if !strings.Contains(err.Error(), version) {
					t.Fatalf("a non-credential header was redacted: %v", err)
				}
			})
		}
	}
}
