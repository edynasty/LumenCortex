package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/protocol"
)

// trustedProject opens an Engine on a git workspace whose own lumencortex.json defines checks, with
// the workspace trusted (ADR-0013) unless untrusted is set.
func trustedProject(t *testing.T, checks string, untrusted bool) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	initGitRepo(t, root)
	setProjectChecks(t, root, checks)
	trust := `"trustedWorkspaces": [` + quoteJSON(root) + `], `
	if untrusted {
		trust = ""
	}
	useGlobalConfig(t, strings.Replace(boundCatalog("test/b0"), `{"model"`, `{`+trust+`"model"`, 1))
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine, root
}

func setProjectChecks(t *testing.T, root, checks string) {
	t.Helper()
	writeFile(t, filepath.Join(root, config.FileName), `{"checks": `+checks+`}`)
}

func approvalState(t *testing.T, engine *Engine) CheckApprovalState {
	t.Helper()
	st, err := engine.CheckApprovalState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// decideNow runs the evaluator for one check of a session exactly as the completion gate does.
func decideNow(t *testing.T, engine *Engine, sessionID, check string) evaluator.Verdict {
	t.Helper()
	ctx := context.Background()
	ev, err := engine.sessionEvaluator(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	specs, _, _, err := engine.approvedChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, err := runChecks{engine: engine, ev: ev, specs: specs, store: engine.store, sessionID: sessionID}.Decide(ctx, []string{check}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 {
		t.Fatalf("verdicts %+v", v)
	}
	return v[0]
}

// writingProvider writes lumencortex.json with each body in turn (one write_file call per step), then
// answers "done" on every later step.
func writingProvider(bodies ...string) *exitProvider {
	return &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n <= len(bodies) {
			args, _ := json.Marshal(map[string]string{"path": config.FileName, "content": bodies[n-1]})
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{{ID: "w" + string(rune('0'+n)), Name: "write_file", Arguments: args}}}, FinishReason: "tool_calls"}, nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
	}}
}

func passedRuntimeResults(t *testing.T, engine *Engine, sessionID, check string) int {
	t.Helper()
	rows, err := engine.CheckResults(context.Background(), sessionID, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Check == check && r.Status == evaluator.StatusPassed {
			n++
		}
	}
	return n
}

const failingGate = `{"gate": {"kind": "test", "command": "exit 1"}}`

// P3 (opus-review/final/run.json) reproduced against the approval: a run rewrites the failing check to
// `exit 0`. The rewrite revokes the approval in the transaction that records it, the rewritten check is
// not run, and neither that run nor the next one on the thread completes on it. A user who approves the
// rewritten set makes it count again, under a new epoch.
func TestRunThatRewritesItsCheckCannotPassIt(t *testing.T) {
	engine, root := trustedProject(t, failingGate, false)
	ctx := context.Background()
	approved := approveChecks(t, engine)
	if approved.Epoch != 1 || len(approved.Checks) != 1 || approved.Checks[0].Spec.Source != config.CheckSourceWorkspace {
		t.Fatalf("approved state %+v", approved)
	}
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "make the gate pass"})
	if err != nil {
		t.Fatal(err)
	}
	rewritten := `{"checks": {"gate": {"kind": "test", "command": "exit 0"}}}`
	r1, err := engine.runAgentWith(ctx, handle.ID, writingProvider(rewritten), AgentOptions{Input: "fix", Policy: "workspace", MaxSteps: 3})
	if r1.Status == "completed" {
		t.Fatalf("the run that rewrote its check completed: %+v err=%v", r1, err)
	}
	st := approvalState(t, engine)
	if st.Approved || st.Previous == nil || st.Previous.State != "revoked" || st.Previous.Epoch != 1 {
		t.Fatalf("after the rewrite: %+v", st)
	}
	if st.LastWrite == nil || st.LastWrite.SessionID != handle.ID || st.LastWrite.Path != config.FileName {
		t.Fatalf("the write is not named: %+v", st.LastWrite)
	}
	if len(st.Checks) != 1 || st.Checks[0].Change != CheckChanged || st.Checks[0].Approved {
		t.Fatalf("the rewritten check: %+v", st.Checks)
	}
	r2, err := engine.runAgentWith(ctx, handle.ID, &answering{final: "done"}, AgentOptions{Input: "finish", MaxSteps: 2})
	if r2.Status == "completed" {
		t.Fatalf("the next run completed on the rewritten check: %+v err=%v", r2, err)
	}
	if n := passedRuntimeResults(t, engine, handle.ID, "gate"); n != 0 {
		t.Fatalf("%d passed results of the rewritten check were stored", n)
	}
	if v := decideNow(t, engine, handle.ID, "gate"); v.Passed || v.Result.Status != evaluator.StatusSpecUnapproved {
		t.Fatalf("decide: %+v", v)
	}
	if _, err := engine.AcceptCheck(ctx, handle.ID, "gate", "desktop-user"); !errors.Is(err, ErrSpecUnapproved) {
		t.Fatalf("accepting the rewritten check: %v", err)
	}

	again := approveChecks(t, engine)
	if again.Epoch != 2 || again.Checks[0].Change != CheckUnchanged {
		t.Fatalf("approving the rewritten set: %+v", again)
	}
	if v := decideNow(t, engine, handle.ID, "gate"); !v.Passed {
		t.Fatalf("the approved rewritten check: %+v", v)
	}
	_ = root
}

