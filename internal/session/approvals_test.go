package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func approveSet(t *testing.T, store *Store, project, setDigest string, specs ...ApprovedSpec) int64 {
	t.Helper()
	epoch, err := store.ApproveCheckSet(context.Background(), CheckSetApproval{ProjectID: project, SetDigest: setDigest,
		Specs: specs, ApprovedBy: ApprovedByUser, Actor: "u"})
	if err != nil {
		t.Fatal(err)
	}
	return epoch
}

func latestSet(t *testing.T, store *Store, project string) (*ApprovalRecord, []ApprovalRecord) {
	t.Helper()
	set, specs, err := store.LatestCheckSetApproval(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	return set, specs
}

// ADR-0017: an approval of a set writes the set and each definition under one new epoch and revokes the
// previous one; epochs never repeat, even after a revocation, so approving the old digest again does not
// revive the old epoch; projects do not share approvals; the empty set is approvable.
func TestCheckSetApprovalEpochsAndSupersession(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	if set, _ := latestSet(t, store, "/p"); set != nil {
		t.Fatalf("a new project has approval %+v", set)
	}
	a := ApprovedSpec{CheckID: "go-test", SpecDigest: sha("a")}
	e1 := approveSet(t, store, "/p", sha("1"), a)
	set, specs := latestSet(t, store, "/p")
	if e1 != 1 || set.State != ApprovalActive || set.SetDigest != sha("1") || len(specs) != 1 || specs[0].SpecDigest != sha("a") || specs[0].Epoch != 1 {
		t.Fatalf("first approval: epoch %d %+v %+v", e1, set, specs)
	}
	e2 := approveSet(t, store, "/p", sha("2"))
	set, specs = latestSet(t, store, "/p")
	if e2 != 2 || set.SetDigest != sha("2") || len(specs) != 0 {
		t.Fatalf("empty set approval: epoch %d %+v %+v", e2, set, specs)
	}
	var active int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM check_approvals WHERE project_id = '/p' AND state = 'active'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active rows after supersession: %d %v", active, err)
	}
	if n, err := store.RevokeCheckApprovals(ctx, "/p"); err != nil || n != 1 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if set, _ = latestSet(t, store, "/p"); set.State != ApprovalRevoked || set.Epoch != 2 {
		t.Fatalf("after revoke: %+v", set)
	}
	if e3 := approveSet(t, store, "/p", sha("1"), a); e3 != 3 {
		t.Fatalf("re-approving the first digest took epoch %d, want 3", e3)
	}
	if other := approveSet(t, store, "/q", sha("1"), a); other != 1 {
		t.Fatalf("another project's epoch: %d", other)
	}
	if set, _ = latestSet(t, store, "/p"); set.Epoch != 3 {
		t.Fatalf("project /p sees %+v", set)
	}
}

// Approvers and fields are validated: a check set is approved by user, trust-flow or experiment-freeze,
// a binding by user, team-decision or experiment-freeze; a definition appears once.
func TestApprovalValidation(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	bad := []CheckSetApproval{
		{ProjectID: "/p", SetDigest: sha("1"), ApprovedBy: ApprovedByTeam, Actor: "u"},
		{ProjectID: "/p", SetDigest: sha("1"), ApprovedBy: "agent", Actor: "u"},
		{ProjectID: "/p", SetDigest: sha("1"), ApprovedBy: ApprovedByUser},
		{ProjectID: "", SetDigest: sha("1"), ApprovedBy: ApprovedByUser, Actor: "u"},
		{ProjectID: "/p", SetDigest: sha("1"), ApprovedBy: ApprovedByUser, Actor: "u",
			Specs: []ApprovedSpec{{CheckID: "a", SpecDigest: sha("a")}, {CheckID: "a", SpecDigest: sha("b")}}},
	}
	for i, a := range bad {
		if _, err := store.ApproveCheckSet(ctx, a); !errors.Is(err, ErrApprovalInvalid) {
			t.Errorf("set %d: %v", i, err)
		}
	}
	b := BindingApproval{ProjectID: "/p", ConstraintRef: "c", ConstraintDigest: sha("3"), CheckID: "a", SpecDigest: sha("a"),
		BindingDigest: sha("4"), ApprovedBy: ApprovedByTrust, Actor: "u"}
	if _, err := store.ApproveConstraintBinding(ctx, b); !errors.Is(err, ErrApprovalInvalid) {
		t.Errorf("trust-flow binding: %v", err)
	}
	if set, _ := latestSet(t, store, "/p"); set != nil {
		t.Fatalf("a refused approval was stored: %+v", set)
	}
}

