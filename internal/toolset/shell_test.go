package toolset

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}

// A shell call reports a workspace change only when one happened: reading commands do not, writes,
// edits to an already dirty file, untracked files, commits and failing commands that wrote do. Where
// the state cannot be read, every call counts as a change.
func TestShellReportsOnlyActualWorkspaceChanges(t *testing.T) {
	root := gitRepo(t)
	set, err := New(Options{Workspace: root, Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	run := func(command string) bool {
		t.Helper()
		result, err := set.Execute(context.Background(), "shell", map[string]any{"command": command}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result.MutatesWorkspace
	}
	for _, c := range []struct {
		command string
		changed bool
	}{
		{"git status && git log --oneline && cat a.txt", false},
		{"echo b >> a.txt", true},
		{"git diff", false},
		{"echo c >> a.txt", true}, // a.txt was already dirty
		{"mkdir -p d && echo x > d/new.txt", true},
		{"ls -R", false},
		{"git add -A && git commit -qm next", true},
		{"echo y > a.txt; exit 3", true},
		{"exit 1", false},
		{"echo ignored > out.log && printf 'out.log\\n' >> .git/info/exclude", false},
	} {
		if got := run(c.command); got != c.changed {
			t.Errorf("%q: changed=%v, want %v", c.command, got, c.changed)
		}
	}

	plain, err := New(Options{Workspace: t.TempDir(), Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := plain.Execute(context.Background(), "shell", map[string]any{"command": "true"}, nil); err != nil || !result.MutatesWorkspace {
		t.Fatalf("outside git a command must count as a change: %+v err=%v", result, err)
	}
}