// Removing the only required check, making it optional, or emptying the set each changes the set: the
// approval no longer covers it, the remaining checks are not run, and an approval of the old digest is
// refused as stale.
func TestShrinkingTheCheckSetLeavesItUnapproved(t *testing.T) {
	both := `{"gate": {"kind": "test", "command": "exit 1"}, "lint": {"kind": "test", "command": "true"}}`
	for _, c := range []struct {
		name, checks string
		removed      int
		remaining    string
	}{
		{"delete the required check", `{"lint": {"kind": "test", "command": "true"}}`, 1, "lint"},
		{"make it optional", `{"gate": {"kind": "test", "command": "exit 1", "required": false}, "lint": {"kind": "test", "command": "true"}}`, 0, "lint"},
		{"empty the set", `{}`, 2, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			engine, root := trustedProject(t, both, false)
			ctx := context.Background()
			before := approveChecks(t, engine)
			handle, err := engine.NewSession(ctx, SessionOptions{Goal: c.name})
			if err != nil {
				t.Fatal(err)
			}
			if v := decideNow(t, engine, handle.ID, "lint"); !v.Passed {
				t.Fatalf("lint under the approved set: %+v", v)
			}
			setProjectChecks(t, root, c.checks)
			st := approvalState(t, engine)
			if st.Approved || st.SetDigest == before.SetDigest || len(st.Removed) != c.removed {
				t.Fatalf("after the change: approved=%v digest same=%v removed=%+v", st.Approved, st.SetDigest == before.SetDigest, st.Removed)
			}
			if c.remaining != "" {
				if v := decideNow(t, engine, handle.ID, c.remaining); v.Passed || v.Result.Status != evaluator.StatusSpecUnapproved {
					t.Fatalf("%s after the change: %+v", c.remaining, v)
				}
				if info := checkInfo(t, engine, handle.ID, c.remaining); info.Satisfied {
					t.Fatalf("the earlier pass still satisfies %s: %+v", c.remaining, info)
				}
			}
			if _, err := engine.ApproveChecks(ctx, ApproveChecksRequest{SetDigest: before.SetDigest, ApprovedBy: ApprovedByUser, Actor: "u"}); !errors.Is(err, ErrApprovalStale) {
				t.Fatalf("approving the old digest: %v", err)
			}
		})
	}
}

// A run that writes the definition and then restores it byte for byte still revoked the approval: the
// set digest is the approved one again, but the epoch is gone and approving again takes a new one. The
// pass decided under the old epoch does not count under the new one.
func TestWriteThenRevertKeepsTheRevocation(t *testing.T) {
	original := `{"checks": {"gate": {"kind": "test", "command": "true"}}}`
	engine, _ := trustedProject(t, `{"gate": {"kind": "test", "command": "true"}}`, false)
	ctx := context.Background()
	first := approveChecks(t, engine)
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "revert"})
	if err != nil {
		t.Fatal(err)
	}
	if v := decideNow(t, engine, handle.ID, "gate"); !v.Passed {
		t.Fatalf("approved gate: %+v", v)
	}
	rewritten := `{"checks": {"gate": {"kind": "test", "command": "exit 0"}}}`
	// The revocation leaves the required check unapproved, so the run cannot complete on it.
	if r, _ := engine.runAgentWith(ctx, handle.ID, writingProvider(rewritten, original), AgentOptions{Input: "edit", Policy: "workspace", MaxSteps: 4}); r.Status == "completed" {
		t.Fatalf("the run completed on a revoked approval: %+v", r)
	}
	st := approvalState(t, engine)
	if st.SetDigest != first.SetDigest || st.Approved || st.Previous.State != "revoked" {
		t.Fatalf("after write and revert: %+v", st)
	}
	if info := checkInfo(t, engine, handle.ID, "gate"); info.Satisfied {
		t.Fatalf("the pass under the revoked epoch satisfies: %+v", info)
	}
	second := approveChecks(t, engine)
	if second.Epoch <= first.Epoch {
		t.Fatalf("re-approval epoch %d after %d", second.Epoch, first.Epoch)
	}
	info := checkInfo(t, engine, handle.ID, "gate")
	if info.Satisfied {
		t.Fatalf("a pass from epoch %d satisfies epoch %d: %+v", first.Epoch, second.Epoch, info)
	}
	if v := decideNow(t, engine, handle.ID, "gate"); !v.Passed || v.Reused {
		t.Fatalf("under the new epoch the check must run again: %+v", v)
	}
}

