//go:build darwin || linux

package editing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func assertNoLCXTempLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".lcx-") {
			t.Fatalf("unexpected private staging/quarantine leftover in %s: %s", dir, ent.Name())
		}
	}
}

// assertOnlyReportedLCXLeftovers allows a private leftover only when err names its path and it holds
// exactly want (a deliberately kept original); any other leftover is debris.
func assertOnlyReportedLCXLeftovers(t *testing.T, dir string, err error, want []byte) {
	t.Helper()
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, ent := range entries {
		if !strings.HasPrefix(ent.Name(), ".lcx-") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err == nil || !strings.Contains(err.Error(), ent.Name()) || readErr != nil || !bytes.Equal(raw, want) {
			t.Fatalf("unreported or unexpected private leftover in %s: %s (err=%v)", dir, ent.Name(), err)
		}
	}
}

func attachSmallMacOSDiskImage(t *testing.T, fsType string, sizeMB int) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS hdiutil disk image test")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dmg := filepath.Join(base, "small.dmg")
	mnt := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	createCmd := exec.Command("hdiutil", "create",
		"-size", strconv.Itoa(sizeMB)+"m",
		"-fs", fsType,
		"-volname", "lcpre01",
		dmg,
	)
	if out, err := createCmd.CombinedOutput(); err != nil {
		t.Skipf("hdiutil create (%s) unavailable in this environment: %v\n%s", fsType, err, out)
	}
	attachCmd := exec.Command("hdiutil", "attach", "-nobrowse", "-mountpoint", mnt, dmg)
	if out, err := attachCmd.CombinedOutput(); err != nil {
		t.Skipf("hdiutil attach (%s) unavailable in this environment: %v\n%s", fsType, err, out)
	}
	t.Cleanup(func() {
		for range 5 {
			if out, err := exec.Command("hdiutil", "detach", mnt).CombinedOutput(); err == nil {
				return
			} else {
				_ = out
				time.Sleep(50 * time.Millisecond)
			}
		}
		_ = exec.Command("hdiutil", "detach", "-force", mnt).Run()
	})
	realMnt, err := filepath.EvalSymlinks(mnt)
	if err != nil {
		return mnt
	}
	return realMnt
}

