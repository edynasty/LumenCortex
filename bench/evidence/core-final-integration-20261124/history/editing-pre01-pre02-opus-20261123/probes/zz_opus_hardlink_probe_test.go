//go:build darwin

package editing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpusHardlinkMatrix(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(base, "ws")
	for _, d := range []string{"a", "b", "c"} {
		_ = os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	orig := []byte("shared-v1\n")
	p1 := filepath.Join(root, "a", "x.txt")
	_ = os.WriteFile(p1, orig, 0o644)
	p2, p3, outside := filepath.Join(root, "b", "y.txt"), filepath.Join(root, "c", "z.txt"), filepath.Join(base, "outside.txt")
	for _, l := range []string{p2, p3, outside} {
		if err := os.Link(p1, l); err != nil {
			t.Fatal(err)
		}
	}
	e := newTestEngine(t, root)
	v2 := []byte("shared-v2\n")
	_, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{{Path: "a/x.txt", Content: &v2}}})
	t.Logf("single update err=%v", err)
	for _, l := range []string{p1, p2, p3, outside} {
		b, _ := os.ReadFile(l)
		i1, _ := os.Stat(p1)
		il, _ := os.Stat(l)
		t.Logf("HL alias %s = %q same=%v", filepath.Base(l), b, os.SameFile(i1, il))
	}
	// Two aliases of one inode in one batch.
	v3a, v3b := []byte("v3-from-a\n"), []byte("v3-from-b\n")
	_, err = e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{{Path: "a/x.txt", Content: &v3a}, {Path: "b/y.txt", Content: &v3b}}})
	t.Logf("HL batch err=%v", err)
	for _, l := range []string{p1, p2, p3, outside} {
		b, _ := os.ReadFile(l)
		t.Logf("HL batch alias %s = %q", filepath.Base(l), b)
	}
}
