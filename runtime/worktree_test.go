package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initGitRepo(t *testing.T, root string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
}

func TestSessionWorktreeLifecycleAndAgentWorkspace(t *testing.T) {
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

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "isolated task"})
	if err != nil {
		t.Fatal(err)
	}
	local, err := engine.SessionRuntime(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if local.Kind != RuntimeLocal || local.Path != root {
		t.Fatalf("local=%#v", local)
	}

	identity, err := engine.AttachWorktree(context.Background(), handle.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Kind != RuntimeWorktree || identity.Path == root || identity.Branch == "" {
		t.Fatalf("identity=%#v", identity)
	}
	if _, err := os.Stat(filepath.Join(identity.Path, "README.md")); err != nil {
		t.Fatal(err)
	}
	agentPath, err := engine.agentWorkspace(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if agentPath != identity.Path {
		t.Fatalf("agent workspace=%q want=%q", agentPath, identity.Path)
	}

	if err := os.WriteFile(filepath.Join(identity.Path, "isolated.txt"), []byte("worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "isolated.txt")); !os.IsNotExist(err) {
		t.Fatalf("main workspace unexpectedly contains isolated file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(identity.Path, "README.md"), []byte("changed in worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionStatus, err := engine.SessionGitStatus(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionStatus.Files) == 0 {
		t.Fatal("expected worktree session git status")
	}
	sessionDiff, err := engine.SessionGitDiff(context.Background(), handle.ID, "README.md", false)
	if err != nil {
		t.Fatal(err)
	}
	if sessionDiff.Content == "" {
		t.Fatal("expected worktree session diff")
	}
	mainStatus, err := engine.GitStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(mainStatus.Files) != 0 {
		t.Fatalf("main workspace should remain clean: %#v", mainStatus.Files)
	}
	if _, err := handle.RunShell(context.Background(), "printf shell-isolated > shell.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(identity.Path, "shell.txt")); err != nil {
		t.Fatalf("worktree shell output missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "shell.txt")); !os.IsNotExist(err) {
		t.Fatalf("main workspace unexpectedly contains shell output: %v", err)
	}

	if err := engine.RemoveSessionWorktree(context.Background(), handle.ID, true); err != nil {
		t.Fatal(err)
	}
	after, err := engine.SessionRuntime(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Kind != RuntimeLocal || after.Path != root {
		t.Fatalf("after=%#v", after)
	}
	if _, err := os.Stat(identity.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists: %v", err)
	}
}

func TestManagedWorktreePathRejectsEscape(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.validateManagedWorktreePath(filepath.Join(filepath.Dir(engine.worktreeRoot), "escape")); err == nil {
		t.Fatal("expected managed worktree path escape rejection")
	}
}
