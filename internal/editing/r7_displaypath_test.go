package editing

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR7DisplayPathsStructuredAndFreeText covers the shared R7 path scrubber: structured OS errors
// are rewritten field by field (spaces survive), joins keep every cause, free text handles quoted
// paths and Windows/UNC spellings, and no workspace root or real root leaks.
func TestR7DisplayPathsStructuredAndFreeText(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "my ws root")
	outside := filepath.Join(base, "my ws root-sibling")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(root, "pkg", "inside.go")
	if err := os.WriteFile(inside, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(outside, "pkg", "secret.txt")
	if err := os.WriteFile(outFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("structured_path_error_with_spaces", func(t *testing.T) {
		p := &prepared{abs: inside, rel: "pkg/inside.go", key: canonicalPath(inside)}
		got := e.sanitizeEngineError(p, &os.PathError{Op: "open", Path: inside, Err: os.ErrPermission})
		if strings.Contains(got, root) || strings.Contains(got, base) {
			t.Fatalf("host root leaked: %q", got)
		}
		if !strings.Contains(got, "pkg/inside.go") || !strings.Contains(got, "permission denied") {
			t.Fatalf("structured meaning lost: %q", got)
		}
	})

	t.Run("sibling_path_error_spaces_keeps_basename_only", func(t *testing.T) {
		p := &prepared{abs: inside, rel: "pkg/inside.go", key: canonicalPath(inside)}
		got := e.sanitizeEngineError(p, &os.PathError{Op: "stat", Path: outFile, Err: os.ErrNotExist})
		if strings.Contains(got, root) || strings.Contains(got, base) {
			t.Fatalf("host root leaked: %q", got)
		}
		if !strings.Contains(got, "<outside-workspace>/secret.txt") {
			t.Fatalf("outside sibling not fail-closed: %q", got)
		}
	})

	t.Run("link_error_old_and_new_are_separate", func(t *testing.T) {
		p := &prepared{abs: inside, rel: "pkg/inside.go", key: canonicalPath(inside)}
		got := e.sanitizeEngineError(p, &os.LinkError{Op: "rename", Old: inside, New: outFile, Err: os.ErrPermission})
		if strings.Contains(got, root) || strings.Contains(got, base) {
			t.Fatalf("host root leaked: %q", got)
		}
		if !strings.Contains(got, "pkg/inside.go") || !strings.Contains(got, "<outside-workspace>/secret.txt") {
			t.Fatalf("link fields lost: %q", got)
		}
	})

	t.Run("joined_errors_keep_every_cause", func(t *testing.T) {
		joined := errors.Join(
			&os.PathError{Op: "write", Path: inside, Err: os.ErrPermission},
			&os.PathError{Op: "close", Path: outFile, Err: errors.New("close failed")},
		)
		got := e.sanitizeEngineError(nil, joined)
		if strings.Contains(got, root) || strings.Contains(got, base) {
			t.Fatalf("host root leaked: %q", got)
		}
		for _, want := range []string{"write pkg/inside.go:", "close <outside-workspace>/secret.txt:", "close failed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("joined cause %q lost in %q", want, got)
			}
		}
	})

	t.Run("free_text_quoted_and_windows_unc", func(t *testing.T) {
		msg := `open "` + inside + `": denied; sibling ` + outFile + `; win C:\Users\Alice\Repo-sibling\key.pem; unc \\srv\share\cred.db; url https://example.com/a/b`
		got := e.sanitizeHostPathsInText(msg)
		if strings.Contains(got, root) || strings.Contains(got, base) || strings.Contains(got, ".-sibling") {
			t.Fatalf("host root leaked or sibling corrupted: %q", got)
		}
		for _, want := range []string{
			`open "pkg/inside.go": denied`,
			"<outside-workspace>/secret.txt",
			"<outside-workspace>/key.pem",
			"<outside-workspace>/cred.db",
			"https://example.com/a/b",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("missing %q in %q", want, got)
			}
		}
	})

	t.Run("file_url_path_is_scrubbed", func(t *testing.T) {
		got := e.sanitizeHostPathsInText(`go to "file://` + inside + `" now`)
		if strings.Contains(got, root) || strings.Contains(got, base) {
			t.Fatalf("file URL leaked host root: %q", got)
		}
		if !strings.Contains(got, "file://pkg/inside.go") {
			t.Fatalf("file URL target not rewritten: %q", got)
		}
	})
}

// TestR7DiagnosticPathFailsClosed covers diagnostic relativization before a clean receipt exists:
// relative escapes and unresolvable symlinks become the outside placeholder, and the workspace root
// itself is ".".
func TestR7DiagnosticPathFailsClosed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "my ws")
	outsideDir := filepath.Join(base, "my ws-sibling")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	// A dangling symlink inside the workspace: resolution fails, so the display fails closed.
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(outsideDir, "missing"), dangling); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"workspace root", root, "."},
		{"relative inside", "pkg/inside.go", "pkg/inside.go"},
		{"relative dotdot", "../my ws-sibling/secret.txt", "<outside-workspace>/secret.txt"},
		{"absolute inside nonexistent", filepath.Join(root, "pkg/future/new.go"), "pkg/future/new.go"},
		{"absolute sibling with space", filepath.Join(outsideDir, "secret.txt"), "<outside-workspace>/secret.txt"},
		{"dangling symlink", filepath.Join(dangling, "x.go"), "<outside-workspace>/x.go"},
		{"windows outside", `C:\Users\alice\secret\token.json`, "<outside-workspace>/token.json"},
		{"unc outside", `\\corp\share\payroll.csv`, "<outside-workspace>/payroll.csv"},
	}
	for _, tc := range cases {
		if got := e.relativizeDiagnosticPath(tc.in, nil); got != tc.want {
			t.Fatalf("%s: relativizeDiagnosticPath(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
