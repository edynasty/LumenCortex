//go:build !windows

package editing

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestR5RedR402ARollbackRestoreFailure reproduces R4-02-A on unmodified 0b3b8e72:
// A, B, C exist; A and B are updated; C hits CAS conflict; rollback restores B with RLIMIT_FSIZE=0
// so B is truncated to "" and restore Write fails with EFBIG, while A restores cleanly.
func TestR5RedR402ARollbackRestoreFailure(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) }()

	root := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeFile(t, root, n+".txt", "old-"+n+"\n")
	}
	e := newTestEngine(t, root)
	e.beforeWrite = func(p string) {
		if filepath.Base(p) == "c.txt" {
			_ = os.WriteFile(p, []byte("foreign-c\n"), 0o644)
		}
	}
	e.SetTestFaultHooks(nil, func(p string) error {
		switch filepath.Base(p) {
		case "b.txt":
			lim := syscall.Rlimit{Cur: 0, Max: orig.Max}
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
				t.Fatalf("setrlimit 0: %v", err)
			}
		case "a.txt":
			_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
		}
		return nil
	})
	a, b, c := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{
		Actor: testActor(),
		Tool:  "apply_patch",
		Changes: []FileChange{
			{Path: "a.txt", Content: &a},
			{Path: "b.txt", Content: &b},
			{Path: "c.txt", Content: &c},
		},
	})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
	if err == nil {
		t.Fatal("expected Commit error")
	}
	diskB := readFileContent(t, root, "b.txt")
	t.Logf("err=%v diskB=%q (rev=%s) intendedRev=%s rec=%+v res=%+v", err, diskB, RevOf([]byte(diskB)), RevOf(b), rec, res)

	// 1. Rollback error must not be hidden behind the original CAS conflict error.
	if !strings.Contains(err.Error(), "rollback") && !strings.Contains(err.Error(), "file too large") {
		t.Errorf("R4-02-A DEFECT: rollback failure error was swallowed behind CAS error: %v", err)
	}
	// 2. B must NEVER retain published_verified / Verified=true when rollback failed.
	for _, rf := range rec.Files {
		if rf.Path == "b.txt" {
			if rf.Verified || rf.PublishState == PublishSucceededAndVerified {
				t.Errorf("R4-02-A DEFECT: failed rollback on b.txt (disk=%q) still claims Verified=%v PublishState=%s", diskB, rf.Verified, rf.PublishState)
			}
		}
	}
	// 3. Result.Files must NOT report b.txt as published_verified or NewRev=RevOf(new-b) when disk != new-b.
	for _, fr := range res.Files {
		if fr.Path == "b.txt" {
			if fr.PublishState == PublishSucceededAndVerified || (diskB != string(b) && fr.NewRev == RevOf(b)) {
				t.Errorf("R4-02-A DEFECT: Result.Files reports stale state for b.txt: %+v (disk=%q)", fr, diskB)
			}
		}
	}
	// 4. SnapshotStore must NOT register RevOf(new-b) when disk != new-b.
	if diskB != string(b) && e.LatestRev(testActor(), filepath.Join(root, "b.txt")) == RevOf(b) {
		t.Errorf("R4-02-A DEFECT: SnapshotStore registered intended NewRev %s for b.txt even though disk=%q", RevOf(b), diskB)
	}
}

// TestR5RedR402BStalePerFileState reproduces R4-02-B on unmodified 0b3b8e72:
//  1. Created file b.txt externally appended before c.txt fails -> rollback refuses unlink, yet
//     receipt/result retain PublishState=published_verified, Verified=true, stale NewRev, and stale snapshot.
//  2. All modified files cleanly rolled back -> CommitState remains partially_published.
func TestR5RedR402BStalePerFileState(t *testing.T) {
	t.Run("created_file_refused_rollback_not_published_verified", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		e := newTestEngine(t, root)
		bPath := filepath.Join(root, "b.txt")
		e.SetPublishFaultHooks(PublishFaultHooks{
			BeforePublish: func(dst string) error {
				if filepath.Base(dst) == "c.txt" {
					f, oerr := os.OpenFile(bPath, os.O_WRONLY|os.O_APPEND, 0)
					if oerr != nil {
						return oerr
					}
					_, _ = f.Write([]byte("FOREIGN\n"))
					_ = f.Close()
					return os.ErrPermission
				}
				return nil
			},
		})
		a, b, c := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &a},
				{Path: "b.txt", Content: &b, Create: true},
				{Path: "c.txt", Content: &c, Create: true},
			},
		})
		if err == nil {
			t.Fatal("expected Commit error")
		}
		for _, rf := range rec.Files {
			if rf.Path == "b.txt" && (rf.Verified || rf.PublishState == PublishSucceededAndVerified) {
				t.Errorf("R4-02-B DEFECT: externally modified created b.txt still claims Verified=%v PublishState=%s", rf.Verified, rf.PublishState)
			}
		}
		for _, fr := range res.Files {
			if fr.Path == "b.txt" && (fr.PublishState == PublishSucceededAndVerified || fr.NewRev == RevOf(b)) {
				t.Errorf("R4-02-B DEFECT: externally modified created b.txt carries stale Result: %+v", fr)
			}
		}
		if e.LatestRev(testActor(), bPath) == RevOf(b) {
			t.Errorf("R4-02-B DEFECT: SnapshotStore registered stale RevOf(new-b) %s for externally modified b.txt", RevOf(b))
		}
	})

	t.Run("all_files_rolled_back_not_partially_published", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b\n")
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			BeforePublish: func(dst string) error {
				if filepath.Base(dst) == "c.txt" {
					return os.ErrPermission
				}
				return nil
			},
		})
		a, b, c := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &a},
				{Path: "b.txt", Content: &b},
				{Path: "c.txt", Content: &c, Create: true},
			},
		})
		if err == nil {
			t.Fatal("expected Commit error")
		}
		if rec.State == CommitStatePartiallyPublished || res.CommitState == CommitStatePartiallyPublished {
			t.Errorf("R4-02-B DEFECT: fully rolled-back commit still reports State=%s Result.CommitState=%s", rec.State, res.CommitState)
		}
	})
}
