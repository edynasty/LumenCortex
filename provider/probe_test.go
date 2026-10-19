package provider_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider"
)

func requestForProbe() protocol.ProviderRequest {
	return protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "Reply with the single word ok."}}, MaxTokens: 64}
}

// probeModel builds the ResolvedModel of one probe from the three environment variables, or
// skips the test with the exact variables it wants.
func probeModel(t *testing.T, baseVar, keyEnvVar, modelVar, pkg string) config.ResolvedModel {
	t.Helper()
	base, keyEnv, model := os.Getenv(baseVar), os.Getenv(keyEnvVar), os.Getenv(modelVar)
	if base == "" || keyEnv == "" || model == "" {
		t.Skipf("no real probe configured: set %s, %s (naming the variable that holds the key) and %s",
			baseVar, keyEnvVar, modelVar)
	}
	if os.Getenv(keyEnv) == "" {
		t.Skipf("%s is not set; put the API key in the variable %s names", keyEnvVar, keyEnvVar)
	}
	return config.ResolvedModel{
		Ref: "probe/probe", ProviderID: "probe", Package: pkg, ModelID: model,
		BaseURL: base, APIKeyRef: "{env:" + keyEnv + "}", Output: 1024,
		RequestTimeout: 60 * time.Second, IdleTimeout: 30 * time.Second,
	}
}

func probeCompletes(t *testing.T, m config.ResolvedModel, opts provider.Options) {
	t.Helper()
	p, err := provider.FromModel(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	response, err := p.Complete(ctx, requestForProbe())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if strings.TrimSpace(response.Message.Content) == "" {
		t.Fatalf("probe: empty answer: %+v", response)
	}
	t.Logf("probe answered: %q", response.Message.Content)
}

// Opt-in probes against real endpoints. They are skipped unless the three variables name one, and
// they send one bounded request: "reply with the single word ok", no tools, no thinking.
func TestProbeAnthropic(t *testing.T) {
	probeCompletes(t, probeModel(t, "LCX_PROBE_ANTHROPIC_BASE_URL", "LCX_PROBE_ANTHROPIC_KEY_ENV", "LCX_PROBE_ANTHROPIC_MODEL", config.PackageAnthropicMessages), provider.Options{DisableStreaming: true})
}

func TestProbeOpenAICompatible(t *testing.T) {
	probeCompletes(t, probeModel(t, "LCX_PROBE_OPENAI_BASE_URL", "LCX_PROBE_OPENAI_KEY_ENV", "LCX_PROBE_OPENAI_MODEL", config.PackageOpenAICompatible), provider.Options{DisableStreaming: true})
}
