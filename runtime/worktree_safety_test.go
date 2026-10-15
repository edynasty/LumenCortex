package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// ADR-0005: worktree ownership, leases and cleanup, against real git repositories.

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func worktreeEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	initGitRepo(t, root)
	engine, err := Open(Options{Workspace: root, Budget: Budget{MaxAgents: 8}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine, root
}

func attachedSession(t *testing.T, engine *Engine, goal string) (*SessionHandle, SessionRuntime) {
	t.Helper()
	handle := newTestSession(t, engine, goal)
	identity, err := engine.AttachWorktree(context.Background(), handle.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return handle, identity
}

func commitIn(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-m", "add "+name)
}

func branchExists(root, branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = root
	return cmd.Run() == nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func boundKind(t *testing.T, engine *Engine, id string) string {
	t.Helper()
	identity, err := engine.SessionRuntime(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Kind
}

// discardWorktree removes the session's worktree with a discard of exactly the
// inspected state.
func discardWorktree(t *testing.T, engine *Engine, id string) WorktreeCleanupResult {
	t.Helper()
	in, err := engine.InspectSessionWorktree(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.RemoveSessionWorktree(context.Background(), id, WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: in.Head, ExpectStatusDigest: in.StatusDigest}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// discardWorktreeQuietly is best-effort test cleanup.
func discardWorktreeQuietly(engine *Engine, id string) {
	in, err := engine.InspectSessionWorktree(context.Background(), id)
	if err == nil {
		_, _ = engine.RemoveSessionWorktree(context.Background(), id, WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: in.Head, ExpectStatusDigest: in.StatusDigest}})
	}
}

func refusal(t *testing.T, err error, want error) *WorktreeRefusal {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
	var r *WorktreeRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err=%v is not a *WorktreeRefusal", err)
	}
	return r
}

// P1 inverted: deleting a session with a clean worktree removes the directory, the
// branch, the session and both lock files.
func TestDeleteSessionCleansItsWorktree(t *testing.T) {
	engine, root := worktreeEngine(t)
	handle, identity := attachedSession(t, engine, "clean")
	lease := filepath.Join(engine.runDir(), worktreeLeaseID(identity.Path)+".lock")
	if _, err := handle.RunShell(context.Background(), "true"); err != nil { // creates the lease file
		t.Fatal(err)
	}
	if !pathExists(lease) {
		t.Fatal("a worktree use should leave its lease file")
	}

	result, err := engine.DeleteSession(context.Background(), handle.ID, WorktreeCleanup{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Worktree.DirectoryRemoved || !result.Worktree.BranchDeleted || result.Worktree.Action != "removed" {
		t.Fatalf("result=%+v", result)
	}
	if pathExists(identity.Path) || branchExists(root, identity.Branch) {
		t.Fatalf("worktree survived: dir=%v branch=%v", pathExists(identity.Path), branchExists(root, identity.Branch))
	}
	if pathExists(lease) {
		t.Fatal("lease file survived the owner's deletion")
	}
	if _, _, err := engine.Session(context.Background(), handle.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("session after delete: %v", err)
	}
}

// P2 inverted: a running session's worktree can be neither removed nor deleted.
func TestWorktreeOfARunningSessionCannotBeRemoved(t *testing.T) {
	engine, _ := worktreeEngine(t)
	handle, identity := attachedSession(t, engine, "busy")
	supervisor := startBlockedRun(t, engine, handle.ID)

	if _, err := engine.RemoveSessionWorktree(context.Background(), handle.ID, WorktreeCleanup{}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("remove during a run: err=%v, want ErrRunHeld", err)
	}
	if _, err := engine.DeleteSession(context.Background(), handle.ID, WorktreeCleanup{}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("delete during a run: err=%v, want ErrRunHeld", err)
	}
	if _, err := engine.ApplySessionWorktree(context.Background(), handle.ID); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("apply during a run: err=%v, want ErrRunHeld", err)
	}
	if !pathExists(identity.Path) || boundKind(t, engine, handle.ID) != RuntimeWorktree {
		t.Fatal("a refused removal changed the worktree or the binding")
	}
	supervisor.Cancel(handle.ID)
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)
}

// spawningProvider makes the parent spawn one read-only child and answer after the
// tool result; the child blocks until release is closed. Parent and child share it
// concurrently, so it keeps no state.
type spawningProvider struct{ release <-chan struct{} }

func (spawningProvider) Model() string { return "spawning-test" }

func (p spawningProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if strings.Contains(req.Messages[0].Content, "read-only LumenCortex subagent") {
		select {
		case <-p.release:
			return protocol.ProviderResponse{Message: protocol.Message{Content: "child finding"}}, nil
		case <-ctx.Done():
			return protocol.ProviderResponse{}, ctx.Err()
		}
	}
	if req.Messages[len(req.Messages)-1].Role == "tool" {
		return protocol.ProviderResponse{Message: protocol.Message{Content: "parent answered"}}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
		{ID: "spawn-1", Name: "spawn_subagent", Arguments: json.RawMessage(`{"goal":"look around"}`)},
	}}, FinishReason: "tool_calls"}, nil
}

// spawnBorrower runs the owner until it has ended while its read-only child still
// runs in the owner's worktree (probe P5). Closing release lets the child finish.
func spawnBorrower(t *testing.T, engine *Engine, ownerID string) (*RunSupervisor, string, chan struct{}) {
	t.Helper()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Close)
	release := make(chan struct{})
	if _, err := supervisor.startWith(context.Background(), ownerID, spawningProvider{release: release}, AgentOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, ownerID, 10*time.Second)
	tree, err := supervisor.SubagentTree(context.Background(), ownerID)
	if err != nil || len(tree) != 1 {
		t.Fatalf("subagent tree=%v err=%v", tree, err)
	}
	child := tree[0].SessionID
	if !supervisor.Active(child) {
		t.Fatal("the child should still run after its parent ended")
	}
	return supervisor, child, release
}

