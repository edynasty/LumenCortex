package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxProjectionEdges bounds the edges of one session a projection examines. A session with more edges
// citing fenced sources is not projected: it stays isolated (DATA_AND_FAILURES §2: wider, never
// narrower).
const MaxProjectionEdges = 1024

// reachedFence is true for a session fence a revocation wrote because its walk reached the session
// from another one (its tombstone names that revocation's source). Only such a fence can be projected:
// any other fence on a session - the revocation's own source, or one written by anything else - means
// the session itself is withdrawn.
const reachedFence = `(json_type(tombstone_json, '$.revocation') IS 'text' AND json_extract(tombstone_json, '$.revocation') IS NOT scope_ref)`

// Reasons a thread cannot be projected (Projection.Isolated). Each is a code, not content.
const (
	// IsolatedRevoked: the session itself is revoked (a fence on it that is not reachedFence); its
	// whole content is withdrawn.
	IsolatedRevoked = "revoked_source"
	// IsolatedParent: a session that started this one is fenced, so its goal and first input derive
	// from fenced content.
	IsolatedParent = "fenced_parent"
	// IsolatedLineage: the session holds model output without a request record (history older than
	// request provenance, or imported), or an edge list that was cut: what it read cannot be told.
	IsolatedLineage = "incomplete_lineage"
	// IsolatedUnmapped: an edge citing a fenced source names something other than a transcript row a
	// run can withhold.
	IsolatedUnmapped = "unmapped_source"
	// IsolatedExposed: a model answer was produced from a request that carried a row of a fenced
	// source; that answer, and everything after it, may restate the source (free paraphrase).
	IsolatedExposed = "exposed"
	// IsolatedBound: more than MaxProjectionEdges edges cite fenced sources.
	IsolatedBound = "bound"
)

// Projection is what a new run of a session may see after revocations (DATA_AND_FAILURES §5 step 3).
// It is derived from durable rows only - fences, edges, request envelopes and the transcript - so a
// restart or another process computes the same one.
type Projection struct {
	// Epoch is the newest epoch of a fence on the session, 0 when none.
	Epoch int64
	// Open is true when a fence on the session is not projected yet: the session's own work is refused
	// until a Start projects it (AcknowledgeProjection) - or for good, when Isolated is set.
	Open bool
	// Withheld are the transcript seqs, ascending, whose content no run may send: the rows that carry
	// content of a fenced source. They stay in the window with their content replaced.
	Withheld []int64
	// Isolated is the reason the thread cannot be projected, "" when it can.
	Isolated string
}

