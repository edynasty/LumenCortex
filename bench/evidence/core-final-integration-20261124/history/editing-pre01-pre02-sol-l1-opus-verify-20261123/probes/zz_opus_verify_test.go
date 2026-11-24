//go:build darwin || linux

package editing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Independent Opus verification probes for SOL-PRE-L1-01/02. Not part of the repository.

func TestZZOpusVerifyLinkChild(t *testing.T) {
	src := os.Getenv("ZZ_OPUS_LINK_SRC")
	if src == "" {
		return
	}
	if err := os.Link(src, os.Getenv("ZZ_OPUS_LINK_DST")); err != nil {
		t.Fatalf("child link: %v", err)
	}
}

func zzChildLink(t *testing.T, src, dst string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestZZOpusVerifyLinkChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ZZ_OPUS_LINK_SRC="+src, "ZZ_OPUS_LINK_DST="+dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child link failed: %v %s", err, out)
	}
	return cmd.ProcessState.Pid()
}

type zzObs struct {
	ino   uint64
	nlink uint64
	data  string
}

func zzObserve(t *testing.T, path string) zzObs {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st := info.Sys().(*syscall.Stat_t)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return zzObs{ino: uint64(st.Ino), nlink: uint64(st.Nlink), data: string(b)}
}

func zzLCXEntries(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".lcx-") {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	return out
}

func zzRoot(t *testing.T) (base, root string) {
	t.Helper()
	b, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(b, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return b, root
}

func zzSwapCounter(t *testing.T, inner func(src, dst string) error) *int {
	t.Helper()
	saved := renameSwapForPublish
	calls := 0
	renameSwapForPublish = func(src, dst string) error {
		calls++
		if inner != nil {
			return inner(src, dst)
		}
		return saved(src, dst)
	}
	t.Cleanup(func() { renameSwapForPublish = saved })
	return &calls
}

func zzCommit(t *testing.T, e *Engine, changes ...FileChange) (Result, CommitReceipt, error) {
	t.Helper()
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	done := make(chan struct{})
	var res Result
	var err error
	go func() {
		defer close(done)
		res, err = e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: changes})
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("commit did not return within 20s (stripe lock leak or deadlock)")
	}
	return res, rec, err
}

func zzRow(t *testing.T, res Result, rec CommitReceipt, rel string) (FileResult, FileCommitReceipt) {
	t.Helper()
	var fr *FileResult
	for i := range res.Files {
		if res.Files[i].Path == rel {
			fr = &res.Files[i]
		}
	}
	var rr *FileCommitReceipt
	for i := range rec.Files {
		if rec.Files[i].Path == rel {
			rr = &rec.Files[i]
		}
	}
	if fr == nil || rr == nil {
		t.Fatalf("row %s missing: res=%+v rec=%+v", rel, res.Files, rec.Files)
	}
	return *fr, *rr
}

func zzAssertRefused(t *testing.T, res Result, rec CommitReceipt, err error, rel string, wantState PublishState, wantCode string) {
	t.Helper()
	fr, rr := zzRow(t, res, rec, rel)
	if wantCode != "" && CodeOf(err) != wantCode {
		t.Errorf("%s: code=%q want %q (err=%v)", rel, CodeOf(err), wantCode, err)
	}
	if rec.Committed || res.CommitState == CommitStateCommitted {
		t.Errorf("%s: refusal reported committed: res=%s rec=%+v", rel, res.CommitState, rec)
	}
	if fr.PublishState != wantState || rr.PublishState != wantState {
		t.Errorf("%s: publishState result=%s receipt=%s want %s", rel, fr.PublishState, rr.PublishState, wantState)
	}
	if fr.Published || fr.Verified || rr.Published || rr.Verified || rr.Wrote || fr.NewRev != "" || rr.NewSHA256 != "" {
		t.Errorf("%s: trusted publication fields on refusal: %+v %+v", rel, fr, rr)
	}
}

