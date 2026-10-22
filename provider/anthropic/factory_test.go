package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
)

// goroutines is the current count, after a short settle so a just-finished test's leftovers are
// not attributed to the next one.
func goroutines() int {
	time.Sleep(50 * time.Millisecond)
	return runtime.NumGoroutine()
}

// waitForGoroutines polls until the count returns to want or the deadline passes; it returns the
// excess.
func waitForGoroutines(want int, deadline time.Duration) int {
	for start := time.Now(); time.Since(start) < deadline; {
		if got := goroutines(); got <= want {
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	return goroutines() - want
}

func TestFromModelUsesResolvedSettings(t *testing.T) {
	var gotKey, gotVersion, gotHeader, gotPath, gotModelID string
	var gotMaxTokens any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotHeader = r.Header.Get("X-Org")
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModelID, _ = body["model"].(string)
		gotMaxTokens = body["max_tokens"]
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer server.Close()
	t.Setenv("FROM_MODEL_KEY", "k-ant-123")
	m := config.ResolvedModel{
		Ref: "p/m", ProviderID: "p", ModelID: "upstream", BaseURL: server.URL,
		APIKeyRef: "{env:FROM_MODEL_KEY}", Headers: map[string]string{"X-Org": "acme"}, Output: 2048,
		IdleTimeout: time.Minute,
	}
	client, err := FromModel(m, Options{DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	if client.Model() != "upstream" {
		t.Fatalf("model=%q", client.Model())
	}
	if _, err := client.Complete(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	if gotKey != "k-ant-123" || gotHeader != "acme" || gotVersion != defaultAnthropicVersion {
		t.Fatalf("key=%q header=%q version=%q", gotKey, gotHeader, gotVersion)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotModelID != "upstream" || gotMaxTokens != float64(2048) {
		t.Fatalf("model=%q max_tokens=%v", gotModelID, gotMaxTokens)
	}
	if client.httpClient.Timeout != 0 {
		t.Fatalf("the factory set http.Client.Timeout=%v; timeouts belong to the adapter", client.httpClient.Timeout)
	}

	// A missing key variable fails before anything is built, naming only the variable.
	t.Setenv("FROM_MODEL_KEY", "")
	if _, err := FromModel(m, Options{}); err == nil || !strings.Contains(err.Error(), "FROM_MODEL_KEY") || strings.Contains(err.Error(), "k-ant-123") {
		t.Fatalf("missing key accepted: %v", err)
	}
	// No destination is refused before any request.
	m.BaseURL = ""
	t.Setenv("FROM_MODEL_KEY", "k-ant-123")
	if _, err := FromModel(m, Options{}); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("no endpoint: %v", err)
	}
}

// The factory carries the model's resolved reasoning.format to the client: an adaptive model sends
// adaptive thinking, a model that declares no format keeps the budget default, and a format this
// adapter does not implement (an openai-compatible one) is refused before a request is sent.
func TestFromModelCarriesTheReasoningFormat(t *testing.T) {
	var body map[string]any
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer server.Close()
	complete := func(format, effort string) error {
		m := config.ResolvedModel{Ref: "p/m", ProviderID: "p", ModelID: "m", BaseURL: server.URL, ReasoningFormat: format}
		client, err := FromModel(m, Options{DisableStreaming: true, DisableRetries: true})
		if err != nil {
			return err
		}
		req := request()
		req.ReasoningEffort = effort
		_, err = client.Complete(context.Background(), req)
		return err
	}

	if err := complete(config.ReasoningAdaptive, "high"); err != nil {
		t.Fatal(err)
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || body["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("adaptive body=%v", body)
	}
	if err := complete("", "high"); err != nil {
		t.Fatal(err)
	}
	if thinking, _ := body["thinking"].(map[string]any); thinking["type"] != "enabled" {
		t.Fatalf("default body=%v, want the budget encoding", body)
	}
	sent := hits.Load()
	if err := complete(config.ReasoningNone, "high"); err == nil {
		t.Fatal("the openai-compatible format was accepted")
	}
	if hits.Load() != sent {
		t.Fatalf("a refused format still sent %d request(s)", hits.Load()-sent)
	}
}
