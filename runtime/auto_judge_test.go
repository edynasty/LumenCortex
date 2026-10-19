package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/provider"
)

// The judge goes through the normal provider factory: both wire protocols must classify each level,
// with no tools on the request.
func TestJudgeClassifiesOpenAICompatible(t *testing.T) {
	var current atomic.Value
	current.Store(`{"level":"low"}`)
	var sawTools, sawEffort atomic.Bool
	var effort atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		_, tools := body["tools"]
		sawTools.Store(tools)
		if value, ok := body["reasoning_effort"].(string); ok {
			sawEffort.Store(true)
			effort.Store(value)
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{}}`, current.Load().(string))
	}))
	defer server.Close()
	t.Setenv("JUDGE_KEY", "test-key")

	m := config.ResolvedModel{
		Ref: "p/judge", ProviderID: "p", ModelID: "judge", Package: config.PackageOpenAICompatible,
		BaseURL: server.URL + "/v1", APIKeyRef: "{env:JUDGE_KEY}",
		ReasoningFormat: config.ReasoningEffortField,
		Efforts:         []config.Effort{config.EffortLow, config.EffortHigh},
	}
	client, err := provider.FromModel(m, provider.Options{DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []config.Effort{config.EffortNone, config.EffortLow, config.EffortMedium, config.EffortHigh, config.EffortMax} {
		current.Store(fmt.Sprintf(`{"level":%q}`, level))
		got, source, fallback, err := agent.ClassifyWithJudge(context.Background(), client, m.Efforts, time.Second, "input", "prev", "last")
		if err != nil || got != level || source != agent.AutoSourceJudge || fallback != "" {
			t.Fatalf("level=%q: got=%q source=%q fallback=%q err=%v", level, got, source, fallback, err)
		}
	}
	if sawTools.Load() {
		t.Fatal("the judge request carried tools")
	}
	if !sawEffort.Load() || effort.Load() != "low" {
		t.Fatalf("judge reasoning effort = %v", effort.Load())
	}
}

func TestJudgeClassifiesAnthropic(t *testing.T) {
	var current atomic.Value
	current.Store(`{"level":"low"}`)
	var sawTools atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		_, tools := body["tools"]
		sawTools.Store(tools)
		if _, thinking := body["thinking"]; thinking {
			t.Error("the judge request asked for thinking despite effort none")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":%q}],"model":"judge","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, current.Load().(string))
	}))
	defer server.Close()
	t.Setenv("JUDGE_KEY", "test-key")

	m := config.ResolvedModel{
		Ref: "p/judge", ProviderID: "p", ModelID: "judge", Package: config.PackageAnthropicMessages,
		BaseURL: server.URL, APIKeyRef: "{env:JUDGE_KEY}", Output: 4096, OutputSet: true,
		Efforts: []config.Effort{config.EffortNone, config.EffortLow},
	}
	client, err := provider.FromModel(m, provider.Options{DisableStreaming: true, DisableRetries: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, level := range []config.Effort{config.EffortNone, config.EffortLow, config.EffortMedium, config.EffortHigh, config.EffortMax} {
		current.Store(fmt.Sprintf(`{"level":%q}`, level))
		got, source, fallback, err := agent.ClassifyWithJudge(context.Background(), client, m.Efforts, time.Second, "input", "prev", "last")
		if err != nil || got != level || source != agent.AutoSourceJudge || fallback != "" {
			t.Fatalf("level=%q: got=%q source=%q fallback=%q err=%v", level, got, source, fallback, err)
		}
	}
	if sawTools.Load() {
		t.Fatal("the judge request carried tools")
	}
}
