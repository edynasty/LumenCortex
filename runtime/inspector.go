package runtime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/edynasty/LumenCortex/internal/diag"
	"github.com/edynasty/LumenCortex/protocol"
)

// WorkerTree is a bounded view of the existing supervisor's session relations.
// It is an observation, never a second registry or permission grant.
type WorkerTree struct {
	Root       SubagentNode `json:"root"`
	CapturedAt time.Time    `json:"capturedAt"`
	MaxDepth   int          `json:"maxDepth"`
	MaxNodes   int          `json:"maxNodes"`
}

func (s *RunSupervisor) GetWorkerTree(ctx context.Context, sessionID string) (WorkerTree, error) {
	if s == nil || s.engine == nil {
		return WorkerTree{}, ErrRunSupervisorClosed
	}
	if err := s.engine.runtimeEventAccess(ctx, sessionID); err != nil {
		return WorkerTree{}, err
	}
	root, err := s.subagentNode(ctx, sessionID, "", "main")
	if err != nil {
		return WorkerTree{}, err
	}
	children, err := s.SubagentTree(ctx, sessionID)
	if err != nil {
		return WorkerTree{}, err
	}
	root.Children = children
	return WorkerTree{Root: root, CapturedAt: time.Now().UTC(), MaxDepth: MaxSubagentTreeDepth, MaxNodes: MaxSubagentTreeNodes}, nil
}

func inspectorJSON(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	return diag.EventPayload(value)
}

type RuntimeEventDetails struct {
	Event         Event           `json:"event"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	ContentStatus string          `json:"contentStatus"`
}

// GetRuntimeEventDetails resolves existing authorized transcript rows, never stores a second copy.
func (e *Engine) GetRuntimeEventDetails(ctx context.Context, sessionID, eventID string) (RuntimeEventDetails, error) {
	if err := e.runtimeEventAccess(ctx, sessionID); err != nil {
		return RuntimeEventDetails{}, err
	}
	event, err := e.store.RuntimeEvent(ctx, sessionID, eventID)
	if err != nil {
		return RuntimeEventDetails{}, err
	}
	details := RuntimeEventDetails{Event: event, ContentStatus: "metadata_only"}
	if event.RunID == "" || event.CorrelationID == "" {
		return details, nil
	}
	rows, err := e.store.RuntimeToolMessages(ctx, sessionID, event.RunID, event.CorrelationID)
	if err != nil {
		return details, err
	}
	messages, err := e.hostMessages(ctx, sessionID, rows)
	if err != nil {
		return details, err
	}
	for _, row := range messages {
		if row.Withheld {
			details.Arguments = nil
			details.Result = nil
			details.ContentStatus = "withheld"
			return details, nil
		}
		var message protocol.Message
		if err = json.Unmarshal(row.JSON, &message); err != nil {
			return details, err
		}
		for _, call := range message.ToolCalls {
			if call.ID == event.CorrelationID {
				details.Arguments = diag.EventPayload(call.Arguments)
				details.ContentStatus = "bounded_transcript"
			}
		}
		if message.ToolCallID == event.CorrelationID {
			raw, _ := json.Marshal(map[string]string{"content": message.Content})
			details.Result = diag.EventPayload(raw)
			details.ContentStatus = "bounded_transcript"
		}
	}
	if err = e.runtimeEventAccess(ctx, sessionID); err != nil {
		return RuntimeEventDetails{}, err
	}
	return details, nil
}