// P4 inverted: a borrower only unbinds; it cannot remove, discard or apply the
// owner's worktree.
func TestBorrowerCannotRemoveTheOwnersWorktree(t *testing.T) {
	engine, root := worktreeEngine(t)
	owner, identity := attachedSession(t, engine, "owner")
	supervisor, child, release := spawnBorrower(t, engine, owner.ID)
	close(release)
	waitUntilIdle(t, supervisor, child, 10*time.Second)

	childRuntime, err := engine.SessionRuntime(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if childRuntime.Path != identity.Path || childRuntime.Owner != owner.ID {
		t.Fatalf("child binding=%+v, want the owner's worktree with owner %s", childRuntime, owner.ID)
	}
	in, err := engine.InspectSessionWorktree(context.Background(), child)
	if err != nil || in.Role != WorktreeBorrower || in.Owner != owner.ID {
		t.Fatalf("inspection=%+v err=%v", in, err)
	}
	discard := WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: in.Head, ExpectStatusDigest: in.StatusDigest}}
	if _, err := engine.RemoveSessionWorktree(context.Background(), child, discard); !errors.Is(err, ErrWorktreeBorrowed) {
		t.Fatalf("borrower discard: err=%v, want ErrWorktreeBorrowed", err)
	}
	if _, err := engine.ApplySessionWorktree(context.Background(), child); !errors.Is(err, ErrWorktreeBorrowed) {
		t.Fatalf("borrower apply: err=%v, want ErrWorktreeBorrowed", err)
	}
	result, err := engine.RemoveSessionWorktree(context.Background(), child, WorktreeCleanup{})
	if err != nil || result.Action != "unbound" || result.DirectoryRemoved || result.BranchDeleted {
		t.Fatalf("borrower remove: result=%+v err=%v", result, err)
	}
	if !pathExists(identity.Path) || !branchExists(root, identity.Branch) || boundKind(t, engine, owner.ID) != RuntimeWorktree {
		t.Fatal("the borrower's removal touched the owner's worktree")
	}
	if boundKind(t, engine, child) != RuntimeLocal {
		t.Fatal("the borrower is still bound")
	}
}

