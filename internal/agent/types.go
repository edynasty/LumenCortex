package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

type SessionState struct {
	ID     string
	Goal   string
	Status string
	Final  string
	Usage  protocol.Usage
}

// SessionPatch changes session columns.
type SessionPatch struct {
	Status     *string
	Final      *string
	ClearFinal bool
	Usage      *protocol.Usage
	Error      map[string]any
	ClearError bool
}

// TaskStateChange is the Task State version a write commits: Next.Version is its base, and the store
// writes base+1 with Reason. InboxThrough, when positive, consumes the inbox edits Next applied.
type TaskStateChange struct {
	Reason       string
	Next         taskstate.State
	InboxThrough int64
}

type Store interface {
	Get(context.Context, string) (SessionState, error)
	// RecentMessages returns the newest limit rows whose seq is at least fromSeq, oldest first.
	RecentMessages(ctx context.Context, sessionID string, fromSeq int64, limit int) ([]protocol.Message, error)
	// AppendMessage appends one row and, when change is non-nil, the Task State version the row
	// caused, in one transaction. It returns the row's seq and the new version (0 without a change).
	AppendMessage(ctx context.Context, sessionID string, m protocol.Message, change *TaskStateChange) (int64, int64, error)
	AppendStep(context.Context, string, int64, any) error
	NextStep(context.Context, string) (int64, error)
	Update(context.Context, string, SessionPatch) error
	// TaskState returns the newest Task State version (version 0 when there is none).
	TaskState(ctx context.Context, sessionID string) (taskstate.State, error)
	// InboxEdits returns the session's queued host edits in order.
	InboxEdits(ctx context.Context, sessionID string) ([]taskstate.QueuedEdit, error)
	// PutTaskState writes change on its own and returns the new version.
	PutTaskState(ctx context.Context, sessionID string, change TaskStateChange) (int64, error)
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
	MaxSteps               int
	RecentMessages         int
	MaxToolCallsPerStep    int
	ToolAllowlist          []string
	ToolDenylist           []string
	SystemPrompt           string
	AdditionalSystemPrompt string
	MaxTokens              int
	Temperature            *float64
	// Session is the model the run starts on and Failover the config list that follows it; both
	// must be keys of Loop.Models. EffortDefault is the configured default, Escalation the configured
	// escalation (nil for none) and Effort the per-run override ("" for none).
	Session       config.ModelRef
	Failover      []config.ModelRef
	EffortDefault config.Effort
	Escalation    *config.Escalation
	Effort        config.Effort
	// Decisions are the decisions made before the loop started (the Start's model resolution). The
	// first step record of the run carries them.
	Decisions []Decision
	// Resumed reports that the session was interrupted before this Start; the run
	// records an agent.resume checkpoint. Start sets it from the status it found.
	Resumed bool
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
	KindProviderCapacity     = "provider:capacity"
	KindProviderAuth         = "provider:auth"
	KindProviderBadRequest   = "provider:bad_request"
	KindWindowOverflow       = "window_overflow"
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
	Step         int64  `json:"step"`
	FinishReason string `json:"finishReason,omitempty"`
	Content      string `json:"content,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	// ModelRef is the catalog reference of the model that answered.
	ModelRef  config.ModelRef  `json:"modelRef,omitempty"`
	Decisions []Decision       `json:"decisions,omitempty"`
	ToolCalls []toolCallRecord `json:"toolCalls,omitempty"`
	Workflow  any              `json:"workflow,omitempty"`
	WorkUnit  any              `json:"workUnit,omitempty"`
	// TaskState is the Task State version the step ended on.
	TaskState int64 `json:"taskStateVersion,omitempty"`
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
