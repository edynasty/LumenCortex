package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/protocol"
)

// autoConfig is a global catalog with two bound models: test/b0 is the session model, test/b1 the
// optional auto judge. mainExtra adds model settings (reasoning efforts) to b0.
func autoConfig(main, judge, max, mainExtra string) string {
	auto := fmt.Sprintf(`{"max": %q, "timeoutSeconds": 1}`, max)
	if judge != "" {
		auto = fmt.Sprintf(`{"model": %q, "max": %q, "timeoutSeconds": 1}`, judge, max)
	}
	extra := ""
	if mainExtra != "" {
		extra = ", " + mainExtra
	}
	return fmt.Sprintf(`{"model": %q, "effort": {"default": "auto", "auto": %s},
	  "providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {
	    "b0": {"modelID": "b0", "limit": {"context": 8000, "output": 1000}%s},
	    "b1": {"modelID": "b1", "limit": {"context": 8000, "output": 1000}}
	  }}}}`, main, auto, extra)
}

type judgeProvider struct {
	level string
	err   error
	calls atomic.Int32
	last  atomic.Value // protocol.ProviderRequest
}

func (p *judgeProvider) Model() string { return "judge" }

func (p *judgeProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls.Add(1)
	p.last.Store(req)
	if p.err != nil {
		return protocol.ProviderResponse{}, p.err
	}
	return protocol.ProviderResponse{
		Message:      protocol.Message{Content: fmt.Sprintf(`{"level":%q}`, p.level)},
		FinishReason: "stop",
	}, nil
}

func lastRequest(t *testing.T, p *answering) protocol.ProviderRequest {
	t.Helper()
	value := p.last.Load()
	if value == nil {
		t.Fatal("the session model was never asked")
	}
	return value.(protocol.ProviderRequest)
}

func autoDecision(t *testing.T, entries []stepEntry) agent.Decision {
	t.Helper()
	for _, entry := range entries {
		for _, d := range entry.Decisions {
			if d.Rule == agent.RuleEffortAuto {
				return d
			}
		}
	}
	t.Fatalf("no %s decision in %v", agent.RuleEffortAuto, entries)
	return agent.Decision{}
}

func decisionField(t *testing.T, d agent.Decision, field string) string {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(d.Inputs, &value); err != nil {
		t.Fatal(err)
	}
	text, _ := value[field].(string)
	return text
}

func TestAutoEffortJudgeLevelReachesRequest(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "high"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	const sentinel = "SENTINEL-USER-PROMPT"
	handle := newTestSession(t, engine, sentinel+" explain the build")
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run=%+v", r)
	}
	if got := lastRequest(t, main).ReasoningEffort; got != "high" {
		t.Fatalf("main request effort=%q, want high", got)
	}
	if judge.calls.Load() != 1 {
		t.Fatalf("judge calls=%d", judge.calls.Load())
	}
	judgeReq := judge.last.Load().(protocol.ProviderRequest)
	if len(judgeReq.Tools) != 0 || len(judgeReq.Messages) != 2 {
		t.Fatalf("judge request = %+v", judgeReq)
	}
	entries := steps(t, engine, handle.ID)
	d := autoDecision(t, entries)
	if decisionField(t, d, "source") != agent.AutoSourceJudge || decisionField(t, d, "judgeModel") != "test/b1" {
		t.Fatalf("auto decision inputs=%s", d.Inputs)
	}
	if string(d.Output) != `{"level":"high"}` {
		t.Fatalf("auto output=%s", d.Output)
	}
	for _, entry := range entries {
		if strings.Contains(string(entry.raw), sentinel) {
			t.Fatalf("step record carries the user prompt: %s", entry.raw)
		}
	}
	verifyTrace(t, entries)
}

func TestAutoEffortHeuristicWithoutJudge(t *testing.T) {
	engine := openTestEngine(t)
	main := &answering{final: "done"}
	bindRef(engine, "b0", main)
	useGlobalConfig(t, autoConfig("test/b0", "", "high", ""))

	handle := newTestSession(t, engine, "First create the file, then update the handler, run the tests, and fix the failures")
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run=%+v", r)
	}
	if got := lastRequest(t, main).ReasoningEffort; got != "high" {
		t.Fatalf("main request effort=%q, want high", got)
	}
	d := autoDecision(t, steps(t, engine, handle.ID))
	if decisionField(t, d, "source") != agent.AutoSourceHeuristic || decisionField(t, d, "fallbackReason") != agent.AutoFallbackNoModel {
		t.Fatalf("auto decision inputs=%s", d.Inputs)
	}
}

