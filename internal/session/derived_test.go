package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// These tests cover the derived edges a Task State version carries (plan T1-S5, CR-1 realized as
// TaskStateWrite.Edges): they commit with the version or not at all, inherit their source's lineage, and
// stay resolvable through long rewrite chains.

func rewriteEdge(target, from string) DerivedEdge {
	return DerivedEdge{Kind: "message", TargetRef: target, SourceKind: SourceMessage, SourceRef: from, OriginClass: OriginUserDecision, DerivedFrom: from, LineageComplete: true}
}

func stateWrite(base int64, edges ...DerivedEdge) TaskStateWrite {
	return TaskStateWrite{Base: base, Reason: "user", JSON: json.RawMessage(fmt.Sprintf(`{"windowStartSeq":%d}`, base)), Edges: edges}
}

// edgeOf returns the stored edge targeting ref in the session: its source, derived-from and lineage.
func edgeOf(t *testing.T, store *Store, session, ref string) (sourceKind, sourceRef, derivedFrom string, complete bool) {
	t.Helper()
	var df *string
	var lc int
	if err := store.db.QueryRow(`SELECT source_kind, source_ref, derived_from, lineage_complete FROM derived_origins
		WHERE session_id = ? AND kind = 'message' AND target_ref = ?`, session, ref).Scan(&sourceKind, &sourceRef, &df, &lc); err != nil {
		t.Fatalf("edge to %s: %v", ref, err)
	}
	if df != nil {
		derivedFrom = *df
	}
	return sourceKind, sourceRef, derivedFrom, lc == 1
}

// A model note derives from its request; 40 user rewrites in a row each derive from the version before.
// Every rewrite inherits the complete lineage, and the chain from the newest version back to the
// request resolves hop by hop.
func TestRewriteChainInheritsLineageAndResolves(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	reach(t, store, "s", "r1", PurposeMain, RequestAnswered)
	agentNote := "ts:note:n1@" + sha("0")
	if _, _, err := store.CommitToolResult(ctx, ToolCommit{
		SessionID: "s", Message: protoTool("c1"),
		TaskState: &TaskStateWrite{Base: 0, Reason: "tool:note_write", JSON: json.RawMessage(`{"windowStartSeq":0}`)},
		Edges:     []DerivedEdge{{Kind: "message", TargetRef: agentNote, SourceKind: SourceRequest, SourceRef: "r1", OriginClass: OriginAgentGen, LineageComplete: true}},
	}); err != nil {
		t.Fatal(err)
	}
	prev, version := agentNote, int64(1)
	refs := []string{agentNote}
	for i := 1; i <= 40; i++ {
		next := fmt.Sprintf("ts:note:n1@sha256:%064x", i)
		v, err := store.PutTaskState(ctx, "s", stateWrite(version, rewriteEdge(next, prev)))
		if err != nil {
			t.Fatalf("rewrite %d: %v", i, err)
		}
		prev, version = next, v
		refs = append(refs, next)
	}
	// Resolve from the newest version back to the request, one stored edge per hop.
	ref, hops := prev, 0
	for {
		kind, src, from, complete := edgeOf(t, store, "s", ref)
		if !complete {
			t.Fatalf("hop %d (%s) is lineage-incomplete", hops, ref)
		}
		hops++
		if kind == SourceRequest {
			if src != "r1" || ref != agentNote {
				t.Fatalf("chain ends at %s -> %s/%s, want the agent note from r1", ref, kind, src)
			}
			break
		}
		if kind != SourceMessage || from != src || src != refs[len(refs)-1-hops] {
			t.Fatalf("hop %d: %s derives from %s/%s (derived_from %s)", hops, ref, kind, src, from)
		}
		ref = src
		if hops > 64 {
			t.Fatal("chain does not end")
		}
	}
	if hops != 41 {
		t.Fatalf("%d hops, want 41 (40 rewrites and the agent note)", hops)
	}
	if latest, _ := store.LatestTaskState(ctx, "s"); latest.Version != 41 {
		t.Fatalf("Task State version %d, want 41", latest.Version)
	}
}

