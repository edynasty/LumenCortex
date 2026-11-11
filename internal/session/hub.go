package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// The Hub control plane's durable host state (migration 0013): the free-text steer inbox, the pause a
// run is held at (projected after a restart), Task Lease claims on work units of a Hub task DAG, and
// the host command idempotency records. All of it is metadata and host-authored text: no model
// reasoning and no transcript copy is stored here.

// MaxSteerBytes bounds one steer message; the injected aside shares the request window's budget.
const MaxSteerBytes = 4096

// MaxHubCommandResultBytes bounds what one command result may persist.
const MaxHubCommandResultBytes = 16384

var (
	// ErrSteerExists reports a steer command id reused for the same session with a different body.
	ErrSteerExists = errors.New("steer command id already names a different message")
	// ErrLeaseHeld reports a work unit whose Task Lease a live holder owns.
	ErrLeaseHeld = errors.New("work unit is claimed by a live Task Lease")
	// ErrLeaseNotHeld reports a renew or release whose holder and lease no longer match.
	ErrLeaseNotHeld = errors.New("Task Lease is not held by this holder")
)

// SteerMessage is one queued free-text instruction. AppliedAt is zero until a run injected it; Seq is
// the position that fixes application order, AppliedSeq the transcript row it became (-1 before).
type SteerMessage struct {
	Seq        int64      `json:"seq"`
	SessionID  string     `json:"sessionId"`
	RunID      string     `json:"runId,omitempty"`
	CommandID  string     `json:"commandId"`
	Body       string     `json:"body,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	AppliedAt  *time.Time `json:"appliedAt,omitempty"`
	AppliedSeq int64      `json:"appliedSeq"`
}

// QueueSteerMessage appends one steer message. A command id already used by the session returns the
// stored message (idempotent) and ErrSteerExists when the body differs, so a host retry never queues the
// instruction twice.
func (s *Store) QueueSteerMessage(ctx context.Context, sessionID, runID, commandID, body string) (SteerMessage, error) {
	m := SteerMessage{SessionID: sessionID, RunID: runID, CommandID: commandID, Body: strings.TrimSpace(body), AppliedSeq: -1}
	if len(m.CommandID) == 0 || len(m.CommandID) > 128 || m.SessionID == "" || len(m.RunID) > 128 || len(m.Body) > MaxSteerBytes || !utf8.ValidString(m.Body) || strings.ContainsRune(m.Body, 0) {
		return m, ErrInvalid
	}
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, sessionID); err != nil {
			return err
		}
		var applied string
		var appliedSeq int64
		var created string
		err := conn.QueryRowContext(ctx, `SELECT seq, run_id, body, created_at, applied_at, applied_seq FROM steer_messages WHERE session_id=? AND command_id=?`, sessionID, commandID).
			Scan(&m.Seq, &m.RunID, &m.Body, &created, &applied, &appliedSeq)
		if err == nil {
			if len([]byte(m.Body)) > MaxSteerBytes {
				return ErrInvalid
			}
			m.CreatedAt = parseTime(created)
			m.AppliedSeq = appliedSeq
			if applied != "" {
				at := parseTime(applied)
				m.AppliedAt = &at
			}
			// A reused id may only replay the same instruction.
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		m.CreatedAt = time.Now().UTC()
		m.AppliedAt = nil
		m.AppliedSeq = -1
		result, err := conn.ExecContext(ctx, `INSERT INTO steer_messages(session_id, run_id, command_id, body, created_at) VALUES(?,?,?,?,?)`,
			sessionID, m.RunID, m.CommandID, m.Body, m.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		m.Seq, err = result.LastInsertId()
		return err
	})
	return m, err
}

// PendingSteerMessages returns the session's unapplied steer messages in sequence order, newest last.
// limit bounds the batch (0 means 8).
func (s *Store) PendingSteerMessages(ctx context.Context, sessionID string, limit int) ([]SteerMessage, error) {
	if limit <= 0 {
		limit = 8
	}
	limit = min(limit, 64)
	rows, err := s.db.QueryContext(ctx, `SELECT seq, run_id, command_id, body, created_at FROM steer_messages
WHERE session_id = ? AND applied_at = '' ORDER BY seq LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SteerMessage{}
	for rows.Next() {
		m := SteerMessage{SessionID: sessionID, AppliedSeq: -1}
		var created string
		if err := rows.Scan(&m.Seq, &m.RunID, &m.CommandID, &m.Body, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SteerMessages returns the session's steer messages in sequence order, applied ones included: the host
// reads what it queued and which transcript row each became.
func (s *Store) SteerMessages(ctx context.Context, sessionID string, limit int) ([]SteerMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	rows, err := s.db.QueryContext(ctx, `SELECT seq, run_id, command_id, body, created_at, applied_at, applied_seq
FROM steer_messages WHERE session_id = ? ORDER BY seq LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SteerMessage{}
	for rows.Next() {
		m := SteerMessage{SessionID: sessionID}
		var created, applied string
		if err := rows.Scan(&m.Seq, &m.RunID, &m.CommandID, &m.Body, &created, &applied, &m.AppliedSeq); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		if applied != "" {
			at := parseTime(applied)
			m.AppliedAt = &at
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkSteerApplied records the transcript row a steer message became. The write is idempotent: a message
// already applied keeps its first row.
func (s *Store) MarkSteerApplied(ctx context.Context, sessionID string, seq, appliedSeq int64) error {
	if seq <= 0 || appliedSeq < 0 {
		return ErrInvalid
	}
	var applied, runID string
	var prior int64
	if err := s.db.QueryRowContext(ctx, `SELECT applied_at, applied_seq, run_id FROM steer_messages WHERE session_id=? AND seq=?`, sessionID, seq).Scan(&applied, &prior, &runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if applied != "" {
		if prior != appliedSeq {
			return ErrSteerExists
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE steer_messages SET applied_at=?, applied_seq=? WHERE session_id=? AND seq=? AND applied_at=''`,
		now(), appliedSeq, sessionID, seq)
	return err
}

// RunPause is the pause a session's run is held at.
type RunPause struct {
	SessionID string    `json:"sessionId"`
	RunID     string    `json:"runId"`
	Reason    string    `json:"reason,omitempty"`
	Boundary  string    `json:"boundary,omitempty"`
	Step      int64     `json:"step,omitempty"`
	PausedAt  time.Time `json:"pausedAt"`
}

// RecordRunPause stores the pause the run reached. A pause of a different run replaces the old row: the
// row always names the current run's pause.
func (s *Store) RecordRunPause(ctx context.Context, p RunPause) error {
	if p.SessionID == "" || p.RunID == "" || len(p.RunID) > 128 || len(p.Reason) > 128 || len(p.Boundary) > 64 {
		return ErrInvalid
	}
	if p.PausedAt.IsZero() {
		p.PausedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_pauses(session_id, run_id, reason, boundary, step, paused_at) VALUES(?,?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET run_id=excluded.run_id, reason=excluded.reason, boundary=excluded.boundary, step=excluded.step, paused_at=excluded.paused_at`,
		p.SessionID, p.RunID, p.Reason, p.Boundary, p.Step, p.PausedAt.Format(time.RFC3339Nano))
	return err
}

// ClearRunPause removes the session's pause record when its run resumed or ended.
func (s *Store) ClearRunPause(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM run_pauses WHERE session_id=?`, sessionID)
	return err
}

// RunPauses returns the pause records of the sessions named; a session without one is absent.
func (s *Store) RunPauses(ctx context.Context, sessionIDs []string) (map[string]RunPause, error) {
	out := map[string]RunPause{}
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		var (
			p        RunPause
			pausedAt string
		)
		err := s.db.QueryRowContext(ctx, `SELECT run_id, reason, boundary, step, paused_at FROM run_pauses WHERE session_id=?`, id).
			Scan(&p.RunID, &p.Reason, &p.Boundary, &p.Step, &pausedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		p.SessionID, p.PausedAt = id, parseTime(pausedAt)
		out[id] = p
	}
	return out, nil
}

// TaskLease is one work unit's claim: the single holder allowed to work it until ExpiresAt.
type TaskLease struct {
	RootID    string    `json:"rootId"`
	UnitID    string    `json:"unitId"`
	Holder    string    `json:"holder"`
	LeaseID   string    `json:"leaseId"`
	ExpiresAt time.Time `json:"expiresAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Expired is computed when read; an expired row still exists until it is recovered.
	Expired bool `json:"expired,omitempty"`
}

// ClaimTaskLease takes the unit's lease for holder, setting its expiry. One statement makes it atomic:
// a live claim by another holder matches no row and changes nothing. Claiming a unit whose lease expired
// — or whose holder is the caller — wins; the caller's previous lease id is replaced.
func (s *Store) ClaimTaskLease(ctx context.Context, lease TaskLease, ttl time.Duration) (TaskLease, error) {
	if lease.RootID == "" || lease.UnitID == "" || lease.Holder == "" || lease.LeaseID == "" ||
		len(lease.UnitID) > 128 || len(lease.Holder) > 128 || len(lease.LeaseID) > 128 || ttl <= 0 {
		return lease, ErrInvalid
	}
	at := time.Now().UTC()
	lease.ExpiresAt, lease.UpdatedAt = at.Add(ttl), at
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, lease.RootID); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `INSERT INTO work_unit_leases(root_id, unit_id, holder, lease_id, expires_at, updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(root_id, unit_id) DO UPDATE SET holder=excluded.holder, lease_id=excluded.lease_id, expires_at=excluded.expires_at, updated_at=excluded.updated_at
WHERE work_unit_leases.expires_at <= ? OR work_unit_leases.holder = excluded.holder`,
			lease.RootID, lease.UnitID, lease.Holder, lease.LeaseID, lease.ExpiresAt.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return ErrLeaseHeld
		}
		return nil
	})
	if err != nil {
		return TaskLease{}, err
	}
	lease.Expired = false
	return lease, nil
}

// RenewTaskLease extends one lease the holder still owns. An expired lease cannot be renewed: the unit
// may already belong to another worker, so the holder must claim it again.
func (s *Store) RenewTaskLease(ctx context.Context, root, unit, holder, leaseID string, ttl time.Duration) (TaskLease, error) {
	if root == "" || unit == "" || holder == "" || leaseID == "" || ttl <= 0 {
		return TaskLease{}, ErrInvalid
	}
	at := time.Now().UTC()
	expires := at.Add(ttl)
	result, err := s.db.ExecContext(ctx, `UPDATE work_unit_leases SET expires_at=?, updated_at=?
WHERE root_id=? AND unit_id=? AND holder=? AND lease_id=? AND expires_at > ?`,
		expires.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), root, unit, holder, leaseID, at.Format(time.RFC3339Nano))
	if err != nil {
		return TaskLease{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return TaskLease{}, err
	}
	if changed == 0 {
		return TaskLease{}, ErrLeaseNotHeld
	}
	return TaskLease{RootID: root, UnitID: unit, Holder: holder, LeaseID: leaseID, ExpiresAt: expires, UpdatedAt: at}, nil
}

// RenewHeldTaskLeases extends every live lease holder owns, whatever its root, and returns them. It is
// the step-boundary renewal of a worker's own claims; nothing is written for a holder that owns none.
func (s *Store) RenewHeldTaskLeases(ctx context.Context, holder string, ttl time.Duration) ([]TaskLease, error) {
	if holder == "" || ttl <= 0 {
		return nil, ErrInvalid
	}
	at := time.Now().UTC()
	expires := at.Add(ttl)
	var out []TaskLease
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT root_id, unit_id, lease_id, expires_at FROM work_unit_leases WHERE holder=? AND expires_at > ?`, holder, at.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		for rows.Next() {
			l := TaskLease{Holder: holder, UpdatedAt: at, ExpiresAt: expires}
			var prior string
			if err := rows.Scan(&l.RootID, &l.UnitID, &l.LeaseID, &prior); err != nil {
				rows.Close()
				return err
			}
			_ = prior
			out = append(out, l)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		_, err = conn.ExecContext(ctx, `UPDATE work_unit_leases SET expires_at=?, updated_at=? WHERE holder=? AND expires_at > ?`,
			expires.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), holder, at.Format(time.RFC3339Nano))
		return err
	})
	return out, err
}

// ReleaseTaskLease drops one lease the holder owns, returning its unit to ready at once.
func (s *Store) ReleaseTaskLease(ctx context.Context, root, unit, holder, leaseID string) error {
	if root == "" || unit == "" || holder == "" {
		return ErrInvalid
	}
	query := `DELETE FROM work_unit_leases WHERE root_id=? AND unit_id=? AND holder=?`
	args := []any{root, unit, holder}
	if leaseID != "" {
		query += ` AND lease_id=?`
		args = append(args, leaseID)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrLeaseNotHeld
	}
	return nil
}

// ReleaseTaskLeasesOf drops every lease the holder owns, whatever the root: a run that ends (finished or
// cancelled) leaves no claim a scheduler has to expire. It returns the released leases.
func (s *Store) ReleaseTaskLeasesOf(ctx context.Context, holder string) ([]TaskLease, error) {
	var out []TaskLease
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT root_id, unit_id, lease_id, expires_at, updated_at FROM work_unit_leases WHERE holder=?`, holder)
		if err != nil {
			return err
		}
		for rows.Next() {
			var l TaskLease
			var expires, updated string
			if err := rows.Scan(&l.RootID, &l.UnitID, &l.LeaseID, &expires, &updated); err != nil {
				rows.Close()
				return err
			}
			l.Holder, l.ExpiresAt, l.UpdatedAt = holder, parseTime(expires), parseTime(updated)
			out = append(out, l)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `DELETE FROM work_unit_leases WHERE holder=?`, holder)
		return err
	})
	return out, err
}

// MoveTaskLeases reassigns every lease of from to to, the handoff of a unit's ownership, and gives the
// moved rows the ttl of the new holder's claim: the target renews only at its step boundaries, and its
// first one is a whole model request away, so the source's remaining time cannot bound the new claim.
// A non-positive ttl is refused rather than silently keeping an expiry meant for another holder.
func (s *Store) MoveTaskLeases(ctx context.Context, from, to string, ttl time.Duration) (int, error) {
	if from == "" || to == "" || from == to || ttl <= 0 {
		return 0, ErrInvalid
	}
	at := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE work_unit_leases SET holder=?, expires_at=?, updated_at=? WHERE holder=?`,
		to, at.Add(ttl).Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), from)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// TaskLeases returns the leases under root. Expired rows are reported with Expired set; recovery deletes
// them, so a reader sees the state that a claim would see.
func (s *Store) TaskLeases(ctx context.Context, root string) ([]TaskLease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT unit_id, holder, lease_id, expires_at, updated_at FROM work_unit_leases WHERE root_id=? ORDER BY unit_id`, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	at := time.Now().UTC()
	out := []TaskLease{}
	for rows.Next() {
		l := TaskLease{RootID: root}
		var expires, updated string
		if err := rows.Scan(&l.UnitID, &l.Holder, &l.LeaseID, &expires, &updated); err != nil {
			return nil, err
		}
		l.ExpiresAt, l.UpdatedAt = parseTime(expires), parseTime(updated)
		l.Expired = !l.ExpiresAt.After(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// TaskLeasesHeldBy returns the leases holder owns, whatever their root. Expired rows are reported with
// Expired set, as TaskLeases does.
func (s *Store) TaskLeasesHeldBy(ctx context.Context, holder string) ([]TaskLease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT root_id, unit_id, lease_id, expires_at, updated_at FROM work_unit_leases WHERE holder=? ORDER BY root_id, unit_id`, holder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	at := time.Now().UTC()
	out := []TaskLease{}
	for rows.Next() {
		l := TaskLease{Holder: holder}
		var expires, updated string
		if err := rows.Scan(&l.RootID, &l.UnitID, &l.LeaseID, &expires, &updated); err != nil {
			return nil, err
		}
		l.ExpiresAt, l.UpdatedAt = parseTime(expires), parseTime(updated)
		l.Expired = !l.ExpiresAt.After(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// RecoverExpiredTaskLeases deletes the expired leases under root and returns them: their work units are
// ready again, and the next claim by any worker wins.
func (s *Store) RecoverExpiredTaskLeases(ctx context.Context, root string) ([]TaskLease, error) {
	var out []TaskLease
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		at := time.Now().UTC()
		rows, err := conn.QueryContext(ctx, `SELECT unit_id, holder, lease_id, expires_at, updated_at FROM work_unit_leases WHERE root_id=? AND expires_at <= ?`, root, at.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		for rows.Next() {
			var expires, updated string
			l := TaskLease{RootID: root, Expired: true}
			if err := rows.Scan(&l.UnitID, &l.Holder, &l.LeaseID, &expires, &updated); err != nil {
				rows.Close()
				return err
			}
			l.ExpiresAt, l.UpdatedAt = parseTime(expires), parseTime(updated)
			out = append(out, l)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `DELETE FROM work_unit_leases WHERE root_id=? AND expires_at <= ?`, root, at.Format(time.RFC3339Nano))
		return err
	})
	return out, err
}

// HubCommandRecord is one host command's persisted result, keyed by its CommandID.
type HubCommandRecord struct {
	CommandID string
	SessionID string
	RunID     string
	Verb      string
	Result    []byte
	CreatedAt time.Time
}

// ReserveHubCommand journals a command id before the command runs, with r.Result as its provisional
// result. When the id is already journalled - by an execution that finished, is still running, or died
// before recording its outcome - nothing is written and the existing record is returned with
// reserved=false, so one command id is executed at most once.
func (s *Store) ReserveHubCommand(ctx context.Context, r HubCommandRecord) (record HubCommandRecord, reserved bool, err error) {
	if err := validHubCommandRecord(r); err != nil {
		return HubCommandRecord{}, false, err
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	err = s.writeTx(ctx, func(conn *sql.Conn) error {
		if err := sessionExists(ctx, conn, r.SessionID); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `INSERT INTO hub_commands(command_id, session_id, run_id, verb, result, created_at) VALUES(?,?,?,?,?,?)
ON CONFLICT(command_id) DO NOTHING`, r.CommandID, r.SessionID, r.RunID, r.Verb, string(r.Result), r.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			var created, raw string
			if err := conn.QueryRowContext(ctx, `SELECT command_id, session_id, run_id, verb, result, created_at FROM hub_commands WHERE command_id=?`, r.CommandID).
				Scan(&record.CommandID, &record.SessionID, &record.RunID, &record.Verb, &raw, &created); err != nil {
				return err
			}
			record.Result, record.CreatedAt = []byte(raw), parseTime(created)
			return nil
		}
		record, reserved = r, true
		// Retention: a command record is only useful while a host may retry it.
		_, err = conn.ExecContext(ctx, `DELETE FROM hub_commands WHERE session_id=? AND command_id NOT IN
(SELECT command_id FROM hub_commands WHERE session_id=? ORDER BY created_at DESC LIMIT 1000)`, r.SessionID, r.SessionID)
		return err
	})
	if err != nil {
		return HubCommandRecord{}, false, err
	}
	return record, reserved, nil
}

// CompleteHubCommand replaces the provisional result of a reserved command id with the command's
// outcome. ErrNotFound reports that the reservation is gone (its session was deleted, or retention
// evicted it), so the outcome could not be recorded.
func (s *Store) CompleteHubCommand(ctx context.Context, r HubCommandRecord) error {
	if err := validHubCommandRecord(r); err != nil {
		return err
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE hub_commands SET run_id=?, result=? WHERE command_id=? AND session_id=? AND verb=?`,
			r.RunID, string(r.Result), r.CommandID, r.SessionID, r.Verb)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func validHubCommandRecord(r HubCommandRecord) error {
	if r.CommandID == "" || len(r.CommandID) > 128 || r.SessionID == "" || len(r.RunID) > 128 || r.Verb == "" || len(r.Verb) > 32 ||
		len(r.Result) == 0 || len(r.Result) > MaxHubCommandResultBytes {
		return ErrInvalid
	}
	return nil
}
