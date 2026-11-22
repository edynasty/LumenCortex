package editing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestR6ExternalWriterProcessHelper(t *testing.T) {
	mode := os.Getenv("LC_R6_WRITER_MODE")
	if mode == "" {
		return
	}
	path := os.Getenv("LC_R6_WRITER_PATH")
	data := []byte(os.Getenv("LC_R6_WRITER_DATA"))
	switch mode {
	case "append":
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	case "rewrite":
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(data, 0); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Truncate(int64(len(data))); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	case "truncate":
		if err := os.Truncate(path, 0); err != nil {
			t.Fatal(err)
		}
	case "replace":
		tmp := path + ".r6foreign"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown LC_R6_WRITER_MODE=%q", mode)
	}
}

func runR6ExternalWriter(t *testing.T, mode, path, data string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestR6ExternalWriterProcessHelper$")
	cmd.Env = append(os.Environ(),
		"LC_R6_WRITER_MODE="+mode,
		"LC_R6_WRITER_PATH="+path,
		"LC_R6_WRITER_DATA="+data,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external writer (%s) failed: %v\n%s", mode, err, out)
	}
}

// TestR6ValidateRelativizesDiagnosticsAndSiblingPaths keeps the diagnostic path-privacy case: a
// validation diagnostic from a sibling file must name a workspace-relative path, never the host
// root.
func TestR6ValidateRelativizesDiagnosticsAndSiblingPaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "pkg/helper.go", "package pkg\n\nfunc useVal() int { return sharedVal }\n")
	writeFile(t, root, "pkg/main.go", "package pkg\n\nvar sharedVal = 42\n")

	e := newTestEngine(t, root)
	broken := []byte("package pkg\n\nvar sharedVal = \"now-a-string\"\n")
	_, err := e.Commit(context.Background(), Plan{
		Actor:    testActor(),
		Tool:     "edit_lines",
		Changes:  []FileChange{{Path: "pkg/main.go", Content: &broken}},
		Validate: []Validator{GoSyntax(), GoTypes()},
	})
	editErr, ok := AsError(err)
	if !ok || editErr == nil {
		t.Fatalf("expected editing.Error from GoTypes, got %v", err)
	}
	if strings.Contains(editErr.Path, root) || filepath.IsAbs(editErr.Path) {
		t.Fatalf("editErr.Path leaked absolute host path: %q", editErr.Path)
	}
	if editErr.Path != "pkg/helper.go" {
		t.Fatalf("expected sibling diagnostic path pkg/helper.go, got %q", editErr.Path)
	}
	for _, d := range editErr.Diagnostics {
		if strings.Contains(d.Path, root) || filepath.IsAbs(d.Path) {
			t.Fatalf("Diagnostic.Path leaked absolute host path: %+v", d)
		}
	}
	if err != nil && strings.Contains(err.Error(), root) {
		t.Fatalf("refusal leaked host root: %v", err)
	}
}
