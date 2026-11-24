//go:build !windows

package editing

// Independent R5 state/fault consumer-invariant probes (audit only; not part of the product tree).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func probeLimit(t *testing.T, cur uint64) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	lim.Cur = cur
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
}

func probeDump(t *testing.T, tag string, root string, names []string, res Result, rec CommitReceipt, err error) {
	t.Logf("[%s] err=%v", tag, err)
	t.Logf("[%s] code=%q isEFBIG=%v", tag, CodeOf(err), errors.Is(err, syscall.EFBIG))
	t.Logf("[%s] receipt committed=%v dirty=%v state=%s", tag, rec.Committed, rec.DirtyFailure, rec.State)
	for _, f := range rec.Files {
		t.Logf("[%s]  rf %s state=%s staged=%v pub=%v ver=%v rb=%v wrote=%v created=%v deleted=%v new=%q",
			tag, f.Path, f.PublishState, f.Staged, f.Published, f.Verified, f.RolledBack, f.Wrote, f.Created, f.Deleted, f.NewSHA256)
	}
	t.Logf("[%s] result.CommitState=%s files=%d", tag, res.CommitState, len(res.Files))
	for _, f := range res.Files {
		t.Logf("[%s]  res %s state=%s pub=%v rb=%v changed=%v newRev=%q", tag, f.Path, f.PublishState, f.Published, f.RolledBack, f.Changed, f.NewRev)
	}
	for _, n := range names {
		b, rerr := os.ReadFile(filepath.Join(root, n))
		if rerr != nil {
			t.Logf("[%s]  disk %s: %v", tag, n, rerr)
			continue
		}
		prefix := b
		if len(prefix) > 16 {
			prefix = prefix[:16]
		}
		t.Logf("[%s]  disk %s len=%d prefix=%q", tag, n, len(b), prefix)
	}
}

// H1: partial RLIMIT_FSIZE during writeRestoreConditional of an existing larger original.
func TestProbeH1PartialFSizeRestore(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	var orig syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig)
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) }()
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "b.txt", strings.Repeat("O", 20000))
	writeFile(t, root, "c.txt", "old-c\n")
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), filepath.Join(root, "b.txt"), RevOf([]byte(strings.Repeat("O", 20000))))
	e.beforeWrite = func(p string) {
		if filepath.Base(p) == "c.txt" {
			_ = os.WriteFile(p, []byte("foreign-c\n"), 0o644)
		}
	}
	e.SetTestFaultHooks(nil, func(p string) error {
		switch filepath.Base(p) {
		case "b.txt":
			probeLimit(t, 8192)
		case "a.txt":
			_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
		}
		return nil
	})
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
	res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &na}, {Path: "b.txt", Content: &nb}, {Path: "c.txt", Content: &nc}}})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
	probeDump(t, "H1", root, []string{"a.txt", "b.txt", "c.txt"}, res, rec, err)
	t.Logf("[H1] snapshot b latest=%q", e.LatestRev(testActor(), filepath.Join(root, "b.txt")))
}

// H2: partial RLIMIT_FSIZE during restoreDeletedAtRollback.
func TestProbeH2PartialFSizeDeleteRestore(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	var orig syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig)
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) }()
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "d.txt", strings.Repeat("D", 20000))
	writeFile(t, root, "z.txt", "old-z\n")
	e := newTestEngine(t, root)
	e.beforeWrite = func(p string) {
		if filepath.Base(p) == "z.txt" {
			_ = os.WriteFile(p, []byte("foreign-z\n"), 0o644)
		}
	}
	e.SetTestFaultHooks(nil, func(p string) error {
		switch filepath.Base(p) {
		case "d.txt":
			probeLimit(t, 8192)
		case "a.txt":
			_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
		}
		return nil
	})
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	na, nz := []byte("new-a\n"), []byte("new-z\n")
	res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &na}, {Path: "d.txt", Delete: true}, {Path: "z.txt", Content: &nz}}})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
	probeDump(t, "H2", root, []string{"a.txt", "d.txt", "z.txt"}, res, rec, err)
}

