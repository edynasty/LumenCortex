package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

var ErrCognitiveRevision = errors.New("cognitive revision changed")
var ErrCognitiveCorrupt = errors.New("cognitive store is incomplete or corrupt")

// sourceRunSQL is the one definition of the run a session source names, for a sessions row aliased s:
// the run of its newest main request. An auto-effort judge request is admitted before BeginRun under
// the run id of a Start that may never begin; such a Start writes nothing the source version covers,
// so its id must not replace the run of the content the reference describes.
const sourceRunSQL = `COALESCE((SELECT run_id FROM request_envelopes r WHERE r.session_id=s.id AND r.run_id IS NOT NULL AND r.purpose='` + PurposeMain + `' ORDER BY r.rowid DESC LIMIT 1),'')`

// CognitiveSource issues a reference from current runtime records, never from candidate text.
func (s *Store) CognitiveSource(ctx context.Context, id string) (knowledge.SourceRef, error) {
	var ref knowledge.SourceRef
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if _, fenced, err := sessionFence(ctx, conn, id); err != nil {
			return err
		} else if fenced {
			return ErrFenced
		}
		var status string
		if err := conn.QueryRowContext(ctx, `SELECT status, `+sourceRunSQL+` FROM sessions s WHERE s.id=?`, id).Scan(&status, &ref.RunID); err != nil {
			return err
		}
		if status != "completed" && status != "failed" {
			return fmt.Errorf("source run is not terminal: %s", status)
		}
		digest, err := sourceVersion(ctx, conn, id)
		if err != nil {
			return err
		}
		ref.Kind, ref.ID, ref.SessionID, ref.Digest, ref.Scope, ref.Sensitivity, ref.Provenance = knowledge.KindSession, id, id, digest, SourceScopeProject, knowledge.SensitivitySession, knowledge.ProvenanceRuntime
		return nil
	})
	return ref, err
}

func (s *Store) readCognition(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
	return fn(conn)
}

// PutCognitiveNode commits a revision, its exact evidence and provenance under one writer lock.
// No filesystem, process or model work occurs here. Expected=0 means create, not upsert.
func (s *Store) PutCognitiveNode(ctx context.Context, n knowledge.Node, expected int64, project string) error {
	if n.Kind == "experience" || n.Experience != nil {
		return invalid("experience records require the Experimental commit path")
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error { return putCognitiveNode(ctx, conn, n, expected, project) })
}

