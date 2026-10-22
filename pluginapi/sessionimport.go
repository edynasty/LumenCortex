package pluginapi

import (
	"context"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// SessionImportSource reads the sessions of another agent runtime (CapabilitySessionImport). It only
// reads: it never writes the source, never runs a tool, and returns visible history only. Core owns
// everything after LoadSession returns: bundle validation, bounds, native ids, idempotency,
// provenance, the trailing "imported" row and the one transaction that writes the sessions.
type SessionImportSource interface {
	Plugin
	// ImportSource describes the source. It must not change while the plugin is registered.
	ImportSource() ImportSourceInfo
	// ListSessions returns the top-level sessions under req.Root ("" is the source's default
	// directory), newest first, without reading their transcripts. A missing root is an empty list.
	ListSessions(ctx context.Context, req ImportListRequest) ([]ImportSourceSession, error)
	// LoadSession reads one top-level session and its subagent sessions. It must confine req.Path
	// to req.Root (or the default root) and refuse anything else.
	LoadSession(ctx context.Context, req ImportLoadRequest) (*ImportBundle, error)
}

// ImportSourceInfo describes an import source. ID is recorded in provenance and seeds native
// session ids, so it never changes for a source ([a-z][a-z0-9._-], at most 64 bytes). Converter
// is the version of the conversion rules, recorded with each imported session.
type ImportSourceInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Converter   int    `json:"converter"`
	DefaultRoot string `json:"defaultRoot,omitempty"`
}

// ImportListRequest selects the directory to list.
type ImportListRequest struct {
	Root string
}

// ImportLoadRequest names one top-level session to read.
type ImportLoadRequest struct {
	Root string
	Path string
}

// ImportSourceSession is one top-level session found by ListSessions.
type ImportSourceSession struct {
	Path      string    `json:"path"`
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Size      int64     `json:"size"`
	// Children counts the subagent sessions found beside the session.
	Children int `json:"children"`
}

// ImportBundle is one session tree read from a source: the top-level transcript and its subagent
// transcripts, parents before children.
type ImportBundle struct {
	Root     ImportTranscript
	Children []ImportTranscript
	// ChildrenSkipped counts subagent sessions the source left out because of its own bounds.
	ChildrenSkipped int
	// MaybeRunning is true when the source may still be writing the session.
	MaybeRunning bool
	// Warnings are source-specific findings shown in the preview.
	Warnings []ImportWarning
}

// ImportTranscript is one session of a bundle converted to LumenCortex rows.
//
// Messages hold visible history only: user, assistant and tool rows, where every tool call of an
// assistant row is answered by exactly one tool row directly after it (a call the source never
// answered gets a row with Origin tool_interrupted). Origin may be "", imported_notice,
// imported_marker or tool_interrupted. Hidden reasoning has no field here and must not be put into
// Content.
type ImportTranscript struct {
	// SourceID is the source's stable id of this session; it seeds the native id.
	SourceID string
	// Key is "" for the root and a unique slash path for a child; ParentKey names the child's parent.
	Key       string
	ParentKey string
	Path      string
	Title     string
	Cwd       string
	// Agent is the source's agent name of a subagent session.
	Agent string
	// ParentSession is the source's own parent reference, recorded in provenance only.
	ParentSession string
	// SHA256, Size and ModTime describe the bytes read; Core uses them to detect a changed source.
	SHA256  string
	Size    int64
	ModTime time.Time
	// Aborted is true when the source killed this subagent.
	Aborted bool
	// Exited is true when the source recorded that the session ended.
	Exited bool

	Goal          string
	Model         string
	FirstUser     string
	LastAssistant string
	Messages      []protocol.Message
	Stats         ImportStats
	// OpenToolCalls is true when the branch ended with calls the source has no result for.
	OpenToolCalls bool
	// OffBranch counts messages not on the active branch; Malformed records that could not be read.
	OffBranch int
	Malformed int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ImportWarning is one finding of a preview: a stable code and an English explanation.
type ImportWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ImportStats counts what a conversion copied, changed and left out.
type ImportStats struct {
	// Rows is the number of LumenCortex transcript rows written (Core sets it, counting its own
	// trailing imported row).
	Rows              int `json:"rows"`
	UserMessages      int `json:"userMessages"`
	AssistantMessages int `json:"assistantMessages"`
	ToolCalls         int `json:"toolCalls"`
	ToolResults       int `json:"toolResults"`
	// Notices are visible source notices copied as imported_notice rows; Markers are compaction and
	// branch-return markers.
	Notices int `json:"notices"`
	Markers int `json:"markers"`
	// ThinkingBlocks are hidden-reasoning blocks left out; HiddenMessages are developer/system
	// reminders and notices the source did not display; RetriedTurns are assistant turns without
	// calls that the source replaced by a retry or that carried no visible text.
	ThinkingBlocks int `json:"thinkingBlocks"`
	HiddenMessages int `json:"hiddenMessages"`
	RetriedTurns   int `json:"retriedTurns"`
	// Images are image blocks replaced by a text placeholder.
	Images int `json:"images"`
	// Compactions and BranchReturns are the source events written as markers (their summaries are
	// not copied).
	Compactions   int `json:"compactions"`
	BranchReturns int `json:"branchReturns"`
	// InterruptedCalls got a tool_interrupted result because the source recorded none;
	// OrphanResults answered no open call and were left out; DuplicateCalls repeated an id in one
	// turn and were left out.
	InterruptedCalls int `json:"interruptedCalls"`
	OrphanResults    int `json:"orphanResults"`
	DuplicateCalls   int `json:"duplicateCalls"`
	TruncatedRows    int `json:"truncatedRows"`
}
