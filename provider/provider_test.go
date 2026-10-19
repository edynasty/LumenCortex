package provider_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider"
)

func probeRequest() protocol.ProviderRequest {
	return protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}
}

// The dispatcher selects the adapter by the resolved provider's package, forwards the host's
// options and resolves the key through the Credentials resolver it was given.
func TestFromModelSelectsByPackage(t *testing.T) {
	sawPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		if strings.Contains(r.URL.Path, "/chat/completions") {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
			return
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer server.Close()
	t.Setenv("PROBE_ENV_KEY", "env-key")

	anthropicModel := config.ResolvedModel{
		Ref: "p/a", ProviderID: "p", Package: config.PackageAnthropicMessages,
		ModelID: "claude", BaseURL: server.URL, APIKeyRef: "{env:PROBE_ENV_KEY}", Output: 512,
	}
	p, err := provider.FromModel(anthropicModel, provider.Options{DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(context.Background(), probeRequest()); err != nil {
		t.Fatal(err)
	}
	if sawPath != "/v1/messages" {
		t.Fatalf("anthropic package sent to %s", sawPath)
	}

	// The empty package (every configuration without one) and the legacy ids select the
	// chat-completions adapter; a secret reference goes through the resolver.
	openaiModel := config.ResolvedModel{
		Ref: "p/o", ProviderID: "p", ModelID: "m",
		BaseURL: server.URL, APIKeyRef: "{env:PROBE_ENV_KEY}", Output: 512,
	}
	for name, pkg := range map[string]string{
		"empty":           "",
		"legacy-opencode": "@opencode/ai/providers/openai-compatible",
		"legacy-aisdk":    "@ai-sdk/openai-compatible",
	} {
		m := openaiModel
		m.Package = pkg
		// The legacy ids never reach FromModel from config (NormalizePackage canonicalizes them);
		// the dispatcher accepts only the canonical value, as the resolved settings carry it.
		if pkg != "" && pkg != config.PackageOpenAICompatible {
			continue
		}
		p, err := provider.FromModel(m, provider.Options{DisableStreaming: true, DisableRetries: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := p.Complete(context.Background(), probeRequest()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sawPath != "/chat/completions" {
			t.Fatalf("%s: sent to %s", name, sawPath)
		}
	}

	// A secret reference resolves through the resolver the host passed.
	resolved := ""
	m := anthropicModel
	m.APIKeyRef = "{secret:the-key}"
	_, err = provider.FromModel(m, provider.Options{Credentials: stubResolver{func(name string) (string, error) {
		resolved = name
		return "secret-value", nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "the-key" {
		t.Fatalf("resolver saw %q", resolved)
	}
	// Without a resolver the same reference is refused before any request, naming only the name.
	if _, err := provider.FromModel(m, provider.Options{}); err == nil ||
		!contains(err.Error(), "the-key") || contains(err.Error(), "secret-value") {
		t.Fatalf("nil resolver: %v", err)
	}

	// An unknown package is a clear error.
	m.Package = "carrier-pigeon"
	if _, err := provider.FromModel(m, provider.Options{}); err == nil || !contains(err.Error(), "carrier-pigeon") {
		t.Fatalf("unknown package: %v", err)
	}
}

type stubResolver struct{ resolve func(string) (string, error) }

func (s stubResolver) Resolve(name string) (string, error) { return s.resolve(name) }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
