//go:build unix

package runtime

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// TestHelperKillDuringDeletion is the child process of TestKilledDeletionIsFinishedAtOpen and
// TestKilledBlockedDeletionIsFinishedOnceTheReaderIsGone: it deletes the session and stops at the
// chosen point - "rows" after the first batch, "rebuild" once the session rows are gone and the file
// still owes its rebuild (both after a committed PurgeStep, reporting "held"), "blocked" once
// DeleteSession has returned ErrDeletionPending because another process's reader blocks the WAL
// truncation (reporting "pending") - and waits to be killed.
func TestHelperKillDuringDeletion(t *testing.T) {
	workspace := os.Getenv(crashWorkspaceEnv)
	if workspace == "" {
		t.Skip("helper process only")
	}
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	id, point := os.Getenv(crashSessionEnv), os.Getenv(crashPointEnv)
	if point == "blocked" {
		_, err := engine.DeleteSession(context.Background(), id, WorktreeCleanup{})
		if !errors.Is(err, ErrDeletionPending) {
			t.Fatalf("delete while another process reads: %v, want ErrDeletionPending", err)
		}
		os.Stdout.WriteString("pending\n")
		time.Sleep(5 * time.Minute)
		os.Exit(4) // not reached: the parent kills this process
	}
	testHookPurgeStep = func() {
		_, err := engine.store.Get(context.Background(), id)
		if point == "rows" || (point == "rebuild" && errors.Is(err, session.ErrNotFound)) {
			os.Stdout.WriteString("held\n")
			time.Sleep(5 * time.Minute)
			os.Exit(4) // not reached: the parent kills this process
		}
	}
	_, err = engine.DeleteSession(context.Background(), id, WorktreeCleanup{})
	t.Fatalf("the deletion finished before the kill point %s: %v", point, err)
}

// startDeletionHelper runs TestHelperKillDuringDeletion at point and waits for its report.
func startDeletionHelper(t *testing.T, f deletionFixture, point, report string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperKillDuringDeletion$", "-test.count=1")
	cmd.Env = append(os.Environ(), crashWorkspaceEnv+"="+f.workspace, crashSessionEnv+"="+f.child, crashPointEnv+"="+point)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	line := make(chan string, 1)
	go func() { text, _ := bufio.NewReader(out).ReadString('\n'); line <- text }()
	select {
	case text := <-line:
		if text != report+"\n" {
			t.Fatalf("child said %q, want %q", text, report)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("child did not reach the kill point %s", point)
	}
	return cmd
}

// Plan T3-S4 with a reader in another process: the deleting process finds the WAL truncation
// blocked, reports ErrDeletionPending and is killed with SIGKILL. The deletion stays cleanup_pending
// and the content is still at rest. An Open while the reader is still there does not finish it and
// does not wait for the reader without bound; once the reader is gone, either the next Open
// ("open") or the next DeleteSession ("delete") finishes it, a second Open has nothing to resume,
// and deleting again finds nothing.
func TestKilledBlockedDeletionIsFinishedOnceTheReaderIsGone(t *testing.T) {
	shortCheckpointWait(t)
	for _, finish := range []string{"open", "delete"} {
		t.Run(finish, func(t *testing.T) {
			f := newDeletionFixture(t)
			path := f.engine.store.Path()
			f.supervisor.Close()
			f.engine.Close()

			reader := holdWALReader(t, path)
			cmd := startDeletionHelper(t, f, "blocked", "pending")
			killHolder(t, cmd)
			if len(sentinelAtRest(t, path)) == 0 {
				t.Fatal("fixture: the reader did not keep the content at rest")
			}
			if !markerExists(t, path) {
				t.Fatal("checkpoint marker removed before the truncation")
			}

			ctx := context.Background()
			began := time.Now()
			blocked := openEngineAt(t, f.workspace)
			if took := time.Since(began); took > 30*time.Second {
				t.Fatalf("Open waited %v for the reader", took)
			}
			if states := privacyStates(t, blocked, f.child, f.grandchild); !slices.Equal(states, []string{"cleanup_pending", "cleanup_pending"}) {
				t.Fatalf("privacy rows %v while the reader blocks the truncation", states)
			}
			if withdrawn, err := blocked.store.SessionWithdrawn(ctx, f.child); err != nil || !withdrawn {
				t.Fatalf("the deleted session is not withdrawn while pending: %v %v", withdrawn, err)
			}
			if _, err := blocked.DeleteSession(ctx, f.child, WorktreeCleanup{}); !errors.Is(err, ErrDeletionPending) {
				t.Fatalf("delete while the reader still blocks: %v", err)
			}

			reader.release()
			engine := blocked
			if finish == "open" {
				blocked.Close()
				engine = openEngineAt(t, f.workspace)
			} else {
				res, err := blocked.DeleteSession(ctx, f.child, WorktreeCleanup{})
				if err != nil || !slices.Equal(res.DeletedChildren, []string{f.grandchild}) {
					t.Fatalf("resumed delete %+v %v", res, err)
				}
			}
			if found := sentinelAtRest(t, path); len(found) != 0 {
				t.Fatalf("the sentinel is still in %v", found)
			}
			if states := privacyStates(t, engine, f.child, f.grandchild); !slices.Equal(states, []string{"cleaned", "cleaned"}) {
				t.Fatalf("privacy rows %v after the reader is gone", states)
			}
			if markerExists(t, path) {
				t.Fatal("checkpoint marker left after the truncation")
			}
			engine.Close()
			again := openEngineAt(t, f.workspace)
			if pending, err := again.store.PendingPrivacyDeletions(ctx); err != nil || len(pending) != 0 {
				t.Fatalf("pending after a second Open: %v %v", pending, err)
			}
			if _, err := again.DeleteSession(ctx, f.child, WorktreeCleanup{}); !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("deleting again: %v", err)
			}
			if n := countRows(t, again, `SELECT count(*) FROM session_messages WHERE session_id IN (?, ?)`, f.parent, f.reader); n == 0 {
				t.Fatal("the other sessions lost their transcripts")
			}
		})
	}
}

