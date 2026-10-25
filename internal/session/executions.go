package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// ExecutionOutcome is the persisted outcome of an admitted tool execution.
//
//	admitted ──► completed        (CommitToolResult, tool returned success)
//	admitted ──► failed           (CommitToolResult, tool returned an error or was denied after admission)
//	admitted ──► outcome_unknown  (CommitToolResult for a tool whose effect cannot be told, or Reconcile)
//
// completed, failed and outcome_unknown are terminal. An execution that was admitted and has no post
// record is never replayed: Reconcile marks it outcome_unknown and lineage_complete = 0.
type ExecutionOutcome string

const (
	OutcomeAdmitted ExecutionOutcome = "admitted"
	OutcomeComplete ExecutionOutcome = "completed"
	OutcomeFailed   ExecutionOutcome = "failed"
	OutcomeUnknown  ExecutionOutcome = "outcome_unknown"
)

func (o ExecutionOutcome) valid() bool {
	switch o {
	case OutcomeAdmitted, OutcomeComplete, OutcomeFailed, OutcomeUnknown:
		return true
	}
	return false
}

// Coverage says how much of the tool's effect the post snapshot captured.
type Coverage string

const (
	CoverageComplete    Coverage = "complete"
	CoveragePartial     Coverage = "partial"
	CoverageUnavailable Coverage = "unavailable"
)

func (c Coverage) valid() bool {
	return c == CoverageComplete || c == CoveragePartial || c == CoverageUnavailable
}

// ExecutionAdmission is the record written before a side-effect tool runs. The pre-state is captured
// by the caller before this call (git and file I/O never run inside the transaction): PreRef names it,
// or PreUnavailable says it could not be captured. At most one of the two is set; neither is allowed
// for a tool that needs no pre-state.
type ExecutionAdmission struct {
	SessionID   string
	RunID       string
	ExecutionID string
	// RequestID is the envelope whose answer issued the tool call: the exposure the execution
	// inherits.
	RequestID      string
	ToolName       string
	ArgsDigest     string
	PreRef         string
	PreUnavailable bool
}

func (a ExecutionAdmission) validate() error {
	for _, c := range []struct{ field, v string }{{"session id", a.SessionID}, {"execution id", a.ExecutionID}, {"request id", a.RequestID}, {"tool name", a.ToolName}} {
		if err := checkID(c.field, c.v); err != nil {
			return err
		}
	}
	if err := checkOptionalID("run id", a.RunID); err != nil {
		return err
	}
	if a.ArgsDigest == "" {
		return invalid("args digest is required")
	}
	if err := checkDigest("args digest", a.ArgsDigest); err != nil {
		return err
	}
	if a.PreUnavailable && a.PreRef != "" {
		return invalid("pre ref and pre unavailable exclude each other")
	}
	return checkLooseRef("pre ref", a.PreRef, false)
}

// AdmitExecution records, in one write transaction, that a side-effect tool is about to run. The row
// starts at outcome 'admitted'; lineage_complete starts at 0 only when the pre-state is unavailable and
// is lowered further by CommitToolResult and Reconcile, never raised. When the session is fenced no
// row is written and ErrFenced is returned: the tool must not run. The caller runs the tool only after
// this call returned nil.
func (s *Store) AdmitExecution(ctx context.Context, a ExecutionAdmission) error {
	if err := a.validate(); err != nil {
		return err
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, a.SessionID); err != nil {
			return err
		}
		var one int
		switch err := conn.QueryRowContext(ctx, `SELECT 1 FROM derived_origins WHERE kind = 'execution' AND execution_id = ?`, a.ExecutionID).Scan(&one); {
		case err == nil:
			return fmt.Errorf("%w: execution %s", ErrExists, a.ExecutionID)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, fenced, err := consumerFence(ctx, conn, a.SessionID); err != nil {
			return err
		} else if fenced {
			return ErrFenced
		}
		at := now()
		_, err := conn.ExecContext(ctx, `INSERT INTO derived_origins(
			session_id, run_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete,
			execution_id, tool_name, args_digest, pre_ref, pre_unavailable, outcome, created_at, updated_at)
			VALUES(?, ?, 'execution', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'admitted', ?, ?)`,
			a.SessionID, nullString(a.RunID), a.ExecutionID, SourceRequest, a.RequestID, OriginToolExecution, boolInt(!a.PreUnavailable),
			a.ExecutionID, a.ToolName, a.ArgsDigest, nullString(a.PreRef), boolInt(a.PreUnavailable), at, at)
		return err
	})
}