// TestPRE01RealDiskImageENOSPC reproduces EDITING-PRE-01 on a real 8 MiB macOS disk image:
// overwriting an existing 14,000-byte file when free space (~320 KiB) cannot hold the new content
// must fail with ENOSPC while preserving all 14,000 original bytes untouched and leaving zero
// temporary staging debris.
func TestPRE01RealDiskImageENOSPC(t *testing.T) {
	mnt := attachSmallMacOSDiskImage(t, "HFS+", 8)
	ws := filepath.Join(mnt, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	origBytes := bytes.Repeat([]byte("ORIGINAL-14KB-"), 1000) // exactly 14,000 bytes
	existPath := filepath.Join(ws, "exist.txt")
	if err := os.WriteFile(existPath, origBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Leave ~320 KiB free on the mounted volume.
	var st syscall.Statfs_t
	if err := syscall.Statfs(ws, &st); err != nil {
		t.Fatal(err)
	}
	freeBytes := int64(st.Bavail) * int64(st.Bsize)
	const targetFree = 320 * 1024
	if freeBytes > targetFree+64*1024 {
		ballastSize := freeBytes - targetFree
		ballast := bytes.Repeat([]byte("B"), int(ballastSize))
		if err := os.WriteFile(filepath.Join(ws, "ballast.bin"), ballast, 0o644); err != nil {
			t.Fatalf("write ballast (%d bytes): %v", ballastSize, err)
		}
	}

	e, err := New(Options{Root: ws})
	if err != nil {
		t.Fatal(err)
	}
	actor := testActor()
	e.RegisterRead(actor, existPath, RevOf(origBytes))

	tooBig := bytes.Repeat([]byte("N"), 1_036_000)
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, commitErr := e.Commit(ctx, Plan{
		Actor:   actor,
		Tool:    "write_file",
		Changes: []FileChange{{Path: "exist.txt", BaseRev: RevOf(origBytes), Content: &tooBig}},
	})
	if !errors.Is(commitErr, syscall.ENOSPC) {
		t.Fatalf("expected ENOSPC, got %v", commitErr)
	}
	got, readErr := os.ReadFile(existPath)
	if readErr != nil {
		t.Fatalf("read exist.txt after ENOSPC: %v", readErr)
	}
	if !bytes.Equal(got, origBytes) {
		t.Fatalf("EDITING-PRE-01 regression: exist.txt has %d bytes after ENOSPC, want %d original bytes preserved", len(got), len(origBytes))
	}
	if rec.Committed || rec.DirtyFailure || res.CommitState != CommitStatePrePublishFailure {
		t.Fatalf("expected clean pre-publish failure receipt on ENOSPC, got res=%+v rec=%+v", res, rec)
	}
	assertNoLCXTempLeftovers(t, ws)
}

// TestPRE01SimulatedBlockQuotaENOSPC exercises deterministic 0-byte and 320-KiB partial-write ENOSPC
// failures when updating an existing 14,000-byte file (without requiring IOKit/hdiutil privileges).
// The live destination must retain all 14,000 original bytes on its original inode, report a clean
// pre-publish failure, keep its session snapshot valid or cleanly invalidated without false revision,
// and leave zero .lcx-* debris.
func TestPRE01SimulatedBlockQuotaENOSPC(t *testing.T) {
	origBytes := bytes.Repeat([]byte("ORIGINAL-14KB-"), 1000) // 14,000 bytes
	tooBig := bytes.Repeat([]byte("N"), 1_036_000)

	for _, tc := range []struct {
		name       string
		quotaBytes int
		fallbackFS bool
	}{
		{"zero_free_blocks", 0, false},
		{"partial_320KiB_before_enospc", 320 * 1024, false},
		{"partial_320KiB_fallback_no_rename_swap", 320 * 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fallbackFS {
				oldSwap := renameSwapForPublish
				renameSwapForPublish = func(_, _ string) error { return syscall.ENOTSUP }
				t.Cleanup(func() { renameSwapForPublish = oldSwap })
			}

			root := t.TempDir()
			existPath := filepath.Join(root, "exist.txt")
			if err := os.WriteFile(existPath, origBytes, 0o640); err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Lstat(existPath)
			if err != nil {
				t.Fatal(err)
			}

			e := newTestEngine(t, root)
			actor := testActor()
			e.RegisterRead(actor, existPath, RevOf(origBytes))
			e.SetPublishFaultHooks(PublishFaultHooks{
				WriteStaged: func(_ string, f *os.File, data []byte) (int, error) {
					if tc.quotaBytes == 0 {
						return 0, syscall.ENOSPC
					}
					chunk := tc.quotaBytes
					if chunk > len(data) {
						chunk = len(data)
					}
					n, werr := f.Write(data[:chunk])
					if werr != nil {
						return n, werr
					}
					return n, syscall.ENOSPC
				},
			})

			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			res, commitErr := e.Commit(ctx, Plan{
				Actor:   actor,
				Tool:    "write_file",
				Changes: []FileChange{{Path: "exist.txt", BaseRev: RevOf(origBytes), Content: &tooBig}},
			})
			if !errors.Is(commitErr, syscall.ENOSPC) {
				t.Fatalf("expected ENOSPC, got %v", commitErr)
			}
			got, readErr := os.ReadFile(existPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(got, origBytes) {
				t.Fatalf("exist.txt corrupted on ENOSPC: got %d bytes, want %d original bytes", len(got), len(origBytes))
			}
			afterInfo, err := os.Lstat(existPath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(beforeInfo, afterInfo) || afterInfo.Mode().Perm() != 0o640 {
				t.Fatalf("exist.txt inode or mode changed on ENOSPC: before=%v after=%v", beforeInfo.Mode(), afterInfo.Mode())
			}
			if rec.Committed || rec.DirtyFailure || res.CommitState != CommitStatePrePublishFailure {
				t.Fatalf("expected clean pre-publish failure, got res=%+v rec=%+v", res, rec)
			}
			f, r := r7Files(t, res, rec, "exist.txt")
			if f.PublishState != PublishFailedBeforeWrite || f.Published || r.Wrote || f.Verified || r.Staged || r.NewSHA256 != "" {
				t.Fatalf("unexpected file receipt on ENOSPC: f=%+v r=%+v", f, r)
			}
			assertNoLCXTempLeftovers(t, root)
		})
	}
}

func TestPRE01RLimitChildHelper(t *testing.T) {
	mode := os.Getenv("LC_PRE01_RLIMIT_MODE")
	if mode == "" {
		return
	}
	root := os.Getenv("LC_PRE01_RLIMIT_ROOT")
	signal.Ignore(syscall.SIGXFSZ)
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) }()

	origContent := []byte("important-original-content\n")
	e := newTestEngine(t, root)
	actor := testActor()
	e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf(origContent))

	lim := syscall.Rlimit{Cur: 0, Max: orig.Max}
	if mode == "short_3" {
		lim.Cur = 3
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Fatal(err)
	}
	next := []byte("replacement-exceeding-rlimit\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	_, commitErr := e.Commit(ctx, Plan{
		Actor:   actor,
		Tool:    "write_file",
		Changes: []FileChange{{Path: "a.txt", BaseRev: RevOf(origContent), Content: &next}},
	})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
	if !errors.Is(commitErr, syscall.EFBIG) && !errors.Is(commitErr, io.ErrShortWrite) {
		t.Fatalf("expected EFBIG or ErrShortWrite, got %v", commitErr)
	}
	got, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, origContent) {
		t.Fatalf("EDITING-PRE-01 EFBIG (%s): a.txt became %q, want original %q preserved", mode, got, origContent)
	}
	if rec.Committed || rec.DirtyFailure {
		t.Fatalf("expected clean refusal on EFBIG, got %+v", rec)
	}
}

