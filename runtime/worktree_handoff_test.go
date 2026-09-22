package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWorktreeHandoffPlanBlocksDirtyAndAppliesSafeCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	initGitRepo(t, root)

	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "handoff"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := engine.AttachWorktree(context.Background(), handle.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.RemoveSessionWorktree(context.Background(), handle.ID, true)

	plan, err := engine.WorktreeHandoffPlan(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CanApply || plan.BlockedReason != "worktree has no commits to apply" {
		t.Fatalf("empty plan=%#v", plan)
	}

	if err := os.WriteFile(filepath.Join(identity.Path, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err = engine.WorktreeHandoffPlan(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CanApply || !plan.SourceDirty {
		t.Fatalf("dirty source plan=%#v", plan)
	}

	runGitIn(t, identity.Path, "add", "feature.txt")
	runGitIn(t, identity.Path, "commit", "-m", "feature")
	plan, err = engine.WorktreeHandoffPlan(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CanApply || len(plan.Commits) != 1 || len(plan.SourceFiles) != 1 || plan.SourceFiles[0] != "feature.txt" {
		t.Fatalf("safe plan=%#v", plan)
	}

	result, err := engine.ApplySessionWorktree(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetHead == "" || result.TargetHead == plan.TargetHead {
		t.Fatalf("apply result=%#v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, "feature.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "feature\n" {
		t.Fatalf("feature=%q", data)
	}
	after, err := engine.WorktreeHandoffPlan(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CanApply || len(after.Commits) != 0 || after.BlockedReason != "worktree has no commits to apply" {
		t.Fatalf("post-apply plan=%#v", after)
	}
}

func TestWorktreeHandoffBlocksDirtyTargetAndOverlappingCommittedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	initGitRepo(t, root)

	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "overlap"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := engine.AttachWorktree(context.Background(), handle.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.RemoveSessionWorktree(context.Background(), handle.ID, true)

	if err := os.WriteFile(filepath.Join(identity.Path, "README.md"), []byte("worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, identity.Path, "add", "README.md")
	runGitIn(t, identity.Path, "commit", "-m", "worktree change")

	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := engine.WorktreeHandoffPlan(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CanApply || !plan.TargetDirty {
		t.Fatalf("dirty target plan=%#v", plan)
	}
	if err := os.Remove(filepath.Join(root, "dirty.txt")); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, root, "add", "README.md")
	runGitIn(t, root, "commit", "-m", "main change")

	plan, err = engine.WorktreeHandoffPlan(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.CanApply || len(plan.OverlappingFiles) != 1 || plan.OverlappingFiles[0] != "README.md" {
		t.Fatalf("overlap plan=%#v", plan)
	}
	if _, err := engine.ApplySessionWorktree(context.Background(), handle.ID); err == nil {
		t.Fatal("expected overlapping handoff to be rejected")
	}
}
