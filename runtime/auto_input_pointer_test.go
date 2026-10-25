package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// DATA §6 CR-AJ-1 through a real Engine and SQLite: every Auto judge envelope names the input the
// judge was sent by exactly one pointer (a stored row, or the digest of input BeginRun stores after
// the call), or by none when the Start has no new input; and the transcript row of that input is
// found by Store.RequestsExposing.

func judgeEnvelope(t *testing.T, engine *Engine, sessionID string, index int) session.RequestEnvelope {
	t.Helper()
	envs := storedEnvelopes(t, engine, sessionID)
	if len(envs) <= index {
		t.Fatalf("judge envelopes = %+v, want index %d", envs, index)
	}
	env, err := engine.store.Request(context.Background(), envs[index].RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// userRow returns the seq and text digest of the session's user row whose text is content.
func userRow(t *testing.T, engine *Engine, sessionID, content string) (int64, string) {
	t.Helper()
	rows, err := engine.store.RecentMessages(context.Background(), sessionID, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		var m protocol.Message
		if err := json.Unmarshal(r.JSON, &m); err != nil {
			t.Fatal(err)
		}
		if m.Role == "user" && m.Content == content {
			return r.Seq, inputDigest(m.Content)
		}
	}
	t.Fatalf("no user row %q", content)
	return 0, ""
}

func exposes(t *testing.T, engine *Engine, sessionID string, seq int64, digest, requestID string) bool {
	t.Helper()
	ids, err := engine.store.RequestsExposing(context.Background(), sessionID, seq, digest)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(ids, requestID)
}

func TestAutoJudgeEnvelopeNamesTheStoredGoalRow(t *testing.T) {
	engine := openTestEngine(t)
	bindRef(engine, "b0", &answering{final: "done"})
	bindRef(engine, "b1", &judgeProvider{level: "high"})
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))
	const goal = "explain the build"
	handle := newTestSession(t, engine, goal)
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run=%+v", r)
	}

	env := judgeEnvelope(t, engine, handle.ID, 0)
	seq, _ := userRow(t, engine, handle.ID, goal)
	if env.FirstSeq == nil || env.LastSeq == nil || *env.FirstSeq != seq || *env.LastSeq != seq {
		t.Fatalf("fresh-goal envelope window = %v..%v, want the goal row %d", env.FirstSeq, env.LastSeq, seq)
	}
	if env.PendingInputDigest != "" || env.PrevUserSeq != nil || env.LastAssistantSeq != nil || !env.LineageComplete {
		t.Fatalf("fresh-goal envelope = %+v", env)
	}
	// Found by the row alone, without its text.
	if !exposes(t, engine, handle.ID, seq, "", env.RequestID) {
		t.Fatal("the goal row is not exposed by the judge request that classified it")
	}
}

func TestAutoJudgeEnvelopeNamesPendingInputByDigest(t *testing.T) {
	engine := openTestEngine(t)
	bindRef(engine, "b0", &answering{final: "done"})
	bindRef(engine, "b1", &judgeProvider{level: "high"})
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))
	handle := newTestSession(t, engine, "explain the build")
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("first run=%+v", r)
	}
	const input = "now the tests"
	if r := run(t, engine, handle.ID, AgentOptions{Input: input, Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("second run=%+v", r)
	}

	env := judgeEnvelope(t, engine, handle.ID, 1)
	if env.FirstSeq != nil || env.LastSeq != nil || env.PendingInputDigest != inputDigest(input) || !env.LineageComplete {
		t.Fatalf("pending-input envelope = %+v", env)
	}
	if env.PrevUserSeq == nil || env.LastAssistantSeq == nil {
		t.Fatalf("pending-input envelope has no context rows: %+v", env)
	}
	// BeginRun stored the input after the judge call; its row is found by its text's digest.
	seq, digest := userRow(t, engine, handle.ID, input)
	if !exposes(t, engine, handle.ID, seq, digest, env.RequestID) {
		t.Fatalf("the input row %d is not exposed by the judge request that classified it", seq)
	}
	if exposes(t, engine, handle.ID, seq, "", env.RequestID) {
		t.Fatal("the judge request is reported for its input row without the row's text: it never saw the row")
	}
	if !exposes(t, engine, handle.ID, *env.PrevUserSeq, "", env.RequestID) || !exposes(t, engine, handle.ID, *env.LastAssistantSeq, "", env.RequestID) {
		t.Fatal("the judge's context rows are not exposed by it")
	}
}

func TestAutoJudgeEnvelopeOfAContinuationNamesNoInput(t *testing.T) {
	engine := openTestEngine(t)
	bindRef(engine, "b0", &answering{final: "done"})
	judge := &judgeProvider{level: "high"}
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))
	handle := newTestSession(t, engine, "explain the build")
	// A first Start at a fixed effort leaves no auto decision to reuse, so the continuation asks the
	// judge with no new input.
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortHigh, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("first run=%+v", r)
	}
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("continuation=%+v", r)
	}
	if judge.calls.Load() != 1 {
		t.Fatalf("judge calls = %d, want 1 (the continuation)", judge.calls.Load())
	}
	env := judgeEnvelope(t, engine, handle.ID, 0)
	if env.FirstSeq != nil || env.LastSeq != nil || env.PendingInputDigest != "" || !env.LineageComplete {
		t.Fatalf("continuation envelope = %+v; want no input pointer and a complete lineage", env)
	}
	seq, _ := userRow(t, engine, handle.ID, "explain the build")
	if env.PrevUserSeq == nil || *env.PrevUserSeq != seq || env.LastAssistantSeq == nil {
		t.Fatalf("continuation context = %+v, want the goal row %d and the answer", env, seq)
	}
	if !exposes(t, engine, handle.ID, seq, "", env.RequestID) {
		t.Fatal("the continued request row is not exposed by the judge that saw it")
	}
}

// An input no pointer can name is reported incomplete, never as a full lineage.
func TestAutoTurnWithAnUnnamedInputIsIncomplete(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "goal")
	id, err := engine.admitAutoJudge(context.Background(), handle.ID, "", autoTurn{newInput: "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := engine.store.Request(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if env.LineageComplete {
		t.Fatalf("an input with no pointer was recorded complete: %+v", env)
	}
}
