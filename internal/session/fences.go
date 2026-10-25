package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	// MaxRevokeSessions bounds the reverse walk of one revocation. A walk that reaches more sessions
	// stops and fences every session of the database instead (the project: one database per
	// workspace): wider, never narrower (DATA_AND_FAILURES §2, §5).
	MaxRevokeSessions = 256
	// MaxReportIDs bounds each id list of a revocation report; the counts stay exact.
	MaxReportIDs = 256
)

// ErrNoRevocation reports an epoch no revocation was committed with.
var ErrNoRevocation = errors.New("no revocation with that epoch")

// Revocation is what one RevokeSession committed, read back as it was at its commit: the epoch every
// fence it wrote carries, the sessions it fenced, and the exposure it found in them. InFlight are the
// requests admitted and not settled at the commit - each may have been sent, and may still be sent:
// a request admitted before the revocation is not recalled (DATA_AND_FAILURES §5). Unknown are those
// already unknown, Executions the side-effect tool calls admitted or of unknown outcome; Answered
// counts the requests whose answer was stored (known exposure). Nothing in it is content: ids, counts
// and a short reason code.
type Revocation struct {
	Epoch        int64     `json:"epoch"`
	Source       string    `json:"source"`
	Reason       string    `json:"reason"`
	At           time.Time `json:"at"`
	Sessions     []string  `json:"sessions"`
	SessionCount int       `json:"sessionCount"`
	// Expanded is true when the walk passed MaxRevokeSessions and every session was fenced.
	Expanded       bool     `json:"expanded,omitempty"`
	InFlight       []string `json:"inFlight"`
	InFlightCount  int      `json:"inFlightCount"`
	Unknown        []string `json:"unknown"`
	UnknownCount   int      `json:"unknownCount"`
	Executions     []string `json:"executions"`
	ExecutionCount int      `json:"executionCount"`
	Answered       int      `json:"answered"`
}

// RevokeSession revokes the session sourceID in one write transaction: it walks the reverse index
// from it - the sessions whose edges cite a reached session (derived_origins source_kind 'session'),
// and the children a reached session started (session_relations, child_session edges) - fences every
// session reached with one new epoch, and stores the report on the source's fence row.
//
// The transaction is the linearization point. A request or execution admitted before it is in the
// report; any admission after it reads the fence and is refused; an edge committed after it that cites
// a fenced session is refused (sourceFenced). reason is a short machine code. The walk is index reads
// only, at most MaxRevokeSessions sessions; past that every session is fenced.
func (s *Store) RevokeSession(ctx context.Context, sourceID, reason string) (Revocation, error) {
	if err := checkID("session id", sourceID); err != nil {
		return Revocation{}, err
	}
	if !reasonRE.MatchString(reason) {
		return Revocation{}, invalid("reason must match %s", reasonRE)
	}
	var rev Revocation
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, sourceID); err != nil {
			return err
		}
		var epoch int64
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(epoch), 0) + 1 FROM source_fences`).Scan(&epoch); err != nil {
			return err
		}
		reached, expanded, err := revocationReach(ctx, conn, sourceID)
		if err != nil {
			return err
		}
		at := now()
		rev = Revocation{Epoch: epoch, Source: sourceID, Reason: reason, At: parseTime(at), SessionCount: len(reached), Expanded: expanded}
		rev.Sessions = capIDs(reached)
		if err := exposureIn(ctx, conn, reached, &rev); err != nil {
			return err
		}
		report, err := json.Marshal(rev)
		if err != nil {
			return err
		}
		pointer, err := json.Marshal(map[string]any{"revocation": sourceID, "epoch": epoch})
		if err != nil {
			return err
		}
		for _, id := range reached {
			tombstone := pointer
			if id == sourceID {
				tombstone = report
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, tombstone_json, created_at, updated_at)
				VALUES('session', ?, ?, ?, 'active', ?, ?, ?)`, id, epoch, reason, string(tombstone), at, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Revocation{}, err
	}
	return rev, nil
}

