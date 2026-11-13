package session

import (
	"context"
	"database/sql"
	"errors"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func (s *Store) CognitiveClaimPeers(ctx context.Context, reader knowledge.Reader, a knowledge.Assertion) ([]knowledge.Node, error) {
	var out []knowledge.Node
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		ids, err := selectStrings(ctx, conn, `SELECT id FROM cognition_nodes n WHERE project_id=? AND version=(SELECT max(v.version) FROM cognition_nodes v WHERE v.id=n.id) AND json_extract(json,'$.assertion.kind')=? AND json_extract(json,'$.assertion.target')=? AND COALESCE(json_extract(json,'$.assertion.key'),'')=? ORDER BY id`, reader.ProjectID, a.Kind, a.Target, a.Key)
		if err != nil {
			return err
		}
		for _, id := range ids {
			n, err := loadCognitiveNode(ctx, conn, id, 0)
			if err != nil {
				return err
			}
			visible, err := cognitiveVisible(ctx, conn, n, reader)
			if err != nil {
				return err
			}
			if visible {
				out = append(out, n)
			}
		}
		return nil
	})
	return out, err
}

// CognitiveEvidenceCurrent checks the original authority and later qualified
// failures. It is not freshness: the reader still needs its own scope/fingerprint.
func (s *Store) CognitiveEvidenceCurrent(ctx context.Context, project string, a knowledge.Assertion, e knowledge.Evidence) (bool, error) {
	current := false
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if err := cognitiveEvidence(ctx, conn, project, a, e); err != nil {
			if errors.Is(err, ErrNotQualified) || errors.Is(err, ErrFenced) {
				return nil
			}
			return err
		}
		var superseded int
		err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM eval_results r JOIN eval_results original ON original.session_id=? AND original.seq=? WHERE r.rowid>original.rowid AND r.check_id=original.check_id AND r.fingerprint=original.fingerprint AND r.spec_digest=original.spec_digest AND r.approval_epoch=original.approval_epoch AND r.status='failed' AND `+qualifiedEvalSQL+`)`, e.SessionID, e.ResultSeq).Scan(&superseded)
		current = err == nil && superseded == 0
		return err
	})
	return current, err
}
