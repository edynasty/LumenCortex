package editing

// Review probe (throwaway; lives only in /tmp/lc-adr0024-r3 snapshots, never in the repository).
// Exercises the R2-01 atomic create/publish path with real OS processes and fault points.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type zzStager struct {
	LocalHost
	beforePublish func(target string) error
}

func (h zzStager) OpenCreateTemp(target string) (*os.File, string, error) {
	return NewCreateTempFile(filepath.Dir(target))
}

func (h zzStager) BeforeAtomicPublish(target string) error {
	if h.beforePublish != nil {
		return h.beforePublish(target)
	}
	return nil
}

func zzDebris(root string) []string {
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), ".lcx-create-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func zzWaitFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(50 * time.Microsecond)
	}
	return false
}

func zzCode(err error) string {
	if err == nil {
		return "ok"
	}
	if c := CodeOf(err); c != "" {
		return string(c)
	}
	return "err"
}

// TestZZR3Helper is the child-process entry point; it does nothing unless LC_R3_HELPER is set.
func TestZZR3Helper(t *testing.T) {
	mode := os.Getenv("LC_R3_HELPER")
	if mode == "" {
		t.Skip("helper only")
	}
	root := os.Getenv("LC_R3_ROOT")
	ready := os.Getenv("LC_R3_READY")
	start := os.Getenv("LC_R3_START")
	content := []byte(os.Getenv("LC_R3_CONTENT"))
	if n, _ := strconv.Atoi(os.Getenv("LC_R3_REPEAT")); n > 1 {
		content = bytes.Repeat(content, n)
	}
	ctx := context.Background()
	switch mode {
	case "commit":
		e, err := New(Options{Root: root})
		if err != nil {
			fmt.Println("ZZRESULT setup", err)
			return
		}
		_ = os.WriteFile(ready, []byte("1"), 0o644)
		if !zzWaitFile(start, 10*time.Second) {
			fmt.Println("ZZRESULT nostart")
			return
		}
		var receipt CommitReceipt
		sawExisting := false
		e.afterRead = func(string) { sawExisting = true }
		_, err = e.Commit(WithCommitRecorder(ctx, func(r CommitReceipt) { receipt = r }), Plan{Actor: Actor{Session: "s-child"}, Tool: "write_file",
			Changes: []FileChange{{Path: "shared.txt", Content: &content, AllowCreate: true}}})
		fmt.Printf("ZZRESULT %s dirty=%v committed=%v sawExistingAtPrepare=%v err=%v\n", zzCode(err), receipt.DirtyFailure, receipt.Committed, sawExisting, err)
	case "foreign-rename":
		_ = os.WriteFile(ready, []byte("1"), 0o644)
		if !zzWaitFile(start, 10*time.Second) {
			fmt.Println("ZZRESULT nostart")
			return
		}
		delay, _ := strconv.Atoi(os.Getenv("LC_R3_DELAY_US"))
		time.Sleep(time.Duration(delay) * time.Microsecond)
		tmp := filepath.Join(root, ".foreign-save.tmp")
		if err := os.WriteFile(tmp, content, 0o644); err != nil {
			fmt.Println("ZZRESULT err", err)
			return
		}
		err := os.Rename(tmp, filepath.Join(root, "shared.txt"))
		fmt.Printf("ZZRESULT %s\n", zzCode(err))
	case "pause-before-publish":
		host := zzStager{beforePublish: func(target string) error {
			_ = os.WriteFile(ready, []byte("1"), 0o644)
			time.Sleep(time.Hour)
			return nil
		}}
		lh, _ := NewLocalHost(root)
		host.LocalHost = lh
		e, _ := New(Options{Root: root, Host: host})
		_, err := e.Commit(ctx, Plan{Actor: Actor{Session: "s-child"}, Tool: "write_file",
			Changes: []FileChange{{Path: "a.txt", Content: &content, AllowCreate: true}}})
		fmt.Println("ZZRESULT unexpected-return", err)
	case "pause-after-publish":
		e, _ := New(Options{Root: root})
		e.beforeWrite = func(p string) {
			if filepath.Base(p) == "b.txt" {
				_ = os.WriteFile(ready, []byte("1"), 0o644)
				time.Sleep(time.Hour)
			}
		}
		_, err := e.Commit(ctx, Plan{Actor: Actor{Session: "s-child"}, Tool: "apply_patch",
			Changes: []FileChange{{Path: "a.txt", Content: &content, AllowCreate: true}, {Path: "b.txt", Content: &content, AllowCreate: true}}})
		fmt.Println("ZZRESULT unexpected-return", err)
	case "commit-big":
		e, _ := New(Options{Root: root})
		var receipt CommitReceipt
		_, err := e.Commit(WithCommitRecorder(ctx, func(r CommitReceipt) { receipt = r }), Plan{Actor: Actor{Session: "s-child"}, Tool: "write_file",
			Changes: []FileChange{{Path: "big.txt", Content: &content, AllowCreate: true}}})
		fmt.Printf("ZZRESULT %s dirty=%v committed=%v err=%v\n", zzCode(err), receipt.DirtyFailure, receipt.Committed, err)
	}
}

