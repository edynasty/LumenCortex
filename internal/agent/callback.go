package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// The tool callback is the sandbox capability an execution runtime gets from the loop. It is bound to
// the context of one admitted eval call and never widens the run's authority:
//
//   - Tools and Call both read the current step's effective specs (policy, allowlist, workflow);
//   - eval_*, delegation and subagent tools are refused, so a cell cannot recurse;
//   - bash, background jobs, lsp_rename and every exec-permission MCP tool are refused: the shell tool
//     inherits the host environment and is not sandboxed, so it is not a capability a kernel may use;
//   - a side-effecting tool runs only when the store can settle its execution without a transcript
//     row; otherwise it is refused rather than leaving an unmatched tool call.
//
// This is a description of what the sandbox may reach, not a second permission system.

// executionSettler is implemented by a Store that can settle an admitted execution on its own. A
// nested callback effect uses it so the transcript keeps exactly the top-level tool pairing.
type executionSettler interface {
	SettleExecution(ctx context.Context, sessionID string, result session.ExecutionResult, edges ...session.DerivedEdge) error
}

// callbackAllowedBuiltins are the non-exec tools a nested call may use. They read the workspace or
// the session's own state; the toolset confines them to the workspace.
var callbackAllowedBuiltins = map[string]bool{
	"read_file": true, "list_dir": true, "find_files": true, "search_text": true,
	"read_skill": true, "notebook_list": true, "notebook_read": true,
	"read_thread": true, "recall": true,
	"lsp_definition": true, "lsp_diagnostics": true, "lsp_document_symbols": true,
	"lsp_hover": true, "lsp_references": true, "lsp_workspace_symbols": true,
	// Workspace writes are reachable only when the store can settle their admitted execution
	// without a transcript row (executionSettler); Call refuses them otherwise. They stay inside
	// the workspace the toolset confines them to, and their file edges still invalidate a check
	// definition write.
	"write_file": true, "replace_in_file": true, "apply_patch": true,
	// edit_lines names one path, like replace_in_file; edit_go is left out because a structural rename
	// can write sibling files the call does not name, and a nested call's journal names its targets.
	"edit_lines": true,
}

// callbackDeniedPrefixes are tool families a callback never reaches.
var callbackDeniedPrefixes = []string{"eval", "job_", "wakeup_"}

const (
	// MaxCallbackJournal bounds the nested calls one eval tool call records.
	MaxCallbackJournal = 32
	// MaxCallbackDepth is the nesting a callback may reach. The loop never binds a second callback
	// into a nested call, so a depth above one is refused.
	MaxCallbackDepth = 1
)

// callbackJournal records the nested calls of one eval tool call. It is bounded and is written as an
// event/checkpoint, never as transcript rows: a nested call has no tool call of its own to answer.
type callbackJournal struct {
	Entries []callbackEntry `json:"entries"`
	Dropped int             `json:"dropped,omitempty"`
}

type callbackEntry struct {
	Seq         int    `json:"seq"`
	Tool        string `json:"tool"`
	OK          bool   `json:"ok"`
	Denied      bool   `json:"denied,omitempty"`
	Reason      string `json:"reason,omitempty"`
	ExecutionID string `json:"executionId,omitempty"`
	DurationMS  int64  `json:"durationMs"`
}

func (j *callbackJournal) add(entry callbackEntry) {
	if len(j.Entries) >= MaxCallbackJournal {
		j.Dropped++
		return
	}
	j.Entries = append(j.Entries, entry)
}

// callbackDispatcher implements protocol.ToolCaller for one admitted eval call.
type callbackDispatcher struct {
	loop       *Loop
	sessionID  string
	runID      string
	requestID  string
	step       int64
	parentCall string
	specs      []protocol.ToolSpec
	spend      *Spend
	budget     config.Budget
	lineage    bool
	journal    *callbackJournal
	// mu serializes Call: an active call in a JavaScript kernel can dispatch several callbacks at
	// once, and the dispatcher mutates the run's Spend and its own journal.
	mu     sync.Mutex
	seq    int
	closed bool
}

// toolCallback binds a dispatcher for one eval call. It returns nil for any other tool: a callback is
// a capability of the eval tool alone.
func (l *Loop) toolCallback(sessionID, runID, requestID string, step int64, call protocol.ToolCall, specs []protocol.ToolSpec, spend *Spend, budget config.Budget, lineage bool) protocol.ToolCaller {
	if !isEvalTool(call.Name) {
		return nil
	}
	return &callbackDispatcher{
		loop: l, sessionID: sessionID, runID: runID, requestID: requestID, step: step,
		parentCall: call.ID, specs: specs, spend: spend, budget: budget, lineage: lineage,
		journal: &callbackJournal{},
	}
}

// close drains the active callback before the loop reads shared spend or journal.
// Once closed, neither is mutated again, including by queued or detached calls.
func (d *callbackDispatcher) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
}