func TestPRE01RLimitEFBIGPreservesExistingFile(t *testing.T) {
	for _, mode := range []string{"zero_0", "short_3"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a.txt", "important-original-content\n")
			cmd := exec.Command(os.Args[0], "-test.run=^TestPRE01RLimitChildHelper$")
			cmd.Env = append(os.Environ(), "LC_PRE01_RLIMIT_MODE="+mode, "LC_PRE01_RLIMIT_ROOT="+root)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child (%s): %v\n%s", mode, err, out)
			}
			assertNoLCXTempLeftovers(t, root)
		})
	}
}

// TestPRE01UpdatePreservesPermissionsSymlinksAndHardlinks verifies that staged atomic updates
// preserve custom file mode bits, keep in-workspace symlinks intact while updating their target,
// and keep multi-link regular files linked across updates.
func TestPRE01UpdatePreservesPermissionsSymlinksAndHardlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, root)
	actor := testActor()

	// 1. Custom executable permission 0o751 preserved across staged swap.
	scriptPath := filepath.Join(root, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho v1\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scriptPath, 0o751); err != nil {
		t.Fatal(err)
	}
	v2 := []byte("#!/bin/sh\necho v2\n")
	if _, err := e.Commit(context.Background(), Plan{
		Actor:   actor,
		Tool:    "write_file",
		Changes: []FileChange{{Path: "run.sh", Content: &v2}},
	}); err != nil {
		t.Fatalf("update run.sh: %v", err)
	}
	info, err := os.Lstat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("run.sh permissions = %o, want 0751", info.Mode().Perm())
	}

	// 2. In-workspace symlink stays a symlink while its target updates atomically.
	realPath := filepath.Join(root, "real.txt")
	linkPath := filepath.Join(root, "link.txt")
	if err := os.WriteFile(realPath, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	afterLink := []byte("after-via-symlink\n")
	if _, err := e.Commit(context.Background(), Plan{
		Actor:   actor,
		Tool:    "write_file",
		Changes: []FileChange{{Path: "link.txt", Content: &afterLink}},
	}); err != nil {
		t.Fatalf("update via symlink: %v", err)
	}
	linfo, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if linfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt was replaced with a regular file instead of preserving symlink: mode=%v", linfo.Mode())
	}
	if got := readFileContent(t, root, "real.txt"); got != "after-via-symlink\n" {
		t.Fatalf("real.txt = %q, want %q", got, "after-via-symlink\n")
	}

	// 3. Hard-linked regular files are refused (fail-closed policy): a swap would detach the other
	// link and an in-place rewrite is not failure-safe. Both names keep the shared original.
	h1 := filepath.Join(root, "h1.txt")
	h2 := filepath.Join(root, "h2.txt")
	if err := os.WriteFile(h1, []byte("shared-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(h1, h2); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	hv2 := []byte("shared-v2\n")
	if _, err := e.Commit(context.Background(), Plan{
		Actor:   actor,
		Tool:    "write_file",
		Changes: []FileChange{{Path: "h1.txt", Content: &hv2}},
	}); CodeOf(err) != CodeUnsupportedFilesystem {
		t.Fatalf("update hardlink: err=%v, want %s", err, CodeUnsupportedFilesystem)
	}
	i1, _ := os.Lstat(h1)
	i2, _ := os.Lstat(h2)
	if !os.SameFile(i1, i2) || readFileContent(t, root, "h1.txt") != "shared-v1\n" || readFileContent(t, root, "h2.txt") != "shared-v1\n" {
		t.Fatalf("refused hardlink update changed the file: sameFile=%v h1=%q h2=%q", os.SameFile(i1, i2),
			readFileContent(t, root, "h1.txt"), readFileContent(t, root, "h2.txt"))
	}
	assertNoLCXTempLeftovers(t, root)
}

// TestPRE02UpdateWindowPreservesShortAndLongExternalWrites reproduces both halves of the archived
// TestOpusPRE02Baseline probe plus a same-length overwrite: neither a short 8-byte external write,
// a same-length 25-byte external write, nor a long 44-byte external write in the compare-to-mutate
// window may be overwritten with false success or left as mixed bytes.
func TestPRE02UpdateWindowPreservesShortAndLongExternalWrites(t *testing.T) {
	dir := t.TempDir()
	src, bin := filepath.Join(dir, "main.go"), filepath.Join(dir, "foreign_writer")
	helper := `package main
import (
	"os"
	"time"
)
func waitFile(path string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(100 * time.Microsecond)
	}
	return false
}
func main() {
	target, opened, writeGo, data := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(opened, []byte("1"), 0o644); err != nil {
		os.Exit(2)
	}
	if !waitFile(writeGo) {
		os.Exit(3)
	}
	if _, err := f.Write([]byte(data)); err != nil {
		os.Exit(4)
	}
	_ = f.Close()
}
`
	if err := os.WriteFile(src, []byte(helper), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}

	agent := []byte("agent os process content\n") // 25 bytes
	for _, tc := range []struct {
		name    string
		foreign string
	}{
		{"short_foreign_write_8B", "FOREIGN\n"},
		{"same_len_foreign_write_25B", "FOREIGN-EXACT-25-BYTES!!\n"},
		{"long_foreign_write_44B", "FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, ctrl := t.TempDir(), t.TempDir()
			target := filepath.Join(root, "shared.txt")
			opened, writeGo := filepath.Join(ctrl, "opened"), filepath.Join(ctrl, "write_go")
			cmd := exec.Command(bin, target, opened, writeGo, tc.foreign)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(opened); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("external process did not open target")
				}
				time.Sleep(100 * time.Microsecond)
			}

			e := newTestEngine(t, root)
			actor := Actor{Session: "s-pre02"}
			triggered := false
			triggerForeign := func(dst string) error {
				if dst == target && !triggered {
					triggered = true
					if err := os.WriteFile(writeGo, []byte("1"), 0o644); err != nil {
						return err
					}
					return cmd.Wait()
				}
				return nil
			}
			e.afterTruncate = triggerForeign

			var rec CommitReceipt
			res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
				Actor:   actor,
				Tool:    "write_file",
				Changes: []FileChange{{Path: "shared.txt", Content: &agent, AllowCreate: true}},
			})
			if !triggered {
				_ = os.WriteFile(writeGo, []byte("1"), 0o644)
				_ = cmd.Wait()
			}
			if commitErr == nil || rec.Committed || res.Files[0].Verified {
				t.Fatalf("EDITING-PRE-02 false success on %s: err=%v res=%+v rec=%+v", tc.name, commitErr, res, rec)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.foreign {
				t.Fatalf("EDITING-PRE-02 external bytes corrupted/lost on %s: got %q, want %q", tc.name, got, tc.foreign)
			}
			assertNoLCXTempLeftovers(t, root)
		})
	}
}