func zzSpawn(t *testing.T, env ...string) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestZZR3Helper$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &out
}

func zzResult(out *bytes.Buffer) string {
	sc := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "ZZRESULT ") {
			return strings.TrimPrefix(line, "ZZRESULT ")
		}
	}
	return "noresult: " + out.String()
}

// C.1: several real OS processes, each running Engine.Commit, publish the same new destination.
func TestZZR3MultiProcessPublishRace(t *testing.T) {
	const rounds, procs = 60, 4
	var bad []string
	winners := map[int]int{}
	for r := 0; r < rounds; r++ {
		root := t.TempDir()
		ctl := t.TempDir()
		start := filepath.Join(ctl, "start")
		type child struct {
			cmd     *exec.Cmd
			out     *bytes.Buffer
			content []byte
		}
		var kids []child
		for k := 0; k < procs; k++ {
			unit := fmt.Sprintf("PROC-%d-ROUND-%d|", k, r)
			cmd, out := zzSpawn(t, "LC_R3_HELPER=commit", "LC_R3_ROOT="+root, "LC_R3_READY="+filepath.Join(ctl, fmt.Sprintf("ready%d", k)),
				"LC_R3_START="+start, "LC_R3_CONTENT="+unit, "LC_R3_REPEAT=4000")
			kids = append(kids, child{cmd, out, bytes.Repeat([]byte(unit), 4000)})
		}
		for k := 0; k < procs; k++ {
			if !zzWaitFile(filepath.Join(ctl, fmt.Sprintf("ready%d", k)), 20*time.Second) {
				t.Fatalf("child %d not ready", k)
			}
		}
		_ = os.WriteFile(start, []byte("go"), 0o644)
		oks, conflicts := 0, 0
		winner := -1
		for k, c := range kids {
			_ = c.cmd.Wait()
			res := zzResult(c.out)
			switch {
			case strings.HasPrefix(res, "ok "):
				oks++
				winner = k
			case strings.HasPrefix(res, "conflict "):
				conflicts++
			default:
				bad = append(bad, fmt.Sprintf("round %d child %d: %s", r, k, res))
			}
		}
		got, err := os.ReadFile(filepath.Join(root, "shared.txt"))
		switch {
		case oks != 1 || conflicts != procs-1:
			bad = append(bad, fmt.Sprintf("round %d: oks=%d conflicts=%d", r, oks, conflicts))
		case err != nil:
			bad = append(bad, fmt.Sprintf("round %d: destination missing: %v", r, err))
		case !bytes.Equal(got, kids[winner].content):
			bad = append(bad, fmt.Sprintf("round %d: destination is not the winner's exact bytes (len=%d)", r, len(got)))
		}
		if d := zzDebris(root); len(d) > 0 {
			bad = append(bad, fmt.Sprintf("round %d: temp debris %v", r, d))
		}
		winners[winner]++
	}
	t.Logf("multi-process publish: rounds=%d procs=%d winnerDistribution=%v anomalies=%d", rounds, procs, winners, len(bad))
	for _, b := range bad {
		t.Error(b)
	}
}

