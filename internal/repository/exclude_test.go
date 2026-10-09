package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
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
	return root
}

func readGitExclude(t *testing.T, repo string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertGitClean(t *testing.T, dir string) {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
		t.Fatalf("workspace should stay clean: %q", trimmed)
	}
}

// F3: a workspace reached through a symlink (macOS default TMPDIR:
// /var → /private/var) must still get its .lumencortex exclude entry.
// git resolves symlinks when reporting the repository root, while the
// target keeps the symlinked prefix, so filepath.Rel used to return ".."
// and no pattern was written.
func TestEnsureLocalExcludeThroughSymlinkedWorkspacePath(t *testing.T) {
	repo := initGitRepo(t)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	service, err := New(link)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(link, ".lumencortex")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureLocalExclude(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if exclude := readGitExclude(t, repo); !strings.Contains(exclude, "/.lumencortex/") {
		t.Fatalf("exclude missing pattern: %q", exclude)
	}
	assertGitClean(t, link)
}

func TestEnsureLocalExcludePlainPathStillWorks(t *testing.T) {
	repo, err := filepath.EvalSymlinks(initGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(repo)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(repo, ".lumencortex")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureLocalExclude(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if exclude := readGitExclude(t, repo); !strings.Contains(exclude, "/.lumencortex/") {
		t.Fatalf("exclude missing pattern: %q", exclude)
	}
	assertGitClean(t, repo)
}

func TestEnsureLocalExcludeOutsideRepoWritesNothing(t *testing.T) {
	repo := initGitRepo(t)
	service, err := New(repo)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	before := readGitExclude(t, repo)
	if err := service.EnsureLocalExclude(context.Background(), outside); err != nil {
		t.Fatal(err)
	}
	if after := readGitExclude(t, repo); after != before {
		t.Fatalf("exclude must not change for a target outside the repo: before %q after %q", before, after)
	}
}
