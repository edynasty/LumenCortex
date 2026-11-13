//go:build unix

package runtime

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// Reuse the real deletion subprocess, with cognition's multi-source/reader/second-
// generation fixture rather than the older thread-only fixture. No cancellation
// substitutes for SIGKILL: the parent checks the kernel-reported termination.
func TestCognitionPrivacySIGKILLRecovery(t *testing.T) {
	for _, point := range []string{"rows", "rebuild", "blocked"} {
		t.Run(point, func(t *testing.T) {
			w := newPrivacyWorld(t)
			path := w.f.engine.store.Path()
			if err := w.f.engine.Close(); err != nil {
				t.Fatal(err)
			}
			var held *walReader
			if point == "blocked" {
				held = holdWALReader(t, path)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperKillDuringDeletion$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashWorkspaceEnv+"="+w.f.root, crashSessionEnv+"="+w.a, crashPointEnv+"="+point)
			cmd.Stderr = os.Stderr
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			line, err := bufio.NewReader(out).ReadString('\n')
			want := "held\n"
			if point == "blocked" {
				want = "pending\n"
			}
			if err != nil || line != want {
				t.Fatalf("child did not reach %s: %q %v", point, line, err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("expected real SIGKILL, got %v", err)
			}
			if point == "rows" && len(textAtRest(t, path, w.sentinel)) == 0 {
				t.Fatal("kill point missed the content-bearing cleanup interval")
			}
			if held != nil {
				shortCheckpointWait(t)
				pending := openEngineAt(t, w.f.root)
				w.f.engine = pending
				w.readsBlocked(t)
				if d, err := pending.store.PendingPrivacyDeletion(context.Background(), w.a); err != nil || d == nil {
					t.Fatalf("blocked checkpoint lost pending deletion: %+v %v", d, err)
				}
				if _, err := pending.DeleteSession(context.Background(), w.a, WorktreeCleanup{}); !errors.Is(err, ErrDeletionPending) {
					t.Fatalf("blocked deletion reported success: %v", err)
				}
				held.release()
				if err := pending.Close(); err != nil {
					t.Fatal(err)
				}
			}
			w.f.engine = openEngineAt(t, w.f.root)
			w.assertPurged(t)
			if n := w.rows(t, w.m2); n != 0 {
				t.Fatalf("second-generation maintenance kept %d rows", n)
			}
			if _, err := w.f.engine.DeleteSession(context.Background(), w.a, WorktreeCleanup{}); !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("repeat deletion: %v", err)
			}
			if err := w.f.engine.Close(); err != nil {
				t.Fatal(err)
			}
			w.f.engine = openEngineAt(t, w.f.root)
			w.assertPurged(t)
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".partial") || strings.HasSuffix(entry.Name(), "-journal") {
					t.Fatalf("left temporary database file %s", entry.Name())
				}
			}
			t.Logf("%s: SIGKILL observed; reopened twice; DB/WAL/SHM and live readers clean; source B intact", point)
		})
	}
}

func TestCognitionPrivacyHeldMaintainerProcess(t *testing.T) {
	if os.Getenv("LC_R2_HELD_MAINTAINER") == "child" {
		e := openEngineAt(t, os.Getenv(crashWorkspaceEnv))
		p := &exitProvider{fn: func(ctx context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			_, _ = os.Stdout.WriteString("held\n")
			<-ctx.Done()
			return protocol.ProviderResponse{}, ctx.Err()
		}}
		e.cognition = &CognitionOptions{Maintainer: bind(e, p)}
		_, err := e.MaintainCognition(context.Background())
		t.Fatalf("held maintainer unexpectedly returned: %v", err)
	}
	f := newCognitionFixture(t, true)
	const sentinel = "T5A-R2-CROSS-PROCESS-PRIVATE-79254"
	a := f.run(t, "private source", cogAnswer(sentinel))
	b := f.run(t, "independent source", cogAnswer("independent preserved answer"))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCognitionPrivacyHeldMaintainerProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "LC_R2_HELD_MAINTAINER=child", crashWorkspaceEnv+"="+f.root)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "held\n" {
		t.Fatalf("child did not hold real maintainer: %q %v", line, err)
	}
	if _, err := f.engine.DeleteSession(ctx, a, WorktreeCleanup{}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("purged while another process could still write: %v", err)
	}
	if pending, err := f.engine.store.PendingPrivacyDeletion(ctx, a); err != nil || pending == nil {
		t.Fatalf("held deletion lost pending state: %+v %v", pending, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("not killed by SIGKILL: %v", err)
	}
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.engine = openEngineAt(t, f.root)
		if pending, err := f.engine.store.PendingPrivacyDeletion(ctx, a); err != nil || pending != nil {
			t.Fatalf("recovery left pending: %+v %v", pending, err)
		}
		if found := textAtRest(t, f.engine.store.Path(), sentinel); len(found) != 0 {
			t.Fatalf("recovered private bytes: %v", found)
		}
		if kept, err := f.engine.store.Get(ctx, b); err != nil || kept.Final == nil || *kept.Final != "independent preserved answer" {
			t.Fatalf("unrelated B destroyed: %+v %v", kept, err)
		}
		if err := f.engine.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("remote maintainer lock made deletion pending; SIGKILL released it; two Engine opens completed purge without B data loss")
}
