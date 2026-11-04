package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Plan T2-S1: the read-only source status of a derived target, and the recall candidates older than the
// newest MaxCandidates sessions.

// agentNoteFrom commits a model note of request r1 and returns its ref.
func agentNoteFrom(t *testing.T, store *Store, sessionID, requestID, ref string) {
	t.Helper()
	if _, _, err := store.CommitToolResult(context.Background(), ToolCommit{
		SessionID: sessionID, Message: protoTool("c-" + requestID),
		TaskState: &TaskStateWrite{Base: 0, Reason: "tool:note_write", JSON: json.RawMessage(`{"windowStartSeq":0}`)},
		Edges:     []DerivedEdge{{Kind: "message", TargetRef: ref, SourceKind: SourceRequest, SourceRef: requestID, OriginClass: OriginAgentGen, LineageComplete: true}},
	}); err != nil {
		t.Fatal(err)
	}
}

func noteRef(i int) string { return fmt.Sprintf("ts:note:n1@sha256:%064x", i) }

// rewriteChain stores an agent note from r1 (version 0) and n user rewrites, each from the version
// before, and returns the refs, oldest first.
func rewriteChain(t *testing.T, store *Store, n int) []string {
	t.Helper()
	reach(t, store, "s", "r1", PurposeMain, RequestAnswered)
	refs := []string{noteRef(0)}
	agentNoteFrom(t, store, "s", "r1", refs[0])
	version := int64(1)
	for i := 1; i <= n; i++ {
		v, err := store.PutTaskState(context.Background(), "s", stateWrite(version, rewriteEdge(noteRef(i), refs[i-1])))
		if err != nil {
			t.Fatalf("rewrite %d: %v", i, err)
		}
		version = v
		refs = append(refs, noteRef(i))
	}
	return refs
}