// P5: the parent's run has ended but its child still runs in the worktree. The
// child's lease keeps the owner from removing, applying or deleting; the child
// blocks the deletion as a running dependent.
func TestBorrowerLeaseOutlivesTheOwnersRun(t *testing.T) {
	engine, _ := worktreeEngine(t)
	owner, identity := attachedSession(t, engine, "owner")
	supervisor, child, release := spawnBorrower(t, engine, owner.ID)

	if _, err := engine.RemoveSessionWorktree(context.Background(), owner.ID, WorktreeCleanup{}); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("owner remove while the borrower runs: err=%v, want ErrWorktreeBusy", err)
	}
	if _, err := engine.ApplySessionWorktree(context.Background(), owner.ID); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("owner apply while the borrower runs: err=%v, want ErrWorktreeBusy", err)
	}
	_, err := engine.DeleteSession(context.Background(), owner.ID, WorktreeCleanup{})
	var dependents *DependentsError
	if !errors.As(err, &dependents) || !errors.Is(err, ErrSessionHasDependents) ||
		!reflect.DeepEqual(dependents.Dependents, []SessionDependent{{SessionID: child, Kind: "subagent", Reason: "running"}}) {
		t.Fatalf("delete while the borrower runs: err=%v", err)
	}
	if !pathExists(identity.Path) {
		t.Fatal("a refused operation removed the worktree")
	}

	close(release)
	waitUntilIdle(t, supervisor, child, 10*time.Second)
	result, err := engine.DeleteSession(context.Background(), owner.ID, WorktreeCleanup{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.DeletedChildren, []string{child}) || !result.Worktree.DirectoryRemoved {
		t.Fatalf("result=%+v", result)
	}
	if _, _, err := engine.Session(context.Background(), child); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the finished temporary child should be deleted with its parent: %v", err)
	}
}