// Plan T3-S4 failure path: a process killed between two batches of a deletion leaves it
// cleanup_pending; the session's content is not used meanwhile, and the next Open finishes the
// cleanup - including the rebuild owed after the rows are gone - so no byte of it is left.
func TestKilledDeletionIsFinishedAtOpen(t *testing.T) {
	for _, point := range []string{"rows", "rebuild"} {
		t.Run(point, func(t *testing.T) {
			f := newDeletionFixture(t)
			path := f.engine.store.Path()
			f.supervisor.Close()
			f.engine.Close()

			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperKillDuringDeletion$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashWorkspaceEnv+"="+f.workspace, crashSessionEnv+"="+f.child, crashPointEnv+"="+point)
			cmd.Stderr = os.Stderr
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			line := make(chan string, 1)
			go func() { text, _ := bufio.NewReader(out).ReadString('\n'); line <- text }()
			select {
			case text := <-line:
				if text != "held\n" {
					t.Fatalf("child said %q", text)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("child did not reach the kill point")
			}
			killHolder(t, cmd)
			if point == "rows" && len(sentinelAtRest(t, path)) == 0 {
				t.Fatal("the kill came after the content was gone; the point is not between batches")
			}

			reopened := openEngineAt(t, f.workspace)
			ctx := context.Background()
			if found := sentinelAtRest(t, path); len(found) != 0 {
				t.Fatalf("the sentinel is still in %v after Open", found)
			}
			if pending, err := reopened.store.PendingPrivacyDeletion(ctx, f.child); err != nil || pending != nil {
				t.Fatalf("deletion still pending after Open: %+v %v", pending, err)
			}
			if n := countRows(t, reopened, `SELECT count(*) FROM source_fences WHERE scope_kind = 'privacy' AND state = 'cleaned' AND scope_ref IN (?, ?)`, f.child, f.grandchild); n != 2 {
				t.Fatalf("privacy fences cleaned: %d", n)
			}
			for _, id := range []string{f.child, f.grandchild} {
				if _, err := reopened.store.Get(ctx, id); !errors.Is(err, session.ErrNotFound) {
					t.Fatalf("%s still exists: %v", id, err)
				}
			}
			if n := countRows(t, reopened, `SELECT count(*) FROM session_messages WHERE session_id IN (?, ?)`, f.parent, f.reader); n == 0 {
				t.Fatal("the other sessions lost their transcripts")
			}
		})
	}
}
