package shell

import (
	"context"
	"errors"
	"fmt"
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
	// CleanupError: the command's containment could not be fully removed (for example its cgroup leaf
	// stayed busy). RunTimeout then also returns an error wrapping ErrCleanup; the command's own result
	// above is still accurate. A background reaper keeps retrying the removal (see CleanupFailures).
	CleanupError string `json:"cleanupError,omitempty"`
}

// ErrCleanup marks an error of RunTimeout that comes from removing the command's containment after
// the command itself finished, was cancelled or timed out.
var ErrCleanup = errors.New("shell command cleanup failed")

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
// reports TimedOut with exit code -1. Caller cancellation uses the same bounded
// group shutdown but remains Cancelled, never a command timeout.
func (r *Runner) RunTimeout(ctx context.Context, command string, timeout time.Duration, onChunk func(StreamEvent)) (Result, error) {
	started := time.Now().UTC()
	stdout := streambuf.New(r.HeadBytes, r.TailBytes)
	stderr := streambuf.New(r.HeadBytes, r.TailBytes)

	// Caller cancellation first terminates the group gracefully; the private
	// context is cancelled only when that group's grace period has expired.
	cmdCtx, kill := context.WithCancel(context.WithoutCancel(ctx))
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

	if err := ctx.Err(); err != nil {
		return Result{Command: command, ExitCode: -1, StartedAt: started, Cancelled: true}, err
	}
	group, err := newGuardedGroup(cmd)
	if err != nil {
		return Result{Command: command, ExitCode: -1, StartedAt: started}, err
	}
	err = cmd.Start()
	if err == nil {
		if err = group.started(cmd); err != nil {
			_ = cmd.Wait()
		}
	}
	var timedOut atomic.Bool
	if err == nil {
		var stopWatcher func()
		if timeout > 0 || ctx.Done() != nil {
			exited := make(chan struct{})
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				var deadline <-chan time.Time
				if timeout > 0 {
					timer := time.NewTimer(timeout)
					defer timer.Stop()
					deadline = timer.C
				}
				select {
				case <-exited:
					return
				case <-ctx.Done():
				case <-deadline:
					timedOut.Store(true)
				}
				_ = group.term()
				grace := time.NewTimer(TermGrace)
				defer grace.Stop()
				select {
				case <-grace.C:
					kill()
				case <-exited:
				}
			}()
			stopWatcher = func() {
				close(exited)
				<-finished
			}
		}
		err = cmd.Wait()
		if stopWatcher != nil {
			stopWatcher()
		}
	}
	cleanupErr := group.release(r.Reap || ctx.Err() != nil || timedOut.Load())
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
	var runErr error
	switch {
	case result.Cancelled:
		result.ExitCode = -1
		runErr = ctx.Err()
	case result.TimedOut:
		result.ExitCode = -1
	case err != nil:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			runErr = err
		}
	}
	if cleanupErr != nil {
		result.CleanupError = cleanupErr.Error()
		runErr = errors.Join(runErr, fmt.Errorf("%w: %w", ErrCleanup, cleanupErr))
	}
	return result, runErr
}
