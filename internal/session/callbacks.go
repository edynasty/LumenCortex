package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/edynasty/LumenCortex/protocol"
)

// SettleExecution commits a callback's admitted effect and provenance without a
// transcript tool row. Its parent Eval result supplies the single model-visible
// tool response. Fences and check-definition invalidation are identical to
// CommitToolResult; this is not an alternative permission/admission path.
func (s *Store) SettleExecution(ctx context.Context, sessionID string, x ExecutionResult, edges ...DerivedEdge) error {
	c := ToolCommit{SessionID: sessionID, Message: protocol.Message{Role: "tool"}, Execution: &x, Edges: edges}
	if err := c.validate(); err != nil {
		return err
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
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
		if err := insertEdges(ctx, conn, sessionID, runID, x.ExecutionID, edges); err != nil {
			return err
		}
		return s.revokeOnDefinitionWrite(ctx, conn, edges)
	})
}
