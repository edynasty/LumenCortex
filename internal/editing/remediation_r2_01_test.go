package editing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestR201TwoOSProcessesCreateRace keeps the R2-01 exclusive-create safety case under R7 private
// staging across deterministic IPC barriers and real two-process stress loops. An independent OS
// process performs OpenFile(O_WRONLY|O_CREATE|O_EXCL) followed by Write(foreign), while Engine.Commit
// runs write_file (AllowCreate or Create). Every legitimate interleaving must preserve complete
// unmixed bytes on disk and produce an accurate CommitReceipt, Result, SnapshotStore, and
// conservative MutationEvidence classification (R7R2-NEW-02).
func TestR201TwoOSProcessesCreateRace(t *testing.T) {
	foreign := []byte("FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n")
	agentBytes := []byte("agent os process content\n")

	helperDir := t.TempDir()
	helperSrc := filepath.Join(helperDir, "main.go")
	helperBin := filepath.Join(helperDir, "foreign_writer")
	src := `package main
import (
	"errors"
	"os"
	"time"
)
var payload = []byte("FOREIGN OS PROCESS PAYLOAD 0123456789ABCDEF\n")

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
	mode := os.Args[1]
	target := os.Args[2]
	switch mode {
	case "excl_after_signal":
		ready, goSig := os.Args[3], os.Args[4]
		if err := os.WriteFile(ready, []byte("1"), 0o644); err != nil {
			os.Exit(2)
		}
		if !waitFile(goSig) {
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
		opened, writeGo := os.Args[3], os.Args[4]
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				os.Exit(10)
			}
			os.Exit(2)
		}
		if err := os.WriteFile(opened, []byte("1"), 0o644); err != nil {
			_ = f.Close()
			os.Exit(2)
		}
		if !waitFile(writeGo) {
			_ = f.Close()
			os.Exit(3)
		}
		if _, err := f.Write(payload); err != nil {
			_ = f.Close()
			os.Exit(4)
		}
		_ = f.Close()
		os.Exit(0)

	case "race_once":
		if _, err := os.Stdout.Write([]byte("R")); err != nil {
			os.Exit(2)
		}
		_ = os.Stdout.Close()
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

	case "atomic_race_once":
		extTmp := target + ".foreign-tmp"
		if err := os.WriteFile(extTmp, payload, 0o644); err != nil {
			os.Exit(2)
		}
		if _, err := os.Stdout.Write([]byte("R")); err != nil {
			_ = os.Remove(extTmp)
			os.Exit(2)
		}
		_ = os.Stdout.Close()
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

	waitForSignal := func(t *testing.T, path string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); err == nil {
				return
			}
			time.Sleep(100 * time.Microsecond)
		}
		t.Fatalf("timed out waiting for barrier file %s", path)
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
			if strings.HasPrefix(ent.Name(), ".lcx-create-") {
				t.Fatalf("unexpected private staging leftover: %s", ent.Name())
			}
		}
	}

	// assertRaceOutcome verifies all 11 invariants across every legitimate interleaving.
	// Note on Created vs Staged:
	// - rf.Staged == true means Engine.prepare saw p.existed == false and routed to createAtomicNoReplace.
	// - rf.Created == (p.published && p.created), so rf.Created is true ONLY when LC actually won
	//   no-replace publication; an in-place race after the child's O_EXCL create sets
	//   rf.Staged == false, rf.Created == false, and rf.OldSHA256 != "-" (e.g. sha256("") when 0B).
	assertRaceOutcome := func(t *testing.T, root, target string, e *Engine, actor Actor, res Result, rec CommitReceipt, commitErr error, childExit int, exclusiveCreate bool) {
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
		if !bytes.Equal(got, foreign) && !bytes.Equal(got, agentBytes) {
			t.Fatalf("mixed or corrupted destination bytes (len=%d): %q", len(got), got)
		}

		f, rf := r7Files(t, res, rec, "shared.txt")
		latest := e.LatestRev(actor, target)

		switch {
		case commitErr == nil:
			if !rec.Committed || rec.DirtyFailure || rec.State != CommitStateCommitted || res.CommitState != CommitStateCommitted {
				t.Fatalf("commitErr==nil with non-committed state: res=%+v rec=%+v", res, rec)
			}
			if !rf.Published || !rf.Wrote || !rf.Verified || rf.PublishState != PublishSucceededAndVerified {
				t.Fatalf("commitErr==nil with unverified receipt: %+v", rf)
			}
			if f.NewRev != RevOf(agentBytes) || rf.NewSHA256 != sha256Ref(agentBytes, true) || latest != RevOf(agentBytes) {
				t.Fatalf("verified commit has wrong revision/hash/snapshot: f=%+v rf=%+v latest=%q", f, rf, latest)
			}
			if rf.Staged {
				// Outcome A: LC won exclusive creation; external O_EXCL MUST have failed with EEXIST (exit 10).
				if childWrote || !rf.Created || !f.Created || rf.OldSHA256 != "-" {
					t.Fatalf("staged create win invariant violated: childWrote=%v f=%+v rf=%+v", childWrote, f, rf)
				}
				if !bytes.Equal(got, agentBytes) {
					t.Fatalf("LC won exclusive create but disk=%q", got)
				}
			} else {
				// External created the inode before prepare, so LC entered writeInPlace (Created=false).
				if exclusiveCreate {
					t.Fatalf("Plan{Create:true} must never succeed via writeInPlace: %+v", rf)
				}
				if !childWrote || rf.Created || f.Created || rf.OldSHA256 == "-" {
					t.Fatalf("in-place success invariant violated: childWrote=%v rf=%+v", childWrote, rf)
				}
			}

		case CodeOf(commitErr) == CodeExists:
			// Child won O_EXCL before prepare ran under Plan{Create:true}.
			if !childWrote || rf.Created || rf.Staged || rf.Published || rf.Wrote || rf.Verified || rec.DirtyFailure {
				t.Fatalf("CodeExists invariant violated: childWrote=%v rec=%+v", childWrote, rec)
			}
			if rf.PublishState != PublishFailedBeforeWrite || rec.State != CommitStatePrePublishFailure {
				t.Fatalf("CodeExists state mismatch: rec=%+v", rec)
			}
			if f.NewRev != "" || rf.NewSHA256 != "" || latest != "" || !bytes.Equal(got, foreign) {
				t.Fatalf("CodeExists fabricated post-rev or lost foreign bytes: f=%+v rf=%+v latest=%q got=%q", f, rf, latest, got)
			}

		case rf.Staged:
			// Outcome B1: External O_EXCL won while LC was staging private temp before no-replace publish.
			if !childWrote || CodeOf(commitErr) != CodeConflict || rec.DirtyFailure || rec.Committed || rec.State != CommitStatePrePublishFailure {
				t.Fatalf("staged no-replace loser invariant violated: err=%v childWrote=%v rec=%+v", commitErr, childWrote, rec)
			}
			if rf.Created || rf.Published || rf.Wrote || rf.Verified || rf.OldSHA256 != "-" || rf.PublishState != PublishExternalModification {
				t.Fatalf("staged no-replace loser row invariant violated: %+v", rf)
			}
			if f.NewRev != "" || rf.NewSHA256 != "" || latest != "" || !bytes.Equal(got, foreign) {
				t.Fatalf("staged no-replace loser fabricated post-rev or lost foreign bytes: f=%+v rf=%+v latest=%q got=%q", f, rf, latest, got)
			}

		default:
			// Outcome B2: External O_EXCL created the inode before Engine.prepare (so Created=false, Staged=false).
			// MutationEvidence remains MutationUnknown because rec.Committed is false (and DirtyFailure when true forces OutcomeUnknown).
			if !childWrote || rf.Created || f.Created || rf.OldSHA256 == "-" || rf.Verified || f.Verified {
				t.Fatalf("in-place conflict invariant violated: childWrote=%v f=%+v rf=%+v", childWrote, f, rf)
			}
			if f.NewRev != "" || rf.NewSHA256 != "" || latest != "" || !bytes.Equal(got, foreign) {
				t.Fatalf("in-place conflict fabricated post-rev or lost foreign bytes: f=%+v rf=%+v latest=%q got=%q", f, rf, latest, got)
			}
			if rf.PublishState != PublishExternalModification {
				t.Fatalf("expected PublishExternalModification, got %+v", rf)
			}
			if rec.DirtyFailure {
				// Outcome B2c (round-8 interleaving): write overlapped after Truncate(0) -> CodeConcurrentWrite.
				if CodeOf(commitErr) != CodeConcurrentWrite || rec.State != CommitStatePublishedWithPostError || !rf.Published || !rf.Wrote {
					t.Fatalf("dirty concurrent write conflict mismatch: err=%v rec=%+v", commitErr, rec)
				}
			} else {
				// Outcome B2b: detected in writeInPlace pre-Truncate CAS read -> CodeConflict, Wrote=false.
				if CodeOf(commitErr) != CodeConflict || rec.State != CommitStatePrePublishFailure || rf.Published || rf.Wrote {
					t.Fatalf("pre-truncate external conflict mismatch: err=%v rec=%+v", commitErr, rec)
				}
			}
		}
	}

	t.Run("outcome_a_lc_wins_exclusive_creation", func(t *testing.T) {
		root, ctrl := t.TempDir(), t.TempDir()
		target := filepath.Join(root, "shared.txt")
		ready, goSig := filepath.Join(ctrl, "ready"), filepath.Join(ctrl, "go")
		cmd := exec.Command(helperBin, "excl_after_signal", target, ready, goSig)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForSignal(t, ready)

		e := newTestEngine(t, root)
		actor := Actor{Session: "s-a1"}
		var childExit int
		waitChildAfterPublish := func(dst string) error {
			if dst == target {
				if err := os.WriteFile(goSig, []byte("1"), 0o644); err != nil {
					return err
				}
				childExit = childExitCode(t, cmd.Wait())
			}
			return nil
		}
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterLinkSuccess:   waitChildAfterPublish,
			AfterRenameSuccess: waitChildAfterPublish,
		})
		var rec CommitReceipt
		res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
			Actor:   actor,
			Tool:    "write_file",
			Changes: []FileChange{{Path: "shared.txt", Content: &agentBytes, AllowCreate: true}},
		})
		assertRaceOutcome(t, root, target, e, actor, res, rec, commitErr, childExit, false)
		if commitErr != nil || !rec.Files[0].Created || childExit != 10 {
			t.Fatalf("expected LC to win exclusive creation and child exit 10: err=%v childExit=%d rec=%+v", commitErr, childExit, rec)
		}
	})

	t.Run("outcome_b1_external_wins_before_no_replace_publish", func(t *testing.T) {
		root, ctrl := t.TempDir(), t.TempDir()
		target := filepath.Join(root, "shared.txt")
		ready, goSig := filepath.Join(ctrl, "ready"), filepath.Join(ctrl, "go")
		cmd := exec.Command(helperBin, "excl_after_signal", target, ready, goSig)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForSignal(t, ready)

		e := newTestEngine(t, root)
		actor := Actor{Session: "s-b1"}
		var childExit int
		e.SetPublishFaultHooks(PublishFaultHooks{
			BeforePublish: func(dst string) error {
				if dst == target {
					if err := os.WriteFile(goSig, []byte("1"), 0o644); err != nil {
						return err
					}
					childExit = childExitCode(t, cmd.Wait())
				}
				return nil
			},
		})
		var rec CommitReceipt
		res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
			Actor:   actor,
			Tool:    "write_file",
			Changes: []FileChange{{Path: "shared.txt", Content: &agentBytes, AllowCreate: true}},
		})
		assertRaceOutcome(t, root, target, e, actor, res, rec, commitErr, childExit, false)
		if CodeOf(commitErr) != CodeConflict || rec.DirtyFailure || !rec.Files[0].Staged || childExit != 0 {
			t.Fatalf("expected clean staged no-replace refusal: err=%v childExit=%d rec=%+v", commitErr, childExit, rec)
		}
	})

	t.Run("outcome_b2_external_creates_empty_writes_before_inplace_truncate", func(t *testing.T) {
		root, ctrl := t.TempDir(), t.TempDir()
		target := filepath.Join(root, "shared.txt")
		opened, writeGo := filepath.Join(ctrl, "opened"), filepath.Join(ctrl, "write_go")
		cmd := exec.Command(helperBin, "create_empty_wait_then_write", target, opened, writeGo)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForSignal(t, opened)

		e := newTestEngine(t, root)
		actor := Actor{Session: "s-b2"}
		var childExit int
		e.beforeWrite = func(dst string) {
			if dst == target {
				_ = os.WriteFile(writeGo, []byte("1"), 0o644)
				childExit = childExitCode(t, cmd.Wait())
			}
		}
		var rec CommitReceipt
		res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
			Actor:   actor,
			Tool:    "write_file",
			Changes: []FileChange{{Path: "shared.txt", Content: &agentBytes, AllowCreate: true}},
		})
		assertRaceOutcome(t, root, target, e, actor, res, rec, commitErr, childExit, false)
		if CodeOf(commitErr) != CodeConflict || rec.DirtyFailure || rec.Files[0].Wrote {
			t.Fatalf("expected clean pre-truncate in-place refusal: err=%v rec=%+v", commitErr, rec)
		}
	})

	t.Run("outcome_b3_external_creates_empty_overlaps_inplace_write", func(t *testing.T) {
		// Exact deterministic reproduction of the round-8 interleaving (R7R2-NEW-02):
		// child O_EXCL-created the 0-byte inode before prepare, so LC entered writeInPlace
		// (Staged=false, Created=false, OldSHA256=sha256("")), wrote agentBytes, and then child
		// wrote foreign (44B > 25B) before verifyActual. LC accurately sets DirtyFailure=true.
		root, ctrl := t.TempDir(), t.TempDir()
		target := filepath.Join(root, "shared.txt")
		opened, writeGo := filepath.Join(ctrl, "opened"), filepath.Join(ctrl, "write_go")
		cmd := exec.Command(helperBin, "create_empty_wait_then_write", target, opened, writeGo)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForSignal(t, opened)

		e := newTestEngine(t, root)
		actor := Actor{Session: "s-b3"}
		var childExit int
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterInPlaceWriteClose: func(dst string) error {
				if dst == target {
					if err := os.WriteFile(writeGo, []byte("1"), 0o644); err != nil {
						return err
					}
					childExit = childExitCode(t, cmd.Wait())
				}
				return nil
			},
		})
		var rec CommitReceipt
		res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
			Actor:   actor,
			Tool:    "write_file",
			Changes: []FileChange{{Path: "shared.txt", Content: &agentBytes, AllowCreate: true}},
		})
		assertRaceOutcome(t, root, target, e, actor, res, rec, commitErr, childExit, false)
		if CodeOf(commitErr) != CodeConcurrentWrite || !rec.DirtyFailure || rec.Files[0].Created || rec.Files[0].Staged || !rec.Files[0].Wrote {
			t.Fatalf("expected DirtyFailure=true external_modification_detected on B3 overlap: err=%v rec=%+v", commitErr, rec)
		}
	})

	runStress := func(t *testing.T, exclusiveCreate bool, childMode string) {
		t.Helper()
		const rounds = 30
		var childWins, agentWins, preflightExists, stagedConflicts, inplaceConflicts, dirtyOverlaps int
		for i := range rounds {
			root := t.TempDir()
			target := filepath.Join(root, "shared.txt")
			e := newTestEngine(t, root)
			actor := Actor{Session: "s-stress"}
			cmd := exec.Command(helperBin, childMode, target)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var ready [1]byte
			if _, err := io.ReadFull(stdout, ready[:]); err != nil {
				_ = cmd.Wait()
				t.Fatalf("round %d pipe readiness: %v", i, err)
			}
			_ = stdout.Close()

			var rec CommitReceipt
			res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
				Actor:   actor,
				Tool:    "write_file",
				Changes: []FileChange{{Path: "shared.txt", Content: &agentBytes, Create: exclusiveCreate, AllowCreate: !exclusiveCreate}},
			})
			childExit := childExitCode(t, cmd.Wait())
			assertRaceOutcome(t, root, target, e, actor, res, rec, commitErr, childExit, exclusiveCreate)

			got, _ := os.ReadFile(target)
			if bytes.Equal(got, foreign) {
				childWins++
			} else {
				agentWins++
			}
			switch {
			case commitErr == nil:
			case CodeOf(commitErr) == CodeExists:
				preflightExists++
			case rec.Files[0].Staged:
				stagedConflicts++
			case !rec.DirtyFailure:
				inplaceConflicts++
			default:
				dirtyOverlaps++
			}
		}
		t.Logf("stress (Create=%v, mode=%s): rounds=%d childWins=%d agentWins=%d preflightExists=%d stagedConflicts=%d inplaceConflicts=%d dirtyOverlaps=%d",
			exclusiveCreate, childMode, rounds, childWins, agentWins, preflightExists, stagedConflicts, inplaceConflicts, dirtyOverlaps)
	}

	t.Run("concurrent_exclusive_create_stress", func(t *testing.T) {
		runStress(t, true, "race_once")
	})

	t.Run("concurrent_atomic_allow_create_stress", func(t *testing.T) {
		runStress(t, false, "atomic_race_once")
	})
}

// TestR201ExistingCASConflictPreservesForeignBytes pins the guarded in-place path without rollback:
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
