package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// checkCognitiveDependencies checks exact revisions and scopes in the same
// snapshot/transaction as its consumer. The path bound also refuses cycles.
// A newer Experience revision that only records consumers' uses and their
// feedback is the revision the dependent cited: one consumer's adoption or
// settlement does not change another's premise (experienceUsesOnly).
func checkCognitiveDependencies(ctx context.Context, conn *sql.Conn, n knowledge.Node, reader knowledge.Reader, at time.Time, path map[string]bool) error {
	if path[n.ID] || len(path) >= 32 {
		return invalid("cyclic or excessive cognitive dependencies")
	}
	path[n.ID] = true
	defer delete(path, n.ID)
	for _, ref := range n.Dependencies {
		dep, err := loadCognitiveNode(ctx, conn, ref.ID, 0)
		if err != nil {
			return err
		}
		if dep.Version != ref.Version {
			pinned, err := loadCognitiveNode(ctx, conn, ref.ID, ref.Version)
			if errors.Is(err, ErrNotFound) {
				return ErrCognitiveRevision
			}
			if err != nil {
				return err
			}
			if !experienceUsesOnly(pinned, dep) {
				return ErrCognitiveRevision
			}
		}
		if dep.Status != "active" || dep.Times.TimeState(at) != "" {
			return ErrCognitiveRevision
		}
		visible, err := cognitiveVisible(ctx, conn, dep, reader)
		if err != nil {
			return err
		}
		if !visible || !dep.Scope.Contains(n.Scope) {
			return ErrFenced
		}
		if err := sourcesUnchanged(ctx, conn, dep.SessionID, dep.Sources); err != nil {
			return err
		}
		if err := checkCognitiveDependencies(ctx, conn, dep, reader, at, path); err != nil {
			return err
		}
	}
	return nil
}

// experienceUsesOnly reports whether next is a later revision of the Experience pinned that changes
// nothing but the record of uses: the consumers' adoptions and their settled feedback, which append
// to Uses (knowledge.ExperienceTransition). Definition, state, validation, confidence, sources,
// scope, lifecycle and memory policy must be identical, so a counterexample (needs_revision), a
// definition revision, a revalidation or a host lifecycle change still makes the pin stale.
func experienceUsesOnly(pinned, next knowledge.Node) bool {
	if pinned.Experience == nil || next.Experience == nil || pinned.ID != next.ID || next.Version <= pinned.Version {
		return false
	}
	strip := func(n knowledge.Node) knowledge.Node {
		x := *n.Experience
		x.Uses = nil
		n.Experience = &x
		n.Version, n.Times.RecordedAt, n.RevisionReason = 0, time.Time{}, ""
		return n
	}
	return reflect.DeepEqual(strip(pinned), strip(next))
}

func (s *Store) CognitiveDependenciesCurrent(ctx context.Context, n knowledge.Node, reader knowledge.Reader, at time.Time) error {
	return s.readCognition(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		return checkCognitiveDependencies(ctx, conn, n, reader, at, map[string]bool{})
	})
}

// dependencyLineage materializes transitive source ownership. It lets the existing
// revocation and privacy deletion state machine reach summaries without a second
// privacy engine or a graph traversal during a delete.
func dependencyLineage(ctx context.Context, conn *sql.Conn, n knowledge.Node) error {
	for _, ref := range n.Dependencies {
		dep, err := loadCognitiveNode(ctx, conn, ref.ID, ref.Version)
		if err != nil {
			return err
		}
		owner := dep.SessionID
		if dep.Experience != nil && dep.Experience.State != knowledge.ExperienceNeedsRevision {
			owner = dep.Experience.Origin.Source.SessionID
		}
		if err := cognitiveOrigins(ctx, conn, n.SessionID, n.ID, append([]knowledge.SourceRef{{SessionID: owner}}, dep.Sources...)); err != nil {
			return err
		}
	}
	return nil
}

