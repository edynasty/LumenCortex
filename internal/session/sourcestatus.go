package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// MaxLineageHops bounds one source walk (DATA_AND_FAILURES §4: a lineage traversal visits at most 64
// levels). A chain longer than that is reported incomplete, never followed further.
const MaxLineageHops = 64

// SourceStatus is what the store records about where one derived target of a session came from
// (ADR-0018, plan T2-S1). It holds ids and classes only, never a body.
type SourceStatus struct {
	// Recorded is false when no edge names the target: it was written before provenance existed, or
	// by a writer that recorded none. Such a target has an unknown origin and an incomplete lineage.
	Recorded bool `json:"recorded"`
	// Origin is the origin class of the newest edge of the target (DATA §3): agent_generated,
	// user_decision, host_check, external_data or tool_execution. A user rewrite of agent text keeps
	// the class its edge records, never upgraded by the rewrite.
	Origin string `json:"origin,omitempty"`
	// RootKind and RootRef are the first source that is not another message version: the request, the
	// session or the file the chain ends at.
	RootKind string `json:"rootKind,omitempty"`
	RootRef  string `json:"rootRef,omitempty"`
	// Hops is the number of edges followed, 1 for a target that derives directly from its root.
	Hops int `json:"hops"`
	// LineageComplete is true only when every edge on the chain is complete and the chain reached a
	// root within MaxLineageHops.
	LineageComplete bool `json:"lineageComplete"`
	// Withdrawn is true when the session or any source on the chain is fenced (revoked or held for
	// privacy deletion). A reader must not use the target as current.
	Withdrawn bool `json:"withdrawn"`
}

// SourceStatus walks the derived edges of target in sessionID back to its root, one newest edge per
// hop, and checks the session and every source on the way against the fences. It only reads; a
// missing session is ErrNotFound.
func (s *Store) SourceStatus(ctx context.Context, sessionID, target string) (SourceStatus, error) {
	if err := checkID("session id", sessionID); err != nil {
		return SourceStatus{}, err
	}
	if err := checkLooseRef("target ref", target, true); err != nil {
		return SourceStatus{}, err
	}
	if err := sessionExists(ctx, s.db, sessionID); err != nil {
		return SourceStatus{}, err
	}
	var st SourceStatus
	st.LineageComplete = true
	scopes := []string{}
	used := []any{}
	ref := target
	for {
		if st.Hops == MaxLineageHops {
			st.LineageComplete = false
			break
		}
		var id int64
		var kind, src, origin string
		var complete int
		query := `SELECT id, source_kind, source_ref, origin_class, lineage_complete FROM derived_origins
			WHERE session_id = ? AND target_ref = ? AND kind <> 'execution'`
		args := []any{sessionID, ref}
		if len(used) > 0 {
			// A rewrite back to earlier text repeats a ref; each edge is followed at most once, so the
			// walk takes the ref's next older edge instead of looping.
			query += ` AND id NOT IN (?` + strings.Repeat(",?", len(used)-1) + `)`
			args = append(args, used...)
		}
		err := s.db.QueryRowContext(ctx, query+` ORDER BY id DESC LIMIT 1`, args...).Scan(&id, &kind, &src, &origin, &complete)
		if errors.Is(err, sql.ErrNoRows) {
			// A message version with no edge of its own breaks the chain: the hop before it named a
			// source the store cannot follow.
			if st.Hops > 0 {
				st.LineageComplete = false
			}
			break
		}
		if err != nil {
			return SourceStatus{}, err
		}
		if st.Hops == 0 {
			st.Recorded, st.Origin = true, origin
		}
		used = append(used, id)
		st.Hops++
		st.LineageComplete = st.LineageComplete && complete == 1
		scopes = append(scopes, SourceScopeRef(kind, src))
		if kind != SourceMessage {
			st.RootKind, st.RootRef = kind, src
			break
		}
		ref = src
	}
	if !st.Recorded {
		st.LineageComplete = false
	}
	withdrawn, err := s.anyFenced(ctx, sessionID, scopes)
	if err != nil {
		return SourceStatus{}, err
	}
	st.Withdrawn = withdrawn
	return st, nil
}

// anyFenced reports whether the session or any of the source scope refs has a fence, in one query.
func (s *Store) anyFenced(ctx context.Context, sessionID string, sourceScopes []string) (bool, error) {
	query := `SELECT 1 FROM source_fences WHERE (scope_kind = 'session' AND scope_ref = ?)`
	args := []any{sessionID}
	if len(sourceScopes) > 0 {
		query += ` OR (scope_kind = 'source' AND scope_ref IN (?` + strings.Repeat(",?", len(sourceScopes)-1) + `))`
		for _, sc := range sourceScopes {
			args = append(args, sc)
		}
	}
	var one int
	err := s.db.QueryRowContext(ctx, query+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