func sourceStatus(t *testing.T, store *Store, ref string) SourceStatus {
	t.Helper()
	st, err := store.SourceStatus(context.Background(), "s", ref)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSourceStatusResolvesFortyRewritesToTheRequest(t *testing.T) {
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	refs := rewriteChain(t, store, 40)

	want := SourceStatus{Recorded: true, Origin: OriginUserDecision, RootKind: SourceRequest, RootRef: "r1", Hops: 41, LineageComplete: true}
	if got := sourceStatus(t, store, refs[40]); got != want {
		t.Fatalf("newest of 40 rewrites = %+v, want %+v", got, want)
	}
	if got := sourceStatus(t, store, refs[0]); got.Origin != OriginAgentGen || got.Hops != 1 || got.RootRef != "r1" || !got.LineageComplete {
		t.Fatalf("the agent's own version = %+v", got)
	}
}

// A rewrite back to text an earlier version had repeats that version's ref; the walk still ends at the
// request instead of cycling until the hop bound.
func TestSourceStatusFollowsARewriteBackToEarlierText(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	refs := rewriteChain(t, store, 1)
	if _, err := store.PutTaskState(ctx, "s", stateWrite(2, rewriteEdge(refs[0], refs[1]))); err != nil {
		t.Fatal(err)
	}
	got := sourceStatus(t, store, refs[0])
	if got.RootKind != SourceRequest || got.RootRef != "r1" || got.Hops != 3 || !got.LineageComplete {
		t.Fatalf("A -> B -> A = %+v, want three hops to r1", got)
	}
}

func TestSourceStatusIsWithdrawnByAFenceOnTheSessionOrAnySourceOnTheChain(t *testing.T) {
	for name, fence := range map[string][2]string{
		"session":                 {"session", "s"},
		"the root request":        {"source", SourceScopeRef(SourceRequest, "r1")},
		"an intermediate version": {"source", SourceScopeRef(SourceMessage, noteRef(1))},
	} {
		t.Run(name, func(t *testing.T) {
			store := provStore(t)
			gapSession(t, store, "s", "idle", "")
			refs := rewriteChain(t, store, 3)
			if got := sourceStatus(t, store, refs[3]); got.Withdrawn {
				t.Fatalf("withdrawn before any fence: %+v", got)
			}
			addFence(t, store, fence[0], fence[1])
			if got := sourceStatus(t, store, refs[3]); !got.Withdrawn {
				t.Fatalf("a fence on %s left the newest version current: %+v", name, got)
			}
		})
	}
	// A fence on a source off the chain does not withdraw it.
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	gapSession(t, store, "other", "idle", "")
	refs := rewriteChain(t, store, 2)
	addFence(t, store, "source", SourceScopeRef(SourceRequest, "r9"))
	addFence(t, store, "session", "other")
	if got := sourceStatus(t, store, refs[2]); got.Withdrawn {
		t.Fatalf("an unrelated fence withdrew the chain: %+v", got)
	}
}

func TestSourceStatusOfUnrecordedAndBrokenChainsIsIncomplete(t *testing.T) {
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	if got := sourceStatus(t, store, "ts:note:legacy@"+sha("1")); got.Recorded || got.LineageComplete || got.Hops != 0 {
		t.Fatalf("unrecorded target = %+v", got)
	}
	// A rewrite of an entry with no edge: one hop, then nothing to follow.
	if _, err := store.PutTaskState(context.Background(), "s", stateWrite(0, rewriteEdge(noteRef(1), "ts:note:legacy@"+sha("1")))); err != nil {
		t.Fatal(err)
	}
	if got := sourceStatus(t, store, noteRef(1)); !got.Recorded || got.LineageComplete || got.RootKind != "" {
		t.Fatalf("rewrite of an unrecorded entry = %+v", got)
	}
	if _, err := store.SourceStatus(context.Background(), "nope", noteRef(1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	if _, err := store.SourceStatus(context.Background(), "s", ""); err == nil {
		t.Fatal("an empty target was accepted")
	}
}

func TestSourceStatusStopsAtTheHopBound(t *testing.T) {
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	refs := rewriteChain(t, store, MaxLineageHops+2)
	got := sourceStatus(t, store, refs[len(refs)-1])
	if got.Hops != MaxLineageHops || got.LineageComplete || got.RootKind != "" {
		t.Fatalf("a chain past the bound = %+v, want %d hops and incomplete", got, MaxLineageHops)
	}
}

// contextSession creates a session updated at base+i minutes with the given goal.
func contextSession(t *testing.T, store *Store, id, goal string, at time.Time) {
	t.Helper()
	if err := store.Create(context.Background(), Session{ID: id, Goal: goal, Status: "completed", CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
}

func TestContextRowsMatchingReachesPastTheNewest(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Oldest first: the parent and its child task are older than 600 later sessions.
	contextSession(t, store, "parent", "migrate the billing ledger", base)
	contextSession(t, store, "child", "Rewrite LedgerCompactor for the billing migration", base.Add(time.Minute))
	if err := store.AddRelation(ctx, Relation{ParentSessionID: "parent", ChildSessionID: "child", Kind: "task"}); err != nil {
		t.Fatal(err)
	}
	contextSession(t, store, "old-other", "tune the cache", base.Add(2*time.Minute))
	for i := 0; i < 600; i++ {
		contextSession(t, store, fmt.Sprintf("new%03d", i), "unrelated work", base.Add(time.Duration(10+i)*time.Minute))
	}
	// One session in the newest 500 that also matches: ContextRows covers it, the older query must not.
	contextSession(t, store, "recent-match", "ledger audit", base.Add(1000*time.Minute))
	addFence(t, store, "session", "old-other")

	newest, err := store.ContextRows(ctx, 500, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range newest {
		if r.ID == "child" || r.ID == "parent" {
			t.Fatalf("%s is among the newest 500", r.ID)
		}
	}
	rows, err := store.ContextRowsMatching(ctx, []string{"ledgercompactor", "ledger"}, 500, 500, 64)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if fmt.Sprint(ids) != "[child parent]" {
		t.Fatalf("older matches = %v, want the child (newer) then its parent", ids)
	}
	if rows[0].ParentID != "parent" || rows[0].Kind != "task" {
		t.Fatalf("child row = %+v, want its parent relation", rows[0])
	}
	// A fenced session is still a row, flagged, so a reader can drop it.
	fenced, err := store.ContextRowsMatching(ctx, []string{"cache"}, 500, 500, 64)
	if err != nil || len(fenced) != 1 || !fenced[0].Fenced {
		t.Fatalf("fenced match = %+v err=%v", fenced, err)
	}
	if none, err := store.ContextRowsMatching(ctx, nil, 500, 500, 64); err != nil || len(none) != 0 {
		t.Fatalf("no terms = %v, %v", none, err)
	}
	if lim, err := store.ContextRowsMatching(ctx, []string{"ledger"}, 500, 1, 64); err != nil || len(lim) != 1 || lim[0].ID != "child" {
		t.Fatalf("limit 1 = %+v, %v", lim, err)
	}
}

// Sessions updated at the same instant split the same way in both queries: every session is either
// among the newest or a candidate of the older query, never both and never neither.
func TestContextRowsSplitTiesConsistently(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		contextSession(t, store, fmt.Sprintf("t%02d", i), "same ledger", at)
	}
	newest, err := store.ContextRows(ctx, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	older, err := store.ContextRowsMatching(ctx, []string{"ledger"}, 10, 100, 64)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, r := range append(newest, older...) {
		seen[r.ID]++
	}
	if len(seen) != 30 || len(newest)+len(older) != 30 {
		t.Fatalf("newest %d + older %d over 30 tied sessions; distinct %d", len(newest), len(older), len(seen))
	}
}
