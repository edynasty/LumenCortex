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

// bashDescription is the bash tool's description. It names the program commands really run in (ADR-0019
// §0): bash where it is installed, else the system shell, and never claims bash for it.
func bashDescription(interp shell.Interpreter) string {
	runs := "Commands run in bash (" + interp.String() + "), one process per call: cd and exported variables do not carry over."
	switch interp.Name {
	case "sh":
		runs = "bash is not installed here: commands run in POSIX sh (" + interp.String() + "), one process per call; bash-only syntax fails."
	case "cmd.exe":
		runs = "No bash was found on this Windows host: commands run in cmd.exe (" + interp.String() + "), one process per call; POSIX syntax fails."
	}
	return "Run a command in the workspace with bounded retained output. " + runs + " A command that runs past timeout_sec (or the configured default, or the run's deadline) has its process group terminated and returns its partial output with timedOut: true; its effects are then unknown."
}

// shellTimeout is the timeout of one foreground shell call and what set it: the call's timeout_sec, else
// the configured default, clamped to the configured maximum and bounded by the time left before the run's
// tool deadline. left is false when the tool deadline has already passed: the call must not start.
func (s *Set) shellTimeout(ctx context.Context, args map[string]any) (timeout time.Duration, source string, left bool, err error) {
	if raw, ok := args["timeout_sec"]; ok && raw != nil {
		n, ok := raw.(float64)
		if !ok || n != math.Trunc(n) || n < 1 || n > maxShellTimeoutSeconds {
			return 0, "", false, fmt.Errorf("timeout_sec must be a whole number of seconds from 1 to %d", maxShellTimeoutSeconds)
		}
		timeout, source = time.Duration(n)*time.Second, "argument"
	} else {
		timeout, source = s.shellTimeoutDefault, "config"
	}
	if s.shellMaxTimeout > 0 && timeout > s.shellMaxTimeout {
		timeout = s.shellMaxTimeout
	}
	if deadline, ok := protocol.ToolDeadline(ctx); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, "deadline", false, nil
		}
		if remaining < timeout {
			timeout, source = remaining, "deadline"
		}
	}
	return timeout, source, true, nil
}

// maxShellTimeoutSeconds bounds the timeout_sec argument (one day). The configured maximum
// (config.MaxShellTimeoutSeconds) clamps the effective timeout to a lower value.
const maxShellTimeoutSeconds = 86400

func (s *Set) runShell(ctx context.Context, args map[string]any, onOutput func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	command := fmt.Sprint(args["command"])
	if command == "" {
		return protocol.ToolResult{}, errors.New("bash command is required")
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
	if s.jobs != nil {
		if mode, _ := args["mode"].(string); mode == "background" {
			return s.runShellBackground(ctx, command, args)
		}
	}
	// A command changed the workspace only if its state differs afterwards. When the state cannot be read
	// either time, the command counts as a change.
	before, known := s.repository.WorkspaceState(ctx)
	result, err := s.shell.RunTimeout(ctx, command, timeout, func(chunk shell.StreamEvent) {
		if onOutput != nil {
			onOutput(protocol.ToolOutput{Stream: chunk.Stream, Chunk: string(chunk.Chunk)})
		}
	})
	// A cancelled runner call still carries the output it read and the reason, and so does a command
	// whose containment could not be removed afterwards; only a call that could not run at all is an
	// error.
	if err != nil && !result.Cancelled && !errors.Is(err, shell.ErrCleanup) {
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
	if result.CleanupError != "" {
		out["cleanupError"] = result.CleanupError
	}
	if result.TimedOut {
		seconds := int64(math.Ceil(timeout.Seconds()))
		out["timedOut"], out["timeoutSec"] = true, seconds
		reason := fmt.Sprintf("the command ran past its %ds timeout", seconds)
		if source == "deadline" {
			reason = fmt.Sprintf("the command ran into the run's deadline reserve after %ds", seconds)
		}
		out["error"] = reason + "; its process group was terminated (SIGTERM, then SIGKILL). The output is partial and the command may have left partial effects. Narrow the command or give it a larger timeout_sec."
	} else if result.Cancelled {
		reason := "the run was cancelled"
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = "the run's deadline passed"
		}
		out["error"] = reason + " while the command was running; its process group was terminated (SIGTERM, then SIGKILL). The output is partial."
	}
	content, _ := json.Marshal(out)
	changed := true
	if known {
		after, ok := s.repository.WorkspaceState(ctx)
		changed = !ok || after != before
	}
	return protocol.ToolResult{
		OK:               result.ExitCode == 0 && !result.Cancelled && !result.TimedOut,
		Interrupted:      result.TimedOut || result.Cancelled,
		Content:          string(content),
		MutatesWorkspace: changed,
	}, nil
}