// DerivedEdge is one direct provenance edge from a source to a derived target. Edges are written only
// as part of CommitToolResult; there is no bare insert. Neither side holds text: refs are ids,
// paths-with-blob, or session/seq pairs.
type DerivedEdge struct {
	// Kind is message, file, child_session or artifact.
	Kind        string
	TargetRef   string
	SourceKind  string
	SourceRef   string
	OriginClass string
	// DerivedFrom is the ref+digest of the entry this one rewrote, when it is a rewrite.
	DerivedFrom string
	// LineageComplete is the caller's claim; it is stored lowered when the edge list was cut.
	LineageComplete bool
}

var edgeKinds = map[string]bool{"message": true, "file": true, "child_session": true, "artifact": true}

func (e DerivedEdge) validate() error {
	if !edgeKinds[e.Kind] {
		return invalid("edge kind %q", e.Kind)
	}
	if !knownSourceKinds[e.SourceKind] {
		return invalid("edge source kind %q", e.SourceKind)
	}
	if !knownOriginClasses[e.OriginClass] || e.OriginClass == OriginToolExecution {
		return invalid("edge origin class %q", e.OriginClass)
	}
	for _, c := range []struct {
		field, v string
		req      bool
	}{{"target ref", e.TargetRef, true}, {"source ref", e.SourceRef, true}, {"derived from", e.DerivedFrom, false}} {
		if err := checkLooseRef(c.field, c.v, c.req); err != nil {
			return err
		}
	}
	return nil
}

// ExecutionResult is the post record of an admitted execution.
type ExecutionResult struct {
	ExecutionID string
	Outcome     ExecutionOutcome
	// PostRef names the post snapshot; empty when there is none.
	PostRef  string
	Coverage Coverage
}

// ToolCommit is one tool result: the transcript row, the Task State version it caused, the execution
// post record of an admitted tool and the derived edges, committed together.
type ToolCommit struct {
	SessionID string
	RunID     string
	// Message is the tool row (role tool).
	Message   protocol.Message
	TaskState *TaskStateWrite
	// Execution is set for a tool that went through AdmitExecution; read-only tools leave it nil.
	Execution *ExecutionResult
	Edges     []DerivedEdge
}

func (c ToolCommit) validate() error {
	if err := checkID("session id", c.SessionID); err != nil {
		return err
	}
	if err := checkOptionalID("run id", c.RunID); err != nil {
		return err
	}
	if c.Message.Role != "tool" {
		return invalid("CommitToolResult stores a tool message, not %q", c.Message.Role)
	}
	if c.Execution != nil {
		x := c.Execution
		if err := checkID("execution id", x.ExecutionID); err != nil {
			return err
		}
		if !x.Outcome.valid() || x.Outcome == OutcomeAdmitted {
			return invalid("execution outcome %q", x.Outcome)
		}
		if !x.Coverage.valid() {
			return invalid("execution coverage %q", x.Coverage)
		}
		if err := checkLooseRef("post ref", x.PostRef, false); err != nil {
			return err
		}
	}
	for _, e := range c.Edges {
		if err := e.validate(); err != nil {
			return err
		}
	}
	if c.TaskState != nil {
		return c.TaskState.validateEdges()
	}
	return nil
}

