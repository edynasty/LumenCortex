package editing

// Review probe (throwaway; /tmp only): run the atomic create path on a filesystem given by
// LC_R3_FSROOT (exFAT/FAT/HFS+/overlayfs/tmpfs mounts) and report its publish semantics.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZZR3FSMatrix(t *testing.T) {
	fsroot := os.Getenv("LC_R3_FSROOT")
	if fsroot == "" {
		t.Skip("set LC_R3_FSROOT")
	}
	base, err := os.MkdirTemp(fsroot, "zz-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)

	// Raw primitives.
	a := filepath.Join(base, "prim-a")
	_ = os.WriteFile(a, []byte("a"), 0o644)
	linkErr := os.Link(a, filepath.Join(base, "prim-link"))
	_ = os.WriteFile(filepath.Join(base, "prim-exists"), []byte("x"), 0o644)
	renExistErr := renameNoReplacePlatform(a, filepath.Join(base, "prim-exists"))
	renNewErr := renameNoReplacePlatform(a, filepath.Join(base, "prim-new"))
	t.Logf("primitives: os.Link=%v isLinkFallbackErr=%v | renameNoReplace(existing)=%v | renameNoReplace(new)=%v", linkErr, linkErr != nil && isLinkFallbackErr(linkErr), renExistErr, renNewErr)

	// Engine create + conflict.
	root := filepath.Join(base, "ws")
	_ = os.MkdirAll(root, 0o755)
	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("created by agent\n")
	var receipt CommitReceipt
	_, cerr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r }), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
		Changes: []FileChange{{Path: "new.txt", Content: &body, AllowCreate: true}}})
	got, rerr := os.ReadFile(filepath.Join(root, "new.txt"))
	t.Logf("engine create: commit=%s err=%v committed=%v dirty=%v | file=%q readErr=%v | debris=%v", zzCode(cerr), cerr, receipt.Committed, receipt.DirtyFailure, got, rerr, zzDebris(root))

	lh, _ := NewLocalHost(root)
	host := zzStager{LocalHost: lh, beforePublish: func(target string) error {
		return os.WriteFile(target, []byte("FOREIGN\n"), 0o644)
	}}
	e2, _ := New(Options{Root: root, Host: host})
	receipt = CommitReceipt{}
	_, cerr = e2.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r }), Plan{Actor: Actor{Session: "s"}, Tool: "write_file",
		Changes: []FileChange{{Path: "race.txt", Content: &body, AllowCreate: true}}})
	got, _ = os.ReadFile(filepath.Join(root, "race.txt"))
	t.Logf("engine create vs foreign create before publish: commit=%s dirty=%v | file=%q | debris=%v", zzCode(cerr), receipt.DirtyFailure, got, zzDebris(root))

	// Multi-process race on this filesystem.
	const rounds, procs = 25, 4
	anomalies := 0
	var tot struct{ creators, overwriters, conflicts, others int }
	for r := 0; r < rounds; r++ {
		rroot := filepath.Join(base, fmt.Sprintf("race-%d", r))
		_ = os.MkdirAll(rroot, 0o755)
		ctl := t.TempDir()
		start := filepath.Join(ctl, "start")
		var cmds []*exec.Cmd
		var outs []*bytes.Buffer
		var contents [][]byte
		for k := 0; k < procs; k++ {
			unit := fmt.Sprintf("P%d-R%d|", k, r)
			cmd, out := zzSpawn(t, "LC_R3_HELPER=commit", "LC_R3_ROOT="+rroot, "LC_R3_READY="+filepath.Join(ctl, fmt.Sprintf("ready%d", k)),
				"LC_R3_START="+start, "LC_R3_CONTENT="+unit, "LC_R3_REPEAT=3000")
			cmds, outs, contents = append(cmds, cmd), append(outs, out), append(contents, bytes.Repeat([]byte(unit), 3000))
		}
		for k := 0; k < procs; k++ {
			if !zzWaitFile(filepath.Join(ctl, fmt.Sprintf("ready%d", k)), 20*time.Second) {
				t.Fatalf("child %d not ready", k)
			}
		}
		_ = os.WriteFile(start, []byte("go"), 0o644)
		creators, overwriters, conflicts, others := 0, 0, 0, 0
		okIdx := map[int]bool{}
		for k := range cmds {
			_ = cmds[k].Wait()
			res := zzResult(outs[k])
			switch {
			case strings.HasPrefix(res, "ok ") && strings.Contains(res, "sawExistingAtPrepare=false"):
				creators++
				okIdx[k] = true
			case strings.HasPrefix(res, "ok "):
				overwriters++
				okIdx[k] = true
			case strings.HasPrefix(res, "conflict ") || strings.HasPrefix(res, "concurrent_write_detected "):
				conflicts++
			default:
				others++
				if others <= 2 {
					t.Logf("round %d child %d: %s", r, k, res)
				}
			}
		}
		final, ferr := os.ReadFile(filepath.Join(rroot, "shared.txt"))
		finalIsAnOkWriter := false
		for k := range okIdx {
			if ferr == nil && bytes.Equal(final, contents[k]) {
				finalIsAnOkWriter = true
			}
		}
		tot.creators += creators
		tot.overwriters += overwriters
		tot.conflicts += conflicts
		tot.others += others
		if creators != 1 || !finalIsAnOkWriter || len(zzDebris(rroot)) > 0 {
			anomalies++
			if anomalies <= 6 {
				t.Logf("round %d ANOMALY: creators=%d overwriters=%d conflicts=%d others=%d finalIsAnOkWriter=%v finalErr=%v debris=%v", r, creators, overwriters, conflicts, others, finalIsAnOkWriter, ferr, zzDebris(rroot))
			}
		}
	}
	t.Logf("multi-process race on %s: rounds=%d procs=%d exactlyOneCreatorViolations(anomalies)=%d creators=%d overwritersOfExistingAtPrepare=%d conflicts=%d others=%d", fsroot, rounds, procs, anomalies, tot.creators, tot.overwriters, tot.conflicts, tot.others)
}
