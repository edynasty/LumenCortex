package toolset

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadOnlyPolicyHidesMutationTools(t *testing.T) {
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range set.Specs(nil) {
		if spec.Permission != "read" {
			t.Fatalf("read-only exposed %s (%s)", spec.Name, spec.Permission)
		}
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.resolve("../escape.txt", true); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege varies on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: root, Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.resolve("outside/escape.txt", true); err == nil {
		t.Fatal("expected symlink escape rejection")
	}
}

func TestApplyPatchValidatesBeforeMutation(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	if err := os.WriteFile(a, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: root, Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	_, err = set.Execute(context.Background(), "apply_patch", map[string]any{
		"patches": []map[string]any{
			{"path": "a.txt", "operation": "update", "edits": []map[string]any{{"old_text": "alpha", "new_text": "changed"}}},
			{"path": "b.txt", "operation": "update", "edits": []map[string]any{{"old_text": "missing", "new_text": "x"}}},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected validation failure")
	}
	raw, _ := os.ReadFile(a)
	if string(raw) != "alpha" {
		t.Fatalf("first file mutated before validation completed: %q", raw)
	}
}

func TestReplaceRejectsAmbiguousText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x x"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: root, Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	_, err = set.Execute(context.Background(), "replace_in_file", map[string]any{
		"path": "a.txt", "old_text": "x", "new_text": "y",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err=%v", err)
	}
}
