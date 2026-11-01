package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/protocol"
)

// shellDescription is the shell tool's description.
const shellDescription = "Run a shell command in the workspace with bounded retained output. A command that runs past timeout_sec (or the configured default, or the run's deadline) has its process group terminated and returns its partial output with timedOut: true; its effects are then unknown."

// shellTimeout is the timeout of one foreground shell call and what set it: the call's timeout_sec, else
// the configured default, bounded by the time left before the run's tool deadline. 0 means none. left is
// false when the tool deadline has already passed: the call must not start.
func (s *Set) shellTimeout(ctx context.Context, args map[string]any) (timeout time.Duration, source string, left bool, err error) {
	if raw, ok := args["timeout_sec"]; ok && raw != nil {
		n, ok := raw.(float64)
		if !ok || n != math.Trunc(n) || n < 1 || n > maxShellTimeoutSeconds {
			return 0, "", false, fmt.Errorf("timeout_sec must be a whole number of seconds from 1 to %d", maxShellTimeoutSeconds)
		}
		timeout, source = time.Duration(n)*time.Second, "argument"
	} else if s.shellTimeoutDefault > 0 {
		timeout, source = s.shellTimeoutDefault, "config"
	}
	if deadline, ok := protocol.ToolDeadline(ctx); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, "deadline", false, nil
		}
		if timeout == 0 || remaining < timeout {
			timeout, source = remaining, "deadline"
		}
	}
	return timeout, source, true, nil
}

// maxShellTimeoutSeconds bounds timeout_sec (one day), as config.MaxShellTimeoutSeconds bounds the default.
const maxShellTimeoutSeconds = 86400

func (s *Set) runShell(ctx context.Context, args map[string]any, onOutput func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	command := fmt.Sprint(args["command"])
	if command == "" {
		return protocol.ToolResult{}, errors.New("shell command is required")
	}
	timeout, source, left, err := s.shellTimeout(ctx, args)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if !left {
		content, _ := json.Marshal(map[string]any{
			"command": command,
			"error":   "not run: the run has no time left before its deadline reserve; stop starting work and deliver your answer now",
		})
		return protocol.ToolResult{OK: false, Content: string(content)}, nil
	}
	// A command changed the workspace only if its state differs afterwards. When the state cannot be read
	// either time, the command counts as a change.
	before, known := s.repository.WorkspaceState(ctx)
	result, err := s.shell.RunTimeout(ctx, command, timeout, func(chunk shell.StreamEvent) {
		if onOutput != nil {
			onOutput(protocol.ToolOutput{Stream: chunk.Stream, Chunk: string(chunk.Chunk)})
		}
	})
	if err != nil {
		return protocol.ToolResult{}, err
	}
	out := map[string]any{
		"command":       command,
		"exitCode":      result.ExitCode,
		"cancelled":     result.Cancelled,
		"durationNanos": result.Duration.Nanoseconds(),
		"stdout": map[string]any{
			"head":      string(result.Stdout.Head),
			"tail":      string(result.Stdout.Tail),
			"total":     result.Stdout.Total,
			"truncated": result.Stdout.Truncated,
		},
		"stderr": map[string]any{
			"head":      string(result.Stderr.Head),
			"tail":      string(result.Stderr.Tail),
			"total":     result.Stderr.Total,
			"truncated": result.Stderr.Truncated,
		},
	}
	if result.TimedOut {
		seconds := int64(math.Ceil(timeout.Seconds()))
		out["timedOut"], out["timeoutSec"] = true, seconds
		reason := fmt.Sprintf("the command ran past its %ds timeout", seconds)
		if source == "deadline" {
			reason = fmt.Sprintf("the command ran into the run's deadline reserve after %ds", seconds)
		}
		out["error"] = reason + "; its process group was terminated (SIGTERM, then SIGKILL). The output is partial and the command may have left partial effects. Narrow the command or give it a larger timeout_sec."
	}
	content, _ := json.Marshal(out)
	changed := true
	if known {
		after, ok := s.repository.WorkspaceState(ctx)
		changed = !ok || after != before
	}
	return protocol.ToolResult{
		OK:               result.ExitCode == 0 && !result.Cancelled && !result.TimedOut,
		Interrupted:      result.TimedOut,
		Content:          string(content),
		MutatesWorkspace: changed,
	}, nil
}
