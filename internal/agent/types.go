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
	// Transcript calls fn with every row whose seq is at least fromSeq, oldest first.
	Transcript(ctx context.Context, sessionID string, fromSeq int64, fn func(seq int64, m protocol.Message) error) error
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

// Delegation grants (Options.Delegation).
const (
	DelegationNone     = ""
	DelegationPropose  = "propose"
	DelegationIsolated = "isolated"
)

// ValidDelegation reports whether d is a known grant; "none" is accepted as DelegationNone.
func ValidDelegation(d string) bool {
	return d == DelegationNone || d == "none" || d == DelegationPropose || d == DelegationIsolated
}

// TaskStarter starts an execution task for delegation isolated: a child session that runs the
// objective on its own. It returns the child's session id and a summary for the tool result.
type TaskStarter interface {
	StartTask(ctx context.Context, objective string) (sessionID string, detail any, err error)
}

type Event struct {
	Type      string
	SessionID string
	Data      map[string]any
}

type Options struct {
	MaxSteps               int
	MaxToolCallsPerStep    int
	ToolAllowlist          []string
	ToolDenylist           []string
	SystemPrompt           string
	AdditionalSystemPrompt string
	// Instructions are the agent's instructions (ADR-0009), appended as the last system message after
	// SystemPrompt and AdditionalSystemPrompt (skills). They are plain text: they cannot replace the
	// default prompt or change the policy, authority, permissions or tool set the run was built with,
	// and the loop never persists them.
	Instructions string
	MaxTokens    int
	Temperature  *float64
	// Session is the model the run starts on and Failover the agent's list that follows it; both
	// must be keys of Loop.Models. EffortDefault is the default of a candidate CandidateEfforts does
	// not name, Escalation the configured escalation (nil for none) and Effort the per-run override
	// ("" for none), which wins over every candidate's default.
	Session       config.ModelRef
	Failover      []config.ModelRef
	EffortDefault config.Effort
	Escalation    *config.Escalation
	Effort        config.Effort
	// EffortAutoCeiling is effort.auto.max when EffortDefault was resolved from auto, "" when it
	// was not. A candidate whose reasoning.efforts offers nothing at or below it sends no
	// reasoning parameter (rule effort.auto_unavailable).
	EffortAutoCeiling config.Effort
	// CandidateEfforts are the candidates whose default differs from EffortDefault: an agent list
	// entry with its own effort (ADR-0009), auto already resolved.
	CandidateEfforts map[config.ModelRef]CandidateEffort
	// Decisions are the decisions made before the loop started (the Start's model resolution). The
	// first step record of the run carries them.
	Decisions []Decision
	// Resumed reports that the session was interrupted before this Start; the run
	// records an agent.resume checkpoint. Start sets it from the status it found.
	Resumed bool
	// Interactive marks a conversation turn (ADR-0006 D3). Without a host workflow, a text answer then
	// ends the run completed: open work units and unpassed required checks stay obligations of the
	// thread instead of blocking the answer. A run that changed the workspace still has its required
	// checks decided before it ends. A host workflow keeps the whole completion gate.
	Interactive bool
	// Delegation grants the start_task tool (ADR-0006 D4): DelegationNone (the default) registers no
	// such tool, DelegationPropose records proposals the user starts, DelegationIsolated starts
	// execution tasks through Loop.Tasks. It is independent of the run's tool policy and is checked
	// again when the tool executes.
	Delegation string
}

// CandidateEffort is one candidate's default effort and, when that default was resolved from auto,
// the auto ceiling that applies to it.
type CandidateEffort struct {
	Default     config.Effort
	AutoCeiling config.Effort
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
	KindTranscriptUnpaired   = "transcript_unpaired"
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
	// Window summarizes the request that was answered (design §5).
	Window *windowSummary `json:"window,omitempty"`
}

// windowSummary is the request window of one step: the oldest transcript row it held (-1 for none),
// the groups left out, oldest first, and its estimated tokens.
type windowSummary struct {
	FirstSeq int64 `json:"firstSeq"`
	Dropped  int   `json:"dropped"`
	Tokens   int   `json:"tokens"`
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
