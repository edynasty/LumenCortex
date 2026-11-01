package shell

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
	"sync/atomic"
	"time"

	"github.com/edynasty/LumenCortex/internal/streambuf"
)

type StreamEvent struct {
	Stream string `json:"stream"`
	Chunk  []byte `json:"chunk"`
}

type Result struct {
	Command   string             `json:"command"`
	ExitCode  int                `json:"exitCode"`
	StartedAt time.Time          `json:"startedAt"`
	Duration  time.Duration      `json:"duration"`
	Stdout    streambuf.Snapshot `json:"stdout"`
	Stderr    streambuf.Snapshot `json:"stderr"`
	Cancelled bool               `json:"cancelled"`
	// TimedOut: the command ran past its timeout and its process group was terminated (SIGTERM, then
	// SIGKILL after TermGrace). Cancelled stays false: a timeout is the command's result, not the
	// caller's cancellation.
	TimedOut bool `json:"timedOut,omitempty"`
}

type Runner struct {
	Workspace string
	HeadBytes int
	TailBytes int
	// Reap kills the command's process group when the shell exits, so nothing the command started in
	// the background outlives it. Evaluator checks set it; the agent's shell tool does not, so a
	// detached `server > log &` keeps running. Cancellation and timeout kill the group either way.
	Reap bool
	// Interpreter runs the command; the zero value is System (/bin/sh -lc, cmd.exe /C on Windows).
	// The agent's bash tool sets Bash.
	Interpreter Interpreter
}

// waitDelay bounds how long Run waits, after the shell exited or was killed, for background processes
// to close stdout and stderr; the pipes are then closed and Run returns.
const waitDelay = 2 * time.Second

// TermGrace is how long a timed-out command's process group has, after SIGTERM, to exit before SIGKILL.
const TermGrace = 2 * time.Second

func New(workspace string) *Runner {
	return &Runner{Workspace: workspace, HeadBytes: 8 << 10, TailBytes: 24 << 10}
}

type chunkWriter struct {
	bounded io.Writer
	stream  string
	onChunk func(StreamEvent)
}

func (w chunkWriter) Write(p []byte) (int, error) {
	n, err := w.bounded.Write(p)
	if n > 0 && w.onChunk != nil {
		chunk := append([]byte(nil), p[:n]...)
		w.onChunk(StreamEvent{Stream: w.stream, Chunk: chunk})
	}
	return n, err
}

func (r *Runner) Run(ctx context.Context, command string, onChunk func(StreamEvent)) (Result, error) {
	return r.RunTimeout(ctx, command, 0, onChunk)
}

// RunTimeout is Run with a timeout (0: none). At the timeout the command's process group gets SIGTERM,
// then SIGKILL after TermGrace if it has not exited; the result keeps the output read so far and
// reports TimedOut with exit code -1. Cancelling ctx still kills the group at once and returns
// ctx.Err().
func (r *Runner) RunTimeout(ctx context.Context, command string, timeout time.Duration, onChunk func(StreamEvent)) (Result, error) {
	started := time.Now().UTC()
	stdout := streambuf.New(r.HeadBytes, r.TailBytes)
	stderr := streambuf.New(r.HeadBytes, r.TailBytes)

	// kill ends the command on its own context, so the timeout's SIGKILL goes through the same
	// cancellation path (cmd.Cancel kills the group) without being mistaken for the caller's.
	cmdCtx, kill := context.WithCancel(ctx)
	defer kill()
	interp := r.Interpreter
	if interp.IsZero() {
		interp = System()
	}
	cmd := exec.CommandContext(cmdCtx, interp.Path, append(slices.Clip(interp.Args), command)...)
	cmd.Dir = r.Workspace
	cmd.Stdout = chunkWriter{bounded: stdout, stream: "stdout", onChunk: onChunk}
	cmd.Stderr = chunkWriter{bounded: stderr, stream: "stderr", onChunk: onChunk}
	cmd.WaitDelay = waitDelay
	StartGroup(cmd)

	err := cmd.Start()
	var timedOut atomic.Bool
	if err == nil {
		var stopTimer func()
		if timeout > 0 {
			exited := make(chan struct{})
			pid := cmd.Process.Pid
			timer := time.AfterFunc(timeout, func() {
				select {
				case <-exited: // the command ended as the timer fired
					return
				default:
				}
				timedOut.Store(true)
				_ = TermGroup(pid)
				grace := time.NewTimer(TermGrace)
				defer grace.Stop()
				select {
				case <-grace.C:
					kill()
				case <-exited:
				}
			})
			stopTimer = func() {
				timer.Stop()
				close(exited)
			}
		}
		err = cmd.Wait()
		if stopTimer != nil {
			stopTimer()
		}
	}
	if r.Reap || ctx.Err() != nil || timedOut.Load() {
		_ = KillGroup(cmd)
	}
	// The shell exited successfully but a background process kept the pipes open past waitDelay: the
	// command's own result stands.
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	result := Result{
		Command:   command,
		ExitCode:  0,
		StartedAt: started,
		Duration:  time.Since(started),
		Stdout:    stdout.Snapshot(),
		Stderr:    stderr.Snapshot(),
		Cancelled: errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded),
		TimedOut:  timedOut.Load() && ctx.Err() == nil,
	}
	if result.TimedOut {
		result.ExitCode = -1
		return result, nil
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		if result.Cancelled {
			result.ExitCode = -1
			return result, ctx.Err()
		}
		result.ExitCode = -1
		return result, err
	}
	return result, nil
}
