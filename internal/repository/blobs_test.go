package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func blobWorkspace(t *testing.T) (*Service, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cite is a file of this workspace: a symlinked directory leading outside, a traversal, a directory
// and an oversized file get no blob (so a note cannot cite them, and a recorded cite reads as stale),
// while the regular file next to them is hashed.
func TestFileBlobsOnlyHashFilesInsideTheWorkspace(t *testing.T) {
	s, root := blobWorkspace(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "keys")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "a.go"), "package a\n")
	big, err := os.Create(filepath.Join(root, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(MaxBlobFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	big.Close()
	paths := []string{"a.go", "keys/secret.txt", "../outside.txt", "keys", "big.bin"}
	got, err := s.FileBlobs(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["a.go"]) != 40 {
		t.Fatalf("a.go blob %q", got["a.go"])
	}
	for _, p := range paths[1:] {
		if got[p] != "" {
			t.Fatalf("%s hashed: %q", p, got[p])
		}
	}
}

// A file that git cannot read does not fail the others: it gets no blob, the rest are hashed.
func TestFileBlobsOneUnreadableFileDoesNotFailTheRest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	s, root := blobWorkspace(t)
	write(t, filepath.Join(root, "a.go"), "package a\n")
	write(t, filepath.Join(root, "locked.go"), "package locked\n")
	if err := os.Chmod(filepath.Join(root, "locked.go"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "locked.go"), 0o644) })
	got, err := s.FileBlobs(context.Background(), []string{"locked.go", "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got["locked.go"] != "" || len(got["a.go"]) != 40 {
		t.Fatalf("got %v", got)
	}
}
