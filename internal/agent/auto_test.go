package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

type autoProvider struct {
	response protocol.ProviderResponse
	err      error
	block    bool
	calls    int
	last     protocol.ProviderRequest
}

func (p *autoProvider) Model() string { return "auto" }

func (p *autoProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls++
	p.last = req
	if p.block {
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}
	return p.response, p.err
}

func TestHeuristicEffort(t *testing.T) {
	cases := []struct {
		name, input, prev string
		want              config.Effort
	}{
		{"empty", "", "", config.EffortLow},
		{"greeting", "hi", "", config.EffortLow},
		{"question only", "What does this function do?", "", config.EffortLow},
		{"one file, one edit", "Fix the bug in main.go", "", config.EffortMedium},
		{"code and stack trace", "```\npanic: boom at main.go:12\n```\nFix it", "", config.EffortHigh},
		{"four imperatives and sequence", "First create the file, then update the handler, run the tests, and fix the failures", "", config.EffortHigh},
		{"long prose", strings.Repeat("word ", 400), "", config.EffortMedium},
		{"continuation inherits a hard request", "continue", "Refactor the auth module, migrate the schema, then update every caller", config.EffortHigh},
		{"continuation inherits a question", "go on", "What does this function do?", config.EffortLow},
		{"continuation without context", "continue", "", config.EffortLow},
	}
	for _, c := range cases {
		if got := HeuristicEffort(c.input, c.prev); got != c.want {
			t.Errorf("%s: HeuristicEffort = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParseJudgeLevel(t *testing.T) {
	valid := map[string]config.Effort{
		`{"level":"high"}`:                    config.EffortHigh,
		"```json\n{\"level\": \"low\"}\n```":  config.EffortLow,
		"The level is medium.":                config.EffortMedium,
		"none":                                config.EffortNone,
		`{"level":"MAX"}`:                     config.EffortMax,
		`{"level":"high","why":"multi-step"}`: config.EffortHigh,
	}
	for content, want := range valid {
		if got, ok := ParseJudgeLevel(content); !ok || got != want {
			t.Errorf("ParseJudgeLevel(%q) = %q/%v, want %q", content, got, ok, want)
		}
	}
	for _, content := range []string{"", "auto", `{"reason":"hard"}`, "I cannot help with that"} {
		if got, ok := ParseJudgeLevel(content); ok {
			t.Errorf("ParseJudgeLevel(%q) = %q, want no level", content, got)
		}
	}
}

func TestClassifyWithJudge(t *testing.T) {
	p := &autoProvider{response: protocol.ProviderResponse{Message: protocol.Message{Content: `{"level":"high"}`}}}
	level, source, fallback, err := ClassifyWithJudge(context.Background(), p, []config.Effort{"none", "low", "medium"}, time.Second, "do it", "prev", "reply")
	if err != nil || level != config.EffortHigh || source != AutoSourceJudge || fallback != "" {
		t.Fatalf("level=%q source=%q fallback=%q err=%v", level, source, fallback, err)
	}
	if len(p.last.Tools) != 0 || len(p.last.Messages) != 2 || p.last.ReasoningEffort != "none" {
		t.Fatalf("judge request = %+v", p.last)
	}
	if p.last.Temperature == nil || *p.last.Temperature != 0 {
		t.Fatalf("judge temperature = %v", p.last.Temperature)
	}

	p = &autoProvider{response: protocol.ProviderResponse{Message: protocol.Message{Content: "no idea"}}}
	if _, _, fallback, err := ClassifyWithJudge(context.Background(), p, nil, time.Second, "x", "", ""); err != nil || fallback != AutoFallbackUnparsable {
		t.Fatalf("unparsable fallback=%q err=%v", fallback, err)
	}

	p = &autoProvider{err: &protocol.ProviderError{Class: protocol.ClassAuth, Err: errors.New("denied sk-secret")}}
	if _, _, fallback, err := ClassifyWithJudge(context.Background(), p, nil, time.Second, "x", "", ""); err != nil || fallback != string(protocol.ClassAuth) {
		t.Fatalf("auth fallback=%q err=%v", fallback, err)
	}

	p = &autoProvider{block: true}
	if _, _, fallback, err := ClassifyWithJudge(context.Background(), p, nil, 20*time.Millisecond, "x", "", ""); err != nil || fallback != AutoFallbackTimeout {
		t.Fatalf("timeout fallback=%q err=%v", fallback, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p = &autoProvider{block: true}
	if _, _, _, err := ClassifyWithJudge(ctx, p, nil, time.Second, "x", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation err=%v", err)
	}
}

func TestEffortDecisionAutoUnavailable(t *testing.T) {
	cases := []struct {
		name      string
		def       config.Effort
		ceiling   config.Effort
		supported []config.Effort
		want      config.Effort
		rule      string
	}{
		{"nothing at or below the ceiling", "medium", "high", []config.Effort{"max"}, "", RuleEffortAutoUnavailable},
		{"ceiling none", "low", "none", []config.Effort{"low"}, "", RuleEffortAutoUnavailable},
		{"a level below the ceiling exists", "low", "high", []config.Effort{"low", "max"}, "low", RuleEffortDefault},
		{"clamp raises but a level below the ceiling exists", "high", "high", []config.Effort{"low", "max"}, "max", RuleEffortDefault},
		{"no efforts list leaves the level", "low", "high", nil, "low", RuleEffortDefault},
	}
	for _, c := range cases {
		sent, decision := EffortDecision(c.def, "", nil, Stall{}, 0, c.supported, c.ceiling)
		if sent != c.want || decision.Rule != c.rule {
			t.Errorf("%s: sent=%q rule=%s, want %q/%s", c.name, sent, decision.Rule, c.want, c.rule)
		}
		if err := ReplayDecision(decision); err != nil {
			t.Errorf("%s: replay: %v", c.name, err)
		}
	}

	// Escalation raises the auto-resolved level, up to escalation.max.
	esc := &config.Escalation{SameFailure: 2, NoProgressSteps: 6, Max: config.EffortHigh}
	sent, decision := EffortDecision(config.EffortLow, "", esc, Stall{SameFailure: 2}, 0, []config.Effort{"low", "medium", "high"}, config.EffortHigh)
	if sent != config.EffortMedium || decision.Rule != RuleEffortEscalated {
		t.Fatalf("escalation sent=%q rule=%s", sent, decision.Rule)
	}
	if err := ReplayDecision(decision); err != nil {
		t.Fatalf("escalation replay: %v", err)
	}
}

func TestAutoDecision(t *testing.T) {
	const sentinel = "SENTINEL-USER-TEXT"
	d := AutoDecision(AutoSourceJudge, "", len(sentinel), config.EffortHigh, "p/judge", config.EffortMedium, false)
	if err := ReplayDecision(d); err != nil {
		t.Fatalf("replay: %v", err)
	}
	for _, raw := range []string{string(d.Inputs), string(d.Output)} {
		if strings.Contains(raw, sentinel) || strings.Contains(raw, "sk-") {
			t.Fatalf("decision carries text: %s", raw)
		}
	}
	level, source, ok := AutoDecisionLevel(d)
	if !ok || level != config.EffortMedium || source != AutoSourceJudge {
		t.Fatalf("level=%q source=%q ok=%v", level, source, ok)
	}

	forged := d
	forged.Output = json.RawMessage(`{"level":"max"}`)
	if ReplayDecision(forged) == nil {
		t.Fatal("a level above the ceiling replayed clean")
	}
	forged = d
	forged.Inputs = json.RawMessage(`{"source":"bogus","inputLength":1,"ceiling":"high"}`)
	if ReplayDecision(forged) == nil {
		t.Fatal("an unknown source replayed clean")
	}

	record, err := json.Marshal(map[string]any{"step": 1, "decisions": []Decision{d}})
	if err != nil {
		t.Fatal(err)
	}
	prior, ok := AutoDecisionOfStep(record)
	if !ok {
		t.Fatal("auto decision not found in the step record")
	}
	level, reused, ok := ReusedAutoDecision(prior, config.EffortLow, 0)
	if !ok || level != config.EffortLow || !strings.Contains(string(reused.Inputs), `"reused":true`) {
		t.Fatalf("reused level=%q ok=%v inputs=%s", level, ok, reused.Inputs)
	}
	if err := ReplayDecision(reused); err != nil {
		t.Fatalf("reused replay: %v", err)
	}
}

func TestCappedAutoEffort(t *testing.T) {
	cases := []struct{ level, ceiling, want config.Effort }{
		{"max", "high", "high"},
		{"low", "high", "low"},
		{"medium", "none", "none"},
		{"", "high", ""},
		{"high", "", "high"}, // an invalid ceiling means the default, high
	}
	for _, c := range cases {
		if got := CappedAutoEffort(c.level, c.ceiling); got != c.want {
			t.Errorf("CappedAutoEffort(%q, %q) = %q, want %q", c.level, c.ceiling, got, c.want)
		}
	}
}
