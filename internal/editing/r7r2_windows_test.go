//go:build windows

package editing

// Native Windows acceptance for R6-07 / R7R2-BASE-01 (ADR-0024). These tests settle whether the
// pre-close versus post-close LastWriteTime comparison in publishTempNoReplace and
// removeCreatedWhileOpen yields false uncertainty or leaves private staging files behind. They have
// not been executed natively; cross-compilation proves only that they build. Run them on each
// filesystem under test by pointing LC_R7R2_DIR at a directory on that volume:
//
//	set LC_R7R2_DIR=E:\probe
//	go test -count=1 -v -run "^TestR7R2Windows" ./internal/editing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func r7r2WindowsDir(t *testing.T) string {
	t.Helper()
	d := os.Getenv("LC_R7R2_DIR")
	if d == "" {
		return t.TempDir()
	}
	sub, err := os.MkdirTemp(d, "r7r2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sub) })
	return sub
}

// TestR7R2WindowsLastWriteTimeAcrossClose records whether CloseHandle moves LastWriteTime on a
// written handle. A mismatch is not itself a failure; the contention test below asserts its effect.
func TestR7R2WindowsLastWriteTimeAcrossClose(t *testing.T) {
	dir := r7r2WindowsDir(t)
	f, tmp, err := NewCreateTempFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	if _, err := f.Write([]byte("agent content\n")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	pre, err := captureFileIdentity(f)
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("preClose=%+v postCloseMatchesWithMtime=%v postCloseMatchesIdentityOnly=%v",
		pre.lastWriteTime, pathMatchesIdentity(tmp, pre, true), pathMatchesIdentity(tmp, pre, false))
	if !pathMatchesIdentity(tmp, pre, false) {
		t.Fatalf("file identity changed across CloseHandle")
	}
}

// TestR7R2WindowsPublishContentionCleansPrivateTemp: the destination appears before MoveFileExW.
// The foreign bytes must survive, and the refusal must be clean: not_published and no private
// .lcx-create-* file left behind.
func TestR7R2WindowsPublishContentionCleansPrivateTemp(t *testing.T) {
	dir := r7r2WindowsDir(t)
	dst := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(dst, []byte("FOREIGN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, tmp, err := NewCreateTempFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("agent content\n")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	out, perr := publishTempNoReplace(tmp, f, dst, PublishFaultHooks{})
	t.Logf("state=%s published=%v leftover=%v err=%v", out.state, out.published, out.tempLeftover, perr)
	if got, _ := os.ReadFile(dst); string(got) != "FOREIGN\n" {
		t.Fatalf("destination changed to %q", got)
	}
	if !errors.Is(perr, os.ErrExist) {
		t.Fatalf("want ErrExist, got %v", perr)
	}
	if out.state != PublishNone || out.tempLeftover {
		t.Errorf("false uncertainty: state=%s leftover=%v, want not_published without leftover", out.state, out.tempLeftover)
	}
	if _, err := os.Lstat(tmp); err == nil {
		_ = os.Remove(tmp)
		t.Errorf("private staging file left behind: %s", filepath.Base(tmp))
	}
}

// TestR7R2WindowsLongNameCreateVerifies: a destination name longer than the staging name, where FAT
// may assign a new file ID on rename. The create must still verify.
func TestR7R2WindowsLongNameCreateVerifies(t *testing.T) {
	root := r7r2WindowsDir(t)
	e := newTestEngine(t, root)
	name := strings.Repeat("n", 80) + ".txt"
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
		{Path: name, Content: bytesPtr("agent\n"), Create: true},
	}})
	fr, rr := r7Files(t, res, rec, name)
	if err != nil || fr.PublishState != PublishSucceededAndVerified || !fr.Verified || rr.NewSHA256 == "" {
		t.Fatalf("long-name create not verified: err=%v state=%s", err, fr.PublishState)
	}
}