// A binding is its own approval: approving one never touches the check set, a new binding of the same
// constraint to the same check supersedes the old, and one is revoked by its digest.
func TestConstraintBindingApproval(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	approveSet(t, store, "/p", sha("1"), ApprovedSpec{CheckID: "a", SpecDigest: sha("a")})
	bind := func(digest, check string) int64 {
		t.Helper()
		e, err := store.ApproveConstraintBinding(ctx, BindingApproval{ProjectID: "/p", ConstraintRef: "c", ConstraintDigest: sha("3"),
			CheckID: check, SpecDigest: sha("a"), BindingDigest: digest, ApprovedBy: ApprovedByTeam, Actor: "u", DecisionRef: "decision:1"})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	if e := bind(sha("4"), "a"); e != 2 {
		t.Fatalf("binding epoch %d, want the project's next (2)", e)
	}
	bind(sha("5"), "a")
	bind(sha("6"), "b")
	active, err := store.ActiveConstraintBindings(ctx, "/p")
	if err != nil || len(active) != 2 || active[0].BindingDigest != sha("5") || active[1].BindingDigest != sha("6") || active[0].DecisionRef != "decision:1" {
		t.Fatalf("active bindings: %+v %v", active, err)
	}
	if set, specs := latestSet(t, store, "/p"); set.State != ApprovalActive || set.Epoch != 1 || len(specs) != 1 {
		t.Fatalf("bindings touched the check set: %+v %+v", set, specs)
	}
	if n, err := store.RevokeConstraintBinding(ctx, "/p", sha("6")); err != nil || n != 1 {
		t.Fatalf("revoke binding: %d %v", n, err)
	}
	if active, _ = store.ActiveConstraintBindings(ctx, "/p"); len(active) != 1 {
		t.Fatalf("after revoke: %+v", active)
	}
}

// ADR-0017: a committed tool result whose file edge names the watched definition file (in any case)
// revokes the project's check approval in the commit's transaction; another path, a store that watches
// nothing, and a message edge do not. LastDefinitionWrite names the write.
func TestDefinitionWriteRevokesInTheCommit(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	reach(t, store, "s", "r1", PurposeMain, RequestAnswered)
	commit := func(exec string, edges ...DerivedEdge) {
		t.Helper()
		if err := store.AdmitExecution(ctx, execAdmission("s", exec, "r1")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CommitToolResult(ctx, toolCommit("s", exec, OutcomeComplete, CoverageComplete, edges...)); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		set, _ := latestSet(t, store, "/p")
		return set.State
	}
	approveSet(t, store, "/p", sha("1"))
	commit("e0", fileEdge("lumencortex.json"))
	if state() != ApprovalActive {
		t.Fatal("a store that watches nothing revoked")
	}
	store.WatchCheckDefinitions("/p", "lumencortex.json")
	commit("e1", fileEdge("src/lumencortex.json"), fileEdge("main.go"))
	commit("e2", DerivedEdge{Kind: "message", TargetRef: "lumencortex.json@x", SourceKind: SourceRequest, SourceRef: "r1", OriginClass: OriginAgentGen})
	if state() != ApprovalActive {
		t.Fatal("another path or a message edge revoked")
	}
	if w, err := store.LastDefinitionWrite(ctx, mustParse(t, "2000-01-01T00:00:00Z")); err != nil || w == nil || w.ExecutionID != "e0" {
		t.Fatalf("last definition write: %+v %v", w, err)
	}
	commit("e3", fileEdge("main.go"), fileEdge("./LumenCortex.JSON"))
	if state() != ApprovalRevoked {
		t.Fatal("writing the definition file did not revoke")
	}
	w, err := store.LastDefinitionWrite(ctx, mustParse(t, "2000-01-01T00:00:00Z"))
	if err != nil || w == nil || w.ExecutionID != "e3" || w.SessionID != "s" || w.RunID != "run1" || w.Path != "./LumenCortex.JSON" {
		t.Fatalf("last definition write: %+v %v", w, err)
	}
	if w, _ := store.LastDefinitionWrite(ctx, w.At.Add(1)); w != nil {
		t.Fatalf("a write before since: %+v", w)
	}
}
