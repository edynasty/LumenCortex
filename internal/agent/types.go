package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

type SessionState struct {
	ID       string
	Goal     string
	Status   string
	Metadata map[string]any
	Final    string
	Usage    protocol.Usage
}

type SessionPatch struct {
	Status     *string
	Metadata   map[string]any
	Final      *string
	ClearFinal bool
	Usage      *protocol.Usage
	Error      map[string]any
	ClearError bool
}

type Store interface {
	Get(context.Context, string) (SessionState, error)
	RecentMessages(context.Context, string, int) ([]protocol.Message, error)
	AppendMessage(context.Context, string, protocol.Message) (int64, error)
	AppendStep(context.Context, string, int64, any) error
	NextStep(context.Context, string) (int64, error)
	Update(context.Context, string, SessionPatch) error
}

type CheckpointStore interface {
	AppendCheckpoint(context.Context, string, string, any) error
}

type Tools interface {
	Specs([]string) []protocol.ToolSpec
	Execute(context.Context, string, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error)
}

type Event struct {
	Type      string
	SessionID string
	Data      map[string]any
}

type Options struct {
	MaxSteps            int
	RecentMessages      int
	MaxToolCallsPerStep int
	ToolAllowlist       []string
	ToolDenylist        []string
	SystemPrompt        string
	AdditionalSystemPrompt string
	WorkflowJSON        []byte
	WorkUnitsJSON       []byte
	MaxTokens           int
	Temperature         *float64
	CognitionEnabled    bool
}

type Result struct {
	SessionID   string
	Status      string
	Final       string
	Usage       protocol.Usage
	WaitingGate bool
	Workflow    any
}

// Session statuses the loop writes.
const (
	StatusRunning     = "running"
	StatusInterrupted = "interrupted"
	StatusWaitingGate = "waiting_gate"
	StatusFailed      = "failed"
	StatusCompleted   = "completed"
)

// Kinds of ExitCause the loop produces. Provider error classes and stale-run
// recovery have their own kinds in later migration items.
const (
	KindCancelled            = "cancelled"
	KindMaxSteps             = "max_steps"
	KindEmptyResponse        = "empty_response"
	KindProviderAvailability = "provider:availability"
	KindWorkflowGate         = "workflow_gate"
	KindConfig               = "config"
	KindStorage              = "storage"
	KindPanic                = "panic"
)

// ExitCause is why a run ended. Every return path of Loop.Run after the session
// row became "running" maps to exactly one ExitCause, persisted by Loop.finish.
type ExitCause struct {
	Status string
	Kind   string
	Err    error
}

// errorData is the sessions.error_json payload: {"kind", "message", "at"}.
func (c ExitCause) errorData(at time.Time) map[string]any {
	message := ""
	if c.Err != nil {
		message = c.Err.Error()
	}
	return map[string]any{"kind": c.Kind, "message": message, "at": at.UTC().Format(time.RFC3339Nano)}
}

// returnErr is the error Run reports: only interrupted and failed exits are errors.
func (c ExitCause) returnErr() error {
	if c.Status == StatusInterrupted || c.Status == StatusFailed {
		return c.Err
	}
	return nil
}

type stepRecord struct {
	Step         int64            `json:"step"`
	FinishReason string           `json:"finishReason,omitempty"`
	Content      string           `json:"content,omitempty"`
	Provider     string           `json:"provider,omitempty"`
	Model        string           `json:"model,omitempty"`
	ToolCalls    []toolCallRecord `json:"toolCalls,omitempty"`
	Workflow     any              `json:"workflow,omitempty"`
	WorkUnit     any              `json:"workUnit,omitempty"`
	Cognition    any              `json:"cognition,omitempty"`
}

type toolCallRecord struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	OK     bool           `json:"ok"`
	Denied bool           `json:"denied,omitempty"`
}

func metadataJSON(value map[string]any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}