// A human acceptance given under one epoch stops counting when the set is re-approved, even with the
// same definitions and the same tree.
func TestOldHumanAcceptanceDoesNotSurviveReapproval(t *testing.T) {
	engine, _ := trustedProject(t, failingGate, false)
	ctx := context.Background()
	approveChecks(t, engine)
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.AcceptCheck(ctx, handle.ID, "gate", "desktop-user"); err != nil {
		t.Fatal(err)
	}
	if !checkInfo(t, engine, handle.ID, "gate").Satisfied {
		t.Fatal("the acceptance does not count under its epoch")
	}
	if _, err := engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeChecks}); err != nil {
		t.Fatal(err)
	}
	if checkInfo(t, engine, handle.ID, "gate").Satisfied {
		t.Fatal("the acceptance counts after the revocation")
	}
	approveChecks(t, engine)
	if checkInfo(t, engine, handle.ID, "gate").Satisfied {
		t.Fatal("the acceptance counts under the next epoch")
	}
}

// Trusting a workspace (ADR-0013) lets its checks into the set; it does not approve them. Withdrawing
// trust drops them, which changes the set; restoring trust after the approval was revoked approves
// nothing.
func TestWorkspaceTrustIsNotApproval(t *testing.T) {
	engine, root := trustedProject(t, failingGate, false)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "trust"})
	if err != nil {
		t.Fatal(err)
	}
	if st := approvalState(t, engine); st.Approved || len(st.Checks) != 1 {
		t.Fatalf("trusted, never approved: %+v", st)
	}
	if v := decideNow(t, engine, handle.ID, "gate"); v.Result.Status != evaluator.StatusSpecUnapproved {
		t.Fatalf("a trusted, unapproved check ran: %+v", v)
	}
	if _, err := engine.AcceptCheck(ctx, handle.ID, "gate", "desktop-user"); !errors.Is(err, ErrSpecUnapproved) {
		t.Fatalf("accepting a trusted, unapproved check: %v", err)
	}
	approved := approveChecks(t, engine)

	// Trust withdrawn: the workspace check is dropped and the set is another one.
	useGlobalConfig(t, boundCatalog("test/b0"))
	if st := approvalState(t, engine); st.Approved || st.SetDigest == approved.SetDigest || len(st.Removed) != 1 || len(st.Warnings) == 0 {
		t.Fatalf("untrusted: %+v", st)
	}
	if _, err := engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeChecks}); err != nil {
		t.Fatal(err)
	}
	// Trust restored: the set digest is the old one, the approval is not.
	useGlobalConfig(t, strings.Replace(boundCatalog("test/b0"), `{"model"`, `{"trustedWorkspaces": [`+quoteJSON(root)+`], "model"`, 1))
	if st := approvalState(t, engine); st.SetDigest != approved.SetDigest || st.Approved {
		t.Fatalf("trust restored: %+v", st)
	}
}

// Epochs only grow: re-approving a digest that was approved before, after a revocation, takes the next
// epoch, and results stored under the old one stay dead.
func TestRestoringAnOldDigestDoesNotReviveItsEpoch(t *testing.T) {
	engine, root := trustedProject(t, `{"gate": {"kind": "test", "command": "true"}}`, false)
	ctx := context.Background()
	a := approveChecks(t, engine)
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "epochs"})
	if err != nil {
		t.Fatal(err)
	}
	if v := decideNow(t, engine, handle.ID, "gate"); !v.Passed {
		t.Fatal(v)
	}
	setProjectChecks(t, root, `{"gate": {"kind": "test", "command": "true", "timeoutSeconds": 120}}`)
	b := approveChecks(t, engine)
	setProjectChecks(t, root, `{"gate": {"kind": "test", "command": "true"}}`)
	if st := approvalState(t, engine); st.SetDigest != a.SetDigest || st.Approved {
		t.Fatalf("old digest restored: %+v", st)
	}
	c := approveChecks(t, engine)
	if !(a.Epoch < b.Epoch && b.Epoch < c.Epoch) {
		t.Fatalf("epochs %d %d %d", a.Epoch, b.Epoch, c.Epoch)
	}
	if v := decideNow(t, engine, handle.ID, "gate"); !v.Passed || v.Reused {
		t.Fatalf("the epoch-%d pass was reused under epoch %d: %+v", a.Epoch, c.Epoch, v)
	}
}
