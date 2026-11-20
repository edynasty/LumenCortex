package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// Side-effect tools are admitted before they run (ADR-0018, plan T1-S4). The loop takes the pre-state
// outside any transaction, records the execution with Store.AdmitExecution, runs the tool only after
// that returned nil, takes the post-state outside any transaction, and commits the tool row, the post
// record and the derived edges with Store.CommitToolResult. The command tool's call, stopped by the run's
// cancellation or deadline, is committed that way before the run ends, settled outcome_unknown; an
// execution that never reaches a commit (any other tool's cancelled call, a crash, a failed write) stays
// admitted until Reconcile makes it outcome_unknown. It is never run again.

// Snapshots take the pre- and post-state of side-effect tools in the run's workspace. They only read.
// *repository.Service implements it.
type Snapshots interface {
	SnapshotFiles(ctx context.Context, paths []string) (repository.FilesSnapshot, error)
	SnapshotWorkspace(ctx context.Context) repository.WorkspaceSnapshot
}

// executionTier is how much a tool call records before it runs (plan T1-S4, frozen).
type executionTier int

const (
	// tierNone: local read-only tools and the in-memory Task State tools. No transaction before the
	// call; the result commit and the next request admission re-check the fences.
	tierNone executionTier = iota
	// tierAdmit: an admission without pre-state. MCP tools whose effective permission is not exec (the
	// arguments leave the process), start_task and spawn_subagent (they derive another session).
	tierAdmit
	// tierFiles: admission with the blobs of the target paths, and those blobs again afterwards.
	tierFiles
	// tierWorkspace: admission with a bounded work tree status and HEAD, and both again afterwards.
	// shell and exec-level MCP tools.
	tierWorkspace
)

const toolSpawnSubagent = "spawn_subagent"

// executionTierOf classifies a call by name and, for an MCP tool, by the effective permission its spec
// carries: the server annotation or the user override resolved at registration (ADR-0013); an MCP
// tool without one is exec.
func executionTierOf(name string, specs []protocol.ToolSpec) executionTier {
	switch name {
	case "write_file", "replace_in_file", "apply_patch", "edit_lines":
		return tierFiles
	case protocol.ToolBash:
		return tierWorkspace
	case "lsp_rename", "edit_go":
		// A rename or a structural Go edit writes files the call's arguments do not name (the language
		// server chooses them; a package rename reaches sibling files), so the whole work tree is
		// snapshotted, as for shell.
		return tierWorkspace
	case "eval_artifact":
		// An export writes a kernel artifact into the session workspace at a path the call names
		// (import and copy only read it). The workspace snapshot is taken whole because the action
		// and its path are one argument set: a pre/post pair that shows no change is complete, and a
		// real export keeps its file edges, so a check-definition write still invalidates approval.
		return tierWorkspace
	case "eval", "eval_create", "eval_control", "eval_runtimes", "eval_sessions":
		// The kernel's own directory is private to the runtime, not the session workspace: the call
		// records no workspace pre/post snapshot. Its spec still reports MutatesWorkspace, which the
		// loop uses to reopen the thread and to retire a closed check episode.
		return tierNone
	case toolStartTask, toolSpawnSubagent:
		return tierAdmit
	}
	if strings.HasPrefix(name, "debug_") {
		// Debugger tools launch, step, or evaluate inside a live debuggee process that can mutate
		// arbitrary workspace files (including Git-ignored outputs) without an editing.CommitReceipt.
		return tierWorkspace
	}
	if !strings.HasPrefix(name, "mcp__") {
		return tierNone
	}
	for _, spec := range specs {
		if spec.Name == name {
			if spec.Permission == "read" || spec.Permission == "write" {
				return tierAdmit
			}
			return tierWorkspace
		}
	}
	return tierWorkspace
}

// fileTargets are the paths a file tool's arguments name. Arguments the tool would refuse may name
// none; such a call changes nothing.
func fileTargets(name string, args map[string]any) []string {
	switch name {
	case "write_file", "replace_in_file", "edit_lines":
		if p, ok := args["path"].(string); ok {
			return []string{p}
		}
	case "apply_patch":
		var out []string
		patches, _ := args["patches"].([]any)
		for _, item := range patches {
			if m, ok := item.(map[string]any); ok {
				if p, ok := m["path"].(string); ok {
					out = append(out, p)
				}
			}
		}
		return out
	}
	return nil
}

