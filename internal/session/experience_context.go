package session

import (
	"context"
	"database/sql"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// ExperienceContext returns current locators owned or adopted by this reader. A thread projection
// does not need unrelated graph nodes or edges. Selection, latest revision and visibility are read
// in one snapshot; the normal scope/source/consumer fences remain authoritative.
func (s *Store) ExperienceContext(ctx context.Context, reader knowledge.Reader) ([]knowledge.Node, error) {
	nodes := []knowledge.Node{}
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		ids, err := selectStrings(ctx, conn, `SELECT DISTINCT n.id FROM derived_origins d
			JOIN cognition_nodes n ON n.session_id=d.session_id AND d.target_ref='cognition:' || n.id
			WHERE d.source_kind='session' AND d.source_ref=? AND d.kind='artifact'
			AND n.kind='experience'
			AND n.version=(SELECT max(current.version) FROM cognition_nodes current WHERE current.id=n.id)
			ORDER BY n.id`, reader.SessionID)
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
				nodes = append(nodes, n)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}
