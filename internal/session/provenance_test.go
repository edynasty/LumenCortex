package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// These tests exercise the request-provenance persistence layer (ADR-0018) against a real SQLite
// database migrated to version 6, with HOME and the XDG directories pointed at temporary directories.

func provStore(t *testing.T) *Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	return gapStore(t)
}

func sha(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func i64(v int64) *int64 { return &v }

func mainAdmission(session, id string) RequestAdmission {
	return RequestAdmission{
		RequestID: id, SessionID: session, RunID: "run1", Step: 1, Attempt: 0, Purpose: PurposeMain,
		FirstSeq: i64(0), LastSeq: i64(4), PendingInputDigest: sha("a"), Refs: []string{"ts:3:note:u1a2b", "sys:project-instructions:v1"},
		ToolSpecsDigest: sha("b"), ProjectionVersion: 1, LineageComplete: true,
	}
}

func autoAdmission(session, id string) RequestAdmission {
	return RequestAdmission{
		RequestID: id, SessionID: session, Purpose: PurposeAutoEffort, PrevUserSeq: i64(2), LastAssistantSeq: i64(3),
		PendingInputDigest: sha("c"), LineageComplete: true,
	}
}

func admit(t *testing.T, store *Store, a RequestAdmission) RequestEnvelope {
	t.Helper()
	env, err := store.AdmitRequest(context.Background(), a)
	if err != nil {
		t.Fatalf("AdmitRequest %s: %v", a.RequestID, err)
	}
	return env
}

func assistant(text string) protocol.Message {
	return protocol.Message{Role: "assistant", Content: text}
}

func addFence(t *testing.T, store *Store, scopeKind, scopeRef string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at) VALUES(?, ?, 3, 'revoked', 'active', 't', 't')`, scopeKind, scopeRef); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// reach brings a fresh request of the given purpose to a state through the public API.
func reach(t *testing.T, store *Store, session, id, purpose string, state RequestState) {
	t.Helper()
	ctx := context.Background()
	a := mainAdmission(session, id)
	if purpose == PurposeAutoEffort {
		a = autoAdmission(session, id)
	}
	admit(t, store, a)
	switch state {
	case RequestAdmitted:
	case RequestAnswered:
		if purpose == PurposeMain {
			if _, err := store.AppendAnswered(ctx, session, id, assistant("done")); err != nil {
				t.Fatal(err)
			}
		} else if err := store.SettleRequest(ctx, id, Settlement{To: RequestAnswered, Decision: json.RawMessage(`{"level":"high"}`)}); err != nil {
			t.Fatal(err)
		}
	default:
		if err := store.SettleRequest(ctx, id, Settlement{To: state, Reason: "test_reason"}); err != nil {
			t.Fatal(err)
		}
	}
}

var allRequestStates = []RequestState{RequestAdmitted, RequestAnswered, RequestFailed, RequestUnknown}

// Every (from, to) pair of the request machine, through SettleRequest (a non-main purpose, so answered
// is reachable too) and, for a main request, through AppendAnswered. Only admitted moves, only to a
// terminal state; a refused move changes nothing.
func TestRequestStateMachineEveryPair(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	n := 0
	for _, from := range allRequestStates {
		for _, to := range allRequestStates {
			n++
			id := fmt.Sprintf("auto-%d", n)
			reach(t, store, "s", id, PurposeAutoEffort, from)
			before, err := store.Request(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			st := Settlement{To: to, Reason: "test_reason"}
			if to == RequestAnswered {
				st = Settlement{To: to, Decision: json.RawMessage(`{"level":"low"}`)}
			}
			err = store.SettleRequest(ctx, id, st)
			after, _ := store.Request(ctx, id)
			if from == RequestAdmitted && to != RequestAdmitted {
				if err != nil || after.State != to {
					t.Errorf("%s -> %s: err=%v state=%s", from, to, err, after.State)
				}
				continue
			}
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s -> %s: err=%v, want ErrInvalidTransition", from, to, err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Errorf("%s -> %s refused but the row changed:\n%+v\n%+v", from, to, before, after)
			}
		}
	}
	for _, from := range allRequestStates {
		id := "main-" + string(from)
		reach(t, store, "s", id, PurposeMain, from)
		before, _ := store.Request(ctx, id)
		messages := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`)
		seq, err := store.AppendAnswered(ctx, "s", id, assistant("second answer"))
		if from == RequestAdmitted {
			if err != nil {
				t.Errorf("AppendAnswered from admitted: %v", err)
			}
			if got, _ := store.Request(ctx, id); got.State != RequestAnswered || got.AnsweredSeq == nil || *got.AnsweredSeq != seq {
				t.Errorf("answered envelope %+v, want answered_seq %d", got, seq)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("AppendAnswered from %s: %v, want ErrInvalidTransition", from, err)
		}
		if after, _ := store.Request(ctx, id); !reflect.DeepEqual(before, after) {
			t.Errorf("AppendAnswered from %s refused but the row changed", from)
		}
		if got := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`); got != messages {
			t.Errorf("AppendAnswered from %s refused but stored a message (%d -> %d)", from, messages, got)
		}
	}
}

func TestSettleRequestRefusesWhatOnlyAppendAnsweredMayDo(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	reach(t, store, "s", "m", PurposeMain, RequestAdmitted)
	if err := store.SettleRequest(ctx, "m", Settlement{To: RequestAnswered}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a main request settled to answered without a message: %v", err)
	}
	for name, st := range map[string]Settlement{
		"unknown target":          {To: "prepared", Reason: "r"},
		"failed without reason":   {To: RequestFailed},
		"unknown without reason":  {To: RequestUnknown},
		"reason is prose":         {To: RequestUnknown, Reason: "the provider said: here is the secret prompt"},
		"decision on unknown":     {To: RequestUnknown, Reason: "r", Decision: json.RawMessage(`{"level":"high"}`)},
		"reason on answered":      {To: RequestAnswered, Reason: "r"},
		"nested decision":         {To: RequestAnswered, Decision: json.RawMessage(`{"level":{"a":1}}`)},
		"decision with a body":    {To: RequestAnswered, Decision: json.RawMessage(`{"level":"please refactor the whole payment module"}`)},
		"decision not an object":  {To: RequestAnswered, Decision: json.RawMessage(`["high"]`)},
		"decision over the bound": {To: RequestAnswered, Decision: json.RawMessage(`{"k":"` + strings.Repeat("x", MaxDecisionBytes) + `"}`)},
	} {
		auto := "auto-" + strings.ReplaceAll(name, " ", "-")
		admit(t, store, autoAdmission("s", auto))
		if err := store.SettleRequest(ctx, auto, st); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if got, _ := store.Request(ctx, auto); got.State != RequestAdmitted {
			t.Errorf("%s: refused but the request is %s", name, got.State)
		}
	}
	if err := store.SettleRequest(ctx, "missing", Settlement{To: RequestUnknown, Reason: "r"}); !errors.Is(err, ErrRequestNotFound) {
		t.Errorf("missing request: %v", err)
	}
	auto := "auto-ok"
	admit(t, store, autoAdmission("s", auto))
	if err := store.SettleRequest(ctx, auto, Settlement{To: RequestAnswered, Decision: json.RawMessage(`{"level":"high"}`)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Request(ctx, auto); got.State != RequestAnswered || string(got.Decision) != `{"level":"high"}` || got.AnsweredSeq != nil {
		t.Errorf("auto judge answer: %+v", got)
	}
}

func TestAdmitRequestStoresTheEnvelopeAndRefusesBadInput(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	env := admit(t, store, mainAdmission("s", "r1"))
	got, err := store.Request(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	env.CreatedAt, env.UpdatedAt, got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(env, got) {
		t.Errorf("returned envelope differs from the stored one:\n%+v\n%+v", env, got)
	}
	if got.State != RequestAdmitted || !got.LineageComplete || got.RunID != "run1" || *got.FirstSeq != 0 || *got.LastSeq != 4 ||
		!reflect.DeepEqual(got.Refs, []string{"ts:3:note:u1a2b", "sys:project-instructions:v1"}) {
		t.Errorf("stored envelope %+v", got)
	}
	if _, err := store.AdmitRequest(ctx, mainAdmission("s", "r1")); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate request id: %v", err)
	}
	if _, err := store.AdmitRequest(ctx, mainAdmission("nope", "r2")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing session: %v", err)
	}

	mutate := func(f func(*RequestAdmission)) RequestAdmission { a := mainAdmission("s", "bad"); f(&a); return a }
	for name, a := range map[string]RequestAdmission{
		"empty id":                  mutate(func(a *RequestAdmission) { a.RequestID = "" }),
		"unknown purpose":           mutate(func(a *RequestAdmission) { a.Purpose = "maintenance" }),
		"main without a window":     mutate(func(a *RequestAdmission) { a.FirstSeq, a.LastSeq = nil, nil }),
		"half a window":             mutate(func(a *RequestAdmission) { a.LastSeq = nil }),
		"inverted window":           mutate(func(a *RequestAdmission) { a.FirstSeq, a.LastSeq = i64(5), i64(1) }),
		"negative seq":              mutate(func(a *RequestAdmission) { a.PrevUserSeq = i64(-1) }),
		"digest of a sentence":      mutate(func(a *RequestAdmission) { a.PendingInputDigest = "please refactor everything" }),
		"ref that is prose":         mutate(func(a *RequestAdmission) { a.Refs = []string{"fix the login bug"} }),
		"ref over the byte bound":   mutate(func(a *RequestAdmission) { a.Refs = []string{strings.Repeat("x", MaxEnvelopeRefBytes+1)} }),
		"run id with a space":       mutate(func(a *RequestAdmission) { a.RunID = "run 1" }),
		"negative projection":       mutate(func(a *RequestAdmission) { a.ProjectionVersion = -1 }),
		"tool specs digest garbage": mutate(func(a *RequestAdmission) { a.ToolSpecsDigest = "sha256:xyz" }),
	} {
		if _, err := store.AdmitRequest(ctx, a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes`); n != 1 {
		t.Errorf("%d envelopes after the refusals, want only r1", n)
	}

	// Refs over the bound are cut, never silently complete.
	long := mainAdmission("s", "long")
	for i := 0; i < MaxEnvelopeRefs+5; i++ {
		long.Refs = append(long.Refs, fmt.Sprintf("msg:%d", i))
	}
	cut := admit(t, store, long)
	if len(cut.Refs) != MaxEnvelopeRefs || cut.LineageComplete {
		t.Errorf("long ref list: %d refs, lineage_complete=%v", len(cut.Refs), cut.LineageComplete)
	}
	// The largest legal ref list fits the column bound.
	full := mainAdmission("s", "full")
	full.Refs = nil
	for i := 0; i < MaxEnvelopeRefs; i++ {
		full.Refs = append(full.Refs, strings.Repeat("r", MaxEnvelopeRefBytes-5)+fmt.Sprintf("%05d", i))
	}
	if e := admit(t, store, full); !e.LineageComplete || len(e.Refs) != MaxEnvelopeRefs {
		t.Errorf("a full legal ref list was cut: %d refs complete=%v", len(e.Refs), e.LineageComplete)
	}
}

