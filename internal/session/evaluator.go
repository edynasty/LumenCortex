package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Artifact is one content-addressed blob of a session (design §4.7): a check's log or the diff a
// non-passing result's fingerprint covers. Hash is "sha256:<hex>" of Data.
type Artifact struct {
	Hash      string    `json:"hash"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	Truncated bool      `json:"truncated"`
	CreatedAt time.Time `json:"createdAt"`
	Data      []byte    `json:"-"`
}

// EvalRow is one stored evaluator result. Seq is the session's own result counter. WorkUnitID is set
// when the result decided a work unit's completion. Artifacts are artifact hashes of the same session.
// SpecDigest and ApprovalEpoch are the approved check definition the result was decided or accepted
// under (ADR-0017); a row stored before approvals existed has neither and never satisfies a check.
// EpisodeID and EpisodeStatus are the check episode the row belongs to, PreFingerprint and
// PostFingerprint the check's fingerprint when the episode began and ended; a row stored before
// episodes has none of them and never satisfies a check either.
type EvalRow struct {
	Seq             int64     `json:"seq"`
	WorkUnitID      string    `json:"workUnitId,omitempty"`
	CheckID         string    `json:"checkId"`
	Kind            string    `json:"kind"`
	Status          string    `json:"status"`
	Provenance      string    `json:"provenance"`
	Fingerprint     string    `json:"fingerprint"`
	SpecDigest      string    `json:"specDigest,omitempty"`
	ApprovalEpoch   int64     `json:"approvalEpoch,omitempty"`
	EpisodeID       string    `json:"episodeId,omitempty"`
	EpisodeStatus   string    `json:"episodeStatus,omitempty"`
	PreFingerprint  string    `json:"preFingerprint,omitempty"`
	PostFingerprint string    `json:"postFingerprint,omitempty"`
	Artifacts       []string  `json:"artifacts"`
	At              time.Time `json:"at"`
}

// Check episode statuses (eval_results.episode_status, ADR-0017). A row is written open and its
// episode is closed once; only closed rows can satisfy a check.
const (
	EpisodeOpen           = "open"
	EpisodeClosed         = "closed"
	EpisodeScopeChanged   = "scope_changed"
	EpisodeSpecUnapproved = "spec_unapproved"
	EpisodeInterrupted    = "interrupted"
)

// ErrEpisodeInvalid refuses an episode close with an unknown status or a missing id.
var ErrEpisodeInvalid = errors.New("invalid check episode")

// PutArtifact stores a blob under its hash. The same hash is the same content, so storing it again
// writes nothing.
func (s *Store) PutArtifact(ctx context.Context, sessionID string, a Artifact) error {
	created := a.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO artifacts(session_id, hash, kind, size, truncated, created_at, data)
		VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(session_id, hash) DO NOTHING`,
		sessionID, a.Hash, a.Kind, a.Size, boolInt(a.Truncated), created.UTC().Format(time.RFC3339Nano), a.Data)
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		return ErrNotFound
	}
	return err
}

// ErrArtifactNotFound reports an artifact hash the session does not hold.
var ErrArtifactNotFound = errors.New("artifact not found")

