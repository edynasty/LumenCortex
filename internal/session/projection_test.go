package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// userRow appends a user row to session and returns its seq.
func userRow(t *testing.T, store *Store, session string, m protocol.Message) int64 {
	t.Helper()
	m.Role = "user"
	seq, err := store.AppendMessage(context.Background(), session, "user", m)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// newest is the session's newest transcript seq.
func newest(t *testing.T, store *Store, session string) int64 {
	t.Helper()
	seq, err := store.NewestMessageSeq(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// answer admits a main request whose window is [first, newest] of the session and stores its answer;
// it returns the admission error, if any, and the envelope's fence epoch.
func answer(t *testing.T, store *Store, session, id string, first int64, m protocol.Message) (int64, error) {
	t.Helper()
	ctx := context.Background()
	a := mainAdmission(session, id)
	a.FirstSeq, a.LastSeq = i64(first), i64(newest(t, store, session))
	env, err := store.AdmitRequest(ctx, a)
	if err != nil {
		return 0, err
	}
	if _, err := store.AppendAnswered(ctx, session, id, m); err != nil {
		return 0, err
	}
	return env.FenceEpoch, nil
}

// sourceThread gives a source session one answered main request: its lineage is recorded, so an edge
// citing it is stored complete (inheritedLineage).
func sourceThread(t *testing.T, store *Store, id string) {
	t.Helper()
	userRow(t, store, id, protocol.Message{Content: "note for " + id})
	if _, err := answer(t, store, id, id+"-a", 0, assistant("SECRET of "+id)); err != nil {
		t.Fatal(err)
	}
}

// readSource stores in session one answered main request calling a thread tool, and the tool row that
// answers it, citing source at its newest seq as read_thread does. It returns the tool row's seq.
func readSource(t *testing.T, store *Store, session, request, call, source string) int64 {
	t.Helper()
	call1 := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: call, Name: "read_thread", Arguments: json.RawMessage(`{}`)}}}
	if _, err := answer(t, store, session, request, 0, call1); err != nil {
		t.Fatal(err)
	}
	seq, _, err := store.CommitToolResult(context.Background(), ToolCommit{
		SessionID: session, RunID: "run1",
		Message: protocol.Message{Role: "tool", ToolCallID: call, Name: "read_thread", Content: "SECRET of " + source},
		Edges: []DerivedEdge{{Kind: "message", TargetRef: "tool:" + request + ":" + call, SourceKind: SourceSession, SourceRef: source,
			OriginClass: OriginAgentGen, DerivedFrom: "session:" + source + "@" + strconv.FormatInt(newest(t, store, source), 10), LineageComplete: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func projection(t *testing.T, store *Store, session string) Projection {
	t.Helper()
	p, err := store.Projection(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Plan T3-S2 at the Store. A thread that read a revoked source and stored no answer after the read can
// be projected: the read's row is withheld, and once a Start acknowledges the projection the session's
// own work is admitted again at the new epoch - but it stays withdrawn as a source. A projection read
// before a later revocation cannot be acknowledged; the later revocation refuses the session again
// until a Start projects it, and the earlier withheld row stays withheld.
func TestProjectionWithholdsAnUnansweredReadAndIsAcknowledged(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	for _, id := range []string{"x", "y", "s", "u"} {
		gapSession(t, store, id, "idle", "")
	}
	sourceThread(t, store, "x")
	sourceThread(t, store, "y")
	if p := projection(t, store, "s"); !reflect.DeepEqual(p, Projection{}) {
		t.Fatalf("projection of an unfenced session: %+v", p)
	}
	userRow(t, store, "s", protocol.Message{Content: "read x"})
	row := readSource(t, store, "s", "r1", "c1", "x")
	rev, err := store.RevokeSession(ctx, "x", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	p := projection(t, store, "s")
	if want := (Projection{Epoch: rev.Epoch, Open: true, Withheld: []int64{row}}); !reflect.DeepEqual(p, want) {
		t.Fatalf("projection %+v, want %+v", p, want)
	}
	if _, err := store.AdmitRequest(ctx, mainAdmission("s", "before-ack")); !errors.Is(err, ErrFenced) {
		t.Fatalf("admission before the projection is acknowledged: %v", err)
	}
	if err := store.AcknowledgeProjection(ctx, "s", rev.Epoch+1); !errors.Is(err, ErrFenced) {
		t.Fatalf("acknowledging an epoch that is not the newest: %v", err)
	}
	if err := store.AcknowledgeProjection(ctx, "s", rev.Epoch); err != nil {
		t.Fatal(err)
	}
	userRow(t, store, "s", protocol.Message{Content: "continue"})
	epoch, err := answer(t, store, "s", "r2", 0, assistant("went on"))
	if err != nil || epoch != rev.Epoch {
		t.Fatalf("request after the projection: epoch %d, %v; want admitted at %d", epoch, err, rev.Epoch)
	}
	// The answer above carried the withheld row only withheld: it does not expose it.
	if p := projection(t, store, "s"); p.Open || p.Isolated != "" || !reflect.DeepEqual(p.Withheld, []int64{row}) {
		t.Fatalf("projection after a projected run: %+v", p)
	}
	// The session is still withdrawn as a source: nothing may cite it.
	edge := DerivedEdge{Kind: "message", TargetRef: "tool:u1:c1", SourceKind: SourceSession, SourceRef: "s", OriginClass: OriginAgentGen, LineageComplete: true}
	if _, _, err := store.CommitToolResult(ctx, ToolCommit{SessionID: "u", RunID: "run1",
		Message: protocol.Message{Role: "tool", ToolCallID: "c1", Name: "read_thread", Content: "s"}, Edges: []DerivedEdge{edge}}); !errors.Is(err, ErrFenced) {
		t.Fatalf("citing a projected session: %v, want ErrFenced", err)
	}

	// s reads y; y is revoked after a projection of s was read: that projection cannot be acknowledged.
	row2 := readSource(t, store, "s", "r3", "c3", "y")
	stale := projection(t, store, "s")
	rev2, err := store.RevokeSession(ctx, "y", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AcknowledgeProjection(ctx, "s", stale.Epoch); !errors.Is(err, ErrFenced) {
		t.Fatalf("acknowledging a projection read before a revocation: %v", err)
	}
	if _, err := store.AdmitRequest(ctx, mainAdmission("s", "after-second")); !errors.Is(err, ErrFenced) {
		t.Fatalf("admission after a second revocation: %v", err)
	}
	p = projection(t, store, "s")
	if want := (Projection{Epoch: rev2.Epoch, Open: true, Withheld: []int64{row, row2}}); !reflect.DeepEqual(p, want) {
		t.Fatalf("projection after the second revocation %+v, want %+v", p, want)
	}
	if err := store.AcknowledgeProjection(ctx, "s", rev2.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := answer(t, store, "s", "r4", 0, assistant("again")); err != nil {
		t.Fatalf("request after the second projection: %v", err)
	}
	// The revocation's report is intact: acknowledging only marks the reached sessions' rows.
	if back, err := store.Revocation(ctx, rev.Epoch); err != nil || back.Source != "x" {
		t.Fatalf("report after the acknowledgement: %+v %v", back, err)
	}
	// A downgrade still sees the fences active.
	if n := count(t, store, `SELECT count(*) FROM source_fences WHERE scope_ref = 's' AND state = 'active'`); n != 2 {
		t.Fatalf("active fences on s: %d, want 2", n)
	}
}

// Two hops: a read x and answered; m read a and answered nothing since. Revoking x reaches a through
// its edge and m through a's. a is exposed and stays isolated; m is projected with its read of a
// withheld - a's answer restates x, and that row is the only place m holds it.
func TestProjectionWithholdsAReadOfAReachedSession(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	for _, id := range []string{"x", "a", "m"} {
		gapSession(t, store, id, "idle", "")
	}
	sourceThread(t, store, "x")
	userRow(t, store, "a", protocol.Message{Content: "read x"})
	readSource(t, store, "a", "a1", "c1", "x")
	if _, err := answer(t, store, "a", "a2", 0, assistant("x says SECRET")); err != nil {
		t.Fatal(err)
	}
	userRow(t, store, "m", protocol.Message{Content: "read a"})
	row := readSource(t, store, "m", "m1", "c1", "a")

	rev, err := store.RevokeSession(ctx, "x", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	if p := projection(t, store, "a"); p.Isolated != IsolatedExposed {
		t.Fatalf("a: %+v, want isolated %s", p, IsolatedExposed)
	}
	if p, want := projection(t, store, "m"), (Projection{Epoch: rev.Epoch, Open: true, Withheld: []int64{row}}); !reflect.DeepEqual(p, want) {
		t.Fatalf("m: %+v, want %+v", p, want)
	}
}

// The reasons a thread is never projected: the session is itself revoked; an answer was stored from a
// request that carried the source's row before it was fenced (the answer, and what follows, may restate
// it); a fenced session started it; it holds model output without a request record; an edge list was
// cut; an edge citing the source names no row.
func TestProjectionIsolatesWhatItCannotWithhold(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	for _, id := range []string{"x", "exposed", "child", "legacy", "cut", "unmapped", "fresh"} {
		gapSession(t, store, id, "idle", "")
	}
	sourceThread(t, store, "x")
	userRow(t, store, "exposed", protocol.Message{Content: "go"})
	readSource(t, store, "exposed", "e1", "c1", "x")
	if _, err := answer(t, store, "exposed", "e2", 0, assistant("x says SECRET")); err != nil {
		t.Fatal(err)
	}
	if err := store.AddRelation(ctx, Relation{ParentSessionID: "x", ChildSessionID: "child", Kind: "subagent"}); err != nil {
		t.Fatal(err)
	}
	userRow(t, store, "legacy", protocol.Message{Content: "go"})
	if _, err := store.AppendMessage(ctx, "legacy", "assistant", assistant("an answer older than request records")); err != nil {
		t.Fatal(err)
	}
	readSource(t, store, "legacy", "l1", "c1", "x")
	userRow(t, store, "cut", protocol.Message{Content: "go"})
	readSource(t, store, "cut", "k1", "c1", "x")
	if _, err := store.db.Exec(`UPDATE derived_origins SET lineage_complete = 0 WHERE session_id = 'cut'`); err != nil {
		t.Fatal(err)
	}
	insertEdge(t, store, "unmapped", "message", "ts:note:n1@"+sha("e"), SourceSession, "x")
	userRow(t, store, "fresh", protocol.Message{Content: "go"})
	freshRow := readSource(t, store, "fresh", "f1", "c1", "x")

	rev, err := store.RevokeSession(ctx, "x", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"x": IsolatedRevoked, "exposed": IsolatedExposed, "child": IsolatedParent,
		"legacy": IsolatedLineage, "cut": IsolatedLineage, "unmapped": IsolatedUnmapped,
	} {
		p := projection(t, store, id)
		if !p.Open || p.Epoch != rev.Epoch || p.Isolated != want || p.Withheld != nil {
			t.Errorf("%s: projection %+v, want open at %d, isolated %s", id, p, rev.Epoch, want)
		}
	}
	// The source's own fence is never projected, even when asked to be.
	if err := store.AcknowledgeProjection(ctx, "x", rev.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitRequest(ctx, mainAdmission("x", "x-after")); !errors.Is(err, ErrFenced) {
		t.Fatalf("admission in the revoked source after an acknowledgement: %v", err)
	}
	// The one thread that read x and answered nothing since is projected.
	if p := projection(t, store, "fresh"); p.Isolated != "" || !reflect.DeepEqual(p.Withheld, []int64{freshRow}) {
		t.Fatalf("fresh: %+v", p)
	}
	// A fence written by anything but a revocation's walk means the session itself is withdrawn.
	addFence(t, store, "session", "fresh")
	if p := projection(t, store, "fresh"); p.Isolated != IsolatedRevoked {
		t.Fatalf("fresh with a hand-written fence: %+v", p)
	}
}

// A task result delivered from a child that is revoked later is withheld in the parent, every delivery
// of that child; other rows are not.
func TestProjectionWithholdsTaskResultsOfARevokedChild(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	for _, id := range []string{"p", "c", "d"} {
		gapSession(t, store, id, "idle", "")
	}
	delivery := func(child string) int64 {
		raw, _ := json.Marshal(map[string]string{"sessionId": child, "final": "SECRET of " + child})
		return userRow(t, store, "p", protocol.Message{Origin: protocol.OriginTaskResult, Content: protocol.TaskResultHeader + "\n" + string(raw)})
	}
	first, other, second := delivery("c"), delivery("d"), delivery("c")
	insertEdge(t, store, "p", "message", "task_result:c@7", SourceSession, "c")
	insertEdge(t, store, "p", "message", "task_result:d@3", SourceSession, "d")
	rev, err := store.RevokeSession(ctx, "c", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	p := projection(t, store, "p")
	if want := (Projection{Epoch: rev.Epoch, Open: true, Withheld: []int64{first, second}}); !reflect.DeepEqual(p, want) {
		t.Fatalf("projection %+v, want %+v (not %d)", p, want, other)
	}
}