// The DDL bounds and the Go constants describe the same limits.
func TestBoundsMatchSchema(t *testing.T) {
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	insert := func(refs string) error {
		_, err := store.db.Exec(`INSERT INTO request_envelopes(request_id, session_id, purpose, state, refs_json, lineage_complete, created_at, updated_at)
			VALUES('r' || abs(random()), 's', 'main', 'admitted', ?, 0, 't', 't')`, refs)
		return err
	}
	atBound := `["` + strings.Repeat("x", MaxRefsJSONBytes-4) + `"]`
	if len(atBound) != MaxRefsJSONBytes {
		t.Fatalf("test refs are %d bytes", len(atBound))
	}
	if err := insert(atBound); err != nil {
		t.Errorf("refs_json of exactly MaxRefsJSONBytes rejected: %v", err)
	}
	if err := insert(`["` + strings.Repeat("x", MaxRefsJSONBytes-3) + `"]`); err == nil {
		t.Error("refs_json one byte over the bound accepted")
	}
	worst := make([]string, MaxEnvelopeRefs)
	for i := range worst {
		worst[i] = strings.Repeat("%", MaxEnvelopeRefBytes) // the widest charset member under JSON
	}
	raw, _ := json.Marshal(worst)
	if len(raw) > MaxRefsJSONBytes {
		t.Errorf("MaxEnvelopeRefs x MaxEnvelopeRefBytes encode to %d bytes, over %d", len(raw), MaxRefsJSONBytes)
	}
	edgeInsert := func(n int) error {
		_, err := store.db.Exec(`INSERT INTO derived_origins(session_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete, created_at, updated_at)
			VALUES('s', 'file', ?, 'file', 'x', 'external_data', 0, 't', 't')`, strings.Repeat("p", n))
		return err
	}
	if err := edgeInsert(MaxEdgeRefBytes); err != nil {
		t.Errorf("edge ref at MaxEdgeRefBytes rejected: %v", err)
	}
	if err := edgeInsert(MaxEdgeRefBytes + 1); err == nil {
		t.Error("edge ref over MaxEdgeRefBytes accepted")
	}
	decision := func(n int) error {
		_, err := store.db.Exec(`INSERT INTO request_envelopes(request_id, session_id, purpose, state, decision_json, lineage_complete, created_at, updated_at)
			VALUES('d' || abs(random()), 's', 'auto_effort', 'answered', ?, 0, 't', 't')`, `{"k":"`+strings.Repeat("x", n-8)+`"}`)
		return err
	}
	if err := decision(MaxDecisionBytes); err != nil {
		t.Errorf("decision at MaxDecisionBytes rejected: %v", err)
	}
	if err := decision(MaxDecisionBytes + 1); err == nil {
		t.Error("decision over MaxDecisionBytes accepted")
	}
}

