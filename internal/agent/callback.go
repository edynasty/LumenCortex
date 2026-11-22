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
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/verify"
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
	SettleExecution(ctx context.Context, sessionID string, result session.ExecutionResult, change *TaskStateChange, edges ...session.DerivedEdge) (int64, error)
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
var callbackDeniedPrefixes = []string{"eval", "job_", "wakeup_", "debug_"}

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
	loop        *Loop
	sessionID   string
	runID       string
	requestID   string
	step        int64
	parentCall  string
	specs       []protocol.ToolSpec
	spend       *Spend
	budget      config.Budget
	lineage     bool
	application bool
	journal     *callbackJournal
	run         *run
	stepTools   StepToolsInput
	stop        *ExitCause
	// mu serializes Call: an active call in a JavaScript kernel can dispatch several callbacks at
	// once, and the dispatcher mutates the run's Spend and its own journal.
	mu              sync.Mutex
	seq             int
	closed          bool
	partialNotes    []string
	hadDirtyPublish bool
}

func (d *callbackDispatcher) dirtyPartialSummary() ([]string, bool) {
	if d == nil {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.partialNotes) == 0 {
		return nil, d.hadDirtyPublish
	}
	out := append([]string(nil), d.partialNotes...)
	return out, d.hadDirtyPublish
}

