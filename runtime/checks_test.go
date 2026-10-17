package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// useChecks installs the fixture catalog with the given global checks object (global checks are always
// trusted).
func useChecks(t *testing.T, checks string) {
	t.Helper()
	useGlobalConfig(t, strings.Replace(boundCatalog("test/b0"), `{"model"`, `{"checks": `+checks+`, "model"`, 1))
}

func gitEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	initGitRepo(t, root)
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine, root
}

func checkInfo(t *testing.T, engine *Engine, id, check string) CheckInfo {
	t.Helper()
	info, err := engine.SessionChecks(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range info.Checks {
		if c.Spec.ID == check {
			return c
		}
	}
	t.Fatalf("check %s not in %+v", check, info)
	return CheckInfo{}
}

func taskResolution(t *testing.T, engine *Engine, id string) TaskStateInfo {
	t.Helper()
	info, err := engine.TaskState(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// Design §4.7 and the M4.1 acceptance row: a human acceptance is a result at the current fingerprint;
// the completion gate takes it without running the check, it latches done as evidence, and it stops
// counting once the fingerprint changes.
func TestHumanAcceptanceHoldsUntilTheFingerprintChanges(t *testing.T) {
	useChecks(t, `{"status": {"kind": "test", "command": "test -f ok.txt"}}`)
	engine, root := gitEngine(t)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if c := checkInfo(t, engine, handle.ID, "status"); c.Satisfied || !c.Spec.Required || c.Spec.Source != config.CheckSourceGlobal {
		t.Fatalf("before acceptance: %+v", c)
	}
	if _, err := engine.AcceptCheck(ctx, handle.ID, "nope", "desktop-user"); !errors.Is(err, ErrUnknownCheck) {
		t.Fatalf("unknown check: %v", err)
	}
	accepted, err := engine.AcceptCheck(ctx, handle.ID, "status", "desktop-user")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Provenance != "human:desktop-user" || accepted.Status != evaluator.StatusPassed {
		t.Fatalf("acceptance=%+v", accepted)
	}
	if c := checkInfo(t, engine, handle.ID, "status"); !c.Satisfied || c.Deciding == nil || c.Deciding.Seq != accepted.Seq {
		t.Fatalf("after acceptance: %+v", c)
	}

	result, err := engine.runAgentWith(ctx, handle.ID, &answering{final: "done"}, AgentOptions{Input: "finish"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("run=%+v", result)
	}
	results, err := engine.CheckResults(ctx, handle.ID, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("the gate ran the accepted check: results=%+v", results)
	}
	ts := taskResolution(t, engine, handle.ID)
	r := ts.State.Resolution
	if r.State != taskstate.ResolutionDone || r.By != taskstate.ByEvidence || !slices.Contains(r.Refs, "check:status@eval:1") {
		t.Fatalf("resolution=%+v", r)
	}

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := checkInfo(t, engine, handle.ID, "status"); c.Satisfied || c.Newest == nil || c.Newest.Seq != accepted.Seq {
		t.Fatalf("after an edit the acceptance still counts: %+v", c)
	}
}

// The completion gate runs a failing required check, stores its log and the diff it covers, and
// keeps the run going with a completion_blocked row; the host reads the artifacts back.
func TestCompletionGateRunsRequiredChecksWithArtifacts(t *testing.T) {
	useChecks(t, `{"status": {"kind": "test", "command": "echo checking; test -f ok.txt"}}`)
	engine, root := gitEngine(t)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "gate"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := engine.runAgentWith(ctx, handle.ID, &answering{final: "done"}, AgentOptions{Input: "finish", MaxSteps: 2})
	if err == nil || result.Status != "interrupted" {
		t.Fatalf("a failing required check let the run complete: %+v err=%v", result, err)
	}
	results, err := engine.CheckResults(ctx, handle.ID, -1, 0)
	if err != nil || len(results) == 0 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	failed := results[0]
	if failed.Status != evaluator.StatusFailed || failed.Provenance != evaluator.ProvenanceRuntime || len(failed.Artifacts) != 2 {
		t.Fatalf("result=%+v", failed)
	}
	var kinds []string
	for _, hash := range failed.Artifacts {
		a, err := engine.CheckArtifact(ctx, handle.ID, hash)
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, a.Kind)
		if a.Kind == "log" && !strings.Contains(string(a.Data), "checking") {
			t.Fatalf("log=%q", a.Data)
		}
		if a.Kind == "diff" && !strings.Contains(string(a.Data), "edited") {
			t.Fatalf("diff=%q", a.Data)
		}
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"diff", "log"}) {
		t.Fatalf("artifact kinds=%v", kinds)
	}
	if _, err := engine.CheckArtifact(ctx, handle.ID, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("missing artifact: %v", err)
	}
	page, err := engine.MessagePage(ctx, handle.ID, -1, 50)
	if err != nil {
		t.Fatal(err)
	}
	blocked := false
	for _, row := range page.Messages {
		var m protocol.Message
		decodeJSON(t, row.JSON, &m)
		if m.Origin == protocol.OriginCompletionBlocked && strings.Contains(m.Content, "status: failed") && strings.Contains(m.Content, failed.Artifacts[0]) {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("no completion_blocked row naming the failing check and its artifacts")
	}
}

// Checks run in the session's worktree, not the main checkout. Applying the worktree discharges only
// pending application: Apply alone never latches done, a queued user resolution is never overridden,
// and Apply with every required check satisfied at the current fingerprint latches done.
func TestChecksRunInTheWorktreeAndApplyLatchesOnlyOnEvidence(t *testing.T) {
	useChecks(t, `{"marker": {"kind": "test", "command": "test -f marker.txt"}}`)
	engine, root := gitEngine(t)
	ctx := context.Background()

	apply := func(goal string, accept bool, queueDrop bool) TaskStateInfo {
		t.Helper()
		handle, err := engine.NewSession(ctx, SessionOptions{Goal: goal})
		if err != nil {
			t.Fatal(err)
		}
		identity, err := engine.AttachWorktree(ctx, handle.ID, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { discardWorktreeQuietly(engine, handle.ID) })
		name := goal + ".txt"
		if err := os.WriteFile(filepath.Join(identity.Path, "marker.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(identity.Path, name), []byte(goal+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// The gate runs the check in the worktree, where marker.txt exists; the main checkout has none.
		result, err := engine.runAgentWith(ctx, handle.ID, &answering{final: "done"}, AgentOptions{Input: "finish"})
		if err != nil || result.Status != "completed" {
			t.Fatalf("run in worktree: %+v err=%v", result, err)
		}
		results, _ := engine.CheckResults(ctx, handle.ID, -1, 0)
		if len(results) != 1 || results[0].Status != evaluator.StatusPassed || !strings.HasSuffix(results[0].Fingerprint, "@"+identity.Path+"#"+results[0].Fingerprint[strings.LastIndex(results[0].Fingerprint, "#")+1:]) {
			t.Fatalf("check did not run in the worktree: %+v", results)
		}
		if r := taskResolution(t, engine, handle.ID); r.State.Resolution.State != taskstate.ResolutionOpen ||
			!slices.Contains(r.Obligations, taskstate.ObligationPendingApplication) {
			t.Fatalf("dirty worktree latched or lacks pending application: %+v", r)
		}
		runGitIn(t, identity.Path, "add", "marker.txt", name)
		runGitIn(t, identity.Path, "commit", "-m", goal)
		if accept {
			if _, err := engine.AcceptCheck(ctx, handle.ID, "marker", "desktop-user"); err != nil {
				t.Fatal(err)
			}
		}
		if queueDrop {
			if _, err := engine.ResolveThread(ctx, handle.ID, taskstate.ResolutionDropped, "desktop-user"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := engine.ApplySessionWorktree(ctx, handle.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("apply did not reach the main checkout: %v", err)
		}
		return taskResolution(t, engine, handle.ID)
	}

	// The commit changed the fingerprint, so the earlier runtime pass is stale and nothing else accepts.
	alone := apply("alone", false, false)
	if alone.State.Resolution.State != taskstate.ResolutionOpen || !slices.Equal(alone.Obligations, []string{taskstate.ObligationChecks}) {
		t.Fatalf("Apply alone: %+v", alone)
	}
	dropped := apply("dropped", true, true)
	if dropped.State.Resolution.State != taskstate.ResolutionDropped || dropped.State.Resolution.By != taskstate.ByUser {
		t.Fatalf("a queued user resolution was overridden: %+v", dropped.State.Resolution)
	}
	accepted := apply("accepted", true, false)
	r := accepted.State.Resolution
	if r.State != taskstate.ResolutionDone || r.By != taskstate.ByEvidence || len(r.Refs) == 0 || !strings.HasPrefix(r.Refs[0], "check:marker@eval:") {
		t.Fatalf("Apply with satisfied checks: %+v", r)
	}
	if accepted.Version == 0 {
		t.Fatal("no Task State version was written")
	}
}

// A8: an untrusted workspace's checks are neither run nor required; the host sees why.
func TestUntrustedWorkspaceChecksAreDropped(t *testing.T) {
	engine, root := gitEngine(t)
	if err := os.WriteFile(filepath.Join(root, config.FileName), []byte(`{"checks": {"evil": {"kind": "test", "command": "false"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "untrusted"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := engine.SessionChecks(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Checks) != 0 || len(info.Warnings) != 1 || info.Warnings[0].Key != "checks.evil" {
		t.Fatalf("checks=%+v warnings=%+v", info.Checks, info.Warnings)
	}
	if _, err := engine.AcceptCheck(ctx, handle.ID, "evil", "desktop-user"); !errors.Is(err, ErrUnknownCheck) {
		t.Fatalf("accepting an untrusted check: %v", err)
	}
	result, err := engine.runAgentWith(ctx, handle.ID, &answering{final: "done"}, AgentOptions{Input: "finish"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("an untrusted check blocked completion: %+v err=%v", result, err)
	}
}