// CommitToolResult extends AppendMessageTaskState: in one write transaction it re-reads the fences,
// appends the tool row, writes the Task State version, records the execution's post metadata and
// outcome, and stores the derived edges. It returns the row's seq and the new Task State version (0
// when TaskState is nil).
//
// The execution, when given, must be admitted (else ErrExecutionNotFound / ErrInvalidTransition). If
// the session or the source of any edge is fenced, the tool result body is discarded: no row, Task State
// or edge is written, the execution is settled with its outcome and lineage_complete = 0, that is
// committed, and ErrFenced is returned. The transcript then ends in a tool-call gap that Reconcile
// closes with the interrupted-tool row. More than MaxEdgesPerCommit edges are cut and everything stored
// is marked incomplete. Snapshots, digests and refs are computed by the caller beforehand.
func (s *Store) CommitToolResult(ctx context.Context, c ToolCommit) (int64, int64, error) {
	if err := c.validate(); err != nil {
		return 0, 0, err
	}
	raw, err := json.Marshal(c.Message)
	if err != nil {
		return 0, 0, err
	}
	// The Task State version's edges are the tool result's edges too: one list is fence-checked, cut
	// and stored, so the version is written without its own.
	edges := c.Edges
	var taskState *TaskStateWrite
	if c.TaskState != nil {
		w := *c.TaskState
		edges = append(append([]DerivedEdge(nil), c.Edges...), w.Edges...)
		w.Edges = nil
		taskState = &w
	}
	cut := len(edges) > MaxEdgesPerCommit
	var seq, version int64
	err = s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, c.SessionID); err != nil {
			return err
		}
		var preUnavailable bool
		if x := c.Execution; x != nil {
			var outcome ExecutionOutcome
			var pre int
			err := conn.QueryRowContext(ctx,
				`SELECT outcome, pre_unavailable FROM derived_origins WHERE kind = 'execution' AND execution_id = ? AND session_id = ?`,
				x.ExecutionID, c.SessionID).Scan(&outcome, &pre)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrExecutionNotFound
			}
			if err != nil {
				return err
			}
			if outcome != OutcomeAdmitted {
				return fmt.Errorf("%w: execution %s is %s", ErrInvalidTransition, x.ExecutionID, outcome)
			}
			preUnavailable = pre == 1
		}
		fenced, err := commitFenced(ctx, conn, c.SessionID, edges)
		if err != nil {
			return err
		}
		complete := !cut
		if x := c.Execution; x != nil {
			complete = complete && !fenced && !preUnavailable && x.Coverage == CoverageComplete && x.Outcome != OutcomeUnknown
			if err := settleExecution(ctx, conn, c.SessionID, *x, complete); err != nil {
				return err
			}
		}
		if fenced {
			return commitThen{ErrFenced}
		}
		if seq, err = appendMessageRow(ctx, conn, c.SessionID, "tool", raw); err != nil {
			return err
		}
		if taskState != nil {
			if version, err = putTaskState(ctx, conn, c.SessionID, *taskState); err != nil {
				return err
			}
		}
		execID := ""
		if c.Execution != nil {
			execID = c.Execution.ExecutionID
		}
		return insertEdges(ctx, conn, c.SessionID, c.RunID, execID, edges)
	})
	if err != nil {
		return 0, 0, err
	}
	return seq, version, nil
}