// toolCallback binds the existing dispatcher for an eval or explicit Experience
// application. Both keep the same sandbox/tool/budget policy; neither recurses.
func (l *Loop) toolCallback(sessionID, runID, requestID string, step int64, call protocol.ToolCall, specs []protocol.ToolSpec, spend *Spend, budget config.Budget, lineage bool) protocol.ToolCaller {
	if !isEvalTool(call.Name) && call.Name != "apply_experience" {
		return nil
	}
	return &callbackDispatcher{
		loop: l, sessionID: sessionID, runID: runID, requestID: requestID, step: step,
		parentCall: call.ID, specs: specs, spend: spend, budget: budget, lineage: lineage, application: call.Name == "apply_experience",
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

// An explicit application is already in the host-authorized main run; it may
// execute a foreground bash action. Eval cells keep their narrower sandbox.
func (d *callbackDispatcher) allowed(spec protocol.ToolSpec) bool {
	if d.application && spec.Name == protocol.ToolBash {
		return true
	}
	return callbackSpec(spec)
}

func (d *callbackDispatcher) effectiveSpecs() []protocol.ToolSpec {
	if !d.application || d.run == nil {
		return d.specs
	}
	if d.stop != nil {
		return nil
	}
	input := d.stepTools
	if d.run.wf != nil {
		if len(d.run.wf.WaitingHumanGates()) > 0 {
			return nil
		}
		input.Allowlist = d.run.wf.EffectiveAllowlist(input.Allowlist)
	}
	return StepSpecs(d.loop.Tools, input)
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
	specs := d.effectiveSpecs()
	out := make([]protocol.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if d.allowed(spec) {
			out = append(out, spec)
		}
	}
	return out, nil
}

// Call runs one nested call under the run's own admission, budget and hooks. A refused call returns a
// denied result (the cell can handle it); a failure returns an error the bridge reports.
func (d *callbackDispatcher) Call(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
	// One callback at a time; its live authority, state and budget have one owner.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return protocol.ToolResult{Denied: true, Permission: "sandbox", Content: toolErrorContent("the eval callback capability is closed")}, nil
	}
	started := time.Now()
	d.seq++
	callID := fmt.Sprintf("%s:cb%d", d.parentCall, d.seq)
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
	specs := d.effectiveSpecs()
	if !containsTool(specs, name) {
		return deny("tool is not allowed in the current working set")
	}
	var spec protocol.ToolSpec
	for _, candidate := range specs {
		if candidate.Name == name {
			spec = candidate
			break
		}
	}
	if !d.allowed(spec) {
		return deny("tool is outside the execution sandbox: callbacks cannot recurse, delegate or start background jobs; only explicit Experience applications may run foreground bash")
	}
	if d.application && name == protocol.ToolBash && args["mode"] == "background" {
		return deny("experience actions must finish in the current run")
	}
	if err := validateToolArgs(spec, args); err != nil {
		return deny(err.Error())
	}
	if d.budget.MaxToolCalls > 0 && d.spend.ToolCalls >= d.budget.MaxToolCalls {
		return deny(fmt.Sprintf("the run executed %d tool calls, its limit", d.spend.ToolCalls))
	}
	d.spend.ToolCalls++
	pre := d.loop.runHook(ctx, d.sessionID, d.runID, verifyEventBeforeToolCall, HookRequest{
		Step: d.step, ToolCallID: callID, ToolName: name, ToolArgs: args,
		ValidateArgs: func(candidate map[string]any) error { return validateToolArgs(spec, candidate) },
	})
	if pre.Denied() {
		return deny("denied by a lifecycle hook: " + pre.FeedbackText())
	}
	if pre.Args != nil {
		if d.application {
			before, _ := json.Marshal(args)
			after, _ := json.Marshal(mergeArgs(args, pre.Args))
			if string(before) != string(after) {
				return deny("a hook changed the observed procedure; use a new explicit action")
			}
		}
		args = mergeArgs(args, pre.Args)
		if err := validateToolArgs(spec, args); err != nil {
			return deny("the hook's replacement arguments are invalid for this tool: " + err.Error())
		}
	}
	tier := executionTierOf(name, specs)
	if tier == tierNone && d.application {
		tier = tierAdmit
	}
	var x *execution
	var settler executionSettler
	if tier != tierNone {
		settler, _ = d.loop.Store.(executionSettler)
		if settler == nil || (tier == tierWorkspace && !d.application) {
			return deny("tool has side effects and cannot be admitted inside a cell; call it from the run")
		}
		var cause *ExitCause
		if x, cause = d.loop.admitCallbackExecution(ctx, d.sessionID, d.runID, d.requestID, callID, d.parentCall, name, args, tier); cause != nil {
			return deny("admission failed: " + cause.Err.Error())
		}
		entry.ExecutionID = x.id
	}
	toolCtx := protocol.WithToolCallID(ctx, callID)
	if until, ok := protocol.ToolDeadline(ctx); ok {
		toolCtx = protocol.WithToolDeadline(toolCtx, until)
	}
	toolCtx = x.bindContext(toolCtx)
	result, toolErr := d.loop.Tools.Execute(toolCtx, name, args, nil)
	// A tool cannot manufacture dispatcher-owned execution or provenance metadata.
	result.ExecutionID, result.ExecutionOutcome, result.SourceSessions = "", "", nil
	var sources []string
	sourceComplete := true
	if d.application {
		sources, sourceComplete = resultSessions(name, d.sessionID, result)
	}
	if toolErr != nil {
		result.OK = false
		result.Content = preserveErrorToolContent(result.Content, toolErr)
	}
	if !sourceComplete {
		result.OK, result.Interrupted = false, true
		result.Content = toolErrorContent("nested result provenance is incomplete; content withheld")
	}
	postHook := d.loop.runHook(ctx, d.sessionID, d.runID, verifyEventAfterToolCall, HookRequest{
		Step: d.step, ToolCallID: callID, ToolName: name, ToolArgs: args, ToolResult: &result,
	})
	if feedback := postHook.FeedbackText(); feedback != "" {
		result.Content = appendHookFeedback(result.Content, feedback)
	}
	var transitioned, workflowDenied bool
	if d.application && d.run != nil {
		if result.MutatesWorkspace {
			d.run.ts.Reopen(time.Now().UTC())
			d.run.mutated = true
		}
		if d.loop.Checks != nil && (tier != tierAdmit || !callbackAllowedBuiltins[name] || result.MutatesWorkspace) {
			d.loop.Checks.Mutated()
		}
		var wfErr error
		transitioned, workflowDenied, wfErr = d.loop.observeWorkflow(ctx, d.run, d.sessionID, d.runID, d.step, name, args, result)
		if wfErr != nil {
			d.stop = &ExitCause{Status: StatusFailed, Kind: KindConfig, Err: wfErr}
		} else if workflowDenied {
			d.stop = &ExitCause{Status: StatusInterrupted, Kind: "hook_denied", Err: fmt.Errorf("workflow transition denied by lifecycle hook")}
		}
	}
	if x != nil {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()
		post, edges := d.loop.postExecutionWithResultLineage(settleCtx, x, result, toolErr, d.requestID, d.lineage, sourceComplete)
		if rec, ok := x.recorder.Receipt(); ok && receiptHasDirtyOrUncommittedPublish(rec) {
			d.hadDirtyPublish = true
			note := formatDirtyReceiptSummary(name, rec)
			if len(d.partialNotes) < maxNestedPartialNotes {
				d.partialNotes = append(d.partialNotes, note)
			}
			if entry.Reason == "" {
				entry.Reason = note
			}
		}
		// Sources bind to the actual parent tool row, not an unmatched nested row.
		edges = append(edges, d.loop.sessionSourceEdges(settleCtx, protocol.ToolCall{ID: d.parentCall, Name: name}, d.requestID, d.lineage, sources, sourceComplete)...)
		var change *TaskStateChange
		var settleErr error
		if d.application && d.run != nil {
			change, settleErr = d.run.change(taskstate.ReasonToolPrefix + name)
			if settleErr == nil && change != nil {
				var before taskstate.State
				before, settleErr = taskstate.Decode(0, d.run.saved)
				if settleErr == nil {
					edges = append(edges, modelStateEdges(before, change.Next, d.requestID, d.lineage)...)
				}
			}
		}
		var version int64
		if settleErr == nil {
			version, settleErr = settler.SettleExecution(settleCtx, d.sessionID, *post, change, edges...)
		}
		if settleErr != nil {
			d.stop = storeExit(settleErr)
			entry.OK, entry.Reason = false, "settlement failed: "+settleErr.Error()
			entry.DurationMS = time.Since(started).Milliseconds()
			d.journal.add(entry)
			d.loop.emit("eval.callback", d.sessionID, map[string]any{"step": d.step, "tool": name,
				"executionId": entry.ExecutionID, "settleError": settleErr.Error()})
			return protocol.ToolResult{Interrupted: true, MutatesWorkspace: result.MutatesWorkspace, Permission: "sandbox",
				Content: toolErrorContent("the tool ran but its execution could not be settled: " + settleErr.Error())}, settleErr
		}
		if change != nil {
			d.run.stored(change, version)
		}
		result.ExecutionID, result.ExecutionOutcome = x.id, string(post.Outcome)
		result.SourceSessions = sources
	}
	if transitioned {
		d.loop.runHook(ctx, d.sessionID, d.runID, verify.EventAfterWorkflowTransition, HookRequest{Step: d.step, Detail: map[string]any{"action": d.run.wf.CurrentAction}})
	}
	if d.stop != nil {
		result.OK, result.Interrupted = false, true
		toolErr = d.stop.Err
	}
	entry.OK, entry.DurationMS = result.OK, time.Since(started).Milliseconds()
	d.journal.add(entry)
	d.loop.emit("eval.callback", d.sessionID, map[string]any{"step": d.step, "tool": name, "ok": result.OK,
		"executionId": entry.ExecutionID, "durationMs": entry.DurationMS})
	return result, toolErr
}

// admitCallbackExecution is admitExecution for a nested call: it takes the pre-state and records the
// admission under a unique execution id, preserving both the nested callID and originating top-level
// parentCallID alongside the top-level request exposure it inherits (ADR-0024 R4-01).
func (l *Loop) admitCallbackExecution(ctx context.Context, sessionID, runID, requestID, callID, parentCallID, name string, args map[string]any, tier executionTier) (*execution, *ExitCause) {
	id, err := newExecutionID()
	if err != nil {
		return nil, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	}
	x := &execution{id: id, toolName: name, tier: tier, recorder: &commitRecorder{}}
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
		ToolCallID: callID, ParentToolCallID: parentCallID,
		ToolName: name, ArgsDigest: digestOf(raw),
	}
	if tier != tierAdmit {
		if x.preOK {
			a.PreRef = preRef
			x.preRef = preRef
		} else {
			a.PreUnavailable = true
			x.preUnavailable = true
		}
	}
	if err := l.Store.AdmitExecution(ctx, a); err != nil {
		return nil, storeExit(err)
	}
	runAfterToolAdmitHook(name, x.targets)
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
	feedback = truncateAgentUTF8(feedback, 1024)
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &obj) == nil && obj != nil && isCommitDisplay(obj) {
		obj["hookFeedback"] = marshalJSONValue(feedback)
		if raw, err := json.Marshal(obj); err == nil && len(raw) <= maxPreservedToolErrorBytes {
			return string(raw)
		}
		return content // Optional feedback cannot displace the tool receipt or sources.
	}
	if strings.TrimSpace(content) == "" {
		return "[hook] " + feedback
	}
	return content + "\n\n[hook] " + feedback
}