func TestAdmissionOnAFencedSessionIsRefusedAndRecorded(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	gapSession(t, store, "other", "idle", "")
	addFence(t, store, "session", "s")

	env, err := store.AdmitRequest(ctx, mainAdmission("s", "r"))
	if !errors.Is(err, ErrFenced) || env.State != RequestFailed || env.Reason != ReasonFenced || env.FenceEpoch != 3 {
		t.Fatalf("fenced admission: env=%+v err=%v", env, err)
	}
	if got, _ := store.Request(ctx, "r"); got.State != RequestFailed || got.Reason != ReasonFenced {
		t.Errorf("the refusal was not recorded: %+v", got)
	}
	if err := store.AdmitExecution(ctx, execAdmission("s", "e", "r")); !errors.Is(err, ErrFenced) {
		t.Errorf("fenced AdmitExecution: %v", err)
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins`); n != 0 {
		t.Errorf("a refused execution left %d rows", n)
	}
	if _, err := store.AdmitRequest(ctx, mainAdmission("other", "r-other")); err != nil {
		t.Errorf("an unrelated session was refused: %v", err)
	}
}

// A fence that appears between admission and the answer stops the message: the envelope is settled
// unknown, committed, and the message body is not stored.
func TestAppendAnsweredOnAFencedSessionStoresNoMessage(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))
	before := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`)
	addFence(t, store, "session", "s")
	if _, err := store.AppendAnswered(ctx, "s", "r", assistant("SECRET-ANSWER")); !errors.Is(err, ErrFenced) {
		t.Fatalf("AppendAnswered: %v, want ErrFenced", err)
	}
	if got, _ := store.Request(ctx, "r"); got.State != RequestUnknown || got.Reason != ReasonFencedAnswer {
		t.Errorf("envelope %+v, want unknown/%s", got, ReasonFencedAnswer)
	}
	if after := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`); after != before {
		t.Errorf("the fenced answer was stored (%d -> %d rows)", before, after)
	}
	if n := count(t, store, `SELECT count(*) FROM session_messages WHERE json LIKE '%SECRET-ANSWER%'`); n != 0 {
		t.Error("the fenced answer text is in the transcript")
	}
}

func TestAppendAnsweredIsAtomicAndOwnerChecked(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	gapSession(t, store, "t", "idle", "")
	admit(t, store, mainAdmission("s", "r"))

	if _, err := store.AppendAnswered(ctx, "t", "r", assistant("x")); !errors.Is(err, ErrRequestNotFound) {
		t.Errorf("a request of another session: %v", err)
	}
	if _, err := store.AppendAnswered(ctx, "s", "r", protocol.Message{Role: "user", Content: "x"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a user message: %v", err)
	}
	autoID := "auto"
	admit(t, store, autoAdmission("s", autoID))
	if _, err := store.AppendAnswered(ctx, "s", autoID, assistant("x")); !errors.Is(err, ErrInvalid) {
		t.Errorf("a non-main request answered by a message: %v", err)
	}

	// The message insert fails: the envelope must stay admitted.
	if _, err := store.db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON session_messages BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendAnswered(ctx, "s", "r", assistant("x")); err == nil {
		t.Fatal("AppendAnswered ignored the failing insert")
	}
	if got, _ := store.Request(ctx, "r"); got.State != RequestAdmitted || got.AnsweredSeq != nil {
		t.Errorf("a failed answer changed the envelope: %+v", got)
	}
	if _, err := store.db.Exec(`DROP TRIGGER boom`); err != nil {
		t.Fatal(err)
	}
	// The envelope update fails after the message insert: the message must not remain.
	if _, err := store.db.Exec(`CREATE TRIGGER boom BEFORE UPDATE ON request_envelopes BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	rows := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`)
	if _, err := store.AppendAnswered(ctx, "s", "r", assistant("x")); err == nil {
		t.Fatal("AppendAnswered ignored the failing update")
	}
	if got := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`); got != rows {
		t.Errorf("a message survived a rolled-back answer (%d -> %d rows)", rows, got)
	}
	if _, err := store.db.Exec(`DROP TRIGGER boom`); err != nil {
		t.Fatal(err)
	}
	seq, err := store.AppendAnswered(ctx, "s", "r", assistant("fine"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Request(ctx, "r"); got.State != RequestAnswered || *got.AnsweredSeq != seq {
		t.Errorf("answered envelope %+v", got)
	}
	m := rowsOf(t, store, "s")
	if last := m[len(m)-1]; last.Role != "assistant" || last.Content != "fine" {
		t.Errorf("last transcript row %+v", last)
	}
}

func execAdmission(session, id, request string) ExecutionAdmission {
	return ExecutionAdmission{SessionID: session, RunID: "run1", ExecutionID: id, RequestID: request, ToolName: "shell", ArgsDigest: sha("d"), PreRef: "git:abc123"}
}

func toolCommit(session, exec string, outcome ExecutionOutcome, coverage Coverage, edges ...DerivedEdge) ToolCommit {
	return ToolCommit{
		SessionID: session, RunID: "run1",
		Message:   protocol.Message{Role: "tool", ToolCallID: "call", Name: "shell", Content: "ok"},
		Execution: &ExecutionResult{ExecutionID: exec, Outcome: outcome, PostRef: "git:def456", Coverage: coverage},
		Edges:     edges,
	}
}

func fileEdge(path string) DerivedEdge {
	return DerivedEdge{Kind: "file", TargetRef: path + "@sha1:1234", SourceKind: SourceRequest, SourceRef: "r", OriginClass: OriginAgentGen, LineageComplete: true}
}

func TestExecutionMachineEveryPairAndLineageRules(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))

	// A second commit of any terminal execution is refused, whatever the target outcome, and changes
	// nothing.
	finals := []ExecutionOutcome{OutcomeComplete, OutcomeFailed, OutcomeUnknown}
	for _, first := range finals {
		id := "e-" + string(first)
		if err := store.AdmitExecution(ctx, execAdmission("s", id, "r")); err != nil {
			t.Fatal(err)
		}
		if rec, _ := store.Execution(ctx, id); rec.Outcome != OutcomeAdmitted || !rec.LineageComplete || rec.PreRef != "git:abc123" {
			t.Fatalf("admitted record %+v", rec)
		}
		if _, _, err := store.CommitToolResult(ctx, toolCommit("s", id, first, CoverageComplete)); err != nil {
			t.Fatalf("admitted -> %s: %v", first, err)
		}
		done, _ := store.Execution(ctx, id)
		if done.Outcome != first {
			t.Errorf("outcome %s, want %s", done.Outcome, first)
		}
		for _, second := range finals {
			rows := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`)
			if _, _, err := store.CommitToolResult(ctx, toolCommit("s", id, second, CoveragePartial)); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s -> %s: %v, want ErrInvalidTransition", first, second, err)
			}
			if got, _ := store.Execution(ctx, id); !reflect.DeepEqual(got, done) {
				t.Errorf("%s -> %s refused but the record changed", first, second)
			}
			if got := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`); got != rows {
				t.Errorf("%s -> %s refused but a message was stored", first, second)
			}
		}
	}
	if err := store.AdmitExecution(ctx, execAdmission("s", "e-completed", "r")); !errors.Is(err, ErrExists) {
		t.Errorf("an execution id reused: %v", err)
	}
	if _, _, err := store.CommitToolResult(ctx, toolCommit("s", "ghost", OutcomeComplete, CoverageComplete)); !errors.Is(err, ErrExecutionNotFound) {
		t.Errorf("unknown execution: %v", err)
	}

	// lineage_complete is only ever lowered.
	cases := []struct {
		name     string
		admit    func(*ExecutionAdmission)
		outcome  ExecutionOutcome
		coverage Coverage
		want     bool
	}{
		{"complete", func(*ExecutionAdmission) {}, OutcomeComplete, CoverageComplete, true},
		{"failed tool, complete snapshot", func(*ExecutionAdmission) {}, OutcomeFailed, CoverageComplete, true},
		{"partial coverage", func(*ExecutionAdmission) {}, OutcomeComplete, CoveragePartial, false},
		{"unavailable coverage", func(*ExecutionAdmission) {}, OutcomeComplete, CoverageUnavailable, false},
		{"pre unavailable", func(a *ExecutionAdmission) { a.PreRef, a.PreUnavailable = "", true }, OutcomeComplete, CoverageComplete, false},
		{"outcome unknown", func(*ExecutionAdmission) {}, OutcomeUnknown, CoverageComplete, false},
	}
	for i, tc := range cases {
		id := fmt.Sprintf("lin-%d", i)
		a := execAdmission("s", id, "r")
		tc.admit(&a)
		if err := store.AdmitExecution(ctx, a); err != nil {
			t.Fatal(err)
		}
		if rec, _ := store.Execution(ctx, id); rec.LineageComplete != !a.PreUnavailable {
			t.Errorf("%s: admitted lineage_complete=%v", tc.name, rec.LineageComplete)
		}
		if _, _, err := store.CommitToolResult(ctx, toolCommit("s", id, tc.outcome, tc.coverage)); err != nil {
			t.Fatal(err)
		}
		if rec, _ := store.Execution(ctx, id); rec.LineageComplete != tc.want {
			t.Errorf("%s: lineage_complete=%v, want %v", tc.name, rec.LineageComplete, tc.want)
		}
	}
}