// isEvalTool reports the execution-runtime tool family (eval, eval_create, eval_control,
// eval_artifact, eval_runtimes, eval_sessions).
func isEvalTool(name string) bool {
	return name == "eval" || strings.HasPrefix(name, "eval_")
}

// callbackSpec reports whether a nested call may use spec. It is the whole sandbox policy of the
// bridge, in one place.
func callbackSpec(spec protocol.ToolSpec) bool {
	name := spec.Name
	if isEvalTool(name) || name == toolStartTask || name == toolSpawnSubagent || name == "wait_subagent" || name == "list_subagents" {
		return false
	}
	for _, prefix := range callbackDeniedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	if name == protocol.ToolBash || name == "lsp_rename" || name == "notebook_update" {
		return false
	}
	if strings.HasPrefix(name, "mcp__") {
		// An MCP tool keeps the permission it was registered with; only read and write are reachable
		// from a cell, never exec.
		return spec.Permission == "read" || spec.Permission == "write"
	}
	return callbackAllowedBuiltins[name]
}

// Tools returns the specs a nested call may use: the current step's effective list, filtered by the
// sandbox policy.
func (d *callbackDispatcher) Tools(ctx context.Context) ([]protocol.ToolSpec, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, fmt.Errorf("the eval callback capability is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]protocol.ToolSpec, 0, len(d.specs))
	for _, spec := range d.specs {
		if callbackSpec(spec) {
			out = append(out, spec)
		}
	}
	return out, nil
}

// Call runs one nested call under the run's own admission, budget and hooks. A refused call returns a
// denied result (the cell can handle it); a failure returns an error the bridge reports.
func (d *callbackDispatcher) Call(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
	// One callback at a time: concurrent active-call dispatches must not race the shared Spend or
	// journal, and the admission is bounded by the caller's deadline.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return protocol.ToolResult{Denied: true, Permission: "sandbox", Content: toolErrorContent("the eval callback capability is closed")}, nil
	}
	started := time.Now()
	d.seq++
	entry := callbackEntry{Seq: d.seq, Tool: name}
	deny := func(reason string) (protocol.ToolResult, error) {
		entry.Denied, entry.Reason, entry.DurationMS = true, reason, time.Since(started).Milliseconds()
		d.journal.add(entry)
		d.loop.emit("eval.callback", d.sessionID, map[string]any{"step": d.step, "tool": name, "denied": true, "reason": reason})
		return protocol.ToolResult{OK: false, Denied: true, Permission: "sandbox", Content: toolErrorContent(reason)}, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return deny("tool name is required")
	}
	if deadline, ok := ctx.Deadline(); ok && time.Now().After(deadline) {
		return deny("the call deadline passed before the callback was admitted")
	}
	if until, ok := protocol.ToolDeadline(ctx); ok && time.Now().After(until) {
		return deny("the tool deadline passed before the callback was admitted")
	}
	if err := ctx.Err(); err != nil {
		return protocol.ToolResult{}, err
	}
	// The effective step list is the authority: a tool the run does not offer is not offered here.
	if !containsTool(d.specs, name) {
		return deny("tool is not allowed in the current working set")
	}
	var spec protocol.ToolSpec
	for _, candidate := range d.specs {
		if candidate.Name == name {
			spec = candidate
			break
		}
	}
	if !callbackSpec(spec) {
		return deny("tool is outside the execution sandbox: a cell may use workspace read/state tools, workspace file writes whose execution the store can settle, and read-only MCP tools; it cannot run bash, background jobs, eval, delegation or native exec")
	}
	if d.budget.MaxToolCalls > 0 && d.spend.ToolCalls >= d.budget.MaxToolCalls {
		return deny(fmt.Sprintf("the run executed %d tool calls, its limit", d.spend.ToolCalls))
	}
	d.spend.ToolCalls++

	// Nested calls use the same BeforeToolCall authorization as top-level calls.
	pre := d.loop.runHook(ctx, d.sessionID, d.runID, verifyEventBeforeToolCall, HookRequest{
		Step: d.step, ToolCallID: d.parentCall, ToolName: name, ToolArgs: args,
		ValidateArgs: func(candidate map[string]any) error { return validateToolArgs(spec, candidate) },
	})
	if pre.Denied() {
		return deny("denied by a lifecycle hook: " + pre.FeedbackText())
	}
	if pre.Args != nil {
		args = mergeArgs(args, pre.Args)
		if err := validateToolArgs(spec, args); err != nil {
			return deny("the hook's replacement arguments are invalid for this tool: " + err.Error())
		}
	}

	tier := executionTierOf(name, d.specs)
	var x *execution
	var settler executionSettler
	if tier != tierNone {
		// A nested effect needs a store that can settle its execution without a transcript row.
		settler, _ = d.loop.Store.(executionSettler)
		if settler == nil || tier == tierWorkspace {
			return deny("tool has side effects and cannot be admitted inside a cell; call it from the run")
		}
		var cause *ExitCause
		if x, cause = d.loop.admitCallbackExecution(ctx, d.sessionID, d.runID, d.requestID, name, args, tier); cause != nil {
			return deny("admission failed: " + cause.Err.Error())
		}
		entry.ExecutionID = x.id
	}
	toolCtx := protocol.WithToolCallID(ctx, fmt.Sprintf("%s:cb%d", d.parentCall, d.seq))
	if until, ok := protocol.ToolDeadline(ctx); ok {
		toolCtx = protocol.WithToolDeadline(toolCtx, until)
	}
	result, err := d.loop.Tools.Execute(toolCtx, name, args, nil)
	if err != nil {
		result = protocol.ToolResult{OK: false, Content: toolErrorContent(err.Error())}
	}
	if x != nil {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()
		post, edges := d.loop.postExecution(settleCtx, x, result, err, d.requestID, d.lineage)
		if post != nil {
			if settleErr := settler.SettleExecution(settleCtx, d.sessionID, *post, edges...); settleErr != nil {
				// The tool may have acted and its admission is still open: the store's Reconcile turns
				// it outcome_unknown. A worker must not see this as a successful call, so the callback
				// returns the failure and an interrupted result. ErrFenced is terminal for the run.
				entry.OK, entry.Reason = false, "settlement failed: "+settleErr.Error()
				entry.DurationMS = time.Since(started).Milliseconds()
				d.journal.add(entry)
				d.loop.emit("eval.callback", d.sessionID, map[string]any{"step": d.step, "tool": name,
					"executionId": entry.ExecutionID, "settleError": settleErr.Error()})
				return protocol.ToolResult{Interrupted: true, OK: false, Permission: "sandbox",
					Content: toolErrorContent("the tool ran but its execution could not be settled: " + settleErr.Error())}, settleErr
			}
		}
	}
	post := d.loop.runHook(ctx, d.sessionID, d.runID, verifyEventAfterToolCall, HookRequest{
		Step: d.step, ToolCallID: d.parentCall, ToolName: name, ToolArgs: args, ToolResult: &result,
	})
	if feedback := post.FeedbackText(); feedback != "" {
		result.Content = appendHookFeedback(result.Content, feedback)
	}
	entry.OK, entry.DurationMS = result.OK, time.Since(started).Milliseconds()
	d.journal.add(entry)
	d.loop.emit("eval.callback", d.sessionID, map[string]any{"step": d.step, "tool": name, "ok": result.OK,
		"executionId": entry.ExecutionID, "durationMs": entry.DurationMS})
	return result, nil
}