// CognitiveNodesByKind avoids decoding unrelated graph records on an Agent run.
// A corrupt record in the selected domain still fails closed.
func (s *Store) CognitiveNodesByKind(ctx context.Context, reader knowledge.Reader, kind string) ([]knowledge.Node, error) {
	var nodes []knowledge.Node
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		ids, err := selectStrings(ctx, conn, `SELECT DISTINCT id FROM cognition_nodes WHERE kind=? AND project_id=? ORDER BY id`, kind, reader.ProjectID)
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
	return nodes, err
}

// RecordCognitiveAccess is reader-local use telemetry. A recall is not an
// application and cannot alter evidence, confidence or shared consumer feedback.
func (s *Store) RecordCognitiveAccess(ctx context.Context, refs []knowledge.NodeRef, reader knowledge.Reader, at time.Time) error {
	if len(refs) == 0 {
		return nil
	}
	if len(refs) > 20 || reader.SessionID == "" {
		return invalid("invalid cognition access")
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		for _, ref := range refs {
			n, err := loadCognitiveNode(ctx, conn, ref.ID, 0)
			if err != nil {
				return err
			}
			if n.Version != ref.Version {
				return ErrCognitiveRevision
			}
			visible, err := cognitiveVisible(ctx, conn, n, reader)
			if err != nil {
				return err
			}
			if !visible {
				return ErrFenced
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO cognition_access(node_id,session_id,access_count,last_access_at) VALUES(?,?,1,?) ON CONFLICT(node_id,session_id) DO UPDATE SET access_count=access_count+1,last_access_at=excluded.last_access_at`, n.ID, reader.SessionID, stamp(at)); err != nil {
				return err
			}
		}
		return nil
	})
}

type CognitiveAccessCount struct {
	Count      int       `json:"count"`
	LastAccess time.Time `json:"lastAccess"`
}

func (s *Store) CognitiveAccess(ctx context.Context, reader, id string) (CognitiveAccessCount, error) {
	var count CognitiveAccessCount
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT access_count,last_access_at FROM cognition_access WHERE session_id=? AND node_id=?`, reader, id).Scan(&count.Count, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return count, nil
	}
	if err != nil {
		return count, err
	}
	count.LastAccess = parseStamp(at)
	return count, nil
}

// CommitCognitiveLifecycle accepts only lifecycle metadata revisions. It never
// blesses new evidence or changes a procedure. Entire batches use one CAS write.
func (s *Store) CommitCognitiveLifecycle(ctx context.Context, nodes []knowledge.Node, expected map[string]int64, reader knowledge.Reader) error {
	if len(nodes) == 0 || len(nodes) > 512 || len(expected) != len(nodes) {
		return invalid("invalid cognitive lifecycle batch")
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, n := range nodes {
			if seen[n.ID] {
				return invalid("duplicate lifecycle node")
			}
			seen[n.ID] = true
			old, err := loadCognitiveNode(ctx, conn, n.ID, 0)
			if err != nil {
				return err
			}
			if expected[n.ID] != old.Version || n.Version != old.Version+1 {
				return ErrCognitiveRevision
			}
			if old.SessionID != reader.SessionID && (old.Experience == nil || old.Experience.Origin.Source.SessionID != reader.SessionID) {
				return ErrFenced
			}
			visible, err := cognitiveVisible(ctx, conn, old, reader)
			if err != nil {
				return err
			}
			if !visible {
				return ErrFenced
			}
			if n.Validate() != nil || n.RevisionReason == "" {
				return invalid("invalid lifecycle revision")
			}
			want := old
			want.Version = n.Version
			want.Status = n.Status
			want.Memory = n.Memory
			want.Times.RecordedAt = n.Times.RecordedAt
			want.Times.InvalidatedAt = n.Times.InvalidatedAt
			want.RevisionReason = n.RevisionReason
			if !reflect.DeepEqual(want, n) {
				return invalid("lifecycle change rewrites cognitive content")
			}
			if old.Status == "invalid" && n.Status != "invalid" {
				return invalid("revoked identity cannot be restored")
			}
			if !old.Times.InvalidatedAt.IsZero() && !old.Times.InvalidatedAt.Equal(n.Times.InvalidatedAt) {
				return invalid("invalidation is immutable")
			}
			if old.Times.InvalidatedAt.IsZero() && !n.Times.InvalidatedAt.IsZero() && !n.Times.InvalidatedAt.Equal(n.Times.RecordedAt) {
				return invalid("invalid invalidation time")
			}
			// Archiving is the transition. A record a host archived before a consumer's counterexample
			// settled on it is protected and already archived: its tier or reason may still change.
			if n.Status == "archived" && old.Status != "archived" && old.ProtectedMemory() {
				return invalid("protected memory cannot be archived")
			}
			if n.Status == "active" && old.Status != "active" {
				if n.Times.TimeState(n.Times.RecordedAt) != "" {
					return ErrNotQualified
				}
				if err := sourcesUnchanged(ctx, conn, n.SessionID, n.Sources); err != nil {
					return err
				}
				if err := checkCognitiveDependencies(ctx, conn, n, reader, n.Times.RecordedAt, map[string]bool{}); err != nil {
					return err
				}
			}
			// An unchanged pin on an inactive alias is historical provenance. Withdrawing
			// it must not depend on the target still being current or active.
			if n.Memory != nil && n.Memory.Canonical != nil && (old.Memory == nil || !reflect.DeepEqual(old.Memory.Canonical, n.Memory.Canonical) || n.Status == "active") {
				if _, changing := expected[n.Memory.Canonical.ID]; changing {
					return invalid("canonical target cannot change in the same lifecycle batch")
				}
				target, err := loadCognitiveNode(ctx, conn, n.Memory.Canonical.ID, 0)
				if err != nil {
					return err
				}
				if target.Version != n.Memory.Canonical.Version || target.ID == n.ID || target.SessionID != n.SessionID || target.Scope != n.Scope || target.Status != "active" || target.Memory != nil && target.Memory.Canonical != nil {
					return ErrCognitiveRevision
				}
				if n.Assertion == nil || target.Assertion == nil || *n.Assertion != *target.Assertion || n.Kind != target.Kind || !reflect.DeepEqual(n.Evidence, target.Evidence) || !reflect.DeepEqual(n.Sources, target.Sources) || !reflect.DeepEqual(n.Dependencies, target.Dependencies) || n.Body != target.Body || n.Times.Valid != target.Times.Valid {
					return invalid("consolidation requires equivalent scoped evidence")
				}
			}
		}
		for _, n := range nodes {
			raw, err := json.Marshal(n)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO cognition_nodes(id,version,session_id,project_id,kind,created_at,json) VALUES(?,?,?,?,?,?,?)`, n.ID, n.Version, n.SessionID, n.Scope.ProjectID, n.Kind, stamp(n.Times.RecordedAt), string(raw)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) CommitCognitiveDecision(ctx context.Context, id, runID string, refs []knowledge.NodeRef, raw string, reader knowledge.Reader, at time.Time) error {
	if len(raw) > 16384 || len(refs) > 16 {
		return invalid("cognitive decision exceeds bound")
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		for _, ref := range refs {
			n, err := loadCognitiveNode(ctx, conn, ref.ID, 0)
			if err != nil {
				return err
			}
			if n.Version != ref.Version {
				return ErrCognitiveRevision
			}
			visible, err := cognitiveVisible(ctx, conn, n, reader)
			if err != nil {
				return err
			}
			if !visible {
				return ErrFenced
			}
			sources := slices.Clone(n.Sources)
			owner := n.SessionID
			if n.Experience != nil && n.Experience.State != knowledge.ExperienceNeedsRevision {
				owner = n.Experience.Origin.Source.SessionID
			}
			sources = append(sources, knowledge.SourceRef{SessionID: owner})
			if err := cognitiveOrigins(ctx, conn, reader.SessionID, id, sources); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO cognition_decisions(id,session_id,run_id,created_at,json) VALUES(?,?,?,?,?)`, id, reader.SessionID, runID, stamp(at), raw)
		return err
	})
}
