//go:build darwin

package editing

// Out-of-repository audit probes (Opus independent review of EDITING-PRE-01/PRE-02).
// Copied into a package directory only for the duration of a probe run.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

type probeMeta struct {
	ino, uid, gid, flags uint64
	mode                 os.FileMode
	xattrs               map[string]string
	acl                  string
}

func probeReadMeta(t *testing.T, path string) probeMeta {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	m := probeMeta{ino: st.Ino, uid: uint64(st.Uid), gid: uint64(st.Gid), flags: uint64(st.Flags), mode: info.Mode(), xattrs: map[string]string{}}
	buf := make([]byte, 64<<10)
	n, err := unix.Llistxattr(path, buf)
	if err == nil {
		for _, name := range strings.Split(string(buf[:n]), "\x00") {
			if name == "" {
				continue
			}
			v := make([]byte, 64<<10)
			vn, verr := unix.Lgetxattr(path, name, v)
			if verr != nil {
				m.xattrs[name] = "ERR:" + verr.Error()
				continue
			}
			m.xattrs[name] = string(v[:vn])
		}
	}
	out, _ := exec.Command("/bin/ls", "-led", path).CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 1 {
		m.acl = strings.Join(lines[1:], "|")
	}
	return m
}

func (m probeMeta) String() string {
	keys := make([]string, 0, len(m.xattrs))
	for k := range m.xattrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("ino=%d uid=%d gid=%d mode=%v flags=%#x xattrs=%v acl=%q", m.ino, m.uid, m.gid, m.mode, m.flags, keys, m.acl)
}

// TestOpusProbeMetadataOnUpdate records which metadata survives an ordinary update.
func TestOpusProbeMetadataOnUpdate(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "meta.txt")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(target, "com.example.lc-probe", []byte("user-tag"), 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(target, "com.apple.quarantine", []byte("0081;00000000;LCProbe;"), 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(target, "com.apple.ResourceFork", []byte("rsrc-fork-bytes"), 0); err != nil {
		t.Logf("resource fork set: %v", err)
	}
	if out, err := exec.Command("/bin/chmod", "+a", "user:nobody allow read", target).CombinedOutput(); err != nil {
		t.Fatalf("acl: %v %s", err, out)
	}
	if err := unix.Chflags(target, unix.UF_HIDDEN); err != nil {
		t.Fatal(err)
	}
	// Group different from the directory group (BSD group inheritance gives new files the dir's gid).
	if err := os.Chown(target, -1, 12); err != nil { // "everyone"
		t.Logf("chgrp everyone: %v", err)
	}
	if err := os.Chmod(target, 0o644|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Chflags(target, 0); _ = exec.Command("/bin/chmod", "-N", target).Run() })
	before := probeReadMeta(t, target)
	e := newTestEngine(t, root)
	next := []byte("updated\n")
	if _, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "meta.txt", Content: &next}}}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	after := probeReadMeta(t, target)
	t.Logf("BEFORE %s", before)
	t.Logf("AFTER  %s", after)
	if got, _ := os.ReadFile(target); !bytes.Equal(got, next) {
		t.Fatalf("content=%q", got)
	}
	var lost []string
	if before.ino != after.ino {
		t.Logf("inode changed %d -> %d", before.ino, after.ino)
	}
	if before.mode != after.mode {
		lost = append(lost, fmt.Sprintf("mode %v->%v", before.mode, after.mode))
	}
	if before.gid != after.gid {
		lost = append(lost, fmt.Sprintf("gid %d->%d", before.gid, after.gid))
	}
	if before.uid != after.uid {
		lost = append(lost, fmt.Sprintf("uid %d->%d", before.uid, after.uid))
	}
	if before.flags != after.flags {
		lost = append(lost, fmt.Sprintf("flags %#x->%#x", before.flags, after.flags))
	}
	if before.acl != after.acl {
		lost = append(lost, fmt.Sprintf("acl %q->%q", before.acl, after.acl))
	}
	for k, v := range before.xattrs {
		if k == "com.apple.ResourceFork" {
			// Resource fork follows the data on rewrite semantics; report but compare presence.
		}
		if av, ok := after.xattrs[k]; !ok || av != v {
			lost = append(lost, fmt.Sprintf("xattr %s %q->%q(present=%v)", k, v, av, ok))
		}
	}
	if len(lost) > 0 {
		t.Errorf("METADATA LOST on update: %s", strings.Join(lost, "; "))
	}
}