// Projection reads what a new run of the session may see. It only reads, in one read transaction: the
// caller holds the session's run lock, so no run of the session writes meanwhile, and a revocation
// that commits after the read changes the newest epoch, which AcknowledgeProjection checks.
//
// A row is withheld when an edge of the session cites a fenced source: a session with a fence (any
// state, projected or not) or a source-scoped fence. Such an edge names a tool result row
// ("tool:<request>:<call>") or a task result row ("task_result:<child>@<seq>"). The thread is isolated
// - never projected - when a withheld row was in the window of a request whose answer is stored and
// that was admitted before the row's source was fenced (the answer may restate it, and the model
// output after it too), or for any reason listed with the Isolated codes.
func (s *Store) Projection(ctx context.Context, sessionID string) (Projection, error) {
	if err := checkID("session id", sessionID); err != nil {
		return Projection{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Projection{}, err
	}
	defer tx.Rollback()
	var p Projection
	var newest sql.NullInt64
	var open, revoked int64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(epoch),
		COALESCE(MAX(json_extract(tombstone_json, '$.projected') IS NULL), 0),
		COALESCE(MAX(NOT `+reachedFence+`), 0)
		FROM source_fences WHERE scope_kind = 'session' AND scope_ref = ?`, sessionID).Scan(&newest, &open, &revoked); err != nil {
		return Projection{}, err
	}
	if !newest.Valid {
		// A session no revocation reached cites no fenced session: a revocation fences every session
		// whose edges cite a reached one, and an edge committed after it is refused.
		return p, nil
	}
	p.Epoch, p.Open = newest.Int64, open == 1
	if revoked == 1 {
		p.Isolated = IsolatedRevoked
		return p, nil
	}
	isolated, err := projectionIsolated(ctx, tx, sessionID)
	if err != nil {
		return Projection{}, err
	}
	if isolated != "" {
		p.Isolated = isolated
		return p, nil
	}
	withheld, isolated, err := withheldRows(ctx, tx, sessionID)
	if err != nil {
		return Projection{}, err
	}
	p.Isolated = isolated
	if isolated == "" {
		for seq := range withheld {
			p.Withheld = append(p.Withheld, seq)
		}
		slices.Sort(p.Withheld)
	}
	return p, nil
}

// projectionIsolated checks the session-wide reasons: a fenced parent, model output without a request
// record, and a cut edge list.
func projectionIsolated(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM session_relations r
		JOIN source_fences f ON f.scope_kind = 'session' AND f.scope_ref = r.parent_session_id
		WHERE r.child_session_id = ?1
		UNION ALL SELECT 1 FROM derived_origins d
		JOIN source_fences f ON f.scope_kind = 'session' AND f.scope_ref = d.session_id
		WHERE d.kind = 'child_session' AND d.target_ref = 'session:' || ?1
		LIMIT 1`, sessionID).Scan(&one)
	switch {
	case err == nil:
		return IsolatedParent, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	// Every assistant row is the answer of one main request (AppendAnswered); a row without one is
	// history whose reads left no edges.
	var assistants, answered int64
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM session_messages WHERE session_id = ?1 AND role = 'assistant'),
		(SELECT COUNT(*) FROM request_envelopes WHERE session_id = ?1 AND answered_seq IS NOT NULL)`,
		sessionID).Scan(&assistants, &answered); err != nil {
		return "", err
	}
	if assistants != answered {
		return IsolatedLineage, nil
	}
	// A message edge that cites a session is stored incomplete when its list was cut (a cited session
	// may have no edge at all) or its source's lineage is incomplete (the row may carry content of a
	// source no edge names). Either way what the session carries cannot be told.
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM derived_origins
		WHERE session_id = ? AND kind = 'message' AND source_kind = 'session' AND lineage_complete = 0 LIMIT 1`,
		sessionID).Scan(&one)
	switch {
	case err == nil:
		return IsolatedLineage, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	return "", nil
}

// withheldRows maps every edge of the session that cites a fenced source to its transcript row, with
// the epoch at which that source was first fenced (the earliest fence on it: an edge citing a fenced
// source is refused, so the edge predates that fence). It reports the isolation reason instead when an
// edge does not name a row, there are too many, or a row was exposed to a stored answer.
func withheldRows(ctx context.Context, tx *sql.Tx, sessionID string) (map[int64]int64, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind, target_ref, a, b FROM (
		SELECT d.kind, d.target_ref,
			(SELECT MIN(epoch) FROM source_fences WHERE d.source_kind = 'session' AND scope_kind = 'session' AND scope_ref = d.source_ref) AS a,
			(SELECT MIN(epoch) FROM source_fences WHERE scope_kind = 'source' AND scope_ref = d.source_kind || ':' || d.source_ref) AS b
		FROM derived_origins d WHERE d.session_id = ? AND d.kind <> 'execution')
		WHERE a IS NOT NULL OR b IS NOT NULL LIMIT ?`, sessionID, MaxProjectionEdges+1)
	if err != nil {
		return nil, "", err
	}
	type tainted struct {
		kind, target string
		epoch        int64
	}
	var edges []tainted
	for rows.Next() {
		var t tainted
		var a, b sql.NullInt64
		if err := rows.Scan(&t.kind, &t.target, &a, &b); err != nil {
			rows.Close()
			return nil, "", err
		}
		switch {
		case a.Valid && b.Valid:
			t.epoch = min(a.Int64, b.Int64)
		case a.Valid:
			t.epoch = a.Int64
		default:
			t.epoch = b.Int64
		}
		edges = append(edges, t)
	}
	if err := rows.Close(); err != nil {
		return nil, "", err
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(edges) > MaxProjectionEdges {
		return nil, IsolatedBound, nil
	}
	withheld := map[int64]int64{}
	for _, e := range edges {
		if e.kind != "message" {
			return nil, IsolatedUnmapped, nil
		}
		seqs, err := edgeRows(ctx, tx, sessionID, e.target)
		if err != nil {
			return nil, "", err
		}
		if len(seqs) == 0 {
			return nil, IsolatedUnmapped, nil
		}
		for _, seq := range seqs {
			if prior, ok := withheld[seq]; !ok || e.epoch < prior {
				withheld[seq] = e.epoch
			}
		}
	}
	for seq, epoch := range withheld {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM request_envelopes
			WHERE session_id = ? AND state = 'answered' AND purpose = ? AND first_seq <= ? AND last_seq >= ? AND fence_epoch < ? LIMIT 1`,
			sessionID, PurposeMain, seq, seq, epoch).Scan(&one)
		switch {
		case err == nil:
			return nil, IsolatedExposed, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, "", err
		}
	}
	return withheld, "", nil
}

