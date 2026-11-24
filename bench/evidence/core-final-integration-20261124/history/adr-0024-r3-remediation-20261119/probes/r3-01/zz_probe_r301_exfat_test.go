package editing

// R3-01 remediation probe (throwaway; /tmp only): engine create + existing-file edit on a real
// filesystem named by LC_R301_ROOT. Compiles unchanged on 2ba79ae and on the remediation tree.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestZZR301RealFS(t *testing.T) {
	root := os.Getenv("LC_R301_ROOT")
	if root == "" {
		t.Skip("LC_R301_ROOT not set")
	}
	ws := filepath.Join(root, fmt.Sprintf("ws-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fresh := []byte("hello\n")
	_, cerr := e.Commit(ctx, Plan{Actor: Actor{Session: "s", Run: "r"}, Tool: "write_file", Changes: []FileChange{{Path: "new.txt", Content: &fresh, AllowCreate: true}}})
	t.Logf("CREATE err=%v", cerr)
	t.Logf("CREATE code=%q", CodeOf(cerr))
	updated := []byte("edited\n")
	_, eerr := e.Commit(ctx, Plan{Actor: Actor{Session: "s", Run: "r"}, Tool: "write_file", Changes: []FileChange{{Path: "keep.txt", Content: &updated, AllowCreate: true}}})
	got, _ := os.ReadFile(filepath.Join(ws, "keep.txt"))
	t.Logf("EDIT existing err=%v keep.txt=%q", eerr, got)
	entries, _ := os.ReadDir(ws)
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	t.Logf("WORKSPACE entries=%v", names)
}