// TestOpusProbeHardlinkPRE02 reproduces the original PRE-02 check-to-truncate loss on a multi-link file.
func TestOpusProbeHardlinkPRE02(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	h1, h2 := filepath.Join(root, "h1.txt"), filepath.Join(root, "h2.txt")
	orig := []byte("original-content-25bytes\n")
	if err := os.WriteFile(h1, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(h1, h2); err != nil {
		t.Fatal(err)
	}
	ext, err := os.OpenFile(h2, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), h1, RevOf(orig))
	e.afterTruncate = func(dst string) error {
		_, werr := ext.WriteAt([]byte("FOREIGN\n"), 0)
		return werr
	}
	next := []byte("agent os process content\n")
	var rec CommitReceipt
	_, cerr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "h1.txt", BaseRev: RevOf(orig), Content: &next}}})
	got, _ := os.ReadFile(h1)
	t.Logf("err=%v committed=%v dirty=%v state=%v content=%q", cerr, rec.Committed, rec.DirtyFailure, rec.Files, got)
	if cerr == nil && rec.Committed && !bytes.Contains(got, []byte("FOREIGN")) {
		t.Errorf("PRE-02 RESIDUAL (multi-link): external 8B write silently overwritten while committed")
	}
}

// TestOpusProbeFallbackRestoreOverwritesThirdParty forces the no-RENAME_SWAP fallback and checks that
// conflict recovery never overwrites a third-party replacement of the target path.
func TestOpusProbeFallbackRestoreOverwritesThirdParty(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
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
	saved := renameSwapForPublish
	renameSwapForPublish = func(_, _ string) error { return syscall.ENOTSUP }
	defer func() { renameSwapForPublish = saved }()
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), target, RevOf(orig))
	e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose: func(dst string) error {
		if _, err := ext.WriteAt([]byte("FOREIGN\n"), 0); err != nil {
			return err
		}
		tmp := target + ".third"
		if err := os.WriteFile(tmp, []byte("THIRD-PARTY-REPLACEMENT\n"), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, target)
	}})
	next := []byte("agent os process content\n")
	var rec CommitReceipt
	_, cerr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "f.txt", BaseRev: RevOf(orig), Content: &next}}})
	got, _ := os.ReadFile(target)
	ents, _ := os.ReadDir(root)
	var names []string
	for _, en := range ents {
		names = append(names, en.Name())
	}
	t.Logf("err=%v committed=%v content=%q dir=%v", cerr, rec.Committed, got, names)
	if string(got) != "THIRD-PARTY-REPLACEMENT\n" {
		t.Errorf("fallback conflict recovery overwrote a third-party replacement: target=%q", got)
	}
}

// TestOpusProbeDeleteRestoreOccupiedDiscardsBytes calls the delete recovery helper with an occupied path.
func TestOpusProbeDeleteRestoreOccupiedDiscardsBytes(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	target := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(target, []byte("THIRD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host, err := NewLocalHost(root)
	if err != nil {
		t.Fatal(err)
	}
	ext := []byte("EXTERNAL-BYTES-MUST-SURVIVE\n")
	savedAt, rerr := preserveExternalBytes(host, target, ext, 0o644); t.Logf("savedAt=%s", savedAt)
	found := false
	ents, _ := os.ReadDir(root)
	for _, en := range ents {
		b, _ := os.ReadFile(filepath.Join(root, en.Name()))
		if bytes.Equal(b, ext) {
			found = true
		}
	}
	t.Logf("restoreErr=%v externalBytesPersisted=%v entries=%d", rerr, found, len(ents))
	if rerr == nil && !found {
		t.Errorf("restoreUnlinkedExternalBytes reported success but external bytes exist nowhere")
	}
}

// TestOpusProbeHeldFDWriteBeforeCommitReturn writes through a pre-opened descriptor after the final
// post-publication check but before Commit returns (inside the receipt recorder).
func TestOpusProbeHeldFDWriteBeforeCommitReturn(t *testing.T) {
	for _, op := range []string{"update", "delete"} {
		t.Run(op, func(t *testing.T) {
			root, _ := filepath.EvalSymlinks(t.TempDir())
			target := filepath.Join(root, "f.txt")
			orig := []byte("original-content-25bytes\n")
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
			next := []byte("agent os process content\n")
			ch := FileChange{Path: "f.txt", BaseRev: RevOf(orig), Content: &next}
			if op == "delete" {
				ch = FileChange{Path: "f.txt", BaseRev: RevOf(orig), Delete: true}
			}
			var rec CommitReceipt
			_, cerr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) {
				rec = r
				_, _ = ext.Write([]byte("LATE-HELD-FD-APPEND\n"))
			}), Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{ch}})
			got, statErr := os.ReadFile(target)
			t.Logf("LIMITATION %s: err=%v committed=%v target=%q statErr=%v", op, cerr, rec.Committed, got, statErr)
		})
	}
}