// admitCallbackExecution is admitExecution for a nested call: it takes the pre-state and records the
// admission under a unique execution id, with the top-level request as the exposure it inherits.
func (l *Loop) admitCallbackExecution(ctx context.Context, sessionID, runID, requestID, name string, args map[string]any, tier executionTier) (*execution, *ExitCause) {
	id, err := newExecutionID()
	if err != nil {
		return nil, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	}
	x := &execution{id: id, tier: tier}
	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	var preRef string
	switch tier {
	case tierFiles:
		x.targets = fileTargets(name, args)
		if l.Snapshots != nil {
			if snap, err := l.Snapshots.SnapshotFiles(sctx, x.targets); err == nil {
				x.files, x.preOK, preRef = snap, snap.Complete, snap.Ref()
			}
		}
	case tierWorkspace:
		if l.Snapshots != nil {
			x.ws = l.Snapshots.SnapshotWorkspace(sctx)
			x.preOK, preRef = x.ws.OK && x.ws.Complete, x.ws.Ref()
		}
	}
	cancel()
	raw, _ := json.Marshal(args)
	a := session.ExecutionAdmission{
		SessionID: sessionID, RunID: runID, ExecutionID: id, RequestID: requestID,
		ToolName: name, ArgsDigest: digestOf(raw),
	}
	if tier != tierAdmit {
		if x.preOK {
			a.PreRef = preRef
		} else {
			a.PreUnavailable = true
		}
	}
	if err := l.Store.AdmitExecution(ctx, a); err != nil {
		return nil, storeExit(err)
	}
	return x, nil
}

// mergeArgs overlays a hook's replacement arguments on the call's.
func mergeArgs(args, replacement map[string]any) map[string]any {
	out := make(map[string]any, len(args)+len(replacement))
	for k, v := range args {
		out[k] = v
	}
	for k, v := range replacement {
		out[k] = v
	}
	return out
}

// appendHookFeedback appends bounded hook feedback to a tool result's content without changing what
// the tool itself reported.
func appendHookFeedback(content, feedback string) string {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return content
	}
	if len(feedback) > 1024 {
		feedback = feedback[:1024] + "…"
	}
	if strings.TrimSpace(content) == "" {
		return "[hook] " + feedback
	}
	return content + "\n\n[hook] " + feedback
}