// commitRecorder holds the private CommitReceipt captured for one ExecutionID (ADR-0024 §4.6).
type commitRecorder struct {
	mu      sync.Mutex
	has     bool
	receipt editing.CommitReceipt
}

func (c *commitRecorder) record(r editing.CommitReceipt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.has = true
	c.receipt = r
}

func (c *commitRecorder) Receipt() (editing.CommitReceipt, bool) {
	if c == nil {
		return editing.CommitReceipt{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.receipt, c.has
}

func receiptHasDirtyOrUncommittedPublish(r editing.CommitReceipt) bool {
	if r.DirtyFailure {
		return true
	}
	if r.Committed {
		return false
	}
	for _, f := range r.Files {
		if f.Published || f.Wrote ||
			f.PublishState == editing.PublishUncertain ||
			f.PublishState == editing.PublishRolledBack ||
			f.PublishState == editing.PublishRollbackFailed ||
			f.PublishState == editing.PublishExternalModification ||
			f.PublishState == editing.PublishSucceededWithPostError {
			return true
		}
	}
	return false
}

const (
	maxPreservedToolErrorBytes = 8192
	maxNestedPartialNotes      = 8
	maxNestedPartialNoteBytes  = 512
	maxNestedPartialFiles      = 8
)

func truncateAgentUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	if maxBytes <= len(ellipsis) {
		cut := maxBytes
		for cut > 0 && !utf8.ValidString(s[:cut]) {
			cut--
		}
		return s[:cut]
	}
	limit := maxBytes - len(ellipsis)
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// protectedToolResultKeys lists all provenance, session-lineage, identifier, and commit-state
// fields consumed by resultSessions / sessionResultEdges / cognitive runtime and repair gates.
// Structured truncation NEVER drops these fields (ADR-0024 R5-02).
var protectedToolResultKeys = map[string]bool{
	"sourceSessions":       true,
	"applicationId":        true,
	"adoptionId":           true,
	"nodeId":               true,
	"experienceId":         true,
	"sessionId":            true,
	"id":                   true,
	"withdrawn":            true,
	"session":              true,
	"node":                 true,
	"messages":             true,
	"children":             true,
	"evidence":             true,
	"hits":                 true,
	"sources":              true,
	"maintenanceSessionId": true,
	"candidates":           true,
	"state":                true,
	"ok":                   true,
	"error":                true,
	"code":                 true,
	"commitState":          true,
	"dirtyFailure":         true,
	"partialPublication":   true,
	"path":                 true,
	"hint":                 true,
}

func cloneRawJSONMap(src map[string]json.RawMessage) map[string]json.RawMessage {
	dst := make(map[string]json.RawMessage, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func marshalJSONValue(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// compactJSONObjectToBudget removes only non-protected bulk fields (largest first) and bounds any
// oversized "error" string so the serialized JSON object stays valid and <= maxBytes whenever
// protected provenance/identifier fields themselves fit within maxBytes (ADR-0024 R5-02).
func compactJSONObjectToBudget(base map[string]json.RawMessage, maxBytes int) (map[string]json.RawMessage, bool) {
	working := cloneRawJSONMap(base)
	type candidateKey struct {
		key  string
		size int
	}
	var dropOrder []candidateKey
	for k, v := range working {
		if protectedToolResultKeys[k] ||
			k == "nestedPartialPublications" ||
			k == "nestedPartialPublicationsOmitted" ||
			k == "nestedPartialPublicationsTruncated" ||
			k == "truncated" ||
			k == "omittedFields" {
			continue
		}
		dropOrder = append(dropOrder, candidateKey{key: k, size: len(v)})
	}
	sort.Slice(dropOrder, func(i, j int) bool {
		if dropOrder[i].size != dropOrder[j].size {
			return dropOrder[i].size > dropOrder[j].size
		}
		return dropOrder[i].key < dropOrder[j].key
	})

	changed := false
	var omitted []string
	for _, cand := range dropOrder {
		raw, err := json.Marshal(working)
		if err == nil && len(raw) <= maxBytes {
			break
		}
		delete(working, cand.key)
		omitted = append(omitted, cand.key)
		changed = true
		working["truncated"] = json.RawMessage("true")
	}
	if len(omitted) > 0 {
		sort.Strings(omitted)
		withOmitted := cloneRawJSONMap(working)
		withOmitted["omittedFields"] = marshalJSONValue(omitted)
		if raw, err := json.Marshal(withOmitted); err == nil && len(raw) <= maxBytes {
			working = withOmitted
		}
	}

	// If still over budget and "error" is a long string, bound "error" without dropping provenance.
	if raw, err := json.Marshal(working); err == nil && len(raw) > maxBytes {
		if rawErr, ok := working["error"]; ok {
			var errStr string
			if json.Unmarshal(rawErr, &errStr) == nil && len(errStr) > 256 {
				working["error"] = marshalJSONValue(truncateAgentUTF8(errStr, 256))
				working["truncated"] = json.RawMessage("true")
				changed = true
			}
		}
	}
	return working, changed
}

// preserveErrorToolContent preserves structured, bounded partial-publication or rollback failure
// details already built by a tool when the tool also returned a non-nil error (ADR-0024 R4-02-D, R5-02).
func preserveErrorToolContent(existing string, err error) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	trimmed := strings.TrimSpace(existing)
	if trimmed == "" {
		return toolErrorContent(msg)
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		var rawObj map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &rawObj) == nil && rawObj != nil {
			hasNonEmptyErr := false
			if rawErr, exists := rawObj["error"]; exists {
				var errText string
				if json.Unmarshal(rawErr, &errText) == nil && strings.TrimSpace(errText) != "" {
					hasNonEmptyErr = true
				}
			}
			if hasNonEmptyErr && len(trimmed) <= maxPreservedToolErrorBytes {
				return trimmed
			}
			if !hasNonEmptyErr {
				rawObj = cloneRawJSONMap(rawObj)
				rawObj["error"] = marshalJSONValue(truncateAgentUTF8(msg, 1024))
				if raw, mErr := json.Marshal(rawObj); mErr == nil && len(raw) <= maxPreservedToolErrorBytes {
					return string(raw)
				}
			}
			compacted, _ := compactJSONObjectToBudget(rawObj, maxPreservedToolErrorBytes)
			compacted["truncated"] = json.RawMessage("true")
			if raw, mErr := json.Marshal(compacted); mErr == nil {
				return string(raw)
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"error":   truncateAgentUTF8(msg, 1024),
		"details": truncateAgentUTF8(trimmed, 2048),
	})
	return string(raw)
}

func formatDirtyReceiptSummary(toolName string, r editing.CommitReceipt) string {
	partialPub := r.State == editing.CommitStatePartiallyPublished ||
		r.State == editing.CommitStatePublishedWithPostError ||
		r.State == editing.CommitStatePublicationUncertain ||
		(r.State == "" && receiptHasDirtyOrUncommittedPublish(r))
	state := r.State
	if state == "" {
		state = editing.CommitStatePublicationUncertain
	}
	var b strings.Builder
	b.WriteString(toolName)
	b.WriteString(" commitState=")
	b.WriteString(string(state))
	b.WriteString(" dirtyFailure=")
	b.WriteString(strconv.FormatBool(r.DirtyFailure || receiptHasDirtyOrUncommittedPublish(r)))
	b.WriteString(" partialPublication=")
	b.WriteString(strconv.FormatBool(partialPub))
	if len(r.Files) > 0 {
		b.WriteString(" files=[")
		limit := len(r.Files)
		if limit > maxNestedPartialFiles {
			limit = maxNestedPartialFiles
		}
		for i := 0; i < limit; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			rf := r.Files[i]
			b.WriteString(filepath.ToSlash(rf.Path))
			b.WriteString(":")
			b.WriteString(string(rf.PublishState))
		}
		if len(r.Files) > limit {
			b.WriteString(",...(+")
			b.WriteString(strconv.Itoa(len(r.Files) - limit))
			b.WriteString(")")
		}
		b.WriteString("]")
	}
	return truncateAgentUTF8(b.String(), maxNestedPartialNoteBytes)
}

// appendNestedPartialPublication attaches bounded nested partial-publication notes to a parent tool
// result while strictly preserving JSON validity and all required provenance/identifier fields when
// the parent result is JSON (ADR-0024 R5-02). Non-JSON text is used ONLY when the parent content is
// not valid JSON.
func appendNestedPartialPublication(content string, notes []string) string {
	if len(notes) == 0 {
		return content
	}
	var cleaned []string
	for _, n := range notes {
		if s := strings.TrimSpace(n); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	totalNotes := len(cleaned)
	if totalNotes == 0 {
		return content
	}

	buildBoundedNotes := func(countLimit, byteLimit int) []string {
		if countLimit <= 0 || byteLimit <= 0 {
			return nil
		}
		if countLimit > len(cleaned) {
			countLimit = len(cleaned)
		}
		out := make([]string, 0, countLimit)
		for _, s := range cleaned[:countLimit] {
			out = append(out, truncateAgentUTF8(s, byteLimit))
		}
		return out
	}

	bounded := buildBoundedNotes(maxNestedPartialNotes, maxNestedPartialNoteBytes)
	trimmed := strings.TrimSpace(content)

	if trimmed != "" && json.Valid([]byte(trimmed)) {
		var rawObj map[string]json.RawMessage
		if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &rawObj) == nil && rawObj != nil {
			tryEncodeWithNotes := func(base map[string]json.RawMessage, noteCount, noteBytes int, markTruncated bool) ([]byte, bool) {
				cand := cloneRawJSONMap(base)
				sub := buildBoundedNotes(noteCount, noteBytes)
				if len(sub) > 0 {
					cand["nestedPartialPublications"] = marshalJSONValue(sub)
				} else {
					delete(cand, "nestedPartialPublications")
					cand["nestedPartialPublicationsTruncated"] = json.RawMessage("true")
				}
				if omitted := totalNotes - len(sub); omitted > 0 {
					cand["nestedPartialPublicationsOmitted"] = marshalJSONValue(omitted)
					cand["nestedPartialPublicationsTruncated"] = json.RawMessage("true")
				} else if noteBytes < maxNestedPartialNoteBytes {
					for _, orig := range cleaned[:len(sub)] {
						if len(orig) > noteBytes {
							cand["nestedPartialPublicationsTruncated"] = json.RawMessage("true")
							break
						}
					}
				}
				if markTruncated {
					cand["truncated"] = json.RawMessage("true")
				}
				raw, err := json.Marshal(cand)
				if err != nil {
					return nil, false
				}
				return raw, len(raw) <= maxPreservedToolErrorBytes
			}

			// 1. Fast path: full original JSON object + bounded notes fits <= 8192 bytes.
			if raw, ok := tryEncodeWithNotes(rawObj, maxNestedPartialNotes, maxNestedPartialNoteBytes, false); ok {
				return string(raw)
			}

			// 2. Shrink only the nested notes first so a near-limit parent JSON keeps all fields untouched.
			for _, nc := range []int{4, 2, 1, 0} {
				for _, nb := range []int{256, 128, 64} {
					if nc == 0 && nb != 256 {
						continue
					}
					if raw, ok := tryEncodeWithNotes(rawObj, nc, nb, false); ok {
						return string(raw)
					}
				}
			}

			// 3. Parent JSON itself exceeds (or nearly exceeds) 8192 bytes: drop only non-protected
			// bulk fields (such as "results"/"pad") while preserving all provenance/identifier keys.
			compacted, _ := compactJSONObjectToBudget(rawObj, maxPreservedToolErrorBytes-256)
			for _, nc := range []int{maxNestedPartialNotes, 4, 2, 1, 0} {
				for _, nb := range []int{maxNestedPartialNoteBytes, 256, 128} {
					if nc == 0 && nb != maxNestedPartialNoteBytes {
						continue
					}
					if raw, ok := tryEncodeWithNotes(compacted, nc, nb, true); ok {
						return string(raw)
					}
				}
			}

			// 4. Protected provenance fields alone exceed 8192 bytes: return valid JSON preserving
			// every protected provenance field plus an explicit truncation indicator (never plain text).
			minimal, _ := compactJSONObjectToBudget(rawObj, 0)
			raw, _ := tryEncodeWithNotes(minimal, 1, 128, true)
			if len(raw) > 0 {
				return string(raw)
			}
		}

		// Valid non-object JSON (array or scalar): keep valid JSON rather than appending plain text.
		wrapped := map[string]any{
			"result":                    json.RawMessage(trimmed),
			"nestedPartialPublications": bounded,
		}
		if omitted := totalNotes - len(bounded); omitted > 0 {
			wrapped["nestedPartialPublicationsOmitted"] = omitted
		}
		if raw, err := json.Marshal(wrapped); err == nil && len(raw) <= maxPreservedToolErrorBytes {
			return string(raw)
		}
		fallback := map[string]any{
			"truncated":                 true,
			"nestedPartialPublications": buildBoundedNotes(2, 192),
		}
		if totalNotes > 2 {
			fallback["nestedPartialPublicationsOmitted"] = totalNotes - 2
		}
		raw, _ := json.Marshal(fallback)
		return string(raw)
	}

	block := "[nested_partial_publication] " + strings.Join(bounded, "; ")
	if trimmed == "" {
		return truncateAgentUTF8(block, maxPreservedToolErrorBytes)
	}
	return truncateAgentUTF8(content+"\n\n"+block, maxPreservedToolErrorBytes)
}

// execution is an admitted call between AdmitExecution and CommitToolResult.
type execution struct {
	id             string
	toolName       string
	tier           executionTier
	targets        []string
	files          repository.FilesSnapshot
	ws             repository.WorkspaceSnapshot
	preRef         string
	preUnavailable bool
	// preOK: the pre-state was read whole and its ref was recorded.
	preOK    bool
	recorder *commitRecorder
	// snapshot is the time spent taking the pre- and post-state.
	snapshot time.Duration
}

func (x *execution) bindContext(ctx context.Context) context.Context {
	if x == nil || x.recorder == nil {
		// Isolate non-admitted nested calls from any outer recorder on ctx.
		return editing.WithCommitRecorder(ctx, func(editing.CommitReceipt) {})
	}
	return editing.WithCommitRecorder(ctx, x.recorder.record)
}

var (
	testExecutionHooksMu    sync.RWMutex
	testHookAfterToolAdmit  func(toolName string, targets []string)
	testHookBeforePostFiles func(toolName string, targets []string)
)

// SetTestExecutionHooks installs deterministic timing hooks around execution admission and postFiles
// verification for causal concurrency tests; pass nil, nil to clear.
func SetTestExecutionHooks(afterAdmit, beforePostFiles func(toolName string, targets []string)) {
	testExecutionHooksMu.Lock()
	defer testExecutionHooksMu.Unlock()
	testHookAfterToolAdmit = afterAdmit
	testHookBeforePostFiles = beforePostFiles
}

func runAfterToolAdmitHook(toolName string, targets []string) {
	testExecutionHooksMu.RLock()
	fn := testHookAfterToolAdmit
	testExecutionHooksMu.RUnlock()
	if fn != nil {
		fn(toolName, targets)
	}
}

func runBeforePostFilesHook(toolName string, targets []string) {
	testExecutionHooksMu.RLock()
	fn := testHookBeforePostFiles
	testExecutionHooksMu.RUnlock()
	if fn != nil {
		fn(toolName, targets)
	}
}

// snapshotTimeout bounds each snapshot phase (the pre-state, and the post-state with its hashing). A
// phase that runs out is unavailable, not an error: the tool still runs and its record says so. The
// measured pre+post pair on a 10k-file work tree is well under this (plan T1-S4, report).
var snapshotTimeout = 5 * time.Second

func newExecutionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ex-" + hex.EncodeToString(b[:]), nil
}

// admitExecution takes the pre-state and admits the call. It returns the exit the run takes instead
// of running the tool: cancellation before the admission (nothing was recorded), a fenced session
// (nothing was recorded and nothing may run, R-1), or a storage failure (fail closed).
func (l *Loop) admitExecution(ctx context.Context, sessionID string, opts Options, requestID string, call protocol.ToolCall, args map[string]any, tier executionTier) (*execution, *ExitCause) {
	id, err := newExecutionID()
	if err != nil {
		return nil, &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	}
	x := &execution{id: id, toolName: call.Name, tier: tier, recorder: &commitRecorder{}}
	started := time.Now()
	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	var preRef string
	switch tier {
	case tierFiles:
		x.targets = fileTargets(call.Name, args)
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
	x.snapshot = time.Since(started)
	if err := ctx.Err(); err != nil {
		return nil, &ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err}
	}
	a := session.ExecutionAdmission{
		SessionID: sessionID, RunID: opts.RunID, ExecutionID: id, RequestID: requestID,
		ToolCallID: call.ID,
		ToolName:   call.Name, ArgsDigest: digestOf(call.Arguments),
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
	runAfterToolAdmitHook(call.Name, x.targets)
	return x, nil
}

// storeExit is the exit for a failed admission or commit: a fence ends the run as revoked without a
// retry (R-1), anything else is a storage failure.
func storeExit(err error) *ExitCause {
	if errors.Is(err, session.ErrFenced) {
		return &ExitCause{Status: StatusFailed, Kind: KindFenced, Err: err}
	}
	return &ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
}

// postExecution takes the post-state of an admitted call that returned, and builds its post record and
// derived edges: derived(path, blob) <- the request whose answer issued the call. toolErr is the error
// the tool returned, if any; lineage is whether that request's envelope is lineage-complete.
//
// Outcome: a result the tool returned is completed when OK and failed otherwise. A tool that returned
// an error may still have acted (an MCP server got the arguments, a command started), so the outcome
// is unknown, except for a file tool whose targets the post-state shows unchanged and whole: the file
// tools validate before they write, so that call did nothing. A CommitReceipt reporting DirtyFailure
// upgrades the outcome to OutcomeUnknown and seals ";cm=unknown".
func (l *Loop) postExecution(ctx context.Context, x *execution, result protocol.ToolResult, toolErr error, requestID string, lineage bool) (*session.ExecutionResult, []session.DerivedEdge) {
	started := time.Now()
	defer func() { x.snapshot += time.Since(started) }()
	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	post := &session.ExecutionResult{ExecutionID: x.id, Coverage: session.CoverageComplete}
	var changed []repository.FileState
	var postSnap repository.FilesSnapshot
	var postSnapOK bool
	receipt, hasReceipt := x.recorder.Receipt()
	var sandwichOK bool
	switch x.tier {
	case tierFiles:
		changed, postSnap, postSnapOK, post.Coverage, sandwichOK = l.postFiles(sctx, x, receipt, hasReceipt)
	case tierWorkspace:
		changed, post.PostRef, post.Coverage = l.postWorkspace(sctx, x)
	}
	dirtyReceipt := receiptHasDirtyOrUncommittedPublish(receipt)
	switch {
	case dirtyReceipt:
		post.Outcome = session.OutcomeUnknown
	case toolErr == nil && result.OK:
		post.Outcome = session.OutcomeComplete
	case toolErr == nil && result.Interrupted:
		// The tool stopped the operation partway (a command past its timeout): its effects are unknown.
		post.Outcome = session.OutcomeUnknown
	case toolErr == nil:
		post.Outcome = session.OutcomeFailed
	case x.tier == tierFiles && post.Coverage == session.CoverageComplete && len(changed) == 0:
		post.Outcome = session.OutcomeFailed
	default:
		post.Outcome = session.OutcomeUnknown
	}
	complete := lineage && post.Coverage == session.CoverageComplete && post.Outcome != session.OutcomeUnknown
	edges := make([]session.DerivedEdge, 0, len(changed))
	for _, f := range changed {
		ref, ok := fileEdgeRef(f)
		if !ok {
			// A path the edge column cannot hold is not recorded, so what is stored is incomplete.
			complete = false
			if post.Coverage == session.CoverageComplete {
				post.Coverage = session.CoveragePartial
			}
			continue
		}
		edges = append(edges, session.DerivedEdge{
			Kind: "file", TargetRef: ref, SourceKind: session.SourceRequest, SourceRef: requestID,
			OriginClass: session.OriginAgentGen,
		})
	}
	if x.tier == tierFiles && postSnapOK {
		seal := sealFilesMutation(x, post.Outcome, post.Coverage, postSnap, changed, receipt, hasReceipt, sandwichOK)
		post.PostRef = postSnap.Ref() + ";cm=" + seal
	}
	for i := range edges {
		edges[i].LineageComplete = complete
	}
	return post, edges
}

// postFiles hashes the target paths again, verifies raw disk SHA-256 before and after SnapshotFiles
// against CommitReceipt.NewSHA256, and returns changed files plus causal sandwich verification status.
func (l *Loop) postFiles(ctx context.Context, x *execution, receipt editing.CommitReceipt, hasReceipt bool) ([]repository.FileState, repository.FilesSnapshot, bool, session.Coverage, bool) {
	runBeforePostFilesHook(x.toolName, x.targets)
	if l.Snapshots == nil {
		return nil, repository.FilesSnapshot{}, false, session.CoverageUnavailable, false
	}
	var preInfos []os.FileInfo
	sandwichOK := false
	if hasReceipt && receipt.Committed && !receipt.DirtyFailure {
		preInfos, sandwichOK = verifyReceiptDiskBefore(receipt.Files)
	}
	after, err := l.Snapshots.SnapshotFiles(ctx, x.targets)
	if err != nil {
		return nil, repository.FilesSnapshot{}, false, session.CoverageUnavailable, false
	}
	if sandwichOK {
		sandwichOK = verifyReceiptDiskAfter(receipt.Files, preInfos)
	}
	var changed []repository.FileState
	for _, f := range after.Files {
		if before, ok := x.files.Blob(f.Path); ok && before == f.Blob {
			continue
		}
		if f.Blob != "" {
			changed = append(changed, f)
		}
	}
	coverage := session.CoverageComplete
	switch {
	case !x.preOK:
		coverage = session.CoverageUnavailable
	case !after.Complete:
		coverage = session.CoveragePartial
	}
	return changed, after, true, coverage, sandwichOK
}

func verifyReceiptDiskBefore(files []editing.FileCommitReceipt) ([]os.FileInfo, bool) {
	infos := make([]os.FileInfo, len(files))
	for i, rf := range files {
		if !rf.Wrote {
			continue
		}
		if rf.AbsPath == "" {
			return nil, false
		}
		info, err := os.Lstat(rf.AbsPath)
		if rf.Deleted {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, false
			}
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > editing.MaxFileBytes {
			return nil, false
		}
		if digest, ok := rawFileSHA256(rf.AbsPath); !ok || digest != rf.NewSHA256 {
			return nil, false
		}
		infos[i] = info
	}
	return infos, true
}

func verifyReceiptDiskAfter(files []editing.FileCommitReceipt, preInfos []os.FileInfo) bool {
	if len(preInfos) != len(files) {
		return false
	}
	for i, rf := range files {
		if !rf.Wrote {
			continue
		}
		info, err := os.Lstat(rf.AbsPath)
		if rf.Deleted {
			if !errors.Is(err, os.ErrNotExist) {
				return false
			}
			continue
		}
		pre := preInfos[i]
		if err != nil || pre == nil || !info.Mode().IsRegular() ||
			!os.SameFile(pre, info) || info.Size() != pre.Size() ||
			info.ModTime().UnixNano() != pre.ModTime().UnixNano() {
			return false
		}
		if digest, ok := rawFileSHA256(rf.AbsPath); !ok || digest != rf.NewSHA256 {
			return false
		}
	}
	return true
}

func rawFileSHA256(absPath string) (string, bool) {
	f, err := os.Open(absPath)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, int64(editing.MaxFileBytes)+1))
	if err != nil || len(buf) > editing.MaxFileBytes {
		return "", false
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:]), true
}