// C.3/C.6: a foreign process atomically saves (rename over) the destination at a random moment
// while the agent creates it; the foreign file must never be deleted.
func TestZZR3ForeignRenameReplaceRace(t *testing.T) {
	const rounds = 120
	agentOK, agentConflict, lost, other, overwriteOfExisting := 0, 0, 0, 0, 0
	for r := 0; r < rounds; r++ {
		root := t.TempDir()
		ctl := t.TempDir()
		start := filepath.Join(ctl, "start")
		foreign := []byte(fmt.Sprintf("FOREIGN SAVE %d\n", r))
		cmd, out := zzSpawn(t, "LC_R3_HELPER=foreign-rename", "LC_R3_ROOT="+root, "LC_R3_READY="+filepath.Join(ctl, "ready"),
			"LC_R3_START="+start, "LC_R3_CONTENT="+string(foreign), fmt.Sprintf("LC_R3_DELAY_US=%d", (r%12)*150))
		if !zzWaitFile(filepath.Join(ctl, "ready"), 20*time.Second) {
			t.Fatal("foreign not ready")
		}
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		agent := bytes.Repeat([]byte("agent bytes\n"), 20000)
		sawExisting := false
		e.afterRead = func(string) { sawExisting = true }
		_ = os.WriteFile(start, []byte("go"), 0o644)
		_, cerr := e.Commit(context.Background(), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
			Changes: []FileChange{{Path: "shared.txt", Content: &agent, AllowCreate: true}}})
		_ = cmd.Wait()
		fres := zzResult(out)
		got, rerr := os.ReadFile(filepath.Join(root, "shared.txt"))
		switch {
		case rerr != nil:
			lost++
			t.Errorf("round %d: destination missing after foreign save (%s) agent=%v", r, fres, cerr)
		case !bytes.Equal(got, foreign) && sawExisting:
			overwriteOfExisting++
		case !bytes.Equal(got, foreign):
			other++
			t.Errorf("round %d: final content is not the foreign save (len=%d) foreign=%s agent=%v", r, len(got), fres, cerr)
		}
		if cerr == nil {
			agentOK++
		} else if CodeOf(cerr) == CodeConflict {
			agentConflict++
		}
		if d := zzDebris(root); len(d) > 0 {
			t.Errorf("round %d: temp debris %v", r, d)
		}
	}
	t.Logf("foreign rename race: rounds=%d agentOK=%d agentConflict=%d lost=%d other=%d overwriteOfFileExistingAtPrepare=%d", rounds, agentOK, agentConflict, lost, other, overwriteOfExisting)
}

