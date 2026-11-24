//go:build darwin

package editing

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type opusDarwinMeta struct {
	ino, gid uint64
	flags    uint32
	btime    unix.Timespec
	xattrs   map[string]string
	acl      string
}

func opusReadDarwinMeta(t *testing.T, path string) opusDarwinMeta {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	m := opusDarwinMeta{ino: st.Ino, gid: uint64(st.Gid), flags: st.Flags, btime: st.Btim, xattrs: map[string]string{}}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	names, err := listXattrNames(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		v, err := getXattr(int(f.Fd()), name)
		if err != nil {
			t.Fatal(err)
		}
		m.xattrs[name] = string(v)
	}
	out, err := exec.Command("/bin/ls", "-led", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ls -le: %v %s", err, out)
	}
	if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); len(lines) > 1 {
		m.acl = strings.Join(lines[1:], "|")
	}
	return m
}

func opusPrepareMetadataFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("original\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"com.example.lc-tag":     "user-tag",
		"com.apple.quarantine":   "0081;00000000;LCRegression;",
		"com.apple.ResourceFork": "resource-fork-bytes",
	} {
		if err := unix.Setxattr(path, name, []byte(value), 0); err != nil {
			t.Fatalf("setxattr %s: %v", name, err)
		}
	}
	// A group other than the directory's: BSD semantics give a new sibling the directory's group.
	if err := os.Chown(path, -1, 12); err != nil {
		t.Skipf("cannot set group everyone(12): %v", err)
	}
	if err := unix.Chflags(path, unix.UF_HIDDEN); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Chflags(path, 0)
		_ = exec.Command("/bin/chmod", "-N", path).Run()
	})
}

func opusAssertMetadataKept(t *testing.T, before, after opusDarwinMeta) {
	t.Helper()
	if before.gid != after.gid || before.flags != after.flags || before.btime != after.btime || before.acl != after.acl {
		t.Fatalf("metadata changed: before gid=%d flags=%#x btime=%v acl=%q, after gid=%d flags=%#x btime=%v acl=%q",
			before.gid, before.flags, before.btime, before.acl, after.gid, after.flags, after.btime, after.acl)
	}
	for name, value := range before.xattrs {
		if got, ok := after.xattrs[name]; !ok || got != value {
			t.Fatalf("extended attribute %s lost or changed: %q -> %q (present=%v)", name, value, got, ok)
		}
	}
}

// TestOpusPRE01SwapReplicatesMetadata pins that the staged swap (a new inode) carries the
// destination's group, extended attributes (quarantine, resource fork, user tags), BSD flags and
// birth time instead of silently dropping them.
func TestOpusPRE01SwapReplicatesMetadata(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "meta.txt")
	opusPrepareMetadataFile(t, target)
	before := opusReadDarwinMeta(t, target)

	e := newTestEngine(t, root)
	next := []byte("updated\n")
	if _, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "meta.txt", Content: &next}}}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, next) {
		t.Fatalf("content = %q, want %q", got, next)
	}
	after := opusReadDarwinMeta(t, target)
	if after.ino == before.ino {
		t.Fatalf("update kept inode %d; the staged swap was not used", before.ino)
	}
	opusAssertMetadataKept(t, before, after)
	assertNoLCXTempLeftovers(t, root)
}

// TestOpusPRE01ACLFileRefusedIntact pins the fail-closed policy for a file with an ACL, which a
// swap cannot reproduce: the update is refused as unsupported before the file is touched, so its
// inode, bytes, ACL and other metadata are unchanged (a deny-delete ACL included).
func TestOpusPRE01ACLFileRefusedIntact(t *testing.T) {
	for _, ace := range []string{"user:nobody allow read", "everyone deny delete"} {
		t.Run(strings.ReplaceAll(ace, " ", "_"), func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "acl.txt")
			opusPrepareMetadataFile(t, target)
			if out, err := exec.Command("/bin/chmod", "+a", ace, target).CombinedOutput(); err != nil {
				t.Fatalf("chmod +a: %v %s", err, out)
			}
			before := opusReadDarwinMeta(t, target)
			if before.acl == "" {
				t.Fatal("ACL was not applied")
			}
			e := newTestEngine(t, root)
			next := []byte("updated\n")
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file",
				Changes: []FileChange{{Path: "acl.txt", Content: &next}}})
			if CodeOf(err) != CodeUnsupportedFilesystem || rec.Committed || rec.DirtyFailure {
				t.Fatalf("expected clean unsupported refusal, got err=%v rec=%+v", err, rec)
			}
			f, r := r7Files(t, res, rec, "acl.txt")
			if f.PublishState != PublishUnsupported || f.Published || r.Wrote || f.NewRev != "" || r.NewSHA256 != "" {
				t.Fatalf("unsupported receipt: %+v %+v", f, r)
			}
			if got, _ := os.ReadFile(target); string(got) != "original\n" {
				t.Fatalf("content = %q, want the original", got)
			}
			after := opusReadDarwinMeta(t, target)
			if after.ino != before.ino {
				t.Fatalf("ACL file inode changed %d -> %d", before.ino, after.ino)
			}
			opusAssertMetadataKept(t, before, after)
			assertNoLCXTempLeftovers(t, root)
		})
	}
}

// TestOpusPRE02DeleteFinalCheckKeepsWriteAfterRead writes through a held descriptor after the
// final read of the unlinked delete target but before the EVFILT_VNODE poll. The event reports the
// write; the bytes put back at the path must include it, not the stale pre-write read.
func TestOpusPRE02DeleteFinalCheckKeepsWriteAfterRead(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "victim.txt")
	orig := []byte("original-delete-target\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), target, RevOf(orig))
	e.SetPublishFaultHooks(PublishFaultHooks{AfterFinalExternalRead: func(dst string) {
		if dst == target {
			if _, err := ext.Write([]byte("APPEND-AFTER-FINAL-READ\n")); err != nil {
				t.Errorf("external append: %v", err)
			}
		}
	}})
	var rec CommitReceipt
	_, delErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
		Actor: testActor(), Tool: "apply_patch",
		Changes: []FileChange{{Path: "victim.txt", BaseRev: RevOf(orig), Delete: true}},
	})
	if CodeOf(delErr) != CodeConcurrentWrite || rec.Committed {
		t.Fatalf("expected concurrent write refusal, got err=%v rec=%+v", delErr, rec)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("external bytes not restored: %v", err)
	}
	if want := "original-delete-target\nAPPEND-AFTER-FINAL-READ\n"; string(got) != want {
		t.Fatalf("restored bytes = %q, want %q", got, want)
	}
	assertNoLCXTempLeftovers(t, root)
}