// insertEdges stores the derived edges of one commit. More than MaxEdgesPerCommit are cut and every
// stored edge is then lineage-incomplete. An edge is also stored incomplete when its source is not
// (inheritedLineage): incompleteness propagates forward, so a rewrite or a copy never lands complete
// on top of an incomplete or unrecorded source. The caller validated the edges and re-read the fences
// in the same transaction.
func insertEdges(ctx context.Context, q querier, sessionID, runID, execID string, edges []DerivedEdge) error {
	cut := len(edges) > MaxEdgesPerCommit
	if cut {
		edges = edges[:MaxEdgesPerCommit]
	}
	at := now()
	for _, e := range edges {
		complete := e.LineageComplete && !cut
		if complete {
			inherited, err := inheritedLineage(ctx, q, sessionID, e)
			if err != nil {
				return err
			}
			complete = inherited
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO derived_origins(
			session_id, run_id, kind, target_ref, source_kind, source_ref, origin_class, derived_from, lineage_complete, execution_id, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, nullString(runID), e.Kind, e.TargetRef, e.SourceKind, e.SourceRef, e.OriginClass, nullString(e.DerivedFrom),
			boolInt(complete), nullString(execID), at, at); err != nil {
			return err
		}
	}
	return nil
}

// inheritedLineage is the lineage of an edge's source, read in the commit's transaction:
//   - source kind "message" names an entry or row of the same session (a Task State entry ref): it is
//     complete only when the session records at least one edge to it and every such edge is complete,
//     so an entry written before provenance existed (no edge) is incomplete;
//   - source kind "session" names another session (a child): it is complete only when that session's
//     newest answered main request at or before the seq of DerivedFrom ("session:<id>@<seq>") is, so a
//     child with no recorded request, or a deleted one, is incomplete;
//   - any other source (a request, a file, a check) carries its own lineage in the edge.
//
// Both reads use an index on session_id and are bounded by one session's rows.
func inheritedLineage(ctx context.Context, q querier, sessionID string, e DerivedEdge) (bool, error) {
	switch e.SourceKind {
	case SourceMessage:
		var n, incomplete int64
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(lineage_complete = 0), 0) FROM derived_origins
			WHERE session_id = ? AND kind = 'message' AND target_ref = ?`, sessionID, e.SourceRef).Scan(&n, &incomplete); err != nil {
			return false, err
		}
		return n > 0 && incomplete == 0, nil
	case SourceSession:
		seq := int64(-1)
		if i := strings.LastIndexByte(e.DerivedFrom, '@'); i >= 0 {
			if v, err := strconv.ParseInt(e.DerivedFrom[i+1:], 10, 64); err == nil {
				seq = v
			}
		}
		if seq < 0 {
			return false, nil
		}
		var complete int
		err := q.QueryRowContext(ctx, `SELECT lineage_complete FROM request_envelopes
			WHERE session_id = ? AND state = 'answered' AND purpose = ? AND answered_seq <= ?
			ORDER BY answered_seq DESC LIMIT 1`, e.SourceRef, PurposeMain, seq).Scan(&complete)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return complete == 1, err
	default:
		return true, nil
	}
}

// commitFenced reports whether the session (an unprojected fence) or the source of any edge is fenced.
func commitFenced(ctx context.Context, q rowQuerier, sessionID string, edges []DerivedEdge) (bool, error) {
	if _, fenced, err := consumerFence(ctx, q, sessionID); err != nil || fenced {
		return fenced, err
	}
	for _, e := range edges {
		if fenced, err := sourceFenced(ctx, q, e.SourceKind, e.SourceRef); err != nil || fenced {
			return fenced, err
		}
	}
	return false, nil
}

// settleExecution moves an admitted execution to its post state. The WHERE clause rejects a row that
// is not admitted any more.
func settleExecution(ctx context.Context, q querier, sessionID string, x ExecutionResult, complete bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE derived_origins SET outcome = ?, post_ref = ?, coverage = ?, lineage_complete = ?, updated_at = ?
		 WHERE kind = 'execution' AND execution_id = ? AND session_id = ? AND outcome = 'admitted'`,
		string(x.Outcome), nullString(x.PostRef), string(x.Coverage), boolInt(complete), now(), x.ExecutionID, sessionID)
	if err != nil {
		return err
	}
	return requireOne(res, x.ExecutionID)
}

// ExecutionRecord is the stored execution row.
type ExecutionRecord struct {
	ExecutionID, SessionID, RunID, RequestID, ToolName, ArgsDigest string
	PreRef, PostRef                                                string
	PreUnavailable                                                 bool
	Coverage                                                       Coverage
	Outcome                                                        ExecutionOutcome
	LineageComplete                                                bool
	CreatedAt, UpdatedAt                                           time.Time
}

// Execution returns one execution record, or ErrExecutionNotFound.
func (s *Store) Execution(ctx context.Context, executionID string) (ExecutionRecord, error) {
	var r ExecutionRecord
	var created, updated string
	var pre, complete int
	err := s.db.QueryRowContext(ctx, `SELECT execution_id, session_id, COALESCE(run_id,''), source_ref, tool_name, args_digest,
		COALESCE(pre_ref,''), COALESCE(post_ref,''), pre_unavailable, COALESCE(coverage,''), outcome, lineage_complete, created_at, updated_at
		FROM derived_origins WHERE kind = 'execution' AND execution_id = ?`, executionID).Scan(
		&r.ExecutionID, &r.SessionID, &r.RunID, &r.RequestID, &r.ToolName, &r.ArgsDigest,
		&r.PreRef, &r.PostRef, &pre, &r.Coverage, &r.Outcome, &complete, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionRecord{}, ErrExecutionNotFound
	}
	if err != nil {
		return ExecutionRecord{}, err
	}
	r.PreUnavailable, r.LineageComplete = pre == 1, complete == 1
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	return r, nil
}