// revocationReach returns sourceID and every session the reverse walk reaches from it, source first,
// or every session of the database with expanded = true once more than MaxRevokeSessions are reached.
func revocationReach(ctx context.Context, conn *sql.Conn, sourceID string) ([]string, bool, error) {
	reached := []string{sourceID}
	seen := map[string]bool{sourceID: true}
	for i := 0; i < len(reached); i++ {
		next, err := selectStrings(ctx, conn, `
			SELECT session_id FROM derived_origins WHERE source_kind = 'session' AND source_ref = ?1
			UNION SELECT child_session_id FROM session_relations WHERE parent_session_id = ?1
			UNION SELECT substr(target_ref, 9) FROM derived_origins WHERE session_id = ?1 AND kind = 'child_session' AND target_ref LIKE 'session:%'`,
			reached[i])
		if err != nil {
			return nil, false, err
		}
		for _, id := range next {
			if id == "" || seen[id] {
				continue
			}
			if len(reached) == MaxRevokeSessions {
				all, err := selectStrings(ctx, conn, `SELECT id FROM sessions WHERE id <> ? ORDER BY id`, sourceID)
				if err != nil {
					return nil, false, err
				}
				return append([]string{sourceID}, all...), true, nil
			}
			seen[id] = true
			reached = append(reached, id)
		}
	}
	return reached, false, nil
}

// exposureIn fills the exposure part of rev from the requests and executions of the sessions.
func exposureIn(ctx context.Context, conn *sql.Conn, sessions []string, rev *Revocation) error {
	for _, id := range sessions {
		rows, err := conn.QueryContext(ctx, `SELECT request_id, state FROM request_envelopes
			WHERE session_id = ? AND state IN ('admitted','unknown','answered') ORDER BY created_at, request_id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var requestID, state string
			if err := rows.Scan(&requestID, &state); err != nil {
				rows.Close()
				return err
			}
			switch RequestState(state) {
			case RequestAdmitted:
				rev.InFlightCount++
				rev.InFlight = appendCapped(rev.InFlight, requestID)
			case RequestUnknown:
				rev.UnknownCount++
				rev.Unknown = appendCapped(rev.Unknown, requestID)
			default:
				rev.Answered++
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		executions, err := selectStrings(ctx, conn, `SELECT execution_id FROM derived_origins
			WHERE session_id = ? AND kind = 'execution' AND outcome IN ('admitted','outcome_unknown') ORDER BY id`, id)
		if err != nil {
			return err
		}
		rev.ExecutionCount += len(executions)
		for _, x := range executions {
			rev.Executions = appendCapped(rev.Executions, x)
		}
	}
	return nil
}

// SessionFenced reports whether a fence on the session refuses the session's own work: a fence no Start
// has projected yet (consumerFence). It only reads: a caller that must be ordered with a revocation
// reads the fence inside its own write transaction instead.
func (s *Store) SessionFenced(ctx context.Context, sessionID string) (bool, error) {
	_, fenced, err := consumerFence(ctx, s.db, sessionID)
	return fenced, err
}

// SessionWithdrawn reports whether the session is withdrawn as a source: any fence names it, projected
// or not (sessionFence). A read exit returns no content of a withdrawn session to an agent, and stores
// no edge citing it. It only reads; a commit that cites the session re-reads the fence itself.
func (s *Store) SessionWithdrawn(ctx context.Context, sessionID string) (bool, error) {
	_, fenced, err := sessionFence(ctx, s.db, sessionID)
	return fenced, err
}

// Revocation reads back the report of the revocation committed with epoch.
func (s *Store) Revocation(ctx context.Context, epoch int64) (Revocation, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT tombstone_json FROM source_fences
		WHERE scope_kind = 'session' AND epoch = ? AND json_extract(tombstone_json, '$.epoch') = ? AND json_type(tombstone_json, '$.sessions') = 'array'`,
		epoch, epoch).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Revocation{}, ErrNoRevocation
	}
	if err != nil {
		return Revocation{}, err
	}
	var rev Revocation
	if err := json.Unmarshal([]byte(raw), &rev); err != nil {
		return Revocation{}, err
	}
	return rev, nil
}

// RequestStates returns the current state of each request id the database holds.
func (s *Store) RequestStates(ctx context.Context, ids []string) (map[string]RequestState, error) {
	out := make(map[string]RequestState, len(ids))
	for _, id := range ids {
		var state string
		err := s.db.QueryRowContext(ctx, `SELECT state FROM request_envelopes WHERE request_id = ?`, id).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = RequestState(state)
	}
	return out, nil
}

func selectStrings(ctx context.Context, conn *sql.Conn, query string, args ...any) ([]string, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSpace(v))
	}
	return out, rows.Err()
}

func capIDs(ids []string) []string {
	if len(ids) > MaxReportIDs {
		return append([]string(nil), ids[:MaxReportIDs]...)
	}
	return append([]string(nil), ids...)
}

func appendCapped(ids []string, id string) []string {
	if len(ids) >= MaxReportIDs {
		return ids
	}
	return append(ids, id)
}