// TestPRE02PostSwapDisplacedOpenFDWriteRestored verifies that even if an uncooperative external
// process writes to a pre-opened file descriptor during the post-swap verification window (after
// RENAME_SWAP displaced the original inode), updateAtomicSwap detects the displaced write and
// swaps the intact external file back into place with zero mixed bytes and zero leftovers.
func TestPRE02PostSwapDisplacedOpenFDWriteRestored(t *testing.T) {
	agent := []byte("agent os process content\n")
	for _, tc := range []struct {
		name    string
		foreign string
	}{
		{"short_8B_after_swap", "FOREIGN\n"},
		{"long_44B_after_swap", "FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "shared.txt")
			extFD, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = extFD.Close() }()

			e := newTestEngine(t, root)
			e.SetPublishFaultHooks(PublishFaultHooks{
				AfterInPlaceWriteClose: func(dst string) error {
					if dst == target {
						if _, werr := extFD.Write([]byte(tc.foreign)); werr != nil {
							return werr
						}
						_ = extFD.Close()
					}
					return nil
				},
			})

			var rec CommitReceipt
			res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
				Actor:   Actor{Session: "s-postswap"},
				Tool:    "write_file",
				Changes: []FileChange{{Path: "shared.txt", Content: &agent, AllowCreate: true}},
			})
			if CodeOf(commitErr) != CodeConcurrentWrite || rec.Committed || res.Files[0].Verified {
				t.Fatalf("expected CodeConcurrentWrite, got err=%v res=%+v rec=%+v", commitErr, res, rec)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.foreign {
				t.Fatalf("displaced external write lost/mixed: got %q, want %q", got, tc.foreign)
			}
			assertNoLCXTempLeftovers(t, root)
		})
	}
}

