package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/edynasty/LumenCortex/protocol"
)

// SettleExecution commits a callback's admitted effect, Task State and provenance
// without an unmatched transcript row. Fences and check invalidation match
// CommitToolResult; the returned version is zero when state did not change.
func (s *Store) SettleExecution(ctx context.Context, sessionID string, x ExecutionResult, state *TaskStateWrite, edges ...DerivedEdge) (int64, error) {
	c := ToolCommit{SessionID: sessionID, Message: protocol.Message{Role: "tool"}, Execution: &x, TaskState: state, Edges: edges}
	if err := c.validate(); err != nil {
		return 0, err
	}
	if state != nil {
		copy := *state
		edges = append(append([]DerivedEdge(nil), edges...), copy.Edges...)
		copy.Edges = nil
		state = &copy
	}
	var version int64
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, sessionID); err != nil {
			return err
		}
		var outcome ExecutionOutcome
		var pre int
		var runID string
		err := conn.QueryRowContext(ctx, `SELECT outcome, pre_unavailable, COALESCE(run_id,'') FROM derived_origins WHERE kind='execution' AND execution_id=? AND session_id=?`, x.ExecutionID, sessionID).Scan(&outcome, &pre, &runID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrExecutionNotFound
		}
		if err != nil {
			return err
		}
		if outcome != OutcomeAdmitted {
			return fmt.Errorf("%w: execution %s is %s", ErrInvalidTransition, x.ExecutionID, outcome)
		}
		fenced, err := commitFenced(ctx, conn, sessionID, edges)
		if err != nil {
			return err
		}
		complete := !fenced && pre == 0 && len(edges) <= MaxEdgesPerCommit && x.Coverage == CoverageComplete && x.Outcome != OutcomeUnknown
		if err := settleExecution(ctx, conn, sessionID, x, complete); err != nil {
			return err
		}
		if fenced {
			return commitThen{ErrFenced}
		}
		if state != nil {
			if version, err = putTaskState(ctx, conn, sessionID, *state); err != nil {
				return err
			}
		}
		if err := insertEdges(ctx, conn, sessionID, runID, x.ExecutionID, edges); err != nil {
			return err
		}
		return s.revokeOnDefinitionWrite(ctx, conn, edges)
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}
