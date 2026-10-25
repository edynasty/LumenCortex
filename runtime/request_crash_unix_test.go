//go:build unix

package runtime

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

const (
	crashWorkspaceEnv = "LCX_RUNTIME_CRASH_WORKSPACE"
	crashSessionEnv   = "LCX_RUNTIME_CRASH_SESSION"
	crashPointEnv     = "LCX_RUNTIME_CRASH_POINT"
	crashMarkerEnv    = "LCX_RUNTIME_CRASH_MARKER"
)

// The three places a process can die around a provider request, each reached deterministically:
//
//	after_admit   the envelope is committed, Client.Complete has not been called
//	in_complete   Client.Complete is running and never returns
//	before_answer Client.Complete returned an answer that is not stored yet
var crashPoints = []string{"after_admit", "in_complete", "before_answer"}

// TestHelperCrashAtRequestPoint is the child process of TestKilledAroundARequestIsUnknownAndNotResent.
// It runs a real Engine up to the chosen point, reports "held", and waits to be killed.
func TestHelperCrashAtRequestPoint(t *testing.T) {
	workspace := os.Getenv(crashWorkspaceEnv)
	if workspace == "" {
		t.Skip("helper process only")
	}
	point, marker := os.Getenv(crashPointEnv), os.Getenv(crashMarkerEnv)
	hold := func() {
		os.Stdout.WriteString("held\n")
		time.Sleep(5 * time.Minute)
		os.Exit(4) // not reached: the parent kills this process
	}
	switch point {
	case "after_admit":
		testHookAfterAdmit = hold
	case "before_answer":
		testHookBeforeAnswer = hold
	}
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	provider := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		_ = os.WriteFile(marker, []byte("called"), 0o600) // the request reached the model
		if point == "in_complete" {
			hold()
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "unstored answer"}}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), os.Getenv(crashSessionEnv), provider, AgentOptions{Policy: "workspace", MaxSteps: 3}); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(3)
	}
	time.Sleep(5 * time.Minute)
}

// kill -9 at any of the three points leaves the session running and the envelope admitted. The next
// Open turns the envelope into unknown (never answered, never failed: the request may have been
// delivered), the answer that was never stored does not appear, and a resumed run sends exactly one
// new request under a new envelope; the old request is not sent again.
func TestKilledAroundARequestIsUnknownAndNotResent(t *testing.T) {
	for _, point := range crashPoints {
		t.Run(point, func(t *testing.T) {
			workspace := t.TempDir()
			engine := openEngineAt(t, workspace)
			handle := newTestSession(t, engine, "answer me")
			marker := filepath.Join(t.TempDir(), "called")

			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashAtRequestPoint$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashWorkspaceEnv+"="+workspace, crashSessionEnv+"="+handle.ID, crashPointEnv+"="+point, crashMarkerEnv+"="+marker)
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
				t.Fatal("child did not reach the crash point")
			}
			killHolder(t, cmd)

			_, called := os.Stat(marker)
			if wantCalled := point != "after_admit"; (called == nil) != wantCalled {
				t.Fatalf("provider called = %v, want %v at %s", called == nil, wantCalled, point)
			}
			rowsBefore := rowCount(t, handle)

			// The kernel released the run lock; a fresh Open reconciles.
			reopened := openEngineAt(t, workspace)
			envs := envelopeRows(t, reopened, handle.ID)
			if len(envs) != 1 || envs[0].State != "unknown" || envs[0].Reason.String != session.ReasonRestartNoAnswer || envs[0].Answered.Valid {
				t.Fatalf("envelopes after restart = %+v, want one unknown/%s without an answer", envs, session.ReasonRestartNoAnswer)
			}
			if rows := transcript(t, handle); len(rows) < rowsBefore || rows[len(rows)-1].Content == "unstored answer" {
				t.Fatalf("the unstored answer is in the transcript: %+v", rows)
			}
			again := openEngineAt(t, workspace) // Reconcile is idempotent
			if got := envelopeRows(t, again, handle.ID); len(got) != 1 || got[0].State != "unknown" || got[0].Reason.String != session.ReasonRestartNoAnswer {
				t.Fatalf("second reconcile changed the envelope: %+v", got)
			}

			var sent int
			resumed := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				sent++
				return protocol.ProviderResponse{Message: protocol.Message{Content: "resumed"}}, nil
			}}
			supervisor, err := NewRunSupervisor(again)
			if err != nil {
				t.Fatal(err)
			}
			defer supervisor.Close()
			if _, err := supervisor.startWith(context.Background(), handle.ID, resumed, AgentOptions{Input: "carry on", MaxSteps: 3}); err != nil {
				t.Fatal(err)
			}
			waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
			final := envelopeRows(t, again, handle.ID)
			if sent != 1 || len(final) != 2 {
				t.Fatalf("resumed run sent %d requests under %d envelopes, want 1 request and 2 envelopes", sent, len(final))
			}
			if final[0].ID != envs[0].ID || final[0].State != "unknown" || final[1].ID == final[0].ID || final[1].State != "answered" {
				t.Fatalf("envelopes after resume = %+v: the old request must stay unknown and the new one be its own", final)
			}
		})
	}
}
