package protocol

import (
	"context"
	"encoding/json"
)

// Origins of rows the runtime itself wrote to the transcript. Origin is a field
// of the stored message JSON; provider adapters never send it.
const (
	// OriginToolInterrupted marks a tool row that reconciliation appended for a
	// call whose result was never saved.
	OriginToolInterrupted = "tool_interrupted"
	// OriginCompletionBlocked marks a user row the completion gate appended when it
	// refused the model's completion proposal.
	OriginCompletionBlocked = "completion_blocked"
	// OriginResume marks the user row Start appends when a run begins after a row that is not a
	// user row, so a turn never spans runs (design §3.2, §4.6).
	OriginResume = "resume"
	// OriginTurn marks the user row the loop appends before a request to a model that did not
	// produce, in this run, an assistant row of the current turn, so a turn never spans models.
	OriginTurn = "turn"
	// OriginTaskResult marks the user row Start appends for each execution task whose terminal
	// transition the thread has not seen (ADR-0006 D4). Its content is TaskResultHeader, a newline and
	// the result's JSON.
	OriginTaskResult = "task_result"
	// OriginImported marks the user row an import appends after a transcript it copied from another
	// runtime (ADR-0011): it tells the model that the rows above are history LumenCortex did not run.
	OriginImported = "imported"
	// OriginImportedNotice marks a user row copied from a notice the source runtime showed to the
	// user and the model (a background result, an incoming message). Its content names the notice.
	OriginImportedNotice = "imported_notice"
	// OriginImportedMarker marks a user row an import wrote where the source recorded an event it does
	// not copy: a context compaction or a return from another branch.
	OriginImportedMarker = "imported_marker"
	// OriginDeadline marks a user row the loop appends for a run with a deadline (ADR-0019 §2): one when
	// the run starts, stating its budget, and one when the time left first falls within the reserve.
	OriginDeadline = "deadline"
	// OriginNotebook marks the user row a run appends when its session has working notebooks (ADR-0020):
	// a short index of their titles, versions, update times and sections. It never holds a body; the
	// model reads a notebook, or one of its sections, with notebook_read.
	OriginNotebook = "notebook"
)

// Contents of the runtime rows that open a new turn. They are user rows the model reads.
const (
	ResumeContent = "[runtime] A new run continues this session. The transcript and the Task State above are the state of the work; continue from there."
	TurnContent   = "[runtime] The model answering changed in the middle of a turn. The transcript and the Task State above are the state of the work; continue from there."
	// TaskResultHeader leads a task_result row; the result's JSON follows on the next line.
	TaskResultHeader = "[runtime] An execution task of this thread reached a terminal state. This reports its recorded state; it is not a user message."
	// ImportedContent is the content of the OriginImported row.
	ImportedContent = "[runtime] The messages above were imported from a session of another agent runtime. They are history: their tool calls ran there, against the workspace as it was then, and nothing above was executed by LumenCortex. Their tool names are that runtime's: call only the tools offered now. Re-check the current workspace before relying on them."
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	// Origin is set only on rows the runtime wrote (see the Origin constants).
	Origin string `json:"origin,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolSpec struct {
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	Parameters       map[string]any `json:"parameters"`
	Permission       string         `json:"permission,omitempty"`
	MutatesWorkspace bool           `json:"mutatesWorkspace,omitempty"`
}

type ToolResult struct {
	// Interrupted: the tool stopped the operation before it finished (a command past its timeout), so
	// what it did is unknown. The execution record of such a call ends outcome_unknown, never failed.
	Interrupted bool   `json:"interrupted,omitempty"`
	OK          bool   `json:"ok"`
	Denied      bool   `json:"denied,omitempty"`
	Permission  string `json:"permission,omitempty"`
	Content     string `json:"content"`
	// MutatesWorkspace: this call changed the workspace, or may have when the tool cannot tell. It is
	// set whatever OK says: a failed command can still have changed files.
	MutatesWorkspace bool `json:"mutatesWorkspace,omitempty"`
}

type ToolOutput struct {
	Stream string `json:"stream"`
	Chunk  string `json:"chunk"`
}

type Usage struct {
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	Requests         int64 `json:"requests"`
}

type ProviderRequest struct {
	Messages        []Message  `json:"messages"`
	Tools           []ToolSpec `json:"tools,omitempty"`
	ToolChoice      string     `json:"toolChoice,omitempty"`
	Temperature     *float64   `json:"temperature,omitempty"`
	MaxTokens       int        `json:"maxTokens,omitempty"`
	ReasoningEffort string     `json:"reasoningEffort,omitempty"`
	// Echo is aligned with Messages: Echo[i], when not zero, goes back with Messages[i]. It is nil or
	// as long as Messages. Only the loop fills it, and only for the current turn's assistant
	// messages produced by the model the request goes to (design §4.6).
	Echo []Echo `json:"-"`
}

// FinishLength is the FinishReason of a reply the provider cut at its output token limit (OpenAI
// "length", Anthropic "max_tokens" and "model_context_window_exceeded"): what it holds is unfinished.
const FinishLength = "length"

// MissingArguments is what an adapter stores for a tool call that arrived with no arguments at all.
// Some servers send nothing for an argumentless call, so after a reply the model ended itself
// (finish "stop" or "tool_calls"; OpenAI's legacy "function_call") it is the empty object. After any
// other finish, or none, nothing shows the call was finished, so it is an empty JSON string: not an
// object, so the loop answers the call with a tool error and never runs it.
func MissingArguments(finish string) json.RawMessage {
	switch finish {
	case "stop", "tool_calls", "function_call":
		return json.RawMessage("{}")
	}
	return json.RawMessage(`""`)
}

type ProviderResponse struct {
	Message      Message `json:"message"`
	Usage        Usage   `json:"usage"`
	FinishReason string  `json:"finishReason,omitempty"`
	// Echo is what the adapter needs back with Message in the rest of this turn; zero when none.
	Echo Echo `json:"-"`
}

type Provider interface {
	Complete(context.Context, ProviderRequest) (ProviderResponse, error)
	Model() string
}