func putCognitiveNode(ctx context.Context, conn *sql.Conn, n knowledge.Node, expected int64, project string) error {
	if err := n.Validate(); err != nil {
		return err
	}
	if n.Memory != nil && n.Memory.Canonical != nil {
		return invalid("canonical bindings require the lifecycle commit path")
	}
	if n.Version != expected+1 {
		return ErrCognitiveRevision
	}
	if err := sessionExists(ctx, conn, n.SessionID); err != nil {
		return err
	}
	if _, fenced, err := sessionFence(ctx, conn, n.SessionID); err != nil {
		return err
	} else if fenced {
		return ErrFenced
	}
	old, err := loadCognitiveNode(ctx, conn, n.ID, 0)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if errors.Is(err, ErrNotFound) {
		if expected != 0 {
			return ErrCognitiveRevision
		}
	} else {
		if old.Version != expected {
			return ErrCognitiveRevision
		}
		if old.SessionID != n.SessionID || old.Kind != n.Kind || !old.Scope.Contains(n.Scope) {
			return invalid("node revision changed ownership/kind or widened scope")
		}
		if old.Memory != nil && old.Memory.Canonical != nil {
			return invalid("consolidated aliases require the lifecycle commit path")
		}
		if old.Status == "invalid" && n.Status != "invalid" {
			return invalid("invalid identity requires a new record")
		}
		if n.Status == "archived" && old.ProtectedMemory() {
			return invalid("protected memory cannot be archived")
		}
		for _, src := range old.Sources {
			if !slices.ContainsFunc(n.Sources, func(next knowledge.SourceRef) bool {
				return next.Kind == src.Kind && next.ID == src.ID && next.SessionID == src.SessionID
			}) {
				return invalid("revision cannot drop source lineage")
			}
		}
	}
	if n.Status == "archived" && n.ProtectedMemory() {
		return invalid("protected memory cannot be archived")
	}
	reader := knowledge.Reader{OwnerID: n.Scope.OwnerID, ProjectID: n.Scope.ProjectID, WorkspaceID: n.Scope.WorkspaceID, SessionID: n.SessionID, ID: n.Scope.ReaderID, SourceIDs: []string{n.SessionID}}
	if err := checkCognitiveDependencies(ctx, conn, n, reader, n.Times.RecordedAt, map[string]bool{}); err != nil {
		return err
	}
	// The first invalidation is stamped with the revision that records it; a caller cannot supply
	// another instant, and later revisions carry the first one unchanged in its stored form.
	if errors.Is(err, ErrNotFound) || old.Times.InvalidatedAt.IsZero() {
		if !n.Times.InvalidatedAt.IsZero() && !n.Times.InvalidatedAt.Equal(n.Times.RecordedAt) {
			return invalid("invalidation time is assigned when the invalidation is recorded")
		}
	} else {
		if !n.Times.InvalidatedAt.Equal(old.Times.InvalidatedAt) {
			return invalid("revision cannot clear or replace invalidation")
		}
		n.Times.InvalidatedAt = old.Times.InvalidatedAt
	}
	if err := sourcesUnchanged(ctx, conn, n.SessionID, n.Sources); err != nil {
		return err
	}
	for _, ev := range n.Evidence {
		if n.Assertion == nil {
			return ErrNotQualified
		}
		if err := cognitiveEvidence(ctx, conn, project, *n.Assertion, ev); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO cognition_nodes(id,version,session_id,project_id,kind,created_at,json) VALUES(?,?,?,?,?,?,?)`, n.ID, n.Version, n.SessionID, n.Scope.ProjectID, n.Kind, stamp(n.Times.RecordedAt), string(raw)); err != nil {
		return err
	}
	if err := dependencyLineage(ctx, conn, n); err != nil {
		return err
	}
	if n.Kind == "experience" {
		return nil // CommitExperience records its original and consumer lineage once per event.
	}
	return cognitiveOrigins(ctx, conn, n.SessionID, n.ID, n.Sources)
}

func cognitiveOrigins(ctx context.Context, conn *sql.Conn, owner, id string, sources []knowledge.SourceRef) error {
	for _, src := range sources {
		if src.SessionID == owner {
			continue
		}
		if err := insertEdges(ctx, conn, owner, "", "", []DerivedEdge{{Kind: "artifact", TargetRef: "cognition:" + id, SourceKind: SourceSession, SourceRef: src.SessionID, OriginClass: OriginAgentGen, LineageComplete: true}}); err != nil {
			return err
		}
	}
	return nil
}

func cognitiveEvidence(ctx context.Context, conn *sql.Conn, project string, assertion knowledge.Assertion, ev knowledge.Evidence) error {
	if ev.Verification != "passed" && ev.Verification != "failed" {
		return ErrNotQualified
	}
	if _, fenced, err := sessionFence(ctx, conn, ev.SessionID); err != nil {
		return err
	} else if fenced {
		return ErrFenced
	}
	r, err := qualifiedResult(ctx, conn, ev.SessionID, ev.ResultSeq)
	if err != nil {
		return err
	}
	if r.Outcome != ev.Verification || r.SpecDigest != ev.SpecDigest || r.ApprovalEpoch != ev.ApprovalEpoch || r.Fingerprint != ev.Fingerprint {
		return ErrNotQualified
	}
	var active int
	err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM check_approvals b WHERE b.project_id=? AND b.kind='constraint_binding' AND b.state='active' AND b.binding_digest=? AND b.constraint_ref=? AND b.constraint_digest=? AND b.check_id=? AND b.spec_digest=?)
	 AND EXISTS(SELECT 1 FROM check_approvals s WHERE s.project_id=? AND s.kind='check_spec' AND s.state='active' AND s.check_id=? AND s.spec_digest=? AND s.approval_epoch=?)
	 AND EXISTS(SELECT 1 FROM eval_results r WHERE r.session_id=? AND r.seq=? AND r.cognition_epoch >= (SELECT COALESCE(MAX(epoch),0) FROM source_fences WHERE scope_kind='privacy'))`, project, ev.BindingDigest, assertion.Ref(), assertion.ValueDigest(), r.CheckID, r.SpecDigest, project, r.CheckID, r.SpecDigest, r.ApprovalEpoch, ev.SessionID, ev.ResultSeq).Scan(&active)
	if err != nil {
		return err
	}
	if active != 1 {
		return ErrNotQualified
	}
	return nil
}

func loadCognitiveNode(ctx context.Context, conn *sql.Conn, id string, version int64) (knowledge.Node, error) {
	var raw, owner, kind, project, recorded string
	var stored int64
	query := `SELECT json,session_id,kind,project_id,created_at,version FROM cognition_nodes WHERE id=?`
	args := []any{id}
	if version > 0 {
		query += ` AND version=?`
		args = append(args, version)
	}
	query += ` ORDER BY version DESC LIMIT 1`
	err := conn.QueryRowContext(ctx, query, args...).Scan(&raw, &owner, &kind, &project, &recorded, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.Node{}, ErrNotFound
	}
	if err != nil {
		return knowledge.Node{}, err
	}
	n, err := knowledge.DecodeNode(raw)
	if err != nil || n.ID != id || n.SessionID != owner || n.Kind != kind || n.Scope.ProjectID != project || n.Version != stored || stamp(n.Times.RecordedAt) != recorded {
		return knowledge.Node{}, fmt.Errorf("%w: node %s", ErrCognitiveCorrupt, id)
	}
	return n, nil
}