// Every refusal and option of the ADR-0005 §5 matrix for an owner.
func TestWorktreeCleanupMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("dirty", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "dirty")
		if err := os.WriteFile(filepath.Join(identity.Path, "draft.txt"), []byte("unsaved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{})
		r := refusal(t, err, ErrWorktreeDirty)
		if !slices.Equal(r.Inspection.DirtyFiles, []string{"draft.txt"}) {
			t.Fatalf("dirty files=%v", r.Inspection.DirtyFiles)
		}
		_, err = engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{KeepBranch: true})
		refusal(t, err, ErrWorktreeDirty)

		in, err := engine.InspectSessionWorktree(ctx, handle.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(identity.Path, "later.txt"), []byte("written after the user looked\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: in.Head, ExpectStatusDigest: in.StatusDigest}})
		refusal(t, err, ErrDiscardStale)
		if !pathExists(filepath.Join(identity.Path, "later.txt")) {
			t.Fatal("a stale discard destroyed work")
		}

		in, err = engine.InspectSessionWorktree(ctx, handle.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: in.Head, ExpectStatusDigest: in.StatusDigest}})
		if err != nil || !result.Worktree.Discarded || !result.Worktree.DirectoryRemoved || !result.Worktree.BranchDeleted {
			t.Fatalf("discard: result=%+v err=%v", result, err)
		}
		if pathExists(identity.Path) || branchExists(root, identity.Branch) {
			t.Fatal("discard left the worktree behind")
		}
	})

	t.Run("unapplied commits", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "commits")
		commitIn(t, identity.Path, "feature.txt", "feature\n")
		head := gitIn(t, identity.Path, "rev-parse", "HEAD")

		_, err := engine.RemoveSessionWorktree(ctx, handle.ID, WorktreeCleanup{})
		r := refusal(t, err, ErrWorktreeUnapplied)
		if !slices.Equal(r.Inspection.UnappliedCommits, []string{head}) {
			t.Fatalf("unapplied=%v want [%s]", r.Inspection.UnappliedCommits, head)
		}
		if !pathExists(identity.Path) || boundKind(t, engine, handle.ID) != RuntimeWorktree {
			t.Fatal("a refused removal changed state")
		}

		result, err := engine.RemoveSessionWorktree(ctx, handle.ID, WorktreeCleanup{KeepBranch: true})
		if err != nil || !result.DirectoryRemoved || result.BranchDeleted || !result.BranchKept || !slices.Equal(result.Commits, []string{head}) {
			t.Fatalf("keep branch: result=%+v err=%v", result, err)
		}
		if pathExists(identity.Path) || !branchExists(root, identity.Branch) || gitIn(t, root, "rev-parse", identity.Branch) != head {
			t.Fatal("KeepBranch must remove the directory and keep the branch at its commit")
		}
		if boundKind(t, engine, handle.ID) != RuntimeLocal {
			t.Fatal("the session is still bound after removal")
		}
	})

	t.Run("discard unapplied commits", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "commits")
		commitIn(t, identity.Path, "feature.txt", "feature\n")
		result := discardWorktree(t, engine, handle.ID)
		if !result.Discarded || !result.BranchDeleted || len(result.Commits) != 1 {
			t.Fatalf("result=%+v", result)
		}
		if branchExists(root, identity.Branch) {
			t.Fatal("discard kept the branch")
		}
	})

	t.Run("applied commits", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "applied")
		commitIn(t, identity.Path, "feature.txt", "feature\n")
		if _, err := engine.ApplySessionWorktree(ctx, handle.ID); err != nil {
			t.Fatal(err)
		}
		result, err := engine.RemoveSessionWorktree(ctx, handle.ID, WorktreeCleanup{})
		if err != nil || !result.DirectoryRemoved || !result.BranchDeleted {
			t.Fatalf("applied work: result=%+v err=%v", result, err)
		}
		if branchExists(root, identity.Branch) || !pathExists(filepath.Join(root, "feature.txt")) {
			t.Fatal("the applied commit must be in the main checkout and the branch gone")
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "missing")
		commitIn(t, identity.Path, "feature.txt", "feature\n")
		if err := os.RemoveAll(identity.Path); err != nil {
			t.Fatal(err)
		}

		before := rowCount(t, handle)
		supervisor, err := NewRunSupervisor(engine)
		if err != nil {
			t.Fatal(err)
		}
		defer supervisor.Close()
		_, err = supervisor.startWith(ctx, handle.ID, completingProvider("x"), AgentOptions{Input: "must not land"})
		var missing *WorktreeMissingError
		if !errors.As(err, &missing) || !errors.Is(err, ErrWorktreeMissing) || !missing.BranchExists || missing.Branch != identity.Branch {
			t.Fatalf("Start on a missing worktree: err=%v", err)
		}
		if rowCount(t, handle) != before {
			t.Fatal("a Start refused for a missing worktree wrote rows")
		}

		_, err = engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{})
		refusal(t, err, ErrWorktreeUnapplied)
		result, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{KeepBranch: true})
		if err != nil || !result.Worktree.BranchKept {
			t.Fatalf("keep branch of a missing worktree: result=%+v err=%v", result, err)
		}
		if !branchExists(root, identity.Branch) {
			t.Fatal("the branch with unapplied commits was deleted")
		}
		if strings.Contains(gitIn(t, root, "worktree", "list", "--porcelain"), identity.Path) {
			t.Fatal("the missing worktree is still registered")
		}
	})

	t.Run("uncertain ownership", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "switched")
		gitIn(t, identity.Path, "checkout", "-q", "-b", "elsewhere")

		_, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{})
		r := refusal(t, err, ErrWorktreeOwnership)
		if r.Inspection.Role != WorktreeUncertain || r.Inspection.Uncertain == "" {
			t.Fatalf("inspection=%+v", r.Inspection)
		}
		in, err := engine.InspectSessionWorktree(ctx, handle.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: in.Head, ExpectStatusDigest: in.StatusDigest}})
		refusal(t, err, ErrWorktreeOwnership)

		result, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{KeepBranch: true})
		if err != nil || result.Worktree.Action != "retained" || result.Worktree.DirectoryRemoved {
			t.Fatalf("retain: result=%+v err=%v", result, err)
		}
		if !pathExists(identity.Path) || !branchExists(root, "elsewhere") || !branchExists(root, identity.Branch) {
			t.Fatal("an uncertain worktree was touched")
		}
	})

	t.Run("options are exclusive", func(t *testing.T) {
		engine, _ := worktreeEngine(t)
		handle, _ := attachedSession(t, engine, "both")
		if _, err := engine.RemoveSessionWorktree(ctx, handle.ID, WorktreeCleanup{KeepBranch: true, Discard: &WorktreeDiscard{}}); err == nil {
			t.Fatal("KeepBranch with Discard must be rejected")
		}
	})
}