func sealFilesMutation(
	x *execution,
	outcome session.ExecutionOutcome,
	coverage session.Coverage,
	after repository.FilesSnapshot,
	changed []repository.FileState,
	receipt editing.CommitReceipt,
	hasReceipt bool,
	sandwichOK bool,
) string {
	if receiptHasDirtyOrUncommittedPublish(receipt) ||
		outcome != session.OutcomeComplete ||
		x.preUnavailable ||
		!hasPositiveFilesSnapshotRef(x.preRef) ||
		coverage != session.CoverageComplete ||
		!hasReceipt ||
		!receipt.Committed ||
		len(receipt.Files) == 0 ||
		len(receipt.Files) != len(x.targets) ||
		len(receipt.Files) != len(x.files.Files) ||
		len(receipt.Files) != len(after.Files) {
		return string(MutationUnknown)
	}
	preByPath := make(map[string]repository.FileState, len(x.files.Files))
	for _, preF := range x.files.Files {
		key := filepath.ToSlash(preF.Path)
		if key == "" || preF.Blob == "" {
			return string(MutationUnknown)
		}
		if _, dup := preByPath[key]; dup {
			return string(MutationUnknown)
		}
		preByPath[key] = preF
	}
	postByPath := make(map[string]repository.FileState, len(after.Files))
	for _, postF := range after.Files {
		key := filepath.ToSlash(postF.Path)
		if key == "" || postF.Blob == "" {
			return string(MutationUnknown)
		}
		if _, dup := postByPath[key]; dup {
			return string(MutationUnknown)
		}
		postByPath[key] = postF
	}

	seenReceipt := make(map[string]bool, len(receipt.Files))
	wroteCount := 0
	for _, rf := range receipt.Files {
		key := filepath.ToSlash(rf.Path)
		if key == "" || seenReceipt[key] {
			return string(MutationUnknown)
		}
		seenReceipt[key] = true
		preF, okPre := preByPath[key]
		postF, okPost := postByPath[key]
		if !okPre || !okPost {
			return string(MutationUnknown)
		}
		if !rf.Wrote {
			if preF.Blob != postF.Blob {
				return string(MutationUnknown)
			}
			continue
		}
		wroteCount++
		if preF.Blob == postF.Blob {
			return string(MutationUnknown)
		}
		if rf.Deleted && postF.Blob != repository.BlobAbsent {
			return string(MutationUnknown)
		}
	}
	afterRef := after.Ref()
	if wroteCount == 0 {
		if len(changed) == 0 && x.preRef == afterRef {
			return string(MutationNone)
		}
		return string(MutationUnknown)
	}
	if sandwichOK && x.preRef != afterRef {
		return string(MutationProven)
	}
	return string(MutationUnknown)
}

