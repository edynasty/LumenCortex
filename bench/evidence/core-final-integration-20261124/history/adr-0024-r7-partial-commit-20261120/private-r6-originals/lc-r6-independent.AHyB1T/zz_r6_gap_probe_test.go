package editing

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAuditR6GapWriter(t *testing.T) {
	path := os.Getenv("LC_R6_GAP_PATH")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("FOREIGN-GAP\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditR6PreShrinkToTruncateGap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	original := []byte("old\n")
	next := []byte("longer-new\n")
	if err := os.WriteFile(path, next, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	id, err := captureFileIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(original); err != nil {
		t.Fatal(err)
	}
	if state, err := verifyPreShrinkForRollback(path, f, id, original, next); err != nil || state != "" {
		t.Fatalf("pre-shrink check unexpectedly refused: state=%q err=%v", state, err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestAuditR6GapWriter$")
	cmd.Env = append(os.Environ(), "LC_R6_GAP_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("independent writer failed: %v\n%s", err, out)
	}
	if err := f.Truncate(int64(len(original))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, original) || bytes.Contains(got, []byte("FOREIGN-GAP")) {
		t.Logf("gap consequence: append was removed by truncate; disk=%q", got)
		return
	}
	t.Fatalf("unexpected disk after gap sequence: %q", got)
}

func TestAuditR6GapProbeSanity(t *testing.T) {
	if _, err := os.Stat(t.TempDir()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
