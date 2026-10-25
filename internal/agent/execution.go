package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// Side-effect tools are admitted before they run (ADR-0018, plan T1-S4). The loop takes the pre-state
// outside any transaction, records the execution with Store.AdmitExecution, runs the tool only after
// that returned nil, takes the post-state outside any transaction, and commits the tool row, the post
// record and the derived edges with Store.CommitToolResult. An execution that never reaches the commit
// (a crash, a cancellation, a failed write) stays admitted until Reconcile makes it outcome_unknown; it
// is never run again.

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
	case "write_file", "replace_in_file", "apply_patch":
		return tierFiles
	case "shell":
		return tierWorkspace
	case toolStartTask, toolSpawnSubagent:
		return tierAdmit
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
	case "write_file", "replace_in_file":
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

// execution is an admitted call between AdmitExecution and CommitToolResult.
type execution struct {
	id      string
	tier    executionTier
	targets []string
	files   repository.FilesSnapshot
	ws      repository.WorkspaceSnapshot
	// preOK: the pre-state was read whole and its ref was recorded.
	preOK bool
	// snapshot is the time spent taking the pre- and post-state.
	snapshot time.Duration
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
	x := &execution{id: id, tier: tier}
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
		ToolName: call.Name, ArgsDigest: digestOf(call.Arguments),
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
// tools validate before they write, so that call did nothing.
func (l *Loop) postExecution(ctx context.Context, x *execution, result protocol.ToolResult, toolErr error, requestID string, lineage bool) (*session.ExecutionResult, []session.DerivedEdge) {
	started := time.Now()
	defer func() { x.snapshot += time.Since(started) }()
	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	post := &session.ExecutionResult{ExecutionID: x.id, Coverage: session.CoverageComplete}
	var changed []repository.FileState
	switch x.tier {
	case tierFiles:
		changed, post.PostRef, post.Coverage = l.postFiles(sctx, x)
	case tierWorkspace:
		changed, post.PostRef, post.Coverage = l.postWorkspace(sctx, x)
	}
	switch {
	case toolErr == nil && result.OK:
		post.Outcome = session.OutcomeComplete
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
	for i := range edges {
		edges[i].LineageComplete = complete
	}
	return post, edges
}

// postFiles hashes the target paths again and returns those whose blob changed.
func (l *Loop) postFiles(ctx context.Context, x *execution) ([]repository.FileState, string, session.Coverage) {
	if l.Snapshots == nil {
		return nil, "", session.CoverageUnavailable
	}
	after, err := l.Snapshots.SnapshotFiles(ctx, x.targets)
	if err != nil {
		return nil, "", session.CoverageUnavailable
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
	return changed, after.Ref(), coverage
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