// Rollback of an already-published create in a two-file plan whose second file conflicts.
func TestZZR3RollbackOfPublishedCreate(t *testing.T) {
	agentA := []byte("agent A content\n")
	agentB := []byte("agent B content\n")
	cases := []struct {
		name    string
		mutateA func(t *testing.T, a string)
	}{
		{"untouched", func(*testing.T, string) {}},
		{"foreign_append_in_place", func(t *testing.T, a string) {
			f, err := os.OpenFile(a, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.Write([]byte("FOREIGN APPEND\n"))
			_ = f.Close()
		}},
		{"foreign_rename_replace_identical_bytes", func(t *testing.T, a string) {
			tmp := a + ".foreign"
			_ = os.WriteFile(tmp, agentA, 0o644)
			if err := os.Rename(tmp, a); err != nil {
				t.Fatal(err)
			}
		}},
		{"foreign_inplace_rewrite_identical_bytes", func(t *testing.T, a string) {
			f, err := os.OpenFile(a, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.Write(agentA)
			_ = f.Close()
		}},
		{"foreign_inplace_rewrite_identical_bytes_mtime_restored", func(t *testing.T, a string) {
			st, _ := os.Stat(a)
			f, err := os.OpenFile(a, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.Write(agentA)
			_ = f.Close()
			_ = os.Chtimes(a, st.ModTime(), st.ModTime())
		}},
		{"replaced_by_symlink_to_external", func(t *testing.T, a string) {
			ext := filepath.Join(filepath.Dir(filepath.Dir(a)), "external-"+filepath.Base(filepath.Dir(a)))
			_ = os.WriteFile(ext, []byte("EXTERNAL\n"), 0o644)
			_ = os.Remove(a)
			if err := os.Symlink(ext, a); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "ws")
			_ = os.MkdirAll(root, 0o755)
			e, err := New(Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			a := filepath.Join(root, "a.txt")
			e.beforeWrite = func(p string) {
				if filepath.Base(p) == "b.txt" {
					c.mutateA(t, a)
					_ = os.WriteFile(filepath.Join(root, "b.txt"), []byte("FOREIGN B\n"), 0o644)
				}
			}
			var receipt CommitReceipt
			_, cerr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r }), Plan{Actor: Actor{Session: "s"}, Tool: "apply_patch",
				Changes: []FileChange{{Path: "a.txt", Content: &agentA, AllowCreate: true}, {Path: "b.txt", Content: &agentB, AllowCreate: true}}})
			li, lerr := os.Lstat(a)
			state := "absent"
			if lerr == nil {
				if li.Mode()&os.ModeSymlink != 0 {
					tgt, _ := os.Readlink(a)
					body, _ := os.ReadFile(tgt)
					state = fmt.Sprintf("symlink->%s body=%q", filepath.Base(tgt), body)
				} else {
					body, _ := os.ReadFile(a)
					state = fmt.Sprintf("file body=%q", body)
				}
			}
			bBody, _ := os.ReadFile(filepath.Join(root, "b.txt"))
			t.Logf("%s: commit=%s dirty=%v committed=%v | a.txt %s | b.txt=%q | debris=%v", c.name, zzCode(cerr), receipt.DirtyFailure, receipt.Committed, state, bBody, zzDebris(root))
		})
	}
}

// C.7 and destination-exists-before-publication: a symlink or a foreign file appears at the
// destination after prepare but before publication.
func TestZZR3DestinationAppearsBeforePublish(t *testing.T) {
	cases := []struct {
		name   string
		appear func(t *testing.T, base, dst string)
	}{
		{"foreign_regular_file", func(t *testing.T, base, dst string) { _ = os.WriteFile(dst, []byte("FOREIGN\n"), 0o600) }},
		{"symlink_to_external_file", func(t *testing.T, base, dst string) {
			ext := filepath.Join(base, "external.txt")
			_ = os.WriteFile(ext, []byte("EXTERNAL\n"), 0o644)
			_ = os.Symlink(ext, dst)
		}},
		{"dangling_symlink_outside_workspace", func(t *testing.T, base, dst string) {
			_ = os.Symlink(filepath.Join(base, "would-be-created.txt"), dst)
		}},
		{"directory", func(t *testing.T, base, dst string) { _ = os.Mkdir(dst, 0o755) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "ws")
			_ = os.MkdirAll(root, 0o755)
			lh, _ := NewLocalHost(root)
			dst := filepath.Join(root, "new.txt")
			host := zzStager{LocalHost: lh, beforePublish: func(target string) error { c.appear(t, base, target); return nil }}
			e, _ := New(Options{Root: root, Host: host})
			agent := []byte("agent\n")
			var receipt CommitReceipt
			_, cerr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r }), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
				Changes: []FileChange{{Path: "new.txt", Content: &agent, AllowCreate: true}}})
			li, _ := os.Lstat(dst)
			var desc string
			if li != nil && li.Mode()&os.ModeSymlink != 0 {
				tgt, _ := os.Readlink(dst)
				desc = "symlink->" + filepath.Base(tgt)
			} else if li != nil && li.IsDir() {
				desc = "dir"
			} else if li != nil {
				body, _ := os.ReadFile(dst)
				desc = fmt.Sprintf("file %q mode=%v", body, li.Mode())
			}
			ext, _ := os.ReadFile(filepath.Join(base, "external.txt"))
			_, wbErr := os.Stat(filepath.Join(base, "would-be-created.txt"))
			t.Logf("%s: commit=%s dirty=%v | dst=%s | external=%q outsideCreated=%v | debris=%v", c.name, zzCode(cerr), receipt.DirtyFailure, desc, ext, wbErr == nil, zzDebris(root))
		})
	}
}