// A crash after the disk work and before the database write leaves the session bound
// to a cleaned-up worktree; the retry converges.
func TestDeletionRetryAfterPartialCleanup(t *testing.T) {
	ctx := context.Background()

	t.Run("directory removed, branch and session left", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "partial")
		gitIn(t, root, "worktree", "remove", identity.Path)

		result, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{})
		if err != nil || !result.Worktree.BranchDeleted {
			t.Fatalf("retry: result=%+v err=%v", result, err)
		}
		if branchExists(root, identity.Branch) {
			t.Fatal("the retry left the branch")
		}
		if _, _, err := engine.Session(ctx, handle.ID); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("session after retry: %v", err)
		}
	})

	t.Run("discard interrupted after removal needs a fresh authorization", func(t *testing.T) {
		engine, root := worktreeEngine(t)
		handle, identity := attachedSession(t, engine, "partial discard")
		commitIn(t, identity.Path, "feature.txt", "feature\n")
		if err := os.WriteFile(filepath.Join(identity.Path, "draft.txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before, err := engine.InspectSessionWorktree(ctx, handle.ID)
		if err != nil {
			t.Fatal(err)
		}
		gitIn(t, root, "worktree", "remove", "--force", identity.Path) // the crashed attempt's disk step

		stale := WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: before.Head, ExpectStatusDigest: before.StatusDigest}}
		_, err = engine.DeleteSession(ctx, handle.ID, stale)
		refusal(t, err, ErrDiscardStale)
		if !branchExists(root, identity.Branch) {
			t.Fatal("a stale authorization deleted the branch")
		}
		after, err := engine.InspectSessionWorktree(ctx, handle.ID)
		if err != nil || after.Exists || len(after.UnappliedCommits) != 1 {
			t.Fatalf("fresh inspection=%+v err=%v", after, err)
		}
		if _, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{Discard: &WorktreeDiscard{ExpectHead: after.Head, ExpectStatusDigest: after.StatusDigest}}); err != nil {
			t.Fatal(err)
		}
		if branchExists(root, identity.Branch) {
			t.Fatal("the confirmed discard kept the branch")
		}
	})
}

