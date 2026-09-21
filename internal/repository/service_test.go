package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindFilesAndFallbackSearchAreBounded(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package demo\n// needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package demo\n// needle again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := service.FindFiles(context.Background(), "*.go", ".", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Paths) != 1 || !files.Truncated {
		t.Fatalf("files=%#v", files)
	}

	result, err := service.searchFallback(context.Background(), "needle", root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || !result.Truncated {
		t.Fatalf("search=%#v", result)
	}
}

func TestGitStatusAndDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
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
	path := filepath.Join(root, "demo.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "demo.txt")
	run("commit", "-m", "initial")
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.GitStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Files) != 1 || status.Files[0].Path != "demo.txt" {
		t.Fatalf("status=%#v", status)
	}
	diff, err := service.GitDiff(context.Background(), "demo.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Content, "-before") || !strings.Contains(diff.Content, "+after") {
		t.Fatalf("diff=%q", diff.Content)
	}
}

func TestRepositoryPathTraversalRejected(t *testing.T) {
	service, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FindFiles(context.Background(), "*", "../", 10); err == nil {
		t.Fatal("expected traversal rejection")
	}
}