// C.5/C.8/C.9: publication-time faults injected between staging and publication.
func TestZZR3PublishFaults(t *testing.T) {
	cases := []struct {
		name  string
		fault func(t *testing.T, dir string)
	}{
		{"directory_removed", func(t *testing.T, dir string) { _ = os.RemoveAll(dir) }},
		{"directory_made_read_only", func(t *testing.T, dir string) { _ = os.Chmod(dir, 0o555) }},
		{"directory_replaced_by_symlink_outside", func(t *testing.T, dir string) {
			outside := filepath.Join(filepath.Dir(filepath.Dir(dir)), "outside")
			_ = os.MkdirAll(outside, 0o755)
			moved := dir + ".moved"
			_ = os.Rename(dir, moved)
			_ = os.Symlink(outside, dir)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "ws")
			sub := filepath.Join(root, "sub")
			_ = os.MkdirAll(sub, 0o755)
			lh, _ := NewLocalHost(root)
			host := zzStager{LocalHost: lh, beforePublish: func(target string) error { c.fault(t, filepath.Dir(target)); return nil }}
			e, _ := New(Options{Root: root, Host: host})
			agent := []byte("agent\n")
			var receipt CommitReceipt
			_, cerr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r }), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
				Changes: []FileChange{{Path: "sub/new.txt", Content: &agent, AllowCreate: true}}})
			_ = os.Chmod(sub, 0o755)
			var found []string
			_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					rel, _ := filepath.Rel(base, p)
					found = append(found, rel)
				}
				return nil
			})
			t.Logf("%s: commit=%s dirty=%v committed=%v err=%v | files under base=%v", c.name, zzCode(cerr), receipt.DirtyFailure, receipt.Committed, cerr, found)
		})
	}
}

// C.11/C.12: SIGKILL before and after publication.
func TestZZR3KillAroundPublication(t *testing.T) {
	for _, mode := range []string{"pause-before-publish", "pause-after-publish"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ctl := t.TempDir()
			ready := filepath.Join(ctl, "ready")
			cmd, _ := zzSpawn(t, "LC_R3_HELPER="+mode, "LC_R3_ROOT="+root, "LC_R3_READY="+ready, "LC_R3_CONTENT=agent bytes\n")
			if !zzWaitFile(ready, 20*time.Second) {
				t.Fatal("child not ready")
			}
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
			var found []string
			_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					st := info.Sys().(*syscall.Stat_t)
					found = append(found, fmt.Sprintf("%s(nlink=%d,size=%d)", info.Name(), st.Nlink, info.Size()))
				}
				return nil
			})
			t.Logf("%s: after SIGKILL files=%v", mode, found)
		})
	}
}

// C.4/C.10: a real filesystem write error (EFBIG under RLIMIT_FSIZE) while staging the temp file.
func TestZZR3TempWriteFailsEFBIG(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("x", 1023) + "\n"
	script := fmt.Sprintf("ulimit -f 64; exec %q -test.run='^TestZZR3Helper$' -test.count=1", os.Args[0])
	cmd := exec.Command("/bin/bash", "-c", script)
	cmd.Env = append(os.Environ(), "LC_R3_HELPER=commit-big", "LC_R3_ROOT="+root, "LC_R3_CONTENT="+content, "LC_R3_REPEAT=200")
	out, err := cmd.CombinedOutput()
	var found []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			found = append(found, fmt.Sprintf("%s(size=%d)", info.Name(), info.Size()))
		}
		return nil
	})
	res := zzResult(bytes.NewBuffer(out))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res += fmt.Sprintf(" (exit=%v)", exitErr.ProcessState)
	}
	t.Logf("EFBIG: child=%s | files=%v", res, found)
}