// ADR-0005 §6: only finished read-only subagents without their own worktree are
// deleted with their parent; anything else blocks the deletion untouched.
func TestDeleteSessionDescendants(t *testing.T) {
	ctx := context.Background()
	engine, _ := worktreeEngine(t)
	parent := newTestSession(t, engine, "parent")
	temporary := newTestSession(t, engine, "temporary")
	ownsWorktree, ownWorktree := attachedSession(t, engine, "owns a worktree")
	independent := newTestSession(t, engine, "execution task")
	for _, rel := range []session.Relation{
		{ParentSessionID: parent.ID, ChildSessionID: temporary.ID, Kind: "subagent"},
		{ParentSessionID: parent.ID, ChildSessionID: ownsWorktree.ID, Kind: "subagent"},
		{ParentSessionID: parent.ID, ChildSessionID: independent.ID, Kind: "execution"},
	} {
		if err := engine.store.AddRelation(ctx, rel); err != nil {
			t.Fatal(err)
		}
	}

	_, err := engine.DeleteSession(ctx, parent.ID, WorktreeCleanup{})
	var dependents *DependentsError
	if !errors.As(err, &dependents) {
		t.Fatalf("err=%v, want *DependentsError", err)
	}
	got := map[string]string{}
	for _, d := range dependents.Dependents {
		got[d.SessionID] = d.Reason
	}
	if !reflect.DeepEqual(got, map[string]string{ownsWorktree.ID: "owns_worktree", independent.ID: "independent"}) {
		t.Fatalf("dependents=%v", got)
	}
	for _, id := range []string{parent.ID, temporary.ID, ownsWorktree.ID, independent.ID} {
		if _, _, err := engine.Session(ctx, id); err != nil {
			t.Fatalf("a refused deletion removed %s: %v", id, err)
		}
	}
	if !pathExists(ownWorktree.Path) {
		t.Fatal("a refused deletion removed a child's worktree")
	}

	// The user deals with each dependent on its own; the finished temporary child then
	// goes with its parent.
	for _, id := range []string{ownsWorktree.ID, independent.ID} {
		if _, err := engine.DeleteSession(ctx, id, WorktreeCleanup{}); err != nil {
			t.Fatal(err)
		}
	}
	if pathExists(ownWorktree.Path) {
		t.Fatal("the child's own worktree survived its deletion")
	}
	result, err := engine.DeleteSession(ctx, parent.ID, WorktreeCleanup{})
	if err != nil || !slices.Equal(result.DeletedChildren, []string{temporary.ID}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, _, err := engine.Session(ctx, temporary.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the temporary child survived its parent: %v", err)
	}
}

// Leases coordinate Engines in one process: a holder's shared lease blocks every
// exclusive operation of another Engine, and an exclusive lease blocks new runs and
// host writes.
func TestWorktreeLeaseAcrossEngines(t *testing.T) {
	ctx := context.Background()
	engine, root := worktreeEngine(t)
	handle, identity := attachedSession(t, engine, "shared")
	peer := openEngineAt(t, root)

	_, held, err := peer.leaseWorkspace(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RemoveSessionWorktree(ctx, handle.ID, WorktreeCleanup{}); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("remove: err=%v, want ErrWorktreeBusy", err)
	}
	if _, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{}); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("delete: err=%v, want ErrWorktreeBusy", err)
	}
	if _, err := handle.RunShell(ctx, "true"); err != nil {
		t.Fatalf("shared uses must coexist: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	exclusive, err := peer.acquireWorktreeLease(identity.Path, true)
	if err != nil {
		t.Fatal(err)
	}
	before := rowCount(t, handle)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	if _, err := supervisor.startWith(ctx, handle.ID, completingProvider("x"), AgentOptions{Input: "must not land"}); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("Start during a cleanup: err=%v, want ErrWorktreeBusy", err)
	}
	if _, err := engine.SessionGitCommit(ctx, handle.ID, "must not commit"); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("commit during a cleanup: err=%v, want ErrWorktreeBusy", err)
	}
	if rowCount(t, handle) != before {
		t.Fatal("a Start refused by the lease wrote rows")
	}
	if err := exclusive.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RemoveSessionWorktree(ctx, handle.ID, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
}

// Concurrent host writes and deletion: every shell either finishes before the
// deletion or is refused; the deletion either waits (ErrWorktreeBusy) or completes,
// and nothing runs in a removed worktree.
func TestConcurrentWorktreeUseAndDeletion(t *testing.T) {
	ctx := context.Background()
	engine, root := worktreeEngine(t)
	handle, identity := attachedSession(t, engine, "race")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var unexpected []error
	var mu sync.Mutex
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(2 * time.Millisecond): // host writes are bursts, not a saturated loop
				}
				result, err := handle.RunShell(ctx, "test -d .")
				if err == nil && result.ExitCode != 0 {
					err = errors.New("a shell ran without its worktree")
				}
				if err != nil && !errors.Is(err, ErrWorktreeBusy) && !errors.Is(err, session.ErrNotFound) {
					mu.Lock()
					unexpected = append(unexpected, err)
					mu.Unlock()
				}
			}
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{})
		if err == nil {
			break
		}
		if !errors.Is(err, ErrWorktreeBusy) || time.Now().After(deadline) {
			close(stop)
			wg.Wait()
			t.Fatalf("delete: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if len(unexpected) > 0 {
		t.Fatalf("shell errors outside the contract: %v", unexpected)
	}
	if pathExists(identity.Path) || branchExists(root, identity.Branch) {
		t.Fatal("the worktree survived the deletion")
	}
}
