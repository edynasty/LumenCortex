package shell

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
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
}

type Runner struct {
	Workspace string
	HeadBytes int
	TailBytes int
}

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
	started := time.Now().UTC()
	stdout := streambuf.New(r.HeadBytes, r.TailBytes)
	stderr := streambuf.New(r.HeadBytes, r.TailBytes)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-lc", command)
	}
	cmd.Dir = r.Workspace
	cmd.Stdout = chunkWriter{bounded: stdout, stream: "stdout", onChunk: onChunk}
	cmd.Stderr = chunkWriter{bounded: stderr, stream: "stderr", onChunk: onChunk}

	err := cmd.Run()
	result := Result{
		Command:   command,
		ExitCode:  0,
		StartedAt: started,
		Duration:  time.Since(started),
		Stdout:    stdout.Snapshot(),
		Stderr:    stderr.Snapshot(),
		Cancelled: errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded),
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
