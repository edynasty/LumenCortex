package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorktreeConflictsDetectOverlappingChangedFiles(t *testing.T) {
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

	first, err := engine.NewSession(context.Background(), SessionOptions{Goal: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.NewSession(context.Background(), SessionOptions{Goal: "second"})
	if err != nil {
		t.Fatal(err)
	}

	firstRuntime, err := engine.AttachWorktree(context.Background(), first.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	secondRuntime, err := engine.AttachWorktree(context.Background(), second.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	defer engine.RemoveSessionWorktree(context.Background(), first.ID, true)
	defer engine.RemoveSessionWorktree(context.Background(), second.ID, true)

	for _, path := range []string{firstRuntime.Path, secondRuntime.Path} {
		if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	conflicts, err := engine.WorktreeConflicts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts=%#v", conflicts)
	}
	if conflicts[0].Path != "README.md" || len(conflicts[0].Owners) != 2 {
		t.Fatalf("conflict=%#v", conflicts[0])
	}

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("main changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conflicts, err = engine.WorktreeConflicts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || len(conflicts[0].Owners) != 3 {
		t.Fatalf("conflicts with main=%#v", conflicts)
	}
}