// An entry with no recorded edge (written before provenance existed) is an incomplete source: its
// rewrite is stored incomplete, and so is every later rewrite, whatever the caller claims.
func TestRewriteOfAnUnrecordedEntryIsIncompleteForever(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	legacy := "ts:note:n7@" + sha("7")
	first, second := "ts:note:n7@"+sha("8"), "ts:note:n7@"+sha("9")
	if _, err := store.PutTaskState(ctx, "s", stateWrite(0, rewriteEdge(first, legacy))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutTaskState(ctx, "s", stateWrite(1, rewriteEdge(second, first))); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{first, second} {
		if _, _, _, complete := edgeOf(t, store, "s", ref); complete {
			t.Errorf("%s is complete on top of an unrecorded entry", ref)
		}
	}
	// An edge recorded in another session does not count: entries are per session.
	gapSession(t, store, "other", "idle", "")
	if _, err := store.PutTaskState(ctx, "other", stateWrite(0, rewriteEdge("ts:note:n7@"+sha("a"), second))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, complete := edgeOf(t, store, "other", "ts:note:n7@"+sha("a")); complete {
		t.Error("an entry of another session served as a complete source")
	}
}

// A child's result derives from the child as of a seq: complete only when the child's newest answered
// main request at or before that seq is complete. A child without one, or a seq before its answer, is
// incomplete.
func TestChildSourceInheritsTheChildsAnsweredRequest(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "parent", "idle", "")
	gapSession(t, store, "child", "idle", "")
	gapSession(t, store, "bare", "idle", "")
	reach(t, store, "child", "cr1", PurposeMain, RequestAnswered)
	answered, _ := store.Request(ctx, "cr1")
	at := *answered.AnsweredSeq
	edge := func(target, child string, seq int64) DerivedEdge {
		return DerivedEdge{Kind: "message", TargetRef: target, SourceKind: SourceSession, SourceRef: child,
			OriginClass: OriginAgentGen, DerivedFrom: fmt.Sprintf("session:%s@%d", child, seq), LineageComplete: true}
	}
	if _, err := store.PutTaskState(ctx, "parent", stateWrite(0,
		edge("task_result:child@now", "child", at),
		edge("task_result:child@before", "child", at-1),
		edge("task_result:bare@0", "bare", 0),
		DerivedEdge{Kind: "message", TargetRef: "task_result:child@noseq", SourceKind: SourceSession, SourceRef: "child", OriginClass: OriginAgentGen, DerivedFrom: "session:child", LineageComplete: true},
	)); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"task_result:child@now": true, "task_result:child@before": false, "task_result:bare@0": false, "task_result:child@noseq": false}
	for ref, w := range want {
		if _, _, _, complete := edgeOf(t, store, "parent", ref); complete != w {
			t.Errorf("%s complete = %v, want %v", ref, complete, w)
		}
	}
	// An incomplete child request makes the result incomplete.
	a := mainAdmission("child", "cr2")
	a.LineageComplete = false
	admit(t, store, a)
	seq, err := store.AppendAnswered(ctx, "child", "cr2", assistant("later"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutTaskState(ctx, "parent", stateWrite(1, edge("task_result:child@later", "child", seq))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, complete := edgeOf(t, store, "parent", "task_result:child@later"); complete {
		t.Error("a result of an incomplete child request is complete")
	}
}

// Edges commit with the version or not at all: an invalid edge writes nothing, a fenced edge source or
// session refuses the whole version with ErrFenced, a failing insert rolls the version back, and a
// version with edges is still one write transaction.
func TestTaskStateEdgesCommitWithTheVersionOrNotAtAll(t *testing.T) {
	ctx := context.Background()
	base := provStore(t)
	gapSession(t, base, "s", "idle", "")
	gapSession(t, base, "fenced", "idle", "")
	addFence(t, base, "session", "fenced")
	store := countingStore(t, base)
	src := "ts:note:n1@" + sha("1")

	bad := rewriteEdge("ts:note:n1@"+sha("2"), src)
	bad.OriginClass = OriginToolExecution
	if _, err := store.PutTaskState(ctx, "s", stateWrite(0, bad)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid edge: %v", err)
	}
	if commits, _ := writes(func() {
		if _, err := store.PutTaskState(ctx, "s", stateWrite(0, rewriteEdge("ts:note:n1@"+sha("2"), src))); err != nil {
			t.Fatal(err)
		}
	}); commits != 1 {
		t.Errorf("a version with an edge took %d write transactions", commits)
	}

	addFence(t, base, "source", SourceScopeRef(SourceMessage, src))
	if _, err := store.PutTaskState(ctx, "s", stateWrite(1, rewriteEdge("ts:note:n1@"+sha("3"), src))); !errors.Is(err, ErrFenced) {
		t.Fatalf("fenced source: %v", err)
	}
	if _, err := store.PutTaskState(ctx, "fenced", stateWrite(0, rewriteEdge("ts:note:n1@"+sha("4"), "ts:note:n1@"+sha("5")))); !errors.Is(err, ErrFenced) {
		t.Fatalf("fenced session: %v", err)
	}
	if _, err := base.db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON derived_origins WHEN NEW.target_ref LIKE '%boom%' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutTaskState(ctx, "s", stateWrite(1, rewriteEdge("ts:note:boom@"+sha("6"), "ts:note:x@"+sha("6")))); err == nil {
		t.Fatal("PutTaskState ignored the failing edge insert")
	}
	if latest, _ := store.LatestTaskState(ctx, "s"); latest.Version != 1 {
		t.Errorf("Task State version %d, want 1: a refused write advanced it", latest.Version)
	}
	if n := count(t, base, `SELECT count(*) FROM derived_origins`); n != 1 {
		t.Errorf("%d edges stored, want only the committed one", n)
	}
	if latest, _ := store.LatestTaskState(ctx, "fenced"); latest.Version != 0 {
		t.Errorf("a fenced session got Task State version %d", latest.Version)
	}

	// BeginRun stores the Start version's edges in the Start transaction; a refused one leaves the
	// session idle with no row.
	if _, _, err := store.BeginRun(ctx, "s", RunStart{TaskState: &TaskStateWrite{Base: 1, Reason: "start", JSON: json.RawMessage(`{}`),
		Edges: []DerivedEdge{rewriteEdge("ts:note:n1@"+sha("7"), src)}}}); !errors.Is(err, ErrFenced) {
		t.Fatalf("BeginRun with a fenced edge source: %v", err)
	}
	if got, _ := store.Get(ctx, "s"); got.Status != "idle" || count(t, base, `SELECT count(*) FROM session_messages WHERE session_id = 's'`) != 0 {
		t.Fatalf("a refused Start wrote: status %s", got.Status)
	}
	if _, _, err := store.BeginRun(ctx, "s", RunStart{TaskState: &TaskStateWrite{Base: 1, Reason: "start", JSON: json.RawMessage(`{}`),
		Edges: []DerivedEdge{rewriteEdge("ts:note:n2@"+sha("8"), "ts:note:n2@"+sha("9"))}}}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, base, `SELECT count(*) FROM derived_origins WHERE target_ref = ?`, "ts:note:n2@"+sha("8")); n != 1 {
		t.Errorf("BeginRun stored %d edges, want 1", n)
	}
}

// A tool result's own edges and its Task State version's edges are one list: fence-checked together and
// cut together at MaxEdgesPerCommit, after which everything stored is incomplete.
func TestToolCommitCutsItsEdgesAndTheVersionsTogether(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	reach(t, store, "s", "r1", PurposeMain, RequestAnswered)
	var own, state []DerivedEdge
	for i := 0; i < 40; i++ {
		own = append(own, DerivedEdge{Kind: "message", TargetRef: fmt.Sprintf("ts:unit:u%d@%s", i, sha("1")), SourceKind: SourceRequest, SourceRef: "r1", OriginClass: OriginAgentGen, LineageComplete: true})
		state = append(state, DerivedEdge{Kind: "message", TargetRef: fmt.Sprintf("ts:note:n%d@%s", i, sha("2")), SourceKind: SourceRequest, SourceRef: "r1", OriginClass: OriginAgentGen, LineageComplete: true})
	}
	c := ToolCommit{SessionID: "s", Message: protoTool("c1"), Edges: own,
		TaskState: &TaskStateWrite{Base: 0, Reason: "tool:x", JSON: json.RawMessage(`{}`), Edges: state}}
	if _, _, err := store.CommitToolResult(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins`); n != MaxEdgesPerCommit {
		t.Errorf("%d edges stored, want %d", n, MaxEdgesPerCommit)
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins WHERE lineage_complete = 1`); n != 0 {
		t.Errorf("%d edges complete after a cut", n)
	}

	// A fenced source among the version's edges fences the whole tool result.
	addFence(t, store, "source", SourceScopeRef(SourceMessage, "ts:note:gone@"+sha("3")))
	rows := count(t, store, `SELECT count(*) FROM session_messages`)
	c2 := ToolCommit{SessionID: "s", Message: protoTool("c2"),
		TaskState: &TaskStateWrite{Base: 1, Reason: "tool:x", JSON: json.RawMessage(`{"a":1}`), Edges: []DerivedEdge{rewriteEdge("ts:note:n9@"+sha("4"), "ts:note:gone@"+sha("3"))}}}
	if _, _, err := store.CommitToolResult(ctx, c2); !errors.Is(err, ErrFenced) {
		t.Fatalf("fenced Task State edge source: %v", err)
	}
	if got := count(t, store, `SELECT count(*) FROM session_messages`); got != rows {
		t.Error("a fenced tool result stored its row")
	}
}

func protoTool(callID string) protocol.Message {
	return protocol.Message{Role: "tool", ToolCallID: callID, Content: `{"ok":true}`}
}
