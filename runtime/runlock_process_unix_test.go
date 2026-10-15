//go:build unix

package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

const (
	holderWorkspaceEnv = "LCX_RUNTIME_HOLDER_WORKSPACE"
	holderSessionEnv   = "LCX_RUNTIME_HOLDER_SESSION"
)

// TestHelperHoldRun is the child process of TestKillHolderMidToolThenResume. It is a
// real Engine in its own process: it starts a run whose shell tool is mid-call, then
// waits to be killed.
func TestHelperHoldRun(t *testing.T) {
	workspace := os.Getenv(holderWorkspaceEnv)
	if workspace == "" {
		t.Skip("helper process only")
	}
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	events, _ := engine.Events(512)
	provider := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "kill-1", Name: "shell", Arguments: json.RawMessage(`{"command":"echo ready && exec sleep 20"}`)},
		}}, FinishReason: "tool_calls"}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), os.Getenv(holderSessionEnv), provider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(3)
	}
	waitForEventWith(t, events, "tool.output", "ready")
	os.Stdout.WriteString("held\n")
	time.Sleep(5 * time.Minute)
	os.Exit(4) // not reached: the parent kills this process
}

// Real process-level contention (design §4.2, I2): a second process holds a run
// mid-tool; this process can neither start, approve, delete nor recover it. After
// kill -9 the kernel frees the lock; Open reconciles the session and the next Start
// resumes on a paired transcript.
func TestKillHolderMidToolThenResume(t *testing.T) {
	workspace := t.TempDir()
	engine := openEngineAt(t, workspace)
	handle := newTestSession(t, engine, "long task")
	cmd := startHolder(t, workspace, handle.ID)

	// The holder owns the run: it is durably running and this process cannot touch it.
	held := rowCount(t, handle)
	if info, _ := sessionState(t, engine, handle.ID); info.Status != "running" {
		t.Fatalf("status = %q, want running", info.Status)
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	if _, err := supervisor.startWith(context.Background(), handle.ID, completingProvider("x"), AgentOptions{Input: "must not land"}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("Start against a run held by another process: err=%v, want ErrRunHeld", err)
	}
	if _, err := engine.ApproveWorkflowGate(context.Background(), handle.ID, "ship", "alice"); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("ApproveWorkflowGate: err=%v, want ErrRunHeld", err)
	}
	if _, err := engine.DeleteSession(context.Background(), handle.ID, WorktreeCleanup{}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("DeleteSession: err=%v, want ErrRunHeld", err)
	}
	peer := openEngineAt(t, workspace) // Open must skip the held session
	if info, cause := sessionState(t, peer, handle.ID); info.Status != "running" || cause != nil {
		t.Fatalf("Open recovered a run held by another process: status=%q cause=%+v", info.Status, cause)
	}
	if got := rowCount(t, handle); got != held {
		t.Fatalf("transcript changed while another process held the run: %d -> %d", held, got)
	}

	killHolder(t, cmd)
	if info, _ := sessionState(t, engine, handle.ID); info.Status != "running" {
		t.Fatalf("status = %q: the dead holder left the row running", info.Status)
	}
	if rows := transcript(t, handle); rows[len(rows)-1].Role != "assistant" {
		t.Fatalf("expected an unanswered call at the tail: %+v", rows)
	}

	// A fresh Open now owns the free lock and reconciles.
	reopened := openEngineAt(t, workspace)
	info, cause := sessionState(t, reopened, handle.ID)
	if info.Status != "interrupted" || cause == nil || cause.Kind != session.KindStaleRunRecovered {
		t.Fatalf("status=%q cause=%+v, want interrupted/stale_run_recovered", info.Status, cause)
	}
	rows := transcript(t, handle)
	assertPaired(t, rows)
	if last := rows[len(rows)-1]; last.Origin != protocol.OriginToolInterrupted || last.ToolCallID != "kill-1" {
		t.Fatalf("last row = %+v", last)
	}

	// Resume: the model sees a paired transcript.
	var sent []protocol.Message
	resumed := &exitProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		sent = req.Messages
		return protocol.ProviderResponse{Message: protocol.Message{Content: "resumed"}}, nil
	}}
	resumeSupervisor, err := NewRunSupervisor(reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer resumeSupervisor.Close()
	if _, err := resumeSupervisor.startWith(context.Background(), handle.ID, resumed, AgentOptions{Input: "carry on", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, resumeSupervisor, handle.ID, 5*time.Second)
	assertPaired(t, sent)
	assertPaired(t, transcript(t, handle))
	if info, _ := sessionState(t, reopened, handle.ID); info.Status != "completed" {
		t.Fatalf("status = %q, want completed", info.Status)
	}
}

// startHolder runs TestHelperHoldRun in a child process and returns once its run is
// mid-tool.
func startHolder(t *testing.T, workspace, sessionID string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldRun$", "-test.count=1")
	cmd.Env = append(os.Environ(), holderWorkspaceEnv+"="+workspace, holderSessionEnv+"="+sessionID)
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
	go func() {
		text, _ := bufio.NewReader(out).ReadString('\n')
		line <- text
	}()
	select {
	case text := <-line:
		if text != "held\n" {
			t.Fatalf("holder said %q", text)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("holder did not report a run mid-tool")
	}
	return cmd
}

// killHolder sends SIGKILL, so no deferred cleanup runs in the holder.
func killHolder(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if status, _ := cmd.ProcessState.Sys().(syscall.WaitStatus); !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("holder was not killed by SIGKILL: %v", cmd.ProcessState)
	}
}

// ADR-0005 §2 across processes: a run in another process holds the worktree lease,
// so no Engine here can take it exclusively. kill -9 frees it with the run lock, and
// the reconciled session's worktree can then be removed.
func TestKilledHolderReleasesTheWorktreeLease(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workspace := t.TempDir()
	initGitRepo(t, workspace)
	engine := openEngineAt(t, workspace)
	handle := newTestSession(t, engine, "long task in a worktree")
	identity, err := engine.AttachWorktree(context.Background(), handle.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	cmd := startHolder(t, workspace, handle.ID)

	peer := openEngineAt(t, workspace)
	if _, err := peer.acquireWorktreeLease(identity.Path, true); !errors.Is(err, ErrWorktreeBusy) {
		t.Fatalf("exclusive lease while another process runs there: err=%v, want ErrWorktreeBusy", err)
	}
	if _, err := peer.RemoveSessionWorktree(context.Background(), handle.ID, WorktreeCleanup{}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("remove while another process runs: err=%v, want ErrRunHeld", err)
	}

	killHolder(t, cmd)
	lease, err := peer.acquireWorktreeLease(identity.Path, true)
	if err != nil {
		t.Fatalf("the dead holder's lease was not freed: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	reopened := openEngineAt(t, workspace)
	result, err := reopened.RemoveSessionWorktree(context.Background(), handle.ID, WorktreeCleanup{})
	if err != nil || !result.DirectoryRemoved {
		t.Fatalf("remove after the holder died: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(identity.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree survived: %v", err)
	}
}
