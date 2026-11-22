package toolset

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileCreateRaceForeignFilePreserved solidifies the F-01 toolset probe: when an external
// process creates the target file between path validation and (*Set).Open during write_file or
// apply_patch create, the tool refuses cleanly without deleting or mutating the foreign file.
func TestWriteFileCreateRaceForeignFilePreserved(t *testing.T) {
	t.Run("write_file", func(t *testing.T) {
		root := t.TempDir()
		set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "gen"), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "gen", "out.txt")
		external := []byte("EXTERNAL WRITER CONTENT\n")
		fired := false
		testHookBeforeOpen = func(path string) {
			if fired || filepath.Base(path) != "out.txt" {
				return
			}
			fired = true
			if err := os.WriteFile(path, external, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		defer func() { testHookBeforeOpen = nil }()

		result, execErr := set.Execute(context.Background(), "write_file", map[string]any{
			"path":    "gen/out.txt",
			"content": "agent content\n",
		}, nil)
		if !fired {
			t.Fatalf("testHookBeforeOpen did not fire")
		}
		if execErr != nil {
			t.Fatalf("unexpected protocol error: %v", execErr)
		}
		if result.OK {
			t.Fatalf("expected write_file to be refused on concurrent create")
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("foreign file destroyed after write_file refusal: %v", readErr)
		}
		if string(got) != string(external) {
			t.Fatalf("foreign file content mutated: got %q, want %q", got, external)
		}
		if result.MutatesWorkspace {
			t.Fatalf("expected MutatesWorkspace=false when O_EXCL refused before touching disk")
		}
	})

	t.Run("apply_patch_add_file", func(t *testing.T) {
		root := t.TempDir()
		set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "gen"), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "gen", "patch_out.txt")
		external := []byte("EXTERNAL PATCH RACE\n")
		fired := false
		testHookBeforeOpen = func(path string) {
			if fired || filepath.Base(path) != "patch_out.txt" {
				return
			}
			fired = true
			if err := os.WriteFile(path, external, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		defer func() { testHookBeforeOpen = nil }()

		result, execErr := set.Execute(context.Background(), "apply_patch", map[string]any{
			"patches": []map[string]any{
				{"path": "gen/patch_out.txt", "operation": "create", "content": "agent line\n"},
			},
		}, nil)
		if !fired {
			t.Fatalf("testHookBeforeOpen did not fire")
		}
		if execErr != nil {
			t.Fatalf("unexpected protocol error: %v", execErr)
		}
		if result.OK {
			t.Fatalf("expected apply_patch to be refused on concurrent create")
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("foreign file destroyed after apply_patch refusal: %v", readErr)
		}
		if string(got) != string(external) {
			t.Fatalf("foreign file content mutated: got %q, want %q", got, external)
		}
		if result.MutatesWorkspace {
			t.Fatalf("expected MutatesWorkspace=false when O_EXCL refused before touching disk")
		}
	})
}