// edgeRows returns the transcript rows an edge target names: the tool result row of
// "tool:<request>:<call>" (the first tool row answering call after the request's answer), or every
// task result row delivering the child of "task_result:<child>@<seq>". None: the target names no row.
func edgeRows(ctx context.Context, tx *sql.Tx, sessionID, target string) ([]int64, error) {
	switch {
	case strings.HasPrefix(target, "tool:"):
		requestID, callID, ok := strings.Cut(strings.TrimPrefix(target, "tool:"), ":")
		if !ok || requestID == "" || callID == "" {
			return nil, nil
		}
		var seq int64
		err := tx.QueryRowContext(ctx, `SELECT m.seq FROM request_envelopes r
			JOIN session_messages m ON m.session_id = r.session_id AND m.seq > r.answered_seq
			WHERE r.request_id = ? AND r.session_id = ? AND m.role = 'tool' AND json_extract(m.json, '$.tool_call_id') = ?
			ORDER BY m.seq LIMIT 1`, requestID, sessionID, callID).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []int64{seq}, nil
	case strings.HasPrefix(target, "task_result:"):
		at := strings.LastIndexByte(target, '@')
		if at < 0 {
			return nil, nil
		}
		child := target[len("task_result:"):at]
		rows, err := tx.QueryContext(ctx, `SELECT seq, json FROM session_messages
			WHERE session_id = ? AND role = 'user' AND json_extract(json, '$.origin') = 'task_result'`, sessionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var seq int64
			var raw string
			if err := rows.Scan(&seq, &raw); err != nil {
				return nil, err
			}
			if deliveredChild(raw) == child {
				out = append(out, seq)
			}
		}
		return out, rows.Err()
	default:
		return nil, nil
	}
}

// deliveredChild is the child session a task result row reports: the sessionId of the JSON after
// the row's header line, "" when the row cannot be read.
func deliveredChild(raw string) string {
	var m struct {
		Content string `json:"content"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return ""
	}
	_, body, ok := strings.Cut(m.Content, "\n")
	if !ok {
		return ""
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal([]byte(body), &r) != nil {
		return ""
	}
	return r.SessionID
}

// AcknowledgeProjection records, in one write transaction, that a Start projected every fence on the
// session up to epoch (DATA_AND_FAILURES §5 step 3-4): the fences stay - the session is still withdrawn
// as a source, and a downgrade is still refused - but no longer refuse the session's own requests,
// tools and commits (consumerFence), and every run withholds the rows Projection names. The fence
// rows record it as tombstone key "projected" with the epoch. Only reachedFence rows are projected:
// a session that is itself revoked stays fenced.
//
// It writes nothing and returns ErrFenced when the session's newest fence epoch is not epoch: a
// revocation committed after the projection was read, and the next Start reads it again.
func (s *Store) AcknowledgeProjection(ctx context.Context, sessionID string, epoch int64) error {
	if err := checkID("session id", sessionID); err != nil {
		return err
	}
	if epoch < 1 {
		return invalid("epoch must be at least 1")
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		newest, fenced, err := sessionFence(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		if !fenced || newest != epoch {
			return fmt.Errorf("%w: newest fence epoch is %d, the projection read %d", ErrFenced, newest, epoch)
		}
		_, err = conn.ExecContext(ctx, `UPDATE source_fences SET tombstone_json = json_set(tombstone_json, '$.projected', ?), updated_at = ?
			WHERE scope_kind = 'session' AND scope_ref = ? AND json_extract(tombstone_json, '$.projected') IS NULL AND `+reachedFence,
			epoch, now(), sessionID)
		return err
	})
}
