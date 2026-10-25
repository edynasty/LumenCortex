//go:build unix

package runtime

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// TestHelperKillDuringDeletion is the child process of TestKilledDeletionIsFinishedAtOpen: it deletes
// the session and stops, after a committed PurgeStep, at the chosen phase - "rows" after the first
// batch, "rebuild" once the session rows are gone and the file still owes its rebuild - reports
// "held" and waits to be killed.
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
