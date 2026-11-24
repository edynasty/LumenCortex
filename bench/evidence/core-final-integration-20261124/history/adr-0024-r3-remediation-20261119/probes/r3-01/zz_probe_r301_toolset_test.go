package toolset

// R3-01 remediation probe (throwaway; /tmp only): the model-facing write_file tool on a real
// filesystem named by LC_R301_ROOT.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestZZR301ToolsetRealFS(t *testing.T) {
	root := os.Getenv("LC_R301_ROOT")
	if root == "" {
		t.Skip("LC_R301_ROOT not set")
	}
	ws := filepath.Join(root, fmt.Sprintf("tws-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: ws, Policy: PolicyFull, SessionID: "s-r301"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, xerr := set.Execute(ctx, "write_file", map[string]any{"path": "new.txt", "content": "hello\n"}, nil)
	t.Logf("TOOL write_file(new.txt) ok=%v err=%v content=%s", res.OK, xerr, res.Content)
	res2, xerr2 := set.Execute(ctx, "write_file", map[string]any{"path": "keep.txt", "content": "edited\n"}, nil)
	got, _ := os.ReadFile(filepath.Join(ws, "keep.txt"))
	t.Logf("TOOL write_file(keep.txt) ok=%v err=%v keep.txt=%q", res2.OK, xerr2, got)
	entries, _ := os.ReadDir(ws)
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	t.Logf("WORKSPACE entries=%v", names)
}
