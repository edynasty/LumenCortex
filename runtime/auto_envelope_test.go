package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// These tests cover the Auto judge's request envelope (ADR-0018, plan T1-S3): the judge request is
// admitted before it is sent, its outcome is settled on the envelope, and nothing is written to the
// session row or the transcript before BeginRun. The envelopes are read through a second SQLite
// connection, never through a hook in production code.

type storedEnvelope struct {
	RequestID, SessionID, RunID, Purpose, State, Reason, Decision, PendingInputDigest string
	PrevUserSeq, LastAssistantSeq, AnsweredSeq                                        sql.NullInt64
}

// readEnvelopes returns the session's request envelopes in the order they were admitted.
func readEnvelopes(path, sessionID string) ([]storedEnvelope, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT request_id, session_id, COALESCE(run_id,''), purpose, state, COALESCE(reason,''),
		COALESCE(decision_json,''), COALESCE(pending_input_digest,''), prev_user_seq, last_assistant_seq, answered_seq
		FROM request_envelopes WHERE session_id = ? ORDER BY created_at, request_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedEnvelope
	for rows.Next() {
		var e storedEnvelope
		if err := rows.Scan(&e.RequestID, &e.SessionID, &e.RunID, &e.Purpose, &e.State, &e.Reason, &e.Decision,
			&e.PendingInputDigest, &e.PrevUserSeq, &e.LastAssistantSeq, &e.AnsweredSeq); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// storedEnvelopes are the Auto judge envelopes of the session. The main loop admits an envelope for
// each of its own requests (T1-S2); those are not what these tests count, so they are left out.
func storedEnvelopes(t *testing.T, engine *Engine, sessionID string) []storedEnvelope {
	t.Helper()
	all, err := readEnvelopes(engine.store.Path(), sessionID)
	if err != nil {
		t.Fatalf("read envelopes: %v", err)
	}
	var envs []storedEnvelope
	for _, e := range all {
		if e.Purpose == session.PurposeAutoEffort {
			envs = append(envs, e)
		}
	}
	return envs
}

// sessionFacts is the part of a session the Start must not touch before BeginRun.
type sessionFacts struct {
	Status    string
	UpdatedAt string
	Messages  int
	NewestSeq int64
}

func readSessionFacts(t *testing.T, engine *Engine, sessionID string) sessionFacts {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var facts sessionFacts
	if err := db.QueryRow(`SELECT status, updated_at FROM sessions WHERE id = ?`, sessionID).Scan(&facts.Status, &facts.UpdatedAt); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*), COALESCE(MAX(seq), 0) FROM session_messages WHERE session_id = ?`, sessionID).
		Scan(&facts.Messages, &facts.NewestSeq); err != nil {
		t.Fatalf("read transcript facts: %v", err)
	}
	return facts
}

// watchingJudge reads the session's envelopes through a second connection while its request is in
// flight. The admission of that request must be committed and visible by then, and no other request
// may be open: that is the per-call admission transaction.
type watchingJudge struct {
	path, sessionID, level string
	calls                  atomic.Int32
	observed               [][]storedEnvelope // the judge envelopes of the session, per call
	otherAdmitted          int                // main envelopes still admitted during a judge call
	readErr                error
}

func (p *watchingJudge) Model() string { return "judge" }

func (p *watchingJudge) Complete(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls.Add(1)
	envs, err := readEnvelopes(p.path, p.sessionID)
	if err != nil {
		p.readErr = err
	} else {
		// The judge runs before the main loop of this Start: no main request may be open now, and the
		// judge envelopes alone are what the assertions below describe.
		var judges []storedEnvelope
		for _, e := range envs {
			switch {
			case e.Purpose == session.PurposeAutoEffort:
				judges = append(judges, e)
			case e.State == string(session.RequestAdmitted):
				p.otherAdmitted++
			}
		}
		p.observed = append(p.observed, judges)
	}
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", Content: fmt.Sprintf(`{"level":%q}`, p.level)},
		FinishReason: "stop",
	}, nil
}

func TestAutoJudgeAdmitsOneEnvelopeBeforeEachRequest(t *testing.T) {
	engine := openTestEngine(t)
	main := &answering{final: "done"}
	bindRef(engine, "b0", main)
	judge := &watchingJudge{path: engine.store.Path(), level: "high"}
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	handle := newTestSession(t, engine, "explain the build")
	judge.sessionID = handle.ID

	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("first run=%+v", r)
	}
	if judge.readErr != nil {
		t.Fatalf("read the envelopes during the judge call: %v", judge.readErr)
	}
	if judge.calls.Load() != 1 || len(judge.observed) != 1 {
		t.Fatalf("judge calls=%d observations=%d", judge.calls.Load(), len(judge.observed))
	}
	// While the first request was in flight, its admission was the only envelope and it was committed.
	inFlight := judge.observed[0]
	if len(inFlight) != 1 || inFlight[0].Purpose != session.PurposeAutoEffort || inFlight[0].State != string(session.RequestAdmitted) {
		t.Fatalf("during the judge call the session held %+v; want exactly the admitted auto_effort request", inFlight)
	}

	first := storedEnvelopes(t, engine, handle.ID)
	if len(first) != 1 {
		t.Fatalf("envelopes after one judge call = %+v", first)
	}
	env := first[0]
	if env.RequestID != inFlight[0].RequestID {
		t.Fatalf("the settled envelope %s is not the admitted one %s", env.RequestID, inFlight[0].RequestID)
	}
	if env.Purpose != session.PurposeAutoEffort || env.State != string(session.RequestAnswered) || env.Reason != "" {
		t.Fatalf("answered envelope=%+v", env)
	}
	if string(env.Decision) != `{"level":"high"}` {
		t.Fatalf("decision=%s", env.Decision)
	}
	// The fresh session's goal row is the judge's input and is already stored: there is no pending
	// input, no previous request and no assistant reply before this Start.
	if env.PrevUserSeq.Valid || env.LastAssistantSeq.Valid || env.PendingInputDigest != "" {
		t.Fatalf("first turn refs=%+v", env)
	}
	if env.AnsweredSeq.Valid {
		t.Fatalf("a judge request stored an answered seq: %+v", env)
	}
	// The same row through the Store API the runtime writes with.
	settled, err := engine.store.Request(context.Background(), env.RequestID)
	if err != nil || settled.State != session.RequestAnswered || string(settled.Decision) != `{"level":"high"}` {
		t.Fatalf("Store.Request = %+v, %v", settled, err)
	}
	if len(env.RunID) != 32 || strings.Trim(env.RunID, "0123456789abcdef") != "" {
		t.Fatalf("run id %q is not a 128-bit hex identity", env.RunID)
	}

	// A second Start with new input: the transcript holds the goal (seq 0) and the first answer
	// (seq 1), the new input is pending until BeginRun, and the judge is asked again.
	judge.level = "low"
	const input = "second question"
	if r := run(t, engine, handle.ID, AgentOptions{Input: input, Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("second run=%+v", r)
	}
	if judge.calls.Load() != 2 || len(judge.observed) != 2 {
		t.Fatalf("judge calls=%d observations=%d", judge.calls.Load(), len(judge.observed))
	}
	if judge.otherAdmitted != 0 {
		t.Fatalf("%d main requests were open while a judge request was in flight", judge.otherAdmitted)
	}
	inFlight = judge.observed[1]
	if len(inFlight) != 2 {
		t.Fatalf("during the second judge call the session held %+v", inFlight)
	}
	states := []string{inFlight[0].State, inFlight[1].State}
	if !reflect.DeepEqual(states, []string{string(session.RequestAnswered), string(session.RequestAdmitted)}) {
		t.Fatalf("during the second judge call the envelopes were %v", states)
	}

	all := storedEnvelopes(t, engine, handle.ID)
	if len(all) != 2 {
		t.Fatalf("envelopes after two judge calls = %+v", all)
	}
	second := all[1]
	if second.State != string(session.RequestAnswered) || string(second.Decision) != `{"level":"low"}` {
		t.Fatalf("second envelope=%+v", second)
	}
	if !second.PrevUserSeq.Valid || second.PrevUserSeq.Int64 != 0 ||
		!second.LastAssistantSeq.Valid || second.LastAssistantSeq.Int64 != 1 {
		t.Fatalf("second envelope refs=%+v; want prev user 0 and last assistant 1", second)
	}
	if want := inputDigest(input); second.PendingInputDigest != want {
		t.Fatalf("pending input digest=%q, want %q", second.PendingInputDigest, want)
	}
	if second.RunID == env.RunID {
		t.Fatal("two Starts share a run id")
	}
	if main.calls.Load() != 2 {
		t.Fatalf("the session model was asked %d times", main.calls.Load())
	}
}

func TestAutoEffortWithoutAJudgeWritesNoEnvelope(t *testing.T) {
	engine := openTestEngine(t)
	main := &answering{final: "done"}
	bindRef(engine, "b0", main)
	useGlobalConfig(t, autoConfig("test/b0", "", "high", ""))

	handle := newTestSession(t, engine, "explain the build")
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run=%+v", r)
	}
	if envs := storedEnvelopes(t, engine, handle.ID); len(envs) != 0 {
		t.Fatalf("a Start without a judge model wrote %+v", envs)
	}
	d := autoDecision(t, steps(t, engine, handle.ID))
	if decisionField(t, d, "source") != agent.AutoSourceHeuristic || decisionField(t, d, "fallbackReason") != agent.AutoFallbackNoModel {
		t.Fatalf("auto decision inputs=%s", d.Inputs)
	}
}

func TestAutoEffortReusedDecisionWritesNoEnvelope(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "high"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	// The first Start has no judge model: the heuristic resolves the turn, so no envelope can exist.
	useGlobalConfig(t, autoConfig("test/b0", "", "high", ""))

	handle := newTestSession(t, engine, "explain the build")
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("first run=%+v", r)
	}
	if envs := storedEnvelopes(t, engine, handle.ID); len(envs) != 0 {
		t.Fatalf("a Start without a judge model wrote %+v", envs)
	}

	// A judge is now configured, but the continuation has no new input: the turn reuses the level it
	// already resolved, so the judge is not called and no envelope is written.
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))
	if r := run(t, engine, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("resumed run=%+v", r)
	}
	if judge.calls.Load() != 0 {
		t.Fatalf("the reused Start called the judge %d times", judge.calls.Load())
	}
	if envs := storedEnvelopes(t, engine, handle.ID); len(envs) != 0 {
		t.Fatalf("the reused decision wrote %+v", envs)
	}
	reused := false
	for _, entry := range steps(t, engine, handle.ID) {
		for _, d := range entry.Decisions {
			if d.Rule == agent.RuleEffortAuto && strings.Contains(string(d.Inputs), `"reused":true`) {
				reused = true
			}
		}
	}
	if !reused {
		t.Fatal("the continuation did not reuse the turn's auto decision")
	}
}

// blockingJudge waits for the Start's cancellation and reports it, as a provider whose request is
// cut off mid-flight does.
type blockingJudge struct {
	started chan struct{}
	calls   atomic.Int32
}

func (p *blockingJudge) Model() string { return "judge" }

func (p *blockingJudge) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls.Add(1)
	close(p.started)
	<-ctx.Done()
	return protocol.ProviderResponse{}, ctx.Err()
}

func TestCancelledAutoJudgeSettlesUnknownWithoutTouchingTheSession(t *testing.T) {
	engine := openTestEngine(t)
	main := &answering{final: "done"}
	bindRef(engine, "b0", main)
	judge := &blockingJudge{started: make(chan struct{})}
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	handle := newTestSession(t, engine, "explain the build")
	before := readSessionFacts(t, engine, handle.ID)

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := engine.RunAgent(ctx, handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
		finished <- err
	}()
	select {
	case <-judge.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the judge was never called")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the cancelled Start = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled Start did not return")
	}

	if after := readSessionFacts(t, engine, handle.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a cancelled judge changed the session:\nbefore %+v\nafter  %+v", before, after)
	}
	if main.calls.Load() != 0 {
		t.Fatalf("the session model was asked %d times by a cancelled Start", main.calls.Load())
	}
	envs := storedEnvelopes(t, engine, handle.ID)
	if len(envs) != 1 {
		t.Fatalf("envelopes after a cancelled judge = %+v", envs)
	}
	if envs[0].State != string(session.RequestUnknown) || envs[0].Reason != agent.AutoReasonCancelled {
		t.Fatalf("cancelled envelope=%+v, want unknown/cancelled", envs[0])
	}
}

func TestUnparsableAutoJudgeSettlesUnknownAndUsesTheHeuristic(t *testing.T) {
	engine := openTestEngine(t)
	main := &answering{final: "done"}
	judge := &judgeProvider{level: "definitely-not-a-level"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	const input = "First create the file, then update the handler, run the tests, and fix the failures"
	handle := newTestSession(t, engine, "heuristic fallback")
	if r := run(t, engine, handle.ID, AgentOptions{Input: input, Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run=%+v", r)
	}
	if got := lastRequest(t, main).ReasoningEffort; got != "high" {
		t.Fatalf("main request effort=%q, want the heuristic's high", got)
	}
	envs := storedEnvelopes(t, engine, handle.ID)
	if len(envs) != 1 || envs[0].State != string(session.RequestUnknown) || envs[0].Reason != agent.AutoFallbackUnparsable {
		t.Fatalf("envelopes=%+v, want one unknown/unparsable", envs)
	}
	d := autoDecision(t, steps(t, engine, handle.ID))
	if decisionField(t, d, "source") != agent.AutoSourceHeuristic || decisionField(t, d, "fallbackReason") != agent.AutoFallbackUnparsable {
		t.Fatalf("auto decision inputs=%s", d.Inputs)
	}
}

func TestFailedAutoJudgeSettlesUnknownAndUsesTheHeuristic(t *testing.T) {
	engine := openTestEngine(t)
	main := &answering{final: "done"}
	judge := &judgeProvider{err: classed(protocol.ClassAvailability)}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	const input = "First create the file, then update the handler, run the tests, and fix the failures"
	handle := newTestSession(t, engine, "provider failure")
	if r := run(t, engine, handle.ID, AgentOptions{Input: input, Effort: config.EffortAuto, MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run=%+v", r)
	}
	if got := lastRequest(t, main).ReasoningEffort; got != "high" {
		t.Fatalf("main request effort=%q, want the heuristic's high", got)
	}
	envs := storedEnvelopes(t, engine, handle.ID)
	if len(envs) != 1 || envs[0].State != string(session.RequestUnknown) || envs[0].Reason != string(protocol.ClassAvailability) {
		t.Fatalf("envelopes=%+v, want one unknown/%s", envs, protocol.ClassAvailability)
	}
}

func TestFencedSessionSendsNoJudgeRequest(t *testing.T) {
	engine := openTestEngine(t)
	main, judge := &answering{final: "done"}, &judgeProvider{level: "high"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))

	handle := newTestSession(t, engine, "fenced before the judge")
	before := readSessionFacts(t, engine, handle.ID)
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at)
		VALUES('session', ?, 1, 'revoked', 'active', 't', 't')`, handle.ID); err != nil {
		t.Fatal(err)
	}

	_, err = engine.RunAgent(context.Background(), handle.ID, AgentOptions{Effort: config.EffortAuto, MaxSteps: 2})
	if !errors.Is(err, session.ErrFenced) {
		t.Fatalf("Start on a fenced session = %v, want ErrFenced", err)
	}
	if judge.calls.Load() != 0 || main.calls.Load() != 0 {
		t.Fatalf("a fenced Start called judge=%d model=%d", judge.calls.Load(), main.calls.Load())
	}
	// A session that is itself fenced cannot be projected: the Start is refused before anything is
	// admitted (T3-S2, DATA_AND_FAILURES §5 step 3), so not even a failed envelope is written. The
	// store's failed envelope for an admission that meets a fence is covered in internal/session.
	if envs := storedEnvelopes(t, engine, handle.ID); len(envs) != 0 {
		t.Fatalf("envelopes=%+v, want none", envs)
	}
	if after := readSessionFacts(t, engine, handle.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a fenced Start changed the session:\nbefore %+v\nafter  %+v", before, after)
	}
}