func TestCommitToolResultWritesRowStateExecutionAndEdgesTogether(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))
	if err := store.AdmitExecution(ctx, execAdmission("s", "e", "r")); err != nil {
		t.Fatal(err)
	}
	c := toolCommit("s", "e", OutcomeComplete, CoverageComplete, fileEdge("a.go"), fileEdge("b.go"))
	c.TaskState = &TaskStateWrite{Base: 0, Reason: "tool", JSON: json.RawMessage(`{"windowStartSeq":0}`)}
	seq, version, err := store.CommitToolResult(ctx, c)
	if err != nil || version == 0 {
		t.Fatalf("seq=%d version=%d err=%v", seq, version, err)
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins WHERE kind = 'file' AND execution_id = 'e' AND session_id = 's' AND lineage_complete = 1 AND support_status = 'unverified'`); n != 2 {
		t.Errorf("%d edges stored, want 2 unverified complete edges linked to the execution", n)
	}
	if rec, _ := store.Execution(ctx, "e"); rec.Outcome != OutcomeComplete || rec.PostRef != "git:def456" || rec.Coverage != CoverageComplete {
		t.Errorf("execution %+v", rec)
	}

	// A failure in the last step (the edge insert) rolls back the message, the Task State version and
	// the execution update.
	if err := store.AdmitExecution(ctx, execAdmission("s", "e2", "r")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON derived_origins WHEN NEW.kind = 'file' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	rows := count(t, store, `SELECT count(*) FROM session_messages`)
	c2 := toolCommit("s", "e2", OutcomeComplete, CoverageComplete, fileEdge("c.go"))
	c2.TaskState = &TaskStateWrite{Base: version, Reason: "tool", JSON: json.RawMessage(`{"windowStartSeq":1}`)}
	if _, _, err := store.CommitToolResult(ctx, c2); err == nil {
		t.Fatal("CommitToolResult ignored the failing edge insert")
	}
	if rec, _ := store.Execution(ctx, "e2"); rec.Outcome != OutcomeAdmitted {
		t.Errorf("a failed commit changed the execution: %+v", rec)
	}
	if got := count(t, store, `SELECT count(*) FROM session_messages`); got != rows {
		t.Errorf("a message survived the rolled-back commit (%d -> %d)", rows, got)
	}
	if latest, _ := store.LatestTaskState(ctx, "s"); latest.Version != version {
		t.Errorf("Task State advanced to %d inside a rolled-back commit", latest.Version)
	}
}

func TestCommitToolResultBoundsAndValidatesEdges(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))
	if err := store.AdmitExecution(ctx, execAdmission("s", "e", "r")); err != nil {
		t.Fatal(err)
	}
	var edges []DerivedEdge
	for i := 0; i < MaxEdgesPerCommit+3; i++ {
		edges = append(edges, fileEdge(fmt.Sprintf("f%d.go", i)))
	}
	if _, _, err := store.CommitToolResult(ctx, toolCommit("s", "e", OutcomeComplete, CoverageComplete, edges...)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins WHERE kind = 'file'`); n != MaxEdgesPerCommit {
		t.Errorf("%d edges stored, want the bound %d", n, MaxEdgesPerCommit)
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins WHERE lineage_complete = 1`); n != 0 {
		t.Errorf("%d rows still claim a complete lineage after the edge list was cut", n)
	}

	for name, e := range map[string]DerivedEdge{
		"kind execution":     {Kind: "execution", TargetRef: "t", SourceKind: SourceRequest, SourceRef: "r", OriginClass: OriginAgentGen},
		"unknown kind":       {Kind: "blob", TargetRef: "t", SourceKind: SourceRequest, SourceRef: "r", OriginClass: OriginAgentGen},
		"unknown origin":     {Kind: "file", TargetRef: "t", SourceKind: SourceRequest, SourceRef: "r", OriginClass: "trusted"},
		"origin execution":   {Kind: "file", TargetRef: "t", SourceKind: SourceRequest, SourceRef: "r", OriginClass: OriginToolExecution},
		"empty source":       {Kind: "file", TargetRef: "t", SourceKind: SourceRequest, OriginClass: OriginAgentGen},
		"control characters": {Kind: "file", TargetRef: "a\nb", SourceKind: SourceRequest, SourceRef: "r", OriginClass: OriginAgentGen},
		"ref over the bound": {Kind: "file", TargetRef: strings.Repeat("x", MaxEdgeRefBytes+1), SourceKind: SourceRequest, SourceRef: "r", OriginClass: OriginAgentGen},
	} {
		c := ToolCommit{SessionID: "s", Message: protocol.Message{Role: "tool", Content: "x"}, Edges: []DerivedEdge{e}}
		if _, _, err := store.CommitToolResult(ctx, c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := store.CommitToolResult(ctx, ToolCommit{SessionID: "s", Message: protocol.Message{Role: "assistant"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a non-tool message: %v", err)
	}
}

// A revoked source stays revoked: a new tool result cannot re-introduce it through a fresh edge, and the
// result body is dropped while the execution is still settled so that it is never left admitted.
func TestAFencedSourceCannotBeCitedByANewRecord(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))
	for _, scope := range []struct{ kind, ref string }{{"session", "s"}, {"source", SourceScopeRef(SourceSession, "src-session#4")}} {
		store.db.Exec(`DELETE FROM source_fences`)
		execID := "e-" + scope.kind
		if err := store.AdmitExecution(ctx, execAdmission("s", execID, "r")); err != nil {
			t.Fatal(err)
		}
		addFence(t, store, scope.kind, scope.ref)
		rows := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`)
		edge := DerivedEdge{Kind: "message", TargetRef: "s#9", SourceKind: SourceSession, SourceRef: "src-session#4", OriginClass: OriginExternalData, LineageComplete: true}
		_, _, err := store.CommitToolResult(ctx, toolCommit("s", execID, OutcomeComplete, CoverageComplete, edge))
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("%s fence: %v, want ErrFenced", scope.kind, err)
		}
		if got := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 's'`); got != rows {
			t.Errorf("%s fence: the tool result body was stored", scope.kind)
		}
		if n := count(t, store, `SELECT count(*) FROM derived_origins WHERE kind = 'message'`); n != 0 {
			t.Errorf("%s fence: a revoked source was cited by a new edge", scope.kind)
		}
		rec, _ := store.Execution(ctx, execID)
		if rec.Outcome != OutcomeComplete || rec.LineageComplete {
			t.Errorf("%s fence: execution %+v, want settled with an incomplete lineage", scope.kind, rec)
		}
	}
	// Marking a fence cleaned does not lift it.
	store.db.Exec(`UPDATE source_fences SET state = 'cleaned'`)
	if _, err := store.AdmitRequest(ctx, mainAdmission("s", "after-clean")); !errors.Is(err, ErrFenced) && count(t, store, `SELECT count(*) FROM source_fences WHERE scope_kind = 'session'`) > 0 {
		t.Errorf("a cleaned session fence let a request in: %v", err)
	}
}

// Reconcile settles the crash residue in the same transaction as the transcript repair and changes
// nothing a second time.
func TestReconcileSettlesProvenanceIdempotently(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "running", "")
	gapSession(t, store, "other", "running", "")

	reach(t, store, "s", "r-admitted", PurposeMain, RequestAdmitted)
	reach(t, store, "s", "r-auto", PurposeAutoEffort, RequestAdmitted)
	reach(t, store, "s", "r-answered", PurposeMain, RequestAnswered)
	reach(t, store, "s", "r-failed", PurposeMain, RequestFailed)
	reach(t, store, "s", "r-unknown", PurposeMain, RequestUnknown)
	reach(t, store, "other", "r-other", PurposeMain, RequestAdmitted)

	// The crashed run had called a tool: assistant row with the call, no result row.
	gapRow(t, store, "s", "assistant", protocol.Message{ToolCalls: calls("call-1")})
	if err := store.AdmitExecution(ctx, execAdmission("s", "e-crashed", "r-answered")); err != nil {
		t.Fatal(err)
	}
	if err := store.AdmitExecution(ctx, execAdmission("s", "e-done", "r-answered")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CommitToolResult(ctx, toolCommit("s", "e-done", OutcomeComplete, CoverageComplete)); err != nil {
		t.Fatal(err)
	}
	// The completed tool appended a row after the assistant row, so the repair of the gap is what the
	// interrupted row is for; build the crash gap after it.
	gapRow(t, store, "s", "assistant", protocol.Message{ToolCalls: calls("call-2")})

	snapshot := func() string {
		rows, err := store.db.Query(`SELECT request_id, session_id, state, COALESCE(reason,''), lineage_complete, COALESCE(answered_seq,-1), updated_at FROM request_envelopes ORDER BY request_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, sess, state, reason, upd string
			var lc, ans int
			if err := rows.Scan(&id, &sess, &state, &reason, &lc, &ans, &upd); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s|%s|%s|%s|%d|%d|%s\n", id, sess, state, reason, lc, ans, upd)
		}
		ex, err := store.db.Query(`SELECT execution_id, outcome, lineage_complete, COALESCE(post_ref,''), updated_at FROM derived_origins WHERE kind = 'execution' ORDER BY execution_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer ex.Close()
		for ex.Next() {
			var id, outcome, post, upd string
			var lc int
			if err := ex.Scan(&id, &outcome, &lc, &post, &upd); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s|%s|%d|%s|%s\n", id, outcome, lc, post, upd)
		}
		return b.String()
	}
	before := snapshot()
	inserted, err := store.Reconcile(ctx, "s")
	if err != nil || inserted != 1 {
		t.Fatalf("Reconcile: inserted=%d err=%v", inserted, err)
	}
	want := map[string]RequestState{
		"r-admitted": RequestUnknown, "r-auto": RequestUnknown, "r-answered": RequestAnswered, "r-failed": RequestFailed,
		"r-unknown": RequestUnknown, "r-other": RequestAdmitted,
	}
	for id, state := range want {
		got, err := store.Request(ctx, id)
		if err != nil || got.State != state {
			t.Errorf("%s: %+v (err %v), want %s", id, got, err, state)
		}
	}
	if got, _ := store.Request(ctx, "r-admitted"); got.Reason != ReasonRestartNoAnswer {
		t.Errorf("reason %q, want %q", got.Reason, ReasonRestartNoAnswer)
	}
	if rec, _ := store.Execution(ctx, "e-crashed"); rec.Outcome != OutcomeUnknown || rec.LineageComplete || rec.PostRef != "" {
		t.Errorf("crashed execution %+v, want outcome_unknown, incomplete, no post", rec)
	}
	if rec, _ := store.Execution(ctx, "e-done"); rec.Outcome != OutcomeComplete || !rec.LineageComplete || rec.PostRef == "" {
		t.Errorf("completed execution changed: %+v", rec)
	}
	afterFirst := snapshot()
	if afterFirst == before {
		t.Fatal("Reconcile changed nothing")
	}
	// Idempotent: the second run changes nothing, not even updated_at, and appends no row.
	second, err := store.Reconcile(ctx, "s")
	if err != nil || second != 0 {
		t.Fatalf("second Reconcile: inserted=%d err=%v", second, err)
	}
	if got := snapshot(); got != afterFirst {
		t.Errorf("second Reconcile changed provenance:\n%s\nwant\n%s", got, afterFirst)
	}
	// A tool is never replayed: the crashed execution cannot be completed after the recovery.
	if _, _, err := store.CommitToolResult(ctx, toolCommit("s", "e-crashed", OutcomeComplete, CoverageComplete)); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a recovered execution was committed late: %v", err)
	}
	// And the late answer of a recovered request is refused the same way.
	if _, err := store.AppendAnswered(ctx, "s", "r-admitted", assistant("late")); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a recovered request was answered late: %v", err)
	}
	// The other session keeps its admitted row until its own Reconcile.
	if got, _ := store.Request(ctx, "r-other"); got.State != RequestAdmitted {
		t.Errorf("Reconcile of s touched another session: %+v", got)
	}
}

func TestReconcileOfAMissingSessionStillFailsAndWritesNothing(t *testing.T) {
	store := provStore(t)
	if _, err := store.Reconcile(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reconcile of a missing session: %v", err)
	}
}

// Deleting the consumer removes its envelopes and edges; deleting a source session leaves the
// consumer's edges in place; fences and approvals outlive every session.
func TestSessionDeletionAndProvenance(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "consumer", "idle", "")
	gapSession(t, store, "source", "idle", "")
	admit(t, store, mainAdmission("consumer", "r-consumer"))
	admit(t, store, mainAdmission("source", "r-source"))
	if err := store.AdmitExecution(ctx, execAdmission("consumer", "e", "r-consumer")); err != nil {
		t.Fatal(err)
	}
	edge := DerivedEdge{Kind: "child_session", TargetRef: "consumer#2", SourceKind: SourceSession, SourceRef: "source#7", OriginClass: OriginAgentGen, LineageComplete: true}
	if _, _, err := store.CommitToolResult(ctx, toolCommit("consumer", "e", OutcomeComplete, CoverageComplete, edge)); err != nil {
		t.Fatal(err)
	}
	addFence(t, store, "session", "source")
	addFence(t, store, "privacy", "project-1")
	if _, err := store.db.Exec(`INSERT INTO check_approvals(project_id, kind, check_set_digest, approval_epoch, approved_by, actor, state, created_at, updated_at) VALUES('p','check_set','d',1,'user','a','active','t','t')`); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "source"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes WHERE session_id = 'source'`); n != 0 {
		t.Errorf("source envelopes survived their session: %d", n)
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins WHERE session_id = 'consumer' AND source_ref = 'source#7'`); n != 1 {
		t.Errorf("deleting the source removed the consumer's edge (%d left)", n)
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes WHERE session_id = 'consumer'`); n != 1 {
		t.Errorf("deleting the source removed the consumer's envelope")
	}
	if n := count(t, store, `SELECT count(*) FROM source_fences`) + count(t, store, `SELECT count(*) FROM check_approvals`); n != 3 {
		t.Errorf("fences and approvals after deleting the source: %d rows, want 3", n)
	}

	if err := store.Delete(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes`) + count(t, store, `SELECT count(*) FROM derived_origins`); n != 0 {
		t.Errorf("consumer provenance survived its session: %d rows", n)
	}
	if n := count(t, store, `SELECT count(*) FROM source_fences`) + count(t, store, `SELECT count(*) FROM check_approvals`); n != 3 {
		t.Errorf("fences and approvals after deleting every session: %d rows, want 3", n)
	}
}

// Message text, tool arguments and reasoning-like fields never reach the new tables.
func TestProvenanceRowsHoldNoBodies(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	const sentinel = "BODY-SENTINEL-4f2a"
	admit(t, store, mainAdmission("s", "r"))
	if _, err := store.AppendAnswered(ctx, "s", "r", protocol.Message{Role: "assistant", Content: sentinel, ToolCalls: []protocol.ToolCall{{ID: "c", Name: "shell", Arguments: json.RawMessage(`{"command":"` + sentinel + `"}`)}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AdmitExecution(ctx, execAdmission("s", "e", "r")); err != nil {
		t.Fatal(err)
	}
	c := toolCommit("s", "e", OutcomeComplete, CoverageComplete, fileEdge("a.go"))
	c.Message.Content = sentinel
	if _, _, err := store.CommitToolResult(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, `SELECT count(*) FROM session_messages WHERE json LIKE '%`+sentinel+`%'`); n == 0 {
		t.Fatal("the transcript did not store the sentinel; the test proves nothing")
	}
	for _, table := range []string{"request_envelopes", "derived_origins", "source_fences", "check_approvals"} {
		cols := queryStrings(t, store, `SELECT name FROM pragma_table_info(?)`, table)
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = `coalesce(quote("` + c + `"), '')`
		}
		for _, row := range queryStrings(t, store, `SELECT `+strings.Join(parts, `||'|'||`)+` FROM "`+table+`"`) {
			for _, forbidden := range []string{sentinel, "reasoning", "thinking", "echo"} {
				if strings.Contains(strings.ToLower(row), strings.ToLower(forbidden)) {
					t.Errorf("%s row holds %q: %s", table, forbidden, row)
				}
			}
		}
	}
	for _, table := range []string{"request_envelopes", "derived_origins", "source_fences", "check_approvals"} {
		for _, col := range queryStrings(t, store, `SELECT name FROM pragma_table_info(?)`, table) {
			for _, bad := range []string{"content", "body", "text", "reasoning", "thinking", "echo", "arguments", "args_json", "message"} {
				if col == bad {
					t.Errorf("%s has a body-shaped column %q", table, col)
				}
			}
		}
	}
}

func queryStrings(t *testing.T, store *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := store.db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// Under SetMaxOpenConns(1) a write method that touched the Store (or s.db) from inside its transaction
// would wait for the connection it holds. Every write method runs here under a 5 s deadline while
// another goroutine reads continuously; a self-lock shows up as a deadline error.
func TestWriteMethodsDoNotSelfLockUnderConcurrentReads(t *testing.T) {
	store := provStore(t)
	for _, id := range []string{"s", "fenced"} {
		gapSession(t, store, id, "running", "")
	}
	addFence(t, store, "session", "fenced")

	stop := make(chan struct{})
	var reads atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := store.Get(context.Background(), "s"); err != nil {
					t.Errorf("concurrent read: %v", err)
					return
				}
				_, _ = store.Request(context.Background(), "r1")
				reads.Add(1)
			}
		}()
	}
	// Start the writes only once the readers are really running, so every write below contends with a read.
	for deadline := time.Now().Add(5 * time.Second); reads.Load() < 20 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	run := func(name string, f func(ctx context.Context) error, want error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := f(ctx)
		if ctx.Err() != nil {
			t.Errorf("%s did not finish within 5s (self-lock?): %v", name, err)
			return
		}
		if !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Errorf("%s: %v, want %v (after %v)", name, err, want, time.Since(start))
		}
	}
	run("AdmitRequest", func(ctx context.Context) error {
		_, err := store.AdmitRequest(ctx, mainAdmission("s", "r1"))
		return err
	}, nil)
	run("AdmitRequest fenced", func(ctx context.Context) error {
		_, err := store.AdmitRequest(ctx, mainAdmission("fenced", "r-fenced"))
		return err
	}, ErrFenced)
	run("AppendAnswered", func(ctx context.Context) error {
		_, err := store.AppendAnswered(ctx, "s", "r1", assistant("a"))
		return err
	}, nil)
	run("SettleRequest", func(ctx context.Context) error {
		if _, err := store.AdmitRequest(ctx, autoAdmission("s", "r2")); err != nil {
			return err
		}
		return store.SettleRequest(ctx, "r2", Settlement{To: RequestUnknown, Reason: "cancelled"})
	}, nil)
	run("AdmitExecution", func(ctx context.Context) error { return store.AdmitExecution(ctx, execAdmission("s", "e1", "r1")) }, nil)
	run("CommitToolResult", func(ctx context.Context) error {
		_, _, err := store.CommitToolResult(ctx, toolCommit("s", "e1", OutcomeComplete, CoverageComplete, fileEdge("a.go")))
		return err
	}, nil)
	run("Reconcile", func(ctx context.Context) error {
		if _, err := store.AdmitRequest(ctx, mainAdmission("s", "r3")); err != nil {
			return err
		}
		_, err := store.Reconcile(ctx, "s")
		return err
	}, nil)
	// ADR-0017 approval writes, and a commit whose file edge revokes them in its own transaction.
	store.WatchCheckDefinitions("/p", "lumencortex.json")
	run("ApproveCheckSet", func(ctx context.Context) error {
		_, err := store.ApproveCheckSet(ctx, CheckSetApproval{ProjectID: "/p", SetDigest: sha("1"), ApprovedBy: ApprovedByUser, Actor: "u",
			Specs: []ApprovedSpec{{CheckID: "go-test", SpecDigest: sha("2")}}})
		return err
	}, nil)
	run("ApproveConstraintBinding", func(ctx context.Context) error {
		_, err := store.ApproveConstraintBinding(ctx, BindingApproval{ProjectID: "/p", ConstraintRef: "c", ConstraintDigest: sha("3"),
			CheckID: "go-test", SpecDigest: sha("2"), BindingDigest: sha("4"), ApprovedBy: ApprovedByUser, Actor: "u"})
		return err
	}, nil)
	run("RevokeConstraintBinding", func(ctx context.Context) error {
		_, err := store.RevokeConstraintBinding(ctx, "/p", sha("4"))
		return err
	}, nil)
	run("CommitToolResult revoking", func(ctx context.Context) error {
		if err := store.AdmitExecution(ctx, execAdmission("s", "e2", "r1")); err != nil {
			return err
		}
		_, _, err := store.CommitToolResult(ctx, toolCommit("s", "e2", OutcomeComplete, CoverageComplete, fileEdge("lumencortex.json")))
		return err
	}, nil)
	run("RevokeCheckApprovals", func(ctx context.Context) error {
		_, err := store.RevokeCheckApprovals(ctx, "/p")
		return err
	}, nil)
	close(stop)
	wg.Wait()
	if reads.Load() == 0 {
		t.Error("the concurrent readers never ran")
	}
}

// Many writers on distinct and shared sessions: every request ends in exactly one terminal state and
// no write is lost or duplicated.
func TestConcurrentAdmissionAndAnswerKeepOneStatePerRequest(t *testing.T) {
	store := provStore(t)
	const sessions, perSession = 4, 10
	for i := 0; i < sessions; i++ {
		gapSession(t, store, fmt.Sprintf("s%d", i), "running", "")
	}
	var wg sync.WaitGroup
	errs := make(chan error, sessions*perSession)
	for i := 0; i < sessions; i++ {
		for j := 0; j < perSession; j++ {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				sid, rid := fmt.Sprintf("s%d", i), fmt.Sprintf("r-%d-%d", i, j)
				if _, err := store.AdmitRequest(ctx, mainAdmission(sid, rid)); err != nil {
					errs <- err
					return
				}
				if j%2 == 0 {
					_, err := store.AppendAnswered(ctx, sid, rid, assistant("a"))
					errs <- err
					return
				}
				errs <- store.SettleRequest(ctx, rid, Settlement{To: RequestUnknown, Reason: "cancelled"})
			}(i, j)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes WHERE state = 'answered'`); n != sessions*perSession/2 {
		t.Errorf("%d answered, want %d", n, sessions*perSession/2)
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes WHERE state = 'unknown'`); n != sessions*perSession/2 {
		t.Errorf("%d unknown, want %d", n, sessions*perSession/2)
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes WHERE state = 'answered' AND answered_seq IS NULL`); n != 0 {
		t.Errorf("%d answered envelopes without answered_seq", n)
	}
	if n := count(t, store, `SELECT count(*) FROM session_messages m WHERE role = 'assistant' AND NOT EXISTS (SELECT 1 FROM request_envelopes e WHERE e.session_id = m.session_id AND e.answered_seq = m.seq)`); n != 0 {
		t.Errorf("%d assistant rows are not bound to an answered envelope", n)
	}
}

// A database written by an older binary has sessions and messages but no provenance: reading it gives
// no envelope and no edge, and the Store never invents one.
func TestLegacyRowsHaveNoProvenance(t *testing.T) {
	store := provStore(t)
	gapSession(t, store, "old", "idle", "")
	gapRow(t, store, "old", "user", protocol.Message{Content: "legacy question"})
	gapRow(t, store, "old", "assistant", protocol.Message{Content: "legacy answer"})
	if _, err := store.Reconcile(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"request_envelopes", "derived_origins", "source_fences", "check_approvals"} {
		if n := count(t, store, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s holds %d rows for a legacy session", table, n)
		}
	}
	if n := count(t, store, `SELECT count(*) FROM eval_results WHERE episode_id IS NOT NULL`); n != 0 {
		t.Errorf("%d legacy eval_results rows have an episode", n)
	}
}
