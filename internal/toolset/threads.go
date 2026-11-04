package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

// ThreadReader reads earlier work of this workspace for read_thread and recall (ADR-0010 D3). The
// runtime implements it over the session store; both calls are read-only. ReadThread adds one page
// of the session's visible transcript when transcript is true: the newest messages before seq
// before, or the newest messages when before is negative (plan T2-S1).
type ThreadReader interface {
	ReadThread(ctx context.Context, id string, transcript bool, before int64) (any, error)
	Recall(ctx context.Context, query string, limit int) (any, error)
}

func (s *Set) registerThreadTools() {
	if s.threads == nil {
		return
	}
	s.add(protocol.ToolSpec{
		Name: "recall",
		Description: "Search earlier threads and tasks of this workspace for what they decided, tried, abandoned or found: " +
			"matches their objectives, notes, resolutions and final answers. Facts the user gave in earlier conversations " +
			"(hosts, variable names, conventions, decisions, approaches that failed) are often not in the repository; " +
			"call recall before you guess or invent such a fact, and at the start of a task that may continue earlier work. " +
			"Returns up to 8 threads with matching notes; open one with read_thread.",
		Permission: "read",
		Parameters: obj(map[string]any{
			"query": map[string]any{"type": "string", "description": "words to match: names, identifiers, error text, topics"},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 8},
		}, "query"),
	}, s.recall)
	s.add(protocol.ToolSpec{
		Name: "read_thread",
		Description: "Read one earlier thread or task of this workspace by id (from recall, a thread reference, or a task), " +
			"or the current thread by its own id: its objective, status, resolution, notes (each with its applicability; " +
			"stale when a file it cites has changed), open work units, tasks, children and final answer. " +
			"With transcript=true it also returns one page of the thread's visible messages, newest first by page, " +
			"never splitting a tool call from its results; pass the page's nextBefore as before to read further back.",
		Permission: "read",
		Parameters: obj(map[string]any{
			"id":         map[string]any{"type": "string", "description": "the session id"},
			"transcript": map[string]any{"type": "boolean", "description": "include one page of the visible messages"},
			"before":     map[string]any{"type": "integer", "minimum": 0, "description": "read messages before this seq (implies transcript)"},
		}, "id"),
	}, s.readThread)
}

func (s *Set) recall(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	limit := 0
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	result, err := s.threads.Recall(ctx, strings.TrimSpace(stringArg(args, "query")), limit)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return jsonResult(result)
}

func (s *Set) readThread(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	transcript, _ := args["transcript"].(bool)
	before := int64(-1)
	if v, ok := args["before"].(float64); ok {
		if v < 0 || v != float64(int64(v)) {
			return protocol.ToolResult{}, fmt.Errorf("before must be a non-negative integer seq")
		}
		before, transcript = int64(v), true
	}
	result, err := s.threads.ReadThread(ctx, stringArg(args, "id"), transcript, before)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return jsonResult(result)
}

func jsonResult(v any) (protocol.ToolResult, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}