// hasPositiveFilesSnapshotRef reports whether ref is a valid FilesSnapshot ref ("files:<N>:sha256:<64hex>")
// naming at least one target path (N >= 1).
func hasPositiveFilesSnapshotRef(ref string) bool {
	if !strings.HasPrefix(ref, "files:") {
		return false
	}
	rest := strings.TrimPrefix(ref, "files:")
	idx := strings.Index(rest, ":sha256:")
	if idx <= 0 {
		return false
	}
	count, err := strconv.Atoi(rest[:idx])
	if err != nil || count < 1 {
		return false
	}
	hexPart := rest[idx+len(":sha256:"):]
	if len(hexPart) != 64 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		ch := hexPart[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

// splitFilesSnapshotPostRef splits a tierFiles post_ref into its base FilesSnapshot ref and its causal
// seal ("proven", "none", or "unknown"). Legacy rows without a valid ";cm=" seal return seal == "".
func splitFilesSnapshotPostRef(postRef string) (snapRef string, seal string) {
	idx := strings.LastIndex(postRef, ";cm=")
	if idx <= 0 {
		return postRef, ""
	}
	base := postRef[:idx]
	candidate := postRef[idx+len(";cm="):]
	switch candidate {
	case string(MutationProven), string(MutationNone), string(MutationUnknown):
		if hasPositiveFilesSnapshotRef(base) {
			return base, candidate
		}
	}
	return postRef, ""
}

// postWorkspace reads the work tree again, lists the paths whose status entry changed and hashes at most
// MaxEdgesPerCommit of them. A moved HEAD, a cut status, more paths than that, or a path that cannot be
// hashed makes the coverage partial.
func (l *Loop) postWorkspace(ctx context.Context, x *execution) ([]repository.FileState, string, session.Coverage) {
	if l.Snapshots == nil {
		return nil, "", session.CoverageUnavailable
	}
	after := l.Snapshots.SnapshotWorkspace(ctx)
	if !x.ws.OK || !after.OK {
		return nil, after.Ref(), session.CoverageUnavailable
	}
	paths, whole := repository.ChangedPaths(x.ws, after)
	if len(paths) > session.MaxEdgesPerCommit {
		paths, whole = paths[:session.MaxEdgesPerCommit], false
	}
	var changed []repository.FileState
	if len(paths) > 0 {
		blobs, err := l.Snapshots.SnapshotFiles(ctx, paths)
		if err != nil {
			return nil, after.Ref(), session.CoveragePartial
		}
		whole = whole && blobs.Complete
		for _, f := range blobs.Files {
			if f.Blob != "" {
				changed = append(changed, f)
			}
		}
	}
	coverage := session.CoverageComplete
	if !whole || !x.preOK {
		coverage = session.CoveragePartial
	}
	return changed, after.Ref(), coverage
}

// fileEdgeRef is the target ref of a file edge, "<path>@<blob>" (blob "-" for a path the call
// removed), or false when it does not fit the edge column (printable UTF-8, at most MaxEdgeRefBytes).
func fileEdgeRef(f repository.FileState) (string, bool) {
	ref := f.Path + "@" + f.Blob
	if len(ref) > session.MaxEdgeRefBytes || !utf8.ValidString(ref) || strings.IndexFunc(ref, unicode.IsControl) >= 0 {
		return "", false
	}
	return ref, true
}
