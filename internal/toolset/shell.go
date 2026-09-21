package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/protocol"
)

func (s *Set) runShell(ctx context.Context, args map[string]any, onOutput func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	command := fmt.Sprint(args["command"])
	if command == "" {
		return protocol.ToolResult{}, errors.New("shell command is required")
	}
	result, err := s.shell.Run(ctx, command, func(chunk shell.StreamEvent) {
		if onOutput != nil {
			onOutput(protocol.ToolOutput{Stream: chunk.Stream, Chunk: string(chunk.Chunk)})
		}
	})
	if err != nil {
		return protocol.ToolResult{}, err
	}
	content, _ := json.Marshal(map[string]any{
		"command": command,
		"exitCode": result.ExitCode,
		"cancelled": result.Cancelled,
		"durationNanos": result.Duration.Nanoseconds(),
		"stdout": map[string]any{
			"head": string(result.Stdout.Head),
			"tail": string(result.Stdout.Tail),
			"total": result.Stdout.Total,
			"truncated": result.Stdout.Truncated,
		},
		"stderr": map[string]any{
			"head": string(result.Stderr.Head),
			"tail": string(result.Stderr.Tail),
			"total": result.Stderr.Total,
			"truncated": result.Stderr.Truncated,
		},
	})
	return protocol.ToolResult{
		OK: result.ExitCode == 0 && !result.Cancelled,
		Content: string(content),
		MutatesWorkspace: true,
	}, nil
}