// L1-01: independent child creates the alias after staging completed, before final validation.
func TestZZOpusVerifyLateLinkBeforeFinalCheck(t *testing.T) {
	base, root := zzRoot(t)
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("orig-bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "outside-alias.txt")
	before := zzObserve(t, target)
	if before.nlink != 1 {
		t.Fatalf("setup nlink=%d", before.nlink)
	}
	calls := zzSwapCounter(t, nil)
	e := newTestEngine(t, root)
	var stagedSeen string
	childPID := 0
	e.SetPublishFaultHooks(PublishFaultHooks{
		WriteStaged: func(_ string, f *os.File, data []byte) (int, error) {
			stagedSeen = f.Name()
			return f.Write(data)
		},
		BeforePublish: func(string) error {
			b, err := os.ReadFile(stagedSeen)
			if err != nil || string(b) != "agent-next\n" {
				t.Errorf("staging incomplete at barrier: %q %v", b, err)
			}
			childPID = zzChildLink(t, target, alias)
			if zzObserve(t, target).nlink != 2 {
				t.Errorf("child link not visible at barrier")
			}
			return nil
		},
	})
	res, rec, err := zzCommit(t, e, FileChange{Path: "f.txt", Content: bytesPtr("agent-next\n")})
	t.Logf("childPID=%d swapCalls=%d err=%v state=%s", childPID, *calls, err, res.CommitState)
	if childPID == 0 {
		t.Fatal("barrier never reached")
	}
	if *calls != 0 {
		t.Errorf("swap invoked %d times after late link", *calls)
	}
	zzAssertRefused(t, res, rec, err, "f.txt", PublishUnsupported, CodeUnsupportedFilesystem)
	if rec.DirtyFailure || res.CommitState != CommitStatePrePublishFailure {
		t.Errorf("not a clean pre-publication failure: dirty=%v state=%s", rec.DirtyFailure, res.CommitState)
	}
	ta, aa := zzObserve(t, target), zzObserve(t, alias)
	if ta.ino != before.ino || aa.ino != before.ino || ta.data != "orig-bytes\n" || aa.data != "orig-bytes\n" || ta.nlink != 2 {
		t.Errorf("alias relationship or bytes changed: target=%+v alias=%+v before=%+v", ta, aa, before)
	}
	if left := zzLCXEntries(t, root, base); len(left) != 0 {
		t.Errorf("private leftovers: %v", left)
	}
	if snap, ok := e.Snapshots().Latest(testActor().Session, canonicalPath(target)); ok && snap.Rev == RevOf([]byte("agent-next\n")) {
		t.Errorf("snapshot store registered refused revision")
	}
	// No lock leak and normal replacement still works once the alias is gone.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	e.SetPublishFaultHooks(PublishFaultHooks{})
	res2, rec2, err2 := zzCommit(t, e, FileChange{Path: "f.txt", Content: bytesPtr("agent-next\n")})
	fr, rr := zzRow(t, res2, rec2, "f.txt")
	if err2 != nil || !rec2.Committed || fr.PublishState != PublishSucceededAndVerified || rr.NewSHA256 == "" || *calls != 1 {
		t.Errorf("follow-up commit: err=%v rec=%+v swaps=%d", err2, rec2, *calls)
	}
	if got := zzObserve(t, target); got.data != "agent-next\n" || got.ino == before.ino {
		t.Errorf("follow-up did not publish via swap: %+v", got)
	}
	if snap, ok := e.Snapshots().Latest(testActor().Session, canonicalPath(target)); !ok || snap.Rev != RevOf([]byte("agent-next\n")) {
		t.Errorf("follow-up revision not registered")
	}
}

// Known boundary (not a finding): alias created after the final observation, inside the swap call.
func TestZZOpusVerifyLateLinkAfterFinalCheckBoundary(t *testing.T) {
	base, root := zzRoot(t)
	target := filepath.Join(root, "f.txt")
	if err := os.WriteFile(target, []byte("orig-bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "too-late.txt")
	saved := renameSwapForPublish
	calls := zzSwapCounter(t, func(src, dst string) error {
		zzChildLink(t, dst, alias)
		return saved(src, dst)
	})
	e := newTestEngine(t, root)
	res, rec, err := zzCommit(t, e, FileChange{Path: "f.txt", Content: bytesPtr("agent-next\n")})
	fr, _ := zzRow(t, res, rec, "f.txt")
	ta, aa := zzObserve(t, target), zzObserve(t, alias)
	t.Logf("BOUNDARY swaps=%d err=%v state=%s committed=%v target=%+v alias=%+v", *calls, err, fr.PublishState, rec.Committed, ta, aa)
}

func TestZZOpusVerifyUnsupportedSwapErrnos(t *testing.T) {
	cases := []struct {
		name      string
		errno     error
		state     PublishState
		code      string
		wantDirty bool
	}{
		{"ENOTSUP", syscall.ENOTSUP, PublishUnsupported, CodeUnsupportedFilesystem, false},
		{"EOPNOTSUPP", syscall.EOPNOTSUPP, PublishUnsupported, CodeUnsupportedFilesystem, false},
		{"EINVAL", syscall.EINVAL, PublishUnsupported, CodeUnsupportedFilesystem, false},
		{"ENOSYS", syscall.ENOSYS, PublishUnsupported, CodeUnsupportedFilesystem, false},
		{"EACCES_not_unsupported", syscall.EACCES, PublishFailedBeforeWrite, "", false},
		{"EIO_not_unsupported", syscall.EIO, PublishUncertain, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, root := zzRoot(t)
			target := filepath.Join(root, "f.txt")
			if err := os.WriteFile(target, []byte("orig-bytes\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			before := zzObserve(t, target)
			stagedOK := false
			calls := zzSwapCounter(t, func(src, dst string) error {
				b, err := os.ReadFile(src)
				stagedOK = err == nil && string(b) == "agent-next\n" && dst == target
				return tc.errno
			})
			e := newTestEngine(t, root)
			renames := 0
			e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose: func(string) error { renames++; return nil }})
			res, rec, err := zzCommit(t, e, FileChange{Path: "f.txt", Content: bytesPtr("agent-next\n")})
			if *calls != 1 || !stagedOK || renames != 0 {
				t.Errorf("swap calls=%d stagedComplete=%v postPublishEntered=%d", *calls, stagedOK, renames)
			}
			zzAssertRefused(t, res, rec, err, "f.txt", tc.state, tc.code)
			if tc.code == "" && CodeOf(err) == CodeUnsupportedFilesystem {
				t.Errorf("non-unsupported errno converted to unsupported: %v", err)
			}
			if rec.DirtyFailure != tc.wantDirty {
				t.Errorf("dirty=%v want %v (state=%s)", rec.DirtyFailure, tc.wantDirty, res.CommitState)
			}
			after := zzObserve(t, target)
			info, _ := os.Lstat(target)
			if after != before || info.Mode().Perm() != 0o640 {
				t.Errorf("target changed: before=%+v after=%+v mode=%v", before, after, info.Mode())
			}
			if left := zzLCXEntries(t, root); len(left) != 0 {
				t.Errorf("staging leftovers: %v", left)
			}
		})
	}
}

// Partial Commit First: a verified prefix is not rolled back when a later file hits unsupported swap.
func TestZZOpusVerifyUnsupportedAfterVerifiedPrefix(t *testing.T) {
	_, root := zzRoot(t)
	writeFile(t, root, "a.txt", "a-old\n")
	writeFile(t, root, "b.txt", "b-old\n")
	writeFile(t, root, "c.txt", "c-old\n")
	b := filepath.Join(root, "b.txt")
	bBefore := zzObserve(t, b)
	saved := renameSwapForPublish
	zzSwapCounter(t, func(src, dst string) error {
		if dst == b {
			return syscall.ENOTSUP
		}
		return saved(src, dst)
	})
	e := newTestEngine(t, root)
	res, rec, err := zzCommit(t, e,
		FileChange{Path: "a.txt", Content: bytesPtr("a-new\n")},
		FileChange{Path: "b.txt", Content: bytesPtr("b-new\n")},
		FileChange{Path: "c.txt", Content: bytesPtr("c-new\n")})
	if CodeOf(err) != CodeUnsupportedFilesystem || res.CommitState != CommitStatePartiallyPublished || !rec.DirtyFailure || rec.Committed {
		t.Errorf("batch outcome: err=%v state=%s rec=%+v", err, res.CommitState, rec)
	}
	fa, ra := zzRow(t, res, rec, "a.txt")
	if fa.PublishState != PublishSucceededAndVerified || !fa.Verified || fa.RolledBack || ra.RolledBack || ra.NewSHA256 == "" || readFileContent(t, root, "a.txt") != "a-new\n" {
		t.Errorf("verified prefix not preserved: %+v %+v", fa, ra)
	}
	zzAssertRefused(t, res, rec, err, "b.txt", PublishUnsupported, CodeUnsupportedFilesystem)
	if zzObserve(t, b) != bBefore {
		t.Errorf("b changed")
	}
	fc, rc := zzRow(t, res, rec, "c.txt")
	if fc.PublishState != PublishNotAttempted || rc.Wrote || readFileContent(t, root, "c.txt") != "c-old\n" {
		t.Errorf("suffix attempted: %+v %+v", fc, rc)
	}
	if left := zzLCXEntries(t, root); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
}

// Replaces the coverage of the deleted fallback test on the real exchange path: held-FD external
// write to the displaced original plus a third-party atomic replacement of the target, both after
// the swap. Recovery must not overwrite the third party and must keep the external bytes.
func TestZZOpusVerifySwapRecoveryNeverOverwritesThirdParty(t *testing.T) {
	_, root := zzRoot(t)
	target := filepath.Join(root, "f.txt")
	orig := []byte("original-content-25bytes\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	origIno := zzObserve(t, target).ino
	calls := zzSwapCounter(t, nil)
	e := newTestEngine(t, root)
	e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose: func(string) error {
		if _, err := ext.WriteAt([]byte("FOREIGN\n"), 0); err != nil {
			return err
		}
		tmp := target + ".third"
		if err := os.WriteFile(tmp, []byte("THIRD-PARTY-REPLACEMENT\n"), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, target)
	}})
	res, rec, cerr := zzCommit(t, e, FileChange{Path: "f.txt", BaseRev: RevOf(orig), Content: bytesPtr("agent os process content\n")})
	fr, rr := zzRow(t, res, rec, "f.txt")
	t.Logf("swaps=%d err=%v state=%s", *calls, cerr, fr.PublishState)
	if CodeOf(cerr) != CodeConcurrentWrite || rec.Committed || fr.Verified || rr.NewSHA256 != "" || fr.PublishState != PublishExternalModification {
		t.Errorf("outcome: err=%v rec=%+v fr=%+v", cerr, rec, fr)
	}
	if got := zzObserve(t, target); got.data != "THIRD-PARTY-REPLACEMENT\n" {
		t.Errorf("third-party replacement overwritten: %+v", got)
	}
	kept := zzLCXEntries(t, root)
	found := false
	for _, k := range kept {
		o := zzObserve(t, k)
		if o.ino == origIno && o.data == "FOREIGN\n-content-25bytes\n" && strings.Contains(cerr.Error(), k) {
			found = true
		}
	}
	if !found {
		t.Errorf("external bytes not kept at a named private path: kept=%v err=%v", kept, cerr)
	}
}