// H4: create in a new parent directory, then conflict; created file rolled back cleanly.
func TestProbeH4CreatedParentClassification(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "c.txt", "old-c\n")
	e := newTestEngine(t, root)
	e.beforeWrite = func(p string) {
		if filepath.Base(p) == "c.txt" {
			_ = os.WriteFile(p, []byte("foreign-c\n"), 0o644)
		}
	}
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	nb, nc := []byte("new-b\n"), []byte("new-c\n")
	res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "a_dir/b.txt", Content: &nb, Create: true}, {Path: "c.txt", Content: &nc}}})
	probeDump(t, "H4", root, []string{"a_dir/b.txt", "c.txt"}, res, rec, err)
	_, serr := os.Stat(filepath.Join(root, "a_dir"))
	t.Logf("[H4] a_dir stat err=%v", serr)
}

// probeBinaryHost is a custom Host that is not LocalHost/AtomicFileStager and implements ReplaceFile.
type probeBinaryHost struct {
	LocalHost
	failRollbackFor string
	calls           *[]string
}

func (h probeBinaryHost) ReplaceFile(path string, before, after []byte, existed bool) error {
	*h.calls = append(*h.calls, fmt.Sprintf("%s existed=%v before=%q after=%q", filepath.Base(path), existed, before, after))
	if filepath.Base(path) == h.failRollbackFor && bytes.Equal(after, []byte("old-a")) {
		return syscall.EIO
	}
	cur, err := os.ReadFile(path)
	if existed {
		if err != nil || !bytes.Equal(cur, before) {
			return errors.New("probe CAS mismatch")
		}
		return os.WriteFile(path, after, 0o644)
	}
	if err == nil {
		return os.ErrExist
	}
	return os.WriteFile(path, after, 0o644)
}

// H5: custom Host binary ReplaceFile rollback (existing binary updated, created binary, then conflict).
func TestProbeH5BinaryReplaceFileRollback(t *testing.T) {
	for _, fail := range []string{"", "a.bin"} {
		root := t.TempDir()
		writeFile(t, root, "a.bin", "old-a")
		writeFile(t, root, "c.txt", "old-c\n")
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		var calls []string
		host := probeBinaryHost{LocalHost: lh, failRollbackFor: fail, calls: &calls}
		e := newTestEngine(t, root)
		e.beforeWrite = func(p string) {
			if filepath.Base(p) == "c.txt" {
				_ = os.WriteFile(p, []byte("foreign-c\n"), 0o644)
			}
		}
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		na, nb, nc := []byte("new-a"), []byte("new-b"), []byte("new-c\n")
		res, cerr := e.Commit(ctx, Plan{Host: host, Actor: testActor(), Tool: "probe", Changes: []FileChange{
			{Path: "a.bin", Content: &na, Binary: true},
			{Path: "b.bin", Content: &nb, Binary: true, Create: true, AllowCreate: true},
			{Path: "c.txt", Content: &nc}}})
		tag := "H5fail=" + fail
		probeDump(t, tag, root, []string{"a.bin", "b.bin", "c.txt"}, res, rec, cerr)
		for _, c := range calls {
			t.Logf("[%s] ReplaceFile %s", tag, c)
		}
	}
}

// H6: PRE-01 attribution in a multi-file plan: in-place overwrite of existing c.txt fails at a
// partial RLIMIT_FSIZE after Truncate(0); a.txt is rolled back.
func TestProbeH6Pre01MultiFile(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	var orig syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig)
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) }()
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "c.txt", strings.Repeat("C", 20000))
	e := newTestEngine(t, root)
	e.beforeWrite = func(p string) {
		if filepath.Base(p) == "c.txt" {
			probeLimit(t, 8192)
		}
	}
	e.SetTestFaultHooks(nil, func(p string) error {
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
		return nil
	})
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	na, nc := []byte("new-a\n"), []byte(strings.Repeat("N", 20000))
	res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &na}, {Path: "c.txt", Content: &nc}}})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
	probeDump(t, "H6", root, []string{"a.txt", "c.txt"}, res, rec, err)
}
