package editing

// Review probe (throwaway; /tmp only): rollback/overwrite behavior shared by main, a8b4786 and 7d064d0.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestZZR3RollbackRestoreVsForeign(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	_ = os.WriteFile(a, []byte("A0\n"), 0o644)
	_ = os.WriteFile(b, []byte("B0\n"), 0o644)
	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	a1, b1 := []byte("A1\n"), []byte("B1\n")
	e.beforeWrite = func(path string) {
		if strings.HasSuffix(path, "b.txt") {
			f, _ := os.OpenFile(a, os.O_WRONLY|os.O_APPEND, 0)
			_, _ = f.Write([]byte("FOREIGN\n"))
			_ = f.Close()
			_ = os.WriteFile(b, []byte("B-foreign\n"), 0o644)
		}
	}
	_, cerr := e.Commit(context.Background(), Plan{Actor: Actor{Session: "s"}, Tool: "apply_patch",
		Changes: []FileChange{{Path: "a.txt", BaseRev: RevOf([]byte("A0\n")), Content: &a1}, {Path: "b.txt", BaseRev: RevOf([]byte("B0\n")), Content: &b1}}})
	ga, _ := os.ReadFile(a)
	gb, _ := os.ReadFile(b)
	state := "LOST (rollback restored A0 over it)"
	if strings.Contains(string(ga), "FOREIGN") {
		state = "PRESERVED"
	}
	t.Logf("rollback of overwritten a.txt after foreign append: commit err=%v | a.txt=%q -> foreign append %s | b.txt=%q", cerr, ga, state, gb)
}

func TestZZR3ENOSPC(t *testing.T) {
	fsroot := os.Getenv("LC_R3_SMALLFS")
	if fsroot == "" {
		t.Skip("set LC_R3_SMALLFS")
	}
	root, err := os.MkdirTemp(fsroot, "ws-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	orig := bytes.Repeat([]byte("original line\n"), 1000)
	exist := filepath.Join(root, "exist.txt")
	if err := os.WriteFile(exist, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	filler := filepath.Join(fsroot, "filler-"+filepath.Base(root))
	ff, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<10)
	total := int64(0)
	for {
		n, werr := ff.Write(chunk)
		total += int64(n)
		if werr != nil {
			if !errors.Is(werr, syscall.ENOSPC) {
				t.Logf("filler stopped with %v", werr)
			}
			break
		}
	}
	_ = ff.Truncate(total - 320<<10)
	_ = ff.Close()
	defer os.Remove(filler)
	big := bytes.Repeat([]byte("new content line that is long enough\n"), 28000)
	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, cerr := e.Commit(context.Background(), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
		Changes: []FileChange{{Path: "new.txt", Content: &big, AllowCreate: true}}})
	_, nerr := os.Stat(filepath.Join(root, "new.txt"))
	ents, _ := os.ReadDir(root)
	var names []string
	for _, d := range ents {
		names = append(names, d.Name())
	}
	t.Logf("ENOSPC create (%d bytes, ~320KiB free): err=%v | new.txt statErr=%v | entries=%v", len(big), cerr, nerr, names)
	_, cerr = e.Commit(context.Background(), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
		Changes: []FileChange{{Path: "exist.txt", BaseRev: RevOf(orig), Content: &big}}})
	got, _ := os.ReadFile(exist)
	state := "INTACT"
	if !bytes.Equal(got, orig) {
		state = "DESTROYED"
	}
	t.Logf("ENOSPC overwrite of existing %d-byte file: err=%v | exist.txt now %d bytes, original %s, prefixIsNew=%v", len(orig), cerr, len(got), state, bytes.HasPrefix(big, got))
}