func TestAutoEffortCeiling(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "max"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "medium", ""))

	handle := newTestSession(t, engine, "explain the build")
	run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
	if got := lastRequest(t, main).ReasoningEffort; got != "medium" {
		t.Fatalf("main request effort=%q, want medium", got)
	}
	d := autoDecision(t, steps(t, engine, handle.ID))
	if string(d.Output) != `{"level":"medium"}` {
		t.Fatalf("auto output=%s", d.Output)
	}
}

func TestAutoEffortOverrideWins(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "low"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	handle := newTestSession(t, engine, "explain the build")
	run(t, engine, handle.ID, AgentOptions{Effort: config.EffortHigh, MaxSteps: 2})
	if got := lastRequest(t, main).ReasoningEffort; got != "high" {
		t.Fatalf("main request effort=%q, want the explicit override", got)
	}
	if judge.calls.Load() != 0 {
		t.Fatalf("judge ran for a concrete override: %d calls", judge.calls.Load())
	}
	for _, entry := range steps(t, engine, handle.ID) {
		for _, d := range entry.Decisions {
			if d.Rule == agent.RuleEffortAuto {
				t.Fatal("auto resolved despite a concrete override")
			}
		}
	}
}

func TestAutoEffortUnavailable(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "low"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high",
		`"reasoning": {"format": "reasoning-effort", "efforts": ["max"]}`))

	handle := newTestSession(t, engine, "explain the build")
	run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
	if got := lastRequest(t, main).ReasoningEffort; got != "" {
		t.Fatalf("main request effort=%q, want no reasoning parameter", got)
	}
	found := false
	for _, entry := range steps(t, engine, handle.ID) {
		for _, d := range entry.Decisions {
			if d.Rule == agent.RuleEffortAutoUnavailable {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no effort.auto_unavailable decision")
	}
}

func TestAutoEffortJudgeFailureFallsBack(t *testing.T) {
	engine := openTestEngine(t)
	main := &answering{final: "done"}
	judge := &judgeProvider{err: classed(protocol.ClassAvailability)}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	handle := newTestSession(t, engine, "First create the file, then update the handler, run the tests, and fix the failures")
	run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
	if got := lastRequest(t, main).ReasoningEffort; got != "high" {
		t.Fatalf("main request effort=%q, want the heuristic's high", got)
	}
	d := autoDecision(t, steps(t, engine, handle.ID))
	if decisionField(t, d, "source") != agent.AutoSourceHeuristic || decisionField(t, d, "fallbackReason") != string(protocol.ClassAvailability) {
		t.Fatalf("auto decision inputs=%s", d.Inputs)
	}
}

func TestAutoEffortResumeReusesDecision(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "high"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	handle := newTestSession(t, engine, "explain the build")
	run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
	if judge.calls.Load() != 1 {
		t.Fatalf("first run judge calls=%d", judge.calls.Load())
	}
	judge.level = "low"
	run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
	if judge.calls.Load() != 1 {
		t.Fatalf("resume called the judge again: %d calls", judge.calls.Load())
	}
	if got := lastRequest(t, main).ReasoningEffort; got != "high" {
		t.Fatalf("resume request effort=%q, want the persisted high", got)
	}
	var reused agent.Decision
	for _, entry := range steps(t, engine, handle.ID) {
		for _, d := range entry.Decisions {
			if d.Rule == agent.RuleEffortAuto && strings.Contains(string(d.Inputs), `"reused":true`) {
				reused = d
			}
		}
	}
	if reused.Rule == "" {
		t.Fatalf("no reused auto decision: %v", steps(t, engine, handle.ID))
	}
	verifyTrace(t, steps(t, engine, handle.ID))
}

func TestAutoEffortConfigDefault(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "medium"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	handle := newTestSession(t, engine, "explain the build")
	run(t, engine, handle.ID, AgentOptions{MaxSteps: 2}) // effort.default: auto
	if got := lastRequest(t, main).ReasoningEffort; got != "medium" {
		t.Fatalf("main request effort=%q, want the judge's medium", got)
	}
}