// CognitiveNode returns a historical or latest record only while the reader and its source lineage
// remain visible. Source version changes make current use stale but do not erase the historical row.
func (s *Store) CognitiveNode(ctx context.Context, id string, version int64, reader knowledge.Reader) (knowledge.Node, error) {
	var n knowledge.Node
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		var err error
		n, err = loadCognitiveNode(ctx, conn, id, version)
		if err != nil {
			return err
		}
		if version > 0 {
			current, err := loadCognitiveNode(ctx, conn, id, 0)
			if err != nil {
				return err
			}
			if !current.Scope.VisibleTo(reader) {
				return ErrFenced
			}
		}
		visible, err := cognitiveVisible(ctx, conn, n, reader)
		if err != nil {
			return err
		}
		if !visible {
			return ErrFenced
		}
		return nil
	})
	if err != nil {
		return knowledge.Node{}, err
	}
	return n, nil
}

func cognitiveReader(ctx context.Context, conn *sql.Conn, reader knowledge.Reader) error {
	if reader.SessionID == "" {
		return nil
	}
	if err := sessionExists(ctx, conn, reader.SessionID); err != nil {
		return err
	}
	if _, fenced, err := consumerFence(ctx, conn, reader.SessionID); err != nil {
		return err
	} else if fenced {
		return ErrFenced
	}
	return nil
}

func cognitiveVisible(ctx context.Context, conn *sql.Conn, n knowledge.Node, reader knowledge.Reader) (bool, error) {
	if !n.Scope.VisibleTo(reader) {
		return false, nil
	}
	ids := append([]string{n.SessionID}, cognitiveSourceIDs(n.Sources)...)
	ids = append(ids, experienceConsumerIDs(n)...)
	for _, id := range ids {
		if _, fenced, err := sessionFence(ctx, conn, id); err != nil {
			return false, err
		} else if fenced {
			return false, nil
		}
		if err := sessionExists(ctx, conn, id); errors.Is(err, ErrNotFound) {
			return false, nil
		} else if err != nil {
			return false, err
		}
	}
	return true, nil
}

func cognitiveSourceIDs(sources []knowledge.SourceRef) []string {
	ids := make([]string, 0, len(sources))
	for _, src := range sources {
		if !slices.Contains(ids, src.SessionID) {
			ids = append(ids, src.SessionID)
		}
	}
	return ids
}

func (s *Store) CognitiveGraph(ctx context.Context, reader knowledge.Reader) ([]knowledge.Node, []knowledge.Edge, error) {
	nodes := []knowledge.Node{}
	edges := []knowledge.Edge{}
	err := s.readCognition(ctx, func(conn *sql.Conn) error {
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		ids, err := selectStrings(ctx, conn, `SELECT DISTINCT id FROM cognition_nodes ORDER BY id`)
		if err != nil {
			return err
		}
		all := make(map[string]struct{}, len(ids))
		visible := make(map[string]bool, len(ids))
		for _, id := range ids {
			n, err := loadCognitiveNode(ctx, conn, id, 0)
			if err != nil {
				return err
			}
			all[id] = struct{}{}
			ok, err := cognitiveVisible(ctx, conn, n, reader)
			if err != nil {
				return err
			}
			if ok {
				visible[id] = true
				nodes = append(nodes, n)
			}
		}
		rows, err := conn.QueryContext(ctx, `SELECT id,session_id,from_id,to_id,json FROM cognition_edges ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, owner, from, to, raw string
			if err := rows.Scan(&id, &owner, &from, &to, &raw); err != nil {
				return err
			}
			e, err := knowledge.DecodeEdge(raw)
			if err != nil || e.ID != id || e.SessionID != owner || e.From != from || e.To != to {
				return ErrCognitiveCorrupt
			}
			if _, ok := all[from]; !ok {
				return ErrCognitiveCorrupt
			}
			if _, ok := all[to]; !ok {
				return ErrCognitiveCorrupt
			}
			if !visible[from] || !visible[to] {
				continue
			}
			if _, fenced, err := sessionFence(ctx, conn, owner); err != nil {
				return err
			} else if fenced {
				continue
			}
			if err := sessionExists(ctx, conn, owner); err != nil {
				return ErrCognitiveCorrupt
			}
			edges = append(edges, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	return nodes, edges, nil
}

func (s *Store) PutCognitiveEdge(ctx context.Context, e knowledge.Edge, reader knowledge.Reader) error {
	if err := e.Validate(); err != nil {
		return err
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if e.SessionID != reader.SessionID {
			return ErrFenced
		}
		if err := cognitiveReader(ctx, conn, reader); err != nil {
			return err
		}
		for _, id := range []string{e.From, e.To} {
			n, err := loadCognitiveNode(ctx, conn, id, 0)
			if err != nil {
				return err
			}
			ok, err := cognitiveVisible(ctx, conn, n, reader)
			if err != nil {
				return err
			}
			if !ok {
				return ErrFenced
			}
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO cognition_edges(id,session_id,from_id,to_id,json) VALUES(?,?,?,?,?)`, e.ID, e.SessionID, e.From, e.To, string(raw))
		return err
	})
}