// Artifact returns one artifact of the session, or ErrArtifactNotFound.
func (s *Store) Artifact(ctx context.Context, sessionID, hash string) (Artifact, error) {
	var (
		a         Artifact
		created   string
		truncated int
	)
	err := s.db.QueryRowContext(ctx, `SELECT hash, kind, size, truncated, created_at, data FROM artifacts WHERE session_id = ? AND hash = ?`,
		sessionID, hash).Scan(&a.Hash, &a.Kind, &a.Size, &truncated, &created, &a.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	a.Truncated = truncated == 1
	a.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return a, err
}

// AppendEvalResult stores a result as the session's next seq and returns it. Every artifact it names
// must already be stored for the session.
func (s *Store) AppendEvalResult(ctx context.Context, sessionID string, row EvalRow) (int64, error) {
	artifacts := row.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}
	raw, err := json.Marshal(artifacts)
	if err != nil {
		return 0, err
	}
	at := row.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	// IMMEDIATE takes the write lock before reading MAX(seq): a host recording an acceptance in another
	// process cannot read the same seq.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	for _, hash := range artifacts {
		if err := conn.QueryRowContext(ctx, `SELECT 1 FROM artifacts WHERE session_id = ? AND hash = ?`, sessionID, hash).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("%w: eval result names %s", ErrArtifactNotFound, hash)
			}
			return 0, err
		}
	}
	var seq int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM eval_results WHERE session_id = ?`, sessionID).Scan(&seq); err != nil {
		return 0, err
	}
	var unit any
	if row.WorkUnitID != "" {
		unit = row.WorkUnitID
	}
	var specDigest, epoch any
	if row.SpecDigest != "" {
		specDigest = row.SpecDigest
	}
	if row.ApprovalEpoch > 0 {
		epoch = row.ApprovalEpoch
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO eval_results(session_id, seq, work_unit_id, check_id, kind, status, provenance, fingerprint, artifacts_json, at, spec_digest, approval_epoch,
			episode_id, episode_status, pre_fingerprint, post_fingerprint)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, seq, unit, row.CheckID, row.Kind, row.Status, row.Provenance, row.Fingerprint, string(raw), at.UTC().Format(time.RFC3339Nano),
		specDigest, epoch, nullString(row.EpisodeID), nullString(row.EpisodeStatus), nullString(row.PreFingerprint), nullString(row.PostFingerprint)); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return seq, nil
}

// CloseEpisode closes the open rows of one check episode in one short write transaction: each named row
// (seq -> its post fingerprint) gets status and its post fingerprint. A row already closed is left as
// it is, so closing twice changes nothing. It runs nothing and reads no workspace; the caller took the
// post snapshot before.
func (s *Store) CloseEpisode(ctx context.Context, sessionID, episodeID, status string, post map[int64]string) error {
	switch status {
	case EpisodeClosed, EpisodeScopeChanged, EpisodeSpecUnapproved, EpisodeInterrupted:
	default:
		return fmt.Errorf("%w: status %q", ErrEpisodeInvalid, status)
	}
	if episodeID == "" {
		return fmt.Errorf("%w: no episode id", ErrEpisodeInvalid)
	}
	if len(post) == 0 {
		return nil
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		for seq, fingerprint := range post {
			if _, err := conn.ExecContext(ctx, `UPDATE eval_results SET episode_status = ?, post_fingerprint = ?
				WHERE session_id = ? AND seq = ? AND episode_id = ? AND episode_status = 'open'`,
				status, nullString(fingerprint), sessionID, seq, episodeID); err != nil {
				return err
			}
		}
		return nil
	})
}

const evalColumns = `seq, COALESCE(work_unit_id, ''), check_id, kind, status, provenance, fingerprint, artifacts_json, at,
	COALESCE(spec_digest, ''), COALESCE(approval_epoch, 0), COALESCE(episode_id, ''), COALESCE(episode_status, ''),
	COALESCE(pre_fingerprint, ''), COALESCE(post_fingerprint, '')`

// DecidingEvalResult returns the newest result of a closed episode for the check with provenance
// runtime or human:*, the given fingerprint and the given approved definition (spec digest and
// approval epoch, ADR-0017): the one CheckVerdict reads (design §4.7), or nil when there is none. A row
// of another approval, of no approval, or of an episode that did not close is never returned.
func (s *Store) DecidingEvalResult(ctx context.Context, sessionID, checkID, fingerprint, specDigest string, approvalEpoch int64) (*EvalRow, error) {
	return s.oneEvalRow(ctx, `SELECT `+evalColumns+` FROM eval_results
		WHERE session_id = ? AND check_id = ? AND fingerprint = ? AND (provenance = 'runtime' OR provenance GLOB 'human:?*')
			AND spec_digest = ? AND approval_epoch = ? AND episode_status = 'closed'
		ORDER BY seq DESC LIMIT 1`, sessionID, checkID, fingerprint, specDigest, approvalEpoch)
}

// NewestEvalResult returns the newest result for the check whatever its provenance and fingerprint, or
// nil when the check never ran in the session.
func (s *Store) NewestEvalResult(ctx context.Context, sessionID, checkID string) (*EvalRow, error) {
	return s.oneEvalRow(ctx, `SELECT `+evalColumns+` FROM eval_results WHERE session_id = ? AND check_id = ? ORDER BY seq DESC LIMIT 1`,
		sessionID, checkID)
}

// EvalResults returns up to limit results with seq below beforeSeq, newest first; beforeSeq <= 0 starts
// from the newest.
func (s *Store) EvalResults(ctx context.Context, sessionID string, beforeSeq int64, limit int) ([]EvalRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if beforeSeq <= 0 {
		beforeSeq = 1<<63 - 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+evalColumns+` FROM eval_results WHERE session_id = ? AND seq < ? ORDER BY seq DESC LIMIT ?`,
		sessionID, beforeSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EvalRow{}
	for rows.Next() {
		row, err := scanEvalRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) oneEvalRow(ctx context.Context, query string, args ...any) (*EvalRow, error) {
	row, err := scanEvalRow(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func scanEvalRow(r interface{ Scan(...any) error }) (EvalRow, error) {
	var (
		row           EvalRow
		artifacts, at string
	)
	if err := r.Scan(&row.Seq, &row.WorkUnitID, &row.CheckID, &row.Kind, &row.Status, &row.Provenance, &row.Fingerprint, &artifacts, &at,
		&row.SpecDigest, &row.ApprovalEpoch, &row.EpisodeID, &row.EpisodeStatus, &row.PreFingerprint, &row.PostFingerprint); err != nil {
		return EvalRow{}, err
	}
	if err := json.Unmarshal([]byte(artifacts), &row.Artifacts); err != nil {
		return EvalRow{}, err
	}
	var err error
	row.At, err = time.Parse(time.RFC3339Nano, at)
	return row, err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
