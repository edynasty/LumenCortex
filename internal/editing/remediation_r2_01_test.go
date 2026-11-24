package editing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestR201TwoOSProcessesCreateRace keeps the R2-01 exclusive-create safety case under R7 private
// staging. An independent OS process creates the target, by OpenFile(O_WRONLY|O_CREATE|O_EXCL)
// followed by Write(foreign) or by linking a complete file into place, while Engine.Commit runs
// write_file (AllowCreate or Create) on the same path. The external process reports on a pipe that
// it reached its starting point and is let go, on another pipe, at a named point of the commit. The
// deterministic subtests then wait for it to exit, which fixes the interleaving; the stress subtests
// do not, so the two processes really race from that point on. Nothing waits on a clock.
// Every round is checked against the complete set of interleavings the two processes can produce:
// the bytes on disk, the CommitReceipt, the Result and the SnapshotStore (R7R2-NEW-02).
func TestR201TwoOSProcessesCreateRace(t *testing.T) {
	foreign := []byte("FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n")
	agentBytes := []byte("agent os process content\n")
	// What an external write of foreign into a truncated file would leave if in-place truncation mixed bytes.
	mixedBytes := append(append([]byte(nil), agentBytes...), foreign[len(agentBytes):]...)

	helperDir := t.TempDir()
	helperSrc := filepath.Join(helperDir, "main.go")
	helperBin := filepath.Join(helperDir, "foreign_writer")
	src := `package main
import (
	"errors"
	"os"
)
var payload = []byte("FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n")

// ready tells the test this process reached its starting point: one byte on stdout.
func ready() bool {
	_, err := os.Stdout.Write([]byte{1})
	return err == nil
}

// released blocks until the test lets this process go: one byte on stdin.
func released() bool {
	var b [1]byte
	n, err := os.Stdin.Read(b[:])
	return n == 1 && err == nil
}

func main() {
	mode, target := os.Args[1], os.Args[2]
	switch mode {
	case "excl_after_signal":
		if !ready() {
			os.Exit(2)
		}
		if !released() {
			os.Exit(3)
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				os.Exit(10)
			}
			os.Exit(2)
		}
		if _, err := f.Write(payload); err != nil {
			_ = f.Close()
			os.Exit(4)
		}
		_ = f.Close()
		os.Exit(0)

	case "create_empty_wait_then_write":
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				os.Exit(10)
			}
			os.Exit(2)
		}
		if !ready() {
			_ = f.Close()
			os.Exit(2)
		}
		if !released() {
			_ = f.Close()
			os.Exit(3)
		}
		if _, err := f.Write(payload); err != nil {
			_ = f.Close()
			os.Exit(4)
		}
		_ = f.Close()
		os.Exit(0)

	case "atomic_after_signal":
		extTmp := target + ".foreign-tmp"
		if err := os.WriteFile(extTmp, payload, 0o644); err != nil {
			os.Exit(2)
		}
		if !ready() {
			_ = os.Remove(extTmp)
			os.Exit(2)
		}
		if !released() {
			_ = os.Remove(extTmp)
			os.Exit(3)
		}
		err := os.Link(extTmp, target)
		_ = os.Remove(extTmp)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				os.Exit(10)
			}
			os.Exit(2)
		}
		os.Exit(0)

	default:
		os.Exit(5)
	}
}
`
	if err := os.WriteFile(helperSrc, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	buildCmd := exec.Command("go", "build", "-o", helperBin, helperSrc)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper binary failed: %v\n%s", err, out)
	}

	childExitCode := func(t *testing.T, err error) int {
		t.Helper()
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("child failed unexpectedly: %v", err)
		}
		return ee.ExitCode()
	}

	assertNoStagingLeftovers := func(t *testing.T, root string) {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range entries {
			if strings.HasPrefix(ent.Name(), ".lcx-") {
				t.Fatalf("unexpected private staging leftover: %s", ent.Name())
			}
		}
	}

	// The outcomes a round can end in. Staged means prepare saw no file and LC staged a private
	// temp for no-replace publication; otherwise an external inode already existed at prepare and
	// LC ran staged atomic swap over it.
	const (
		// LC published its staged file; the external create lost with EEXIST.
		lcCreated = "lc_created"
		// Plan{Create:true} found the external file while preparing.
		existsBeforePrepare = "exists_before_prepare"
		// The external file appeared while LC staged; no-replace publication was refused.
		stagedPublishRefused = "staged_publish_refused"
		// The external file was complete when LC read it; LC replaced the bytes it had read.
		lcReplacedReadForeign = "lc_replaced_read_foreign"
		// The external write landed before LC's swap; LC wrote nothing to the destination.
		inplaceRefusedBeforeTruncate = "inplace_refused_before_truncate"
		// The external write overlapped LC's swap window; LC restored the intact foreign bytes.
		inplaceOverlapForeignWhole = "inplace_overlap_foreign_whole"
		// The external write landed after LC verified its own bytes on the same inode.
		externalWriteAfterVerified = "external_write_after_verified"
		// LC verified its atomic swap while the pre-existing external descriptor was still empty;
		// the late external write landed on the displaced unlinked descriptor after Commit returned.
		displacedOpenFDAfterVerified = "displaced_open_fd_after_verified"
		// Legacy broken outcomes kept out of valid sets so any regression fails immediately.
		pre02MixedReportedDirty       = "pre02_mixed_bytes_reported_dirty"
		pre02ExternalWriteOverwritten = "pre02_external_write_overwritten"
	)

	// raceOutcome checks one finished round against every interleaving the two processes can produce
	// and returns its name; a round that fits none fails. exclusive is Plan{Create:true}. atomic is an
	// external process that links a complete file and never writes to the published inode.
	raceOutcome := func(t *testing.T, root, target string, preCommitInfo os.FileInfo, e *Engine, actor Actor, res Result, rec CommitReceipt, commitErr error, childExit int, exclusive, atomic bool) string {
		t.Helper()
		if childExit != 0 && childExit != 10 {
			t.Fatalf("child exited with unexpected code %d", childExit)
		}
		childWrote := childExit == 0
		assertNoStagingLeftovers(t, root)

		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("destination missing (lost content): err=%v commitErr=%v childExit=%d", readErr, commitErr, childExit)
		}
		f, rf := r7Files(t, res, rec, "shared.txt")
		latest := e.LatestRev(actor, target)
		emptySHA, foreignSHA := sha256Ref(nil, true), sha256Ref(foreign, true)
		if res.CommitState != rec.State || rec.Committed != (commitErr == nil) {
			t.Fatalf("result, receipt and error disagree: err=%v res=%+v rec=%+v", commitErr, res, rec)
		}

		if commitErr == nil {
			if rec.DirtyFailure || rec.State != CommitStateCommitted {
				t.Fatalf("commitErr==nil with non-committed state: res=%+v rec=%+v", res, rec)
			}
			if !rf.Published || !rf.Wrote || !rf.Verified || rf.PublishState != PublishSucceededAndVerified {
				t.Fatalf("commitErr==nil with unverified receipt: %+v", rf)
			}
			if f.NewRev != RevOf(agentBytes) || rf.NewSHA256 != sha256Ref(agentBytes, true) || latest != RevOf(agentBytes) {
				t.Fatalf("verified commit has wrong revision/hash/snapshot: f=%+v rf=%+v latest=%q", f, rf, latest)
			}
			if rf.Staged {
				// Exactly one creator: LC. The external create must have failed with EEXIST.
				if childWrote || !rf.Created || !f.Created || rf.OldSHA256 != "-" || !bytes.Equal(got, agentBytes) {
					t.Fatalf("staged create win invariant violated: childWrote=%v f=%+v rf=%+v disk=%q", childWrote, f, rf, got)
				}
				return lcCreated
			}
			// Exactly one creator: the external process. LC updated the existing path.
			if exclusive {
				t.Fatalf("Plan{Create:true} must never succeed via existing-file update: %+v", rf)
			}
			if !childWrote || rf.Created || f.Created {
				t.Fatalf("existing-file update success invariant violated: childWrote=%v f=%+v rf=%+v", childWrote, f, rf)
			}
			switch {
			case rf.OldSHA256 == foreignSHA && f.OldRev == RevOf(foreign) && bytes.Equal(got, agentBytes):
				return lcReplacedReadForeign
			case atomic:
				t.Fatalf("an atomically published external file was replaced without being read whole: rf=%+v disk=%q", rf, got)
			case rf.OldSHA256 == emptySHA && bytes.Equal(got, foreign):
				return externalWriteAfterVerified
			case rf.OldSHA256 == emptySHA && bytes.Equal(got, agentBytes):
				if afterInfo, statErr := os.Lstat(target); statErr == nil && preCommitInfo != nil && !os.SameFile(preCommitInfo, afterInfo) {
					return displacedOpenFDAfterVerified
				}
				return pre02ExternalWriteOverwritten
			}
			t.Fatalf("update success left unexplained bytes: rf=%+v disk=%q", rf, got)
		}

		// A failed commit. The external process created the file, LC claims no creation, and nothing
		// LC reports can be mistaken for a verified write: no revision, no hash, no snapshot, and a
		// receipt that is not Committed, which the agent seals as MutationUnknown.
		if !childWrote || rf.Created || f.Created || rf.Verified || f.Verified {
			t.Fatalf("failed commit ownership invariant violated: childWrote=%v f=%+v rf=%+v", childWrote, f, rf)
		}
		if f.NewRev != "" || rf.NewSHA256 != "" || latest != "" {
			t.Fatalf("failed commit fabricated a post-revision: f=%+v rf=%+v latest=%q", f, rf, latest)
		}
		clean := !rec.DirtyFailure && rec.State == CommitStatePrePublishFailure && !rf.Published && !rf.Wrote
		switch {
		case CodeOf(commitErr) == CodeExists:
			if !exclusive || !clean || rf.Staged || rf.PublishState != PublishFailedBeforeWrite || !bytes.Equal(got, foreign) {
				t.Fatalf("CodeExists invariant violated: rec=%+v disk=%q", rec, got)
			}
			return existsBeforePrepare
		case rf.Staged:
			if CodeOf(commitErr) != CodeConflict || !clean || rf.OldSHA256 != "-" || rf.PublishState != PublishExternalModification || !bytes.Equal(got, foreign) {
				t.Fatalf("staged no-replace loser invariant violated: err=%v rec=%+v disk=%q", commitErr, rec, got)
			}
			return stagedPublishRefused
		case exclusive || atomic:
			t.Fatalf("unexpected existing-file failure: exclusive=%v atomic=%v err=%v rec=%+v", exclusive, atomic, commitErr, rec)
		case rf.OldSHA256 != emptySHA || rf.PublishState != PublishExternalModification:
			t.Fatalf("existing-file conflict invariant violated: err=%v rec=%+v", commitErr, rec)
		case !rec.DirtyFailure:
			if CodeOf(commitErr) != CodeConflict || !clean || !bytes.Equal(got, foreign) {
				t.Fatalf("pre-swap external conflict mismatch: err=%v rec=%+v disk=%q", commitErr, rec, got)
			}
			return inplaceRefusedBeforeTruncate
		}
		if CodeOf(commitErr) != CodeConcurrentWrite || rec.State != CommitStatePublishedWithPostError || !rf.Published || !rf.Wrote {
			t.Fatalf("dirty concurrent write conflict mismatch: err=%v rec=%+v", commitErr, rec)
		}
		switch {
		case bytes.Equal(got, foreign):
			return inplaceOverlapForeignWhole
		case bytes.Equal(got, mixedBytes):
			return pre02MixedReportedDirty
		}
		t.Fatalf("dirty failure left unexplained bytes (len=%d): %q", len(got), got)
		return ""
	}

	// Where in LC's commit the external process is let go.
	const (
		beforeCommit    = "before_commit"     // the commit has not started
		afterPrepare    = "after_prepare"     // LC has read the target's state
		beforePublish   = "before_publish"    // LC is about to publish its staged file
		afterPublish    = "after_publish"     // LC published its staged file
		afterTruncate   = "after_truncate"    // LC reached the pre-swap / truncate fault boundary
		afterWriteClose = "after_write_close" // LC swapped/wrote the file and closed its staged handle
	)

	// runRound starts the external process in mode, lets it go at the release point and returns the
	// round's outcome. With wait the release also waits for the external process to exit before the
	// commit continues.
	runRound := func(t *testing.T, exclusive bool, mode, release string, wait bool) string {
		t.Helper()
		root := t.TempDir()
		target := filepath.Join(root, "shared.txt")
		cmd := exec.Command(helperBin, mode, target)
		goSig, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		readySig, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer goSig.Close()
		var ready [1]byte
		if _, err := io.ReadFull(readySig, ready[:]); err != nil {
			_ = goSig.Close()
			t.Fatalf("external process did not reach its starting point: %v (wait: %v)", err, cmd.Wait())
		}

		e := newTestEngine(t, root)
		actor := Actor{Session: "s-race"}
		released, childExit := false, -1
		letGo := func(dst string) error {
			if dst != target || released {
				return nil
			}
			released = true
			if _, err := goSig.Write([]byte{1}); err != nil {
				return err
			}
			if wait {
				childExit = childExitCode(t, cmd.Wait())
			}
			return nil
		}
		switch release {
		case beforeCommit:
			if err := letGo(target); err != nil {
				t.Fatal(err)
			}
		case afterPrepare:
			e.beforeWrite = func(dst string) {
				if err := letGo(dst); err != nil {
					t.Error(err)
				}
			}
		case beforePublish:
			e.SetPublishFaultHooks(PublishFaultHooks{BeforePublish: letGo})
		case afterPublish:
			e.SetPublishFaultHooks(PublishFaultHooks{AfterLinkSuccess: letGo, AfterRenameSuccess: letGo})
		case afterTruncate:
			e.SetPublishFaultHooks(PublishFaultHooks{AfterTruncate: letGo})
		case afterWriteClose:
			e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose: letGo})
		default:
			t.Fatalf("unknown release point %q", release)
		}
		preCommitInfo, _ := os.Lstat(target)
		var rec CommitReceipt
		res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
			Actor:   actor,
			Tool:    "write_file",
			Changes: []FileChange{{Path: "shared.txt", Content: &agentBytes, Create: exclusive, AllowCreate: !exclusive}},
		})
		if !released {
			_ = goSig.Close()
			_ = cmd.Wait()
			t.Fatalf("the commit never reached release point %q: err=%v rec=%+v", release, commitErr, rec)
		}
		if !wait {
			childExit = childExitCode(t, cmd.Wait())
		}
		return raceOutcome(t, root, target, preCommitInfo, e, actor, res, rec, commitErr, childExit, exclusive, mode == "atomic_after_signal")
	}

	// One interleaving each: the external process runs to its end at the release point.
	for _, tc := range []struct {
		name          string
		exclusive     bool
		mode, release string
		want          string
	}{
		{"outcome_a_lc_wins_exclusive_creation", false, "excl_after_signal", afterPublish, lcCreated},
		{"outcome_b0_external_wins_before_exclusive_create_prepare", true, "excl_after_signal", beforeCommit, existsBeforePrepare},
		{"outcome_b1_external_wins_before_no_replace_publish", false, "excl_after_signal", beforePublish, stagedPublishRefused},
		{"outcome_b2_external_creates_empty_writes_before_inplace_truncate", false, "create_empty_wait_then_write", afterPrepare, inplaceRefusedBeforeTruncate},
		// LC swapped its staged file and the external write landed on the displaced descriptor before
		// post-swap verification, so LC restored the intact 44B foreign file and reported dirty failure.
		{"outcome_b3_external_creates_empty_overlaps_inplace_write", false, "create_empty_wait_then_write", afterWriteClose, inplaceOverlapForeignWhole},
		// Former PRE-02 window: staged atomic swap re-checks the live descriptor after AfterTruncate
		// and refuses cleanly before swapping, leaving the external bytes untouched and never mixed.
		{"outcome_b4_external_creates_empty_writes_inside_truncate_window", false, "create_empty_wait_then_write", afterTruncate, inplaceRefusedBeforeTruncate},
		// The external file was whole before LC looked: LC replaces it and records what it read.
		{"outcome_b5_external_complete_before_prepare_replaced_as_read", false, "excl_after_signal", beforeCommit, lcReplacedReadForeign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == inplaceOverlapForeignWhole || tc.want == lcReplacedReadForeign {
				requireExistingFileUpdate(t) // these outcomes replace the existing external file
			}
			if got := runRound(t, tc.exclusive, tc.mode, tc.release, true); got != tc.want {
				t.Fatalf("outcome %q, want %q", got, tc.want)
			}
		})
	}

	// The two processes race from the release point on. Each step names the outcomes its release
	// point leaves possible: what LC had already observed or done when the external process was let
	// go rules the others out.
	type raceStep struct {
		mode, release string
		possible      []string
	}
	runStress := func(t *testing.T, exclusive bool, steps []raceStep) {
		t.Helper()
		const roundsPerStep = 10
		for _, step := range steps {
			counts := map[string]int{}
			for range roundsPerStep {
				got := runRound(t, exclusive, step.mode, step.release, false)
				if !slices.Contains(step.possible, got) {
					t.Fatalf("%s released %s ended in %q, possible: %v", step.mode, step.release, got, step.possible)
				}
				counts[got]++
			}
			names := make([]string, 0, len(counts))
			for name := range counts {
				names = append(names, name)
			}
			slices.Sort(names)
			var tally strings.Builder
			for _, name := range names {
				fmt.Fprintf(&tally, " %s=%d", name, counts[name])
			}
			t.Logf("stress (Create=%v) %s released %s: rounds=%d%s", exclusive, step.mode, step.release, roundsPerStep, tally.String())
		}
	}
	staged := []string{lcCreated, stagedPublishRefused}
	inPlace := []string{inplaceRefusedBeforeTruncate, inplaceOverlapForeignWhole, externalWriteAfterVerified, displacedOpenFDAfterVerified}

	t.Run("concurrent_exclusive_create_stress", func(t *testing.T) {
		runStress(t, true, []raceStep{
			{"excl_after_signal", beforeCommit, append([]string{existsBeforePrepare}, staged...)},
			{"excl_after_signal", afterPrepare, staged},
			{"excl_after_signal", beforePublish, staged},
		})
	})

	t.Run("concurrent_atomic_allow_create_stress", func(t *testing.T) {
		for _, step := range []raceStep{
			{"atomic_after_signal", beforeCommit, append([]string{lcReplacedReadForeign}, staged...)},
			{"atomic_after_signal", afterPrepare, staged},
			{"atomic_after_signal", beforePublish, staged},
		} {
			t.Run(step.release, func(t *testing.T) {
				if step.release == beforeCommit {
					requireExistingFileUpdate(t)
				}
				runStress(t, false, []raceStep{step})
			})
		}
	})

	// The original R2-01 scenario: AllowCreate against an external create followed by a write. With
	// EDITING-PRE-02 closed via staged atomic swap, pre02MixedReportedDirty and pre02ExternalWriteOverwritten
	// are excluded from valid outcomes.
	t.Run("concurrent_two_syscall_allow_create_stress", func(t *testing.T) {
		for _, step := range []raceStep{
			{"excl_after_signal", beforeCommit, append(append([]string{lcReplacedReadForeign}, staged...), inPlace...)},
			{"excl_after_signal", afterPrepare, staged},
			{"excl_after_signal", beforePublish, staged},
			{"create_empty_wait_then_write", beforeCommit, append([]string{lcReplacedReadForeign}, inPlace...)},
			{"create_empty_wait_then_write", afterPrepare, inPlace},
			{"create_empty_wait_then_write", afterTruncate, []string{inplaceRefusedBeforeTruncate, inplaceOverlapForeignWhole, externalWriteAfterVerified, displacedOpenFDAfterVerified}},
			{"create_empty_wait_then_write", afterWriteClose, []string{inplaceOverlapForeignWhole, externalWriteAfterVerified, displacedOpenFDAfterVerified}},
		} {
			t.Run(step.mode+"/"+step.release, func(t *testing.T) {
				if step.mode == "create_empty_wait_then_write" || step.release == beforeCommit {
					requireExistingFileUpdate(t)
				}
				runStress(t, false, []raceStep{step})
			})
		}
	})
}

// TestR201ExistingCASConflictPreservesForeignBytes pins the guarded update path without rollback:
// the foreign bytes stay and the receipt reports an external change, not a write.
func TestR201ExistingCASConflictPreservesForeignBytes(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("FOREIGN WRITE WINS\n")
	e.beforeWrite = func(string) {
		if err := os.WriteFile(target, foreign, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	next := []byte("agent write\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, commitErr := e.Commit(ctx, Plan{
		Actor:   Actor{Session: "s1"},
		Tool:    "write_file",
		Changes: []FileChange{{Path: "existing.txt", Content: &next}},
	})
	if CodeOf(commitErr) != CodeConflict {
		t.Fatalf("expected CodeConflict, got %v", commitErr)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign bytes lost: err=%v got=%q", err, got)
	}
	f, r := r7Files(t, res, rec, "existing.txt")
	if f.PublishState != PublishExternalModification || f.Published || r.Wrote || f.Verified || r.NewSHA256 != "" {
		t.Fatalf("CAS refusal: %+v %+v", f, r)
	}
}