// TestPRE02DeleteConcurrentExternalWritePreserved covers both deterministic check-to-delete window
// mutations (in-place append, same-length overwrite, and atomic rename replacement via BeforeDeleteUnlink)
// and an 80-round concurrent stress loop against non-cooperative external writers.
func TestPRE02DeleteConcurrentExternalWritePreserved(t *testing.T) {
	orig := []byte("original-delete-target\n") // 23 bytes

	t.Run("deterministic_check_to_delete_window", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(target string) error
			want   string
		}{
			{
				name: "external_append",
				mutate: func(target string) error {
					f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						return err
					}
					_, werr := f.Write([]byte("EXTERNAL-APPEND-MUST-SURVIVE\n"))
					return errors.Join(werr, f.Close())
				},
				want: "original-delete-target\nEXTERNAL-APPEND-MUST-SURVIVE\n",
			},
			{
				name: "external_same_length_overwrite",
				mutate: func(target string) error {
					f, err := os.OpenFile(target, os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, werr := f.Write([]byte("FOREIGN-23-BYTE-PAYLOAD\n"[:len(orig)]))
					return errors.Join(werr, f.Close())
				},
				want: "FOREIGN-23-BYTE-PAYLOAD\n"[:len(orig)],
			},
			{
				name: "external_atomic_rename_replace",
				mutate: func(target string) error {
					tmp := target + ".ext-tmp"
					if err := os.WriteFile(tmp, []byte("REPLACED-BY-FOREIGN\n"), 0o644); err != nil {
						return err
					}
					return os.Rename(tmp, target)
				},
				want: "REPLACED-BY-FOREIGN\n",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				root := t.TempDir()
				target := filepath.Join(root, "victim.txt")
				if err := os.WriteFile(target, orig, 0o644); err != nil {
					t.Fatal(err)
				}
				e := newTestEngine(t, root)
				actor := testActor()
				e.RegisterRead(actor, target, RevOf(orig))
				e.SetPublishFaultHooks(PublishFaultHooks{
					BeforeDeleteUnlink: func(dst string) error {
						if dst == target {
							if err := tc.mutate(target); err != nil {
								t.Fatalf("external mutate failed: %v", err)
							}
						}
						return nil
					},
				})

				var rec CommitReceipt
				res, delErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
					Actor:   actor,
					Tool:    "apply_patch",
					Changes: []FileChange{{Path: "victim.txt", BaseRev: RevOf(orig), Delete: true}},
				})
				if CodeOf(delErr) != CodeConflict || rec.Committed || res.Files[0].Deleted || res.Files[0].Verified {
					t.Fatalf("expected delete conflict, got err=%v res=%+v rec=%+v", delErr, res, rec)
				}
				got, err := os.ReadFile(target)
				if err != nil {
					t.Fatalf("external file lost on delete conflict: %v", err)
				}
				if string(got) != tc.want {
					t.Fatalf("external bytes = %q, want %q", got, tc.want)
				}
				assertNoLCXTempLeftovers(t, root)
			})
		}
	})

	t.Run("concurrent_stress_80_rounds", func(t *testing.T) {
		var lostCount atomic.Int64
		for round := range 80 {
			root := t.TempDir()
			target := filepath.Join(root, "victim.txt")
			if err := os.WriteFile(target, orig, 0o644); err != nil {
				t.Fatal(err)
			}
			e := newTestEngine(t, root)
			actor := testActor()
			e.RegisterRead(actor, target, RevOf(orig))

			foreign := []byte("original-delete-target\nEXTERNAL-APPEND-MUST-SURVIVE\n")
			var wg sync.WaitGroup
			start := make(chan struct{})
			var extErr, delErr error
			// The guarantee covers external writes completed before the final post-unlink check of the
			// held descriptor; a write through a pre-opened descriptor after that check reaches only the
			// unlinked inode (documented limitation), so it must not count as a loss here.
			var pastFinalCheck atomic.Bool
			var extCompletedBeforeFinalCheck atomic.Bool
			var rec CommitReceipt

			e.SetPublishFaultHooks(PublishFaultHooks{BeforeFinalExternalCheck: func(string) { pastFinalCheck.Store(true) }})
			e.beforeWrite = func(string) {
				close(start)
				runtime.Gosched()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				time.Sleep(time.Duration(round%7) * 5 * time.Microsecond)
				if round%2 == 0 {
					tmp := target + ".foreign-replace"
					if err := os.WriteFile(tmp, foreign, 0o644); err != nil {
						extErr = err
						return
					}
					extErr = os.Rename(tmp, target)
					if extErr == nil && !pastFinalCheck.Load() {
						extCompletedBeforeFinalCheck.Store(true)
					}
					return
				}
				f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					extErr = err
					return
				}
				_, extErr = f.Write([]byte("EXTERNAL-APPEND-MUST-SURVIVE\n"))
				if extErr == nil && !pastFinalCheck.Load() {
					extCompletedBeforeFinalCheck.Store(true)
				}
				if cerr := f.Close(); extErr == nil {
					extErr = cerr
				}
			}()

			_, delErr = e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
				Actor:   actor,
				Tool:    "apply_patch",
				Changes: []FileChange{{Path: "victim.txt", BaseRev: RevOf(orig), Delete: true}},
			})
			pastFinalCheck.Store(true)
			wg.Wait()

			raw, statErr := os.ReadFile(target)
			if extErr == nil && extCompletedBeforeFinalCheck.Load() && (delErr == nil || rec.Committed) {
				if errors.Is(statErr, os.ErrNotExist) || !bytes.Equal(raw, foreign) {
					lostCount.Add(1)
				}
			}
			// A path recreated by the external writer after the move aside keeps the original in
			// quarantine; that copy is allowed only when the error reports it and it holds the original.
			assertOnlyReportedLCXLeftovers(t, root, delErr, orig)
		}
		if n := lostCount.Load(); n > 0 {
			t.Fatalf("EDITING-PRE-02 deleteCAS lost %d concurrent external writes while reporting success", n)
		}
	})
}
