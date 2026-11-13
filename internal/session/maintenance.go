package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// Persistent cognition maintenance state (R1b/T5a, canonical migration 0014). This database is the one
// maintenance authority of its project: the dirty revision, the cursor, the lease and every candidate
// and observation live here, and every change to them is one writeTx. No model call and no git process
// runs inside one; the caller does that work between a lease and its commit, and the commit re-checks
// the lease, the generation, the cursor and the policy before it writes anything.

// PurposeMaintenance marks a cognitive maintenance session (metadata purpose=maintenance).
const PurposeMaintenance = "maintenance"

var (
	// ErrMaintenanceOff: the database has no maintenance state; maintenance was never enabled for it.
	ErrMaintenanceOff = errors.New("cognitive maintenance is not enabled for this project")
	// ErrNothingPending: the cursor is at the dirty revision.
	ErrNothingPending = errors.New("no maintenance pending")
	// ErrMaintenanceLeaseHeld: another worker holds an unexpired maintenance lease.
	ErrMaintenanceLeaseHeld = errors.New("the maintenance lease is held by another worker")
	// ErrStaleLease: the lease this worker holds was taken over, expired, or the cursor, policy or
	// dirty revision no longer match what it read. Nothing was written.
	ErrStaleLease = errors.New("the maintenance lease is no longer current")
	// ErrNotQualified: the eval result is not a qualified observation (not a runtime result of an
	// approved check in a closed episode with a stable fingerprint).
	ErrNotQualified = errors.New("the eval result is not a qualified observation")
	// ErrSourceChanged: a source of the batch is no longer the version the maintainer read (it changed,
	// is gone, or is fenced), or the maintenance session is fenced. Nothing was written: the pass fails,
	// the cursor stays, and the next pass reads the sources as they are then.
	ErrSourceChanged = errors.New("a maintenance source changed while its batch was processed")
)

// MaintenanceState is the project's maintenance row as stored.
type MaintenanceState struct {
	ProjectID     string    `json:"projectId"`
	PolicyVersion string    `json:"policyVersion"`
	Dirty         int64     `json:"dirtyRevision"`
	Cursor        int64     `json:"cursorRevision"`
	LeaseID       string    `json:"leaseId,omitempty"`
	Generation    int64     `json:"generation"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Pending       string    `json:"pendingState"`
	LastError     string    `json:"lastError,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// Backlog is the number of source rows still to process.
	Backlog int64 `json:"backlog"`
}

// MaintenanceLease is what one worker holds between acquiring and committing: the lease id and
// generation it was given, and the cursor, dirty revision and policy it read under them.
type MaintenanceLease struct {
	LeaseID    string
	Generation int64
	Cursor     int64
	Dirty      int64
	Policy     string
	ExpiresAt  time.Time
}

// MaintenanceSource is one dirty source row.
type MaintenanceSource struct {
	Revision  int64
	Ref       knowledge.SourceRef
	CreatedAt time.Time
}

// EnsureMaintenanceState creates the project's maintenance row when there is none, which turns on
// source tracking: from now on a terminal run advances the dirty revision. An existing row is kept.
func (s *Store) EnsureMaintenanceState(ctx context.Context, projectID, policy string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO maintenance_state(id, project_id, policy_version, updated_at)
		VALUES(1, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, projectID, policy, stamp(now))
	return err
}

// ReadMaintenanceState returns the project's maintenance row, ErrMaintenanceOff when there is none.
func (s *Store) ReadMaintenanceState(ctx context.Context) (MaintenanceState, error) {
	return readMaintenanceState(ctx, s.db)
}

func readMaintenanceState(ctx context.Context, q rowQuerier) (MaintenanceState, error) {
	var st MaintenanceState
	var expires, updated string
	err := q.QueryRowContext(ctx, `SELECT project_id, policy_version, dirty_revision, cursor_revision, lease_id, generation,
		expires_at, pending_state, last_error, updated_at, (SELECT COUNT(*) FROM maintenance_sources)
		FROM maintenance_state WHERE id = 1`).Scan(&st.ProjectID, &st.PolicyVersion, &st.Dirty, &st.Cursor, &st.LeaseID,
		&st.Generation, &expires, &st.Pending, &st.LastError, &updated, &st.Backlog)
	if errors.Is(err, sql.ErrNoRows) {
		return MaintenanceState{}, ErrMaintenanceOff
	}
	if err != nil {
		return MaintenanceState{}, err
	}
	st.ExpiresAt = parseStamp(expires)
	st.UpdatedAt = parseStamp(updated)
	return st, nil
}

// AcquireMaintenanceLease takes the lease when sources are pending and no other worker holds an
// unexpired one: a new lease id and the next generation, valid until now+ttl. A takeover of an expired
// lease is the same write, so the old holder's generation no longer matches and its commit fails even
// when the cursor did not move. The policy is the one this worker runs under; the row records it.
func (s *Store) AcquireMaintenanceLease(ctx context.Context, policy string, now time.Time, ttl time.Duration) (MaintenanceLease, error) {
	var lease MaintenanceLease
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		st, err := readMaintenanceState(ctx, conn)
		if err != nil {
			return err
		}
		if st.Cursor >= st.Dirty {
			return ErrNothingPending
		}
		if st.LeaseID != "" && st.ExpiresAt.After(now) {
			return ErrMaintenanceLeaseHeld
		}
		id, err := newLeaseID()
		if err != nil {
			return err
		}
		lease = MaintenanceLease{LeaseID: id, Generation: st.Generation + 1, Cursor: st.Cursor, Dirty: st.Dirty, Policy: policy,
			ExpiresAt: now.Add(ttl).UTC()}
		_, err = conn.ExecContext(ctx, `UPDATE maintenance_state SET lease_id = ?, generation = ?, expires_at = ?, policy_version = ?,
			pending_state = 'running', updated_at = ? WHERE id = 1`, lease.LeaseID, lease.Generation, stamp(lease.ExpiresAt), policy, stamp(now))
		return err
	})
	return lease, err
}

// RenewMaintenanceLease extends the lease to now+ttl when it is still this worker's and unexpired.
func (s *Store) RenewMaintenanceLease(ctx context.Context, lease *MaintenanceLease, now time.Time, ttl time.Duration) error {
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if _, err := currentLease(ctx, conn, *lease, now); err != nil {
			return err
		}
		expires := now.Add(ttl).UTC()
		if _, err := conn.ExecContext(ctx, `UPDATE maintenance_state SET expires_at = ?, updated_at = ? WHERE id = 1`, stamp(expires), stamp(now)); err != nil {
			return err
		}
		lease.ExpiresAt = expires
		return nil
	})
}

// currentLease re-reads the row and refuses a lease that is not the current one: another lease id or
// generation, an expiry at or before now, a cursor that moved or a policy that changed.
func currentLease(ctx context.Context, q rowQuerier, lease MaintenanceLease, now time.Time) (MaintenanceState, error) {
	st, err := readMaintenanceState(ctx, q)
	if err != nil {
		return st, err
	}
	switch {
	case st.LeaseID != lease.LeaseID || st.Generation != lease.Generation:
		return st, fmt.Errorf("%w: generation %d holds it, this worker has %d", ErrStaleLease, st.Generation, lease.Generation)
	case !st.ExpiresAt.After(now):
		return st, fmt.Errorf("%w: it expired at %s", ErrStaleLease, st.ExpiresAt.Format(time.RFC3339Nano))
	case st.Cursor != lease.Cursor:
		return st, fmt.Errorf("%w: the cursor is %d, this worker read %d", ErrStaleLease, st.Cursor, lease.Cursor)
	case st.PolicyVersion != lease.Policy:
		return st, fmt.Errorf("%w: the policy is %s, this worker runs %s", ErrStaleLease, st.PolicyVersion, lease.Policy)
	}
	return st, nil
}

// MaintenanceSources returns the dirty sources with after < revision <= upTo, oldest first, at most limit.
func (s *Store) MaintenanceSources(ctx context.Context, after, upTo int64, limit int) ([]MaintenanceSource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT revision, kind, source_id, digest, scope, session_id, run_id, sensitivity, provenance, created_at
		FROM maintenance_sources WHERE revision > ? AND revision <= ? ORDER BY revision LIMIT ?`, after, upTo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaintenanceSource
	for rows.Next() {
		var src MaintenanceSource
		var created string
		if err := rows.Scan(&src.Revision, &src.Ref.Kind, &src.Ref.ID, &src.Ref.Digest, &src.Ref.Scope, &src.Ref.SessionID,
			&src.Ref.RunID, &src.Ref.Sensitivity, &src.Ref.Provenance, &created); err != nil {
			return nil, err
		}
		src.CreatedAt = parseStamp(created)
		out = append(out, src)
	}
	return out, rows.Err()
}

// ProcessingKeys resolves the dedupe key of each dirty source to the key its processing is recorded
// under now, and reports which of the resolved keys a standing pass already processed. A record stands
// unless the maintenance session that wrote it is fenced: a revocation or a privacy deletion of one
// source of its batch withdrew everything that session proposed, so the source version is processed
// once more, under the RedoKey of that record, which is itself kept as written. A skip is a decision
// about the source, not a reading of it, and stands.
func (s *Store) ProcessingKeys(ctx context.Context, keys []string) ([]string, map[string]bool, error) {
	resolved := make([]string, len(keys))
	processed := map[string]bool{}
	for i, k := range keys {
		key, stands, err := standingKey(ctx, s.db, k)
		if err != nil {
			return nil, nil, err
		}
		resolved[i] = key
		if stands {
			processed[key] = true
		}
	}
	return resolved, processed, nil
}

// standingKey follows the processing records of key through every fenced maintenance session: the key
// of the record that stands (stands true), or the key the next processing is recorded under.
func standingKey(ctx context.Context, q rowQuerier, key string) (string, bool, error) {
	for {
		var outcome, maintenanceID string
		err := q.QueryRowContext(ctx, `SELECT outcome, maintenance_session_id FROM maintenance_dedupe WHERE dedupe_key = ?`, key).Scan(&outcome, &maintenanceID)
		if errors.Is(err, sql.ErrNoRows) {
			return key, false, nil
		}
		if err != nil {
			return "", false, err
		}
		if outcome == OutcomeSkipped || maintenanceID == "" {
			return key, true, nil
		}
		if _, fenced, err := sessionFence(ctx, q, maintenanceID); err != nil {
			return "", false, err
		} else if !fenced {
			return key, true, nil
		}
		key = knowledge.RedoKey(key, maintenanceID)
	}
}

// requeueCoBatchedSources queues again, in the transaction of the fences of epoch, the sources those
// fences cost their cognition without reaching them. A fenced maintenance session withdraws everything
// it proposed, because its answer is not separable from the source it lost; every other source of its
// batch that is not fenced, still at the version the batch read and has no standing processing is left
// waiting on exactly one dirty row, so the next pass maintains it without the lost source. Nothing of
// the history is deleted or rewritten: the withdrawn candidates and the earlier processing records stay.
//
// It is idempotent: a version that already waits is not queued by another fence. With no lease out, the
// waiting row is read by the next pass under the fences of this transaction, and nothing is written.
// With a lease out, the pass in flight fixed its batch before these fences: it may have passed the
// waiting row over as processed and will delete it with its commit, so the version moves to a new row
// past that batch's frontier and its older rows, which nothing needs any more, are dropped.
func requeueCoBatchedSources(ctx context.Context, conn *sql.Conn, epoch int64, now time.Time) error {
	refs, err := selectStrings(ctx, conn, `SELECT d.source_ref FROM maintenance_dedupe d
		WHERE d.outcome = ? AND d.maintenance_session_id IN (SELECT scope_ref FROM source_fences WHERE scope_kind = 'session' AND epoch = ?)
		GROUP BY d.source_ref ORDER BY MIN(d.revision)`, OutcomeCandidates, epoch)
	if err != nil || len(refs) == 0 {
		return err
	}
	st, err := readMaintenanceState(ctx, conn)
	if err != nil {
		return err
	}
	const waitingRows = ` FROM maintenance_sources WHERE kind = ? AND source_id = ? AND digest = ? AND scope = ? AND revision > ?`
	for _, ref := range refs {
		kind, sourceID, digest, ok := knowledge.ParseRef(ref)
		if !ok {
			continue
		}
		if _, fenced, err := sessionFence(ctx, conn, sourceID); err != nil {
			return err
		} else if fenced {
			continue
		}
		if current, err := sourceVersion(ctx, conn, sourceID); errors.Is(err, ErrNotFound) || (err == nil && current != digest) {
			continue
		} else if err != nil {
			return err
		}
		key := knowledge.DedupeKey(knowledge.SourceRef{Kind: kind, ID: sourceID, Digest: digest}, SourceScopeProject)
		if _, stands, err := standingKey(ctx, conn, key); err != nil {
			return err
		} else if stands {
			continue
		}
		version := []any{kind, sourceID, digest, SourceScopeProject, st.Cursor}
		var waiting int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*)`+waitingRows, version...).Scan(&waiting); err != nil {
			return err
		}
		if waiting > 0 && st.LeaseID == "" {
			continue
		}
		if err := enqueueSourceChange(ctx, conn, sourceID, now); err != nil {
			return err
		}
		if waiting > 0 {
			if _, err := conn.ExecContext(ctx, `DELETE FROM maintenance_sources WHERE kind = ? AND source_id = ? AND digest = ? AND scope = ? AND revision > ?
				AND revision < (SELECT MAX(revision)`+waitingRows+`)`, append(version, version...)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// Dedupe outcomes.
const (
	OutcomeCandidates  = "candidates"
	OutcomeNoCandidate = "no_candidate"
	OutcomeSkipped     = "skipped"
)

// DedupeRecord is one processed source of a batch.
type DedupeRecord struct {
	Key       string
	SourceRef string
	Revision  int64
	Outcome   string
}

// CandidateRecord is one candidate as stored.
type CandidateRecord struct {
	ID                   string      `json:"id"`
	SchemaVersion        int         `json:"schemaVersion"`
	ProjectID            string      `json:"projectId"`
	MaintenanceSessionID string      `json:"maintenanceSessionId"`
	Generation           int64       `json:"generation"`
	Revision             int64       `json:"revision"`
	SourceRefs           []string    `json:"sourceRefs"`
	SourceDigest         string      `json:"sourceDigest"`
	SourceScope          string      `json:"sourceScope"`
	OriginRunID          string      `json:"originRunId,omitempty"`
	OriginSessionID      string      `json:"originSessionId,omitempty"`
	ClaimType            string      `json:"claimType"`
	Subject              string      `json:"subject"`
	Claim                string      `json:"claim"`
	Rationale            string      `json:"rationale"`
	ClaimDigest          string      `json:"claimDigest"`
	CitedPaths           []CitedPath `json:"citedPaths"`
	CheckIDs             []string    `json:"checkIds"`
	// Assertion is the structured proposition of a fact candidate; BindingDigest the host-approved
	// binding that assigned its check (empty when none did); SupportDigest its support identity
	// (knowledge.SupportDigest), which dedupe compares; PurgedAt when a privacy deletion removed its
	// text (zero otherwise).
	Assertion     *knowledge.Assertion `json:"assertion,omitempty"`
	BindingDigest string               `json:"bindingDigest,omitempty"`
	SupportDigest string               `json:"supportDigest"`
	PurgedAt      time.Time            `json:"purgedAt,omitzero"`
	PolicyVersion string               `json:"policyVersion"`
	Status        string               `json:"status"`
	StatusReason  string               `json:"statusReason,omitempty"`
	CreatedAt     time.Time            `json:"createdAt"`
}

// SubjectKey is what two candidates must share to conflict (knowledge.CandidateSubjectKey).
func (c CandidateRecord) SubjectKey() string {
	return knowledge.CandidateSubjectKey(c.Subject, c.Assertion)
}

// CitedPath is a workspace-relative path a candidate cites and the git blob it had when Core adopted
// the candidate.
type CitedPath struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
}

// ObservationRecord is one qualified observation as stored.
type ObservationRecord struct {
	ID            int64     `json:"id"`
	CandidateID   string    `json:"candidateId"`
	CheckID       string    `json:"checkId"`
	SessionID     string    `json:"sessionId"`
	ResultSeq     int64     `json:"resultSeq"`
	EpisodeID     string    `json:"episodeId"`
	Fingerprint   string    `json:"fingerprint"`
	SpecDigest    string    `json:"specDigest"`
	ApprovalEpoch int64     `json:"approvalEpoch"`
	Outcome       string    `json:"outcome"`
	Scope         string    `json:"scope"`
	Purpose       string    `json:"purpose"`
	RunID         string    `json:"runId,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// Observation purposes.
const (
	ObservationAdoption     = "adoption"
	ObservationRevalidation = "revalidation"
)

// ObservationLink names the eval result an observation is made from; every other field is read from
// the result row in the transaction that writes the observation.
type ObservationLink struct {
	CandidateID string
	SessionID   string
	ResultSeq   int64
	Scope       string
	Purpose     string
	RunID       string
}

// MaintenanceCommit is everything one batch commits: the source versions its maintainer read, its
// dedupe records, its candidates and their adoption observations, and the revision the cursor moves to.
type MaintenanceCommit struct {
	Lease                MaintenanceLease
	Through              int64
	MaintenanceSessionID string
	// Sources are the source versions handed to the maintainer: the commit's compare-and-set.
	Sources      []knowledge.SourceRef
	Dedupe       []DedupeRecord
	Candidates   []CandidateRecord
	Observations []ObservationLink
}

// CommitResult reports what a commit wrote.
type CommitResult struct {
	Inserted   []string
	Duplicates int
}

// testHookMaintenanceCommit, when set, runs inside the commit transaction after the candidates and
// before the cursor; an error it returns rolls the whole commit back (crash injection in tests).
var testHookMaintenanceCommit func() error

// CommitMaintenance writes one batch in one transaction, or nothing: it re-checks the lease (id,
// generation, expiry), the cursor and the policy, refuses a Through outside (cursor, dirty], refuses a
// batch whose sources are no longer the versions the maintainer read (sourcesUnchanged,
// ErrSourceChanged), inserts the dedupe records, the candidates (a non-rejected candidate whose support
// identity a live candidate already holds is a duplicate and is not inserted; a candidate of a fenced
// maintenance session is withdrawn, not live, and holds nothing) and their observations,
// deletes the processed source rows, moves the cursor to Through and releases the lease. ErrStaleLease
// and ErrSourceChanged write nothing and leave the dirty revision for the next worker.
func (s *Store) CommitMaintenance(ctx context.Context, c MaintenanceCommit, now time.Time) (CommitResult, error) {
	var result CommitResult
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		result = CommitResult{}
		st, err := currentLease(ctx, conn, c.Lease, now)
		if err != nil {
			return err
		}
		if c.Through <= st.Cursor || c.Through > st.Dirty {
			return fmt.Errorf("%w: the batch ends at revision %d outside (%d, %d]", ErrStaleLease, c.Through, st.Cursor, st.Dirty)
		}
		if err := sourcesUnchanged(ctx, conn, c.MaintenanceSessionID, c.Sources); err != nil {
			return err
		}
		for _, d := range c.Dedupe {
			if _, err := conn.ExecContext(ctx, `INSERT INTO maintenance_dedupe(dedupe_key, source_ref, revision, generation, outcome,
				maintenance_session_id, policy_version, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
				d.Key, d.SourceRef, d.Revision, c.Lease.Generation, d.Outcome, c.MaintenanceSessionID, c.Lease.Policy, stamp(now)); err != nil {
				return fmt.Errorf("record processed source %s: %w", d.SourceRef, err)
			}
		}
		inserted := map[string]bool{}
		for _, cand := range c.Candidates {
			if cand.Status == knowledge.StatusQualified {
				if err := qualifiedSupport(ctx, conn, cand, c.Observations); err != nil {
					return err
				}
			}
			cand.SupportDigest = knowledge.SupportDigest(cand.ClaimDigest, cand.SourceDigest, cand.SourceScope, c.Lease.Policy, cand.BindingDigest)
			if cand.Status != knowledge.StatusRejected {
				var one int
				err := conn.QueryRowContext(ctx, `SELECT 1 FROM cognition_candidates c WHERE c.support_digest = ? AND c.status != 'rejected'
					AND NOT EXISTS (SELECT 1 FROM source_fences f WHERE f.scope_kind = 'session' AND f.scope_ref = c.maintenance_session_id) LIMIT 1`,
					cand.SupportDigest).Scan(&one)
				if err == nil {
					result.Duplicates++
					continue
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			if err := insertCandidate(ctx, conn, cand, c, now); err != nil {
				return err
			}
			inserted[cand.ID] = true
			result.Inserted = append(result.Inserted, cand.ID)
		}
		for _, o := range c.Observations {
			if !inserted[o.CandidateID] {
				continue
			}
			if err := insertObservation(ctx, conn, o, now); err != nil {
				return err
			}
		}
		if testHookMaintenanceCommit != nil {
			if err := testHookMaintenanceCommit(); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM maintenance_sources WHERE revision <= ?`, c.Through); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE maintenance_state SET cursor_revision = ?, lease_id = '', expires_at = '',
			pending_state = 'idle', last_error = '', updated_at = ? WHERE id = 1`, c.Through, stamp(now))
		return err
	})
	if err != nil {
		return CommitResult{}, err
	}
	return result, nil
}

// qualifiedSupport rechecks authority at the commit's linearization point. A
// binding revoked after the model or the check ran must not produce an adopted
// qualified row, observation, dedupe record or cursor advance.
func qualifiedSupport(ctx context.Context, conn *sql.Conn, cand CandidateRecord, links []ObservationLink) error {
	if cand.Assertion == nil || cand.BindingDigest == "" || len(cand.CheckIDs) != 1 {
		return fmt.Errorf("%w: candidate %s has no exact support contract", ErrNotQualified, cand.ID)
	}
	for _, link := range links {
		if link.CandidateID != cand.ID || link.Purpose != ObservationAdoption {
			continue
		}
		r, err := qualifiedResult(ctx, conn, link.SessionID, link.ResultSeq)
		if err != nil {
			return err
		}
		if r.Outcome != "passed" || r.CheckID != cand.CheckIDs[0] {
			continue
		}
		var active int
		err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM check_approvals b
			WHERE b.project_id = ? AND b.kind = 'constraint_binding' AND b.state = 'active'
			AND b.binding_digest = ? AND b.constraint_ref = ? AND b.constraint_digest = ?
			AND b.check_id = ? AND b.spec_digest = ?)
			AND EXISTS(SELECT 1 FROM check_approvals s WHERE s.project_id = ? AND s.kind = 'check_spec'
			AND s.state = 'active' AND s.check_id = ? AND s.spec_digest = ? AND s.approval_epoch = ?)`,
			cand.ProjectID, cand.BindingDigest, cand.Assertion.Ref(), cand.Assertion.ValueDigest(), r.CheckID, r.SpecDigest,
			cand.ProjectID, r.CheckID, r.SpecDigest, r.ApprovalEpoch).Scan(&active)
		if err != nil {
			return err
		}
		if active == 1 {
			return nil
		}
	}
	return fmt.Errorf("%w: candidate %s lost its active passing support", ErrNotQualified, cand.ID)
}

// sourcesUnchanged is the compare-and-set of a commit, in its transaction: the maintenance session is
// not fenced, and every source the maintainer read still exists at the version its ref names and is not
// fenced. A source that changed (a new run, a correction), was deleted or revoked while its batch was
// processed refuses the whole batch with ErrSourceChanged: what the maintainer said was said about a
// version that no longer holds.
func sourcesUnchanged(ctx context.Context, conn *sql.Conn, maintenanceID string, refs []knowledge.SourceRef) error {
	if maintenanceID != "" {
		if _, fenced, err := sessionFence(ctx, conn, maintenanceID); err != nil {
			return err
		} else if fenced {
			return fmt.Errorf("%w: the maintenance session %s is fenced", ErrSourceChanged, maintenanceID)
		}
	}
	for _, r := range refs {
		digest, err := sourceVersion(ctx, conn, r.SessionID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: source %s no longer exists", ErrSourceChanged, r.SessionID)
		}
		if err != nil {
			return err
		}
		if digest != r.Digest {
			return fmt.Errorf("%w: source %s is at %s, the batch read %s", ErrSourceChanged, r.SessionID, digest, r.Digest)
		}
		if _, fenced, err := sessionFence(ctx, conn, r.SessionID); err != nil {
			return err
		} else if fenced {
			return fmt.Errorf("%w: source %s is fenced", ErrSourceChanged, r.SessionID)
		}
	}
	return nil
}

// FailMaintenance releases a lease whose batch could not be committed and records why. The cursor and
// the dirty revision stay: the sources are processed by a later batch. A lease that is no longer
// current is left to its new holder (ErrStaleLease).
func (s *Store) FailMaintenance(ctx context.Context, lease MaintenanceLease, reason string, now time.Time) error {
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		st, err := readMaintenanceState(ctx, conn)
		if err != nil {
			return err
		}
		if st.LeaseID != lease.LeaseID || st.Generation != lease.Generation {
			return fmt.Errorf("%w: generation %d holds it", ErrStaleLease, st.Generation)
		}
		_, err = conn.ExecContext(ctx, `UPDATE maintenance_state SET lease_id = '', expires_at = '', pending_state = 'failed',
			last_error = ?, updated_at = ? WHERE id = 1`, clipText(reason, 512), stamp(now))
		return err
	})
}

func insertCandidate(ctx context.Context, conn *sql.Conn, cand CandidateRecord, c MaintenanceCommit, now time.Time) error {
	refs, err := json.Marshal(nonNil(cand.SourceRefs))
	if err != nil {
		return err
	}
	cited := cand.CitedPaths
	if cited == nil {
		cited = []CitedPath{}
	}
	citedJSON, err := json.Marshal(cited)
	if err != nil {
		return err
	}
	checks, err := json.Marshal(nonNil(cand.CheckIDs))
	if err != nil {
		return err
	}
	assertion := ""
	if cand.Assertion != nil {
		raw, err := json.Marshal(cand.Assertion)
		if err != nil {
			return err
		}
		assertion = string(raw)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO cognition_candidates(candidate_id, schema_version, project_id, maintenance_session_id,
		generation, revision, source_refs_json, source_digest, source_scope, origin_run_id, origin_session_id, claim_type, subject,
		subject_key, proposed_claim, rationale, claim_digest, cited_paths_json, check_ids_json, policy_version, status, status_reason, created_at,
		assertion_json, binding_digest, support_digest)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cand.ID, knowledge.SchemaVersion, cand.ProjectID, c.MaintenanceSessionID, c.Lease.Generation, c.Through, string(refs),
		cand.SourceDigest, cand.SourceScope, cand.OriginRunID, cand.OriginSessionID, cand.ClaimType, cand.Subject,
		cand.SubjectKey(), clipText(cand.Claim, 2048), clipText(cand.Rationale, 1024), cand.ClaimDigest,
		string(citedJSON), string(checks), c.Lease.Policy, cand.Status, clipText(cand.StatusReason, 512), stamp(now),
		assertion, cand.BindingDigest, cand.SupportDigest)
	if err != nil {
		return fmt.Errorf("insert candidate %s: %w", cand.ID, err)
	}
	return nil
}

// QualifiedResult is an eval result that qualifies as an observation.
type QualifiedResult struct {
	CheckID       string
	Outcome       string
	Fingerprint   string
	SpecDigest    string
	ApprovalEpoch int64
	EpisodeID     string
}

// QualifiedResult returns the eval result sessionID/seq when it qualifies as an observation, else
// ErrNotQualified: see qualifiedResult.
func (s *Store) QualifiedResult(ctx context.Context, sessionID string, seq int64) (QualifiedResult, error) {
	return qualifiedResult(ctx, s.db, sessionID, seq)
}

// qualifiedEvalSQL is the one qualification rule for both candidate observations
// and freshness reads of unlinked runtime results. Open, interrupted, unapproved,
// moved-scope, error and model/human records are not observations of check truth.
const qualifiedEvalSQL = `r.provenance = 'runtime' AND r.approval_epoch > 0 AND r.episode_status = 'closed'
	AND r.pre_fingerprint != '' AND r.pre_fingerprint = r.post_fingerprint AND r.fingerprint = r.post_fingerprint
	AND r.status IN ('passed', 'failed')`

func qualifiedResult(ctx context.Context, q rowQuerier, sessionID string, seq int64) (QualifiedResult, error) {
	var r QualifiedResult
	err := q.QueryRowContext(ctx, `SELECT r.check_id, r.status, r.fingerprint, COALESCE(r.spec_digest, ''), r.approval_epoch,
		COALESCE(r.episode_id, '') FROM eval_results r WHERE r.session_id = ? AND r.seq = ? AND `+qualifiedEvalSQL,
		sessionID, seq).Scan(&r.CheckID, &r.Outcome, &r.Fingerprint, &r.SpecDigest, &r.ApprovalEpoch, &r.EpisodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return QualifiedResult{}, fmt.Errorf("%w: eval result %s/%d", ErrNotQualified, sessionID, seq)
	}
	return r, err
}

// CognitionResult is a qualified result considered at one freshness read. It does
// not create an observation or claim run ownership. Superseded means a later
// appended, now-qualified failure at the same check state invalidates this PASS.
type CognitionResult struct {
	SessionID  string
	Seq        int64
	Outcome    string
	At         time.Time
	Superseded bool
}

// CognitionResults reads at most two results: the session's latest qualified
// result and the latest result linked to this candidate in this scope. Every
// qualified failure, linked or not, participates in supersession. One SQL snapshot
// binds selection and supersession to the same committed history; a concurrent
// episode close is either visible to the whole read or to its next invocation.
//
// rowid orders result appends across sessions (Seq is session-local). It is only
// compared within this statement, never returned or persisted as an identity;
// stable result references remain session_id/seq, including across VACUUM.
// The database is bound to one workspace; fingerprints distinguish its worktrees.
func (s *Store) CognitionResults(ctx context.Context, candidateID, scope, sessionID, checkID, fingerprint, specDigest string, approvalEpoch int64) ([]CognitionResult, error) {
	rows, err := s.db.QueryContext(ctx, `WITH matching AS NOT MATERIALIZED (
		SELECT r.rowid AS result_order, r.session_id, r.seq, r.status, r.at, r.cognition_epoch FROM eval_results r
		WHERE r.check_id = ? AND r.fingerprint = ? AND r.spec_digest = ? AND r.approval_epoch = ? AND `+qualifiedEvalSQL+`
	), picked AS (
		SELECT (SELECT result_order FROM matching WHERE session_id = ? ORDER BY result_order DESC LIMIT 1) AS own_result,
			(SELECT m.result_order FROM matching m WHERE EXISTS (
				SELECT 1 FROM cognition_observations o WHERE o.candidate_id = ? AND o.scope = ?
					AND o.session_id = m.session_id AND o.result_seq = m.seq
			) ORDER BY m.result_order DESC LIMIT 1) AS linked_result,
			(SELECT result_order FROM matching WHERE status = 'failed' ORDER BY result_order DESC LIMIT 1) AS failure
	)
	SELECT m.session_id, m.seq, m.status, m.at, m.status = 'passed' AND (m.result_order < COALESCE(p.failure, 0)
		OR m.cognition_epoch < (SELECT COALESCE(MAX(epoch), 0) FROM source_fences WHERE scope_kind = 'privacy'))
	FROM picked p JOIN matching m ON m.result_order = p.own_result OR m.result_order = p.linked_result
	ORDER BY m.result_order DESC`, checkID, fingerprint, specDigest, approvalEpoch, sessionID, candidateID, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CognitionResult, 0, 2)
	for rows.Next() {
		var r CognitionResult
		var at string
		if err := rows.Scan(&r.SessionID, &r.Seq, &r.Outcome, &at, &r.Superseded); err != nil {
			return nil, err
		}
		if r.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// insertObservation derives the observation from the eval result row in this transaction
// (qualifiedResult), for a live candidate that names the result's check.
func insertObservation(ctx context.Context, conn *sql.Conn, o ObservationLink, now time.Time) error {
	r, err := qualifiedResult(ctx, conn, o.SessionID, o.ResultSeq)
	if err != nil {
		return err
	}
	var known int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM cognition_candidates WHERE candidate_id = ? AND status != 'rejected'
		AND EXISTS (SELECT 1 FROM json_each(check_ids_json) WHERE value = ?)`, o.CandidateID, r.CheckID).Scan(&known); err != nil {
		return err
	}
	if known == 0 {
		return fmt.Errorf("%w: candidate %s does not name check %s", ErrNotQualified, o.CandidateID, r.CheckID)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO cognition_observations(candidate_id, check_id, session_id, result_seq, episode_id,
		fingerprint, spec_digest, approval_epoch, outcome, scope, purpose, run_id, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.CandidateID, r.CheckID, o.SessionID, o.ResultSeq, r.EpisodeID, r.Fingerprint, r.SpecDigest, r.ApprovalEpoch, r.Outcome, o.Scope,
		o.Purpose, o.RunID, stamp(now))
	return err
}

// AppendObservation records a revalidation: an observation of a candidate made from a qualified eval
// result. It never updates an earlier observation.
func (s *Store) AppendObservation(ctx context.Context, o ObservationLink, now time.Time) error {
	return s.writeTx(ctx, func(conn *sql.Conn) error { return insertObservation(ctx, conn, o, now) })
}

// CandidateFilter selects candidates; zero values select everything up to the limit. SubjectKey
// selects the candidates stored under one subject key (CandidateRecord.SubjectKey).
type CandidateFilter struct {
	Statuses   []string
	SubjectKey string
	Limit      int
}

const candidateColumns = `candidate_id, schema_version, project_id, maintenance_session_id, generation, revision, source_refs_json,
	source_digest, source_scope, origin_run_id, origin_session_id, claim_type, subject, proposed_claim, rationale, claim_digest,
	cited_paths_json, check_ids_json, policy_version, status, status_reason, created_at, assertion_json, binding_digest, support_digest,
	purged_at`

// Candidates returns candidates newest first.
func (s *Store) Candidates(ctx context.Context, f CandidateFilter) ([]CandidateRecord, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	query := `SELECT ` + candidateColumns + ` FROM cognition_candidates WHERE 1`
	var args []any
	if len(f.Statuses) > 0 {
		query += ` AND status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.Statuses)), ",") + `)`
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.SubjectKey != "" {
		query += ` AND subject_key = ?`
		args = append(args, f.SubjectKey)
	}
	query += ` ORDER BY created_at DESC, candidate_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CandidateRecord{}
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ErrCandidateNotFound reports a candidate id the database does not hold.
var ErrCandidateNotFound = errors.New("candidate not found")

// Candidate returns one candidate, ErrCandidateNotFound when there is none.
func (s *Store) Candidate(ctx context.Context, id string) (CandidateRecord, error) {
	c, err := scanCandidate(s.db.QueryRowContext(ctx, `SELECT `+candidateColumns+` FROM cognition_candidates WHERE candidate_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CandidateRecord{}, ErrCandidateNotFound
	}
	return c, err
}

func scanCandidate(sc scanner) (CandidateRecord, error) {
	var c CandidateRecord
	var refs, cited, checks, created, assertion, purged string
	if err := sc.Scan(&c.ID, &c.SchemaVersion, &c.ProjectID, &c.MaintenanceSessionID, &c.Generation, &c.Revision, &refs,
		&c.SourceDigest, &c.SourceScope, &c.OriginRunID, &c.OriginSessionID, &c.ClaimType, &c.Subject, &c.Claim, &c.Rationale,
		&c.ClaimDigest, &cited, &checks, &c.PolicyVersion, &c.Status, &c.StatusReason, &created, &assertion, &c.BindingDigest,
		&c.SupportDigest, &purged); err != nil {
		return CandidateRecord{}, err
	}
	if err := json.Unmarshal([]byte(refs), &c.SourceRefs); err != nil {
		return CandidateRecord{}, err
	}
	if err := json.Unmarshal([]byte(cited), &c.CitedPaths); err != nil {
		return CandidateRecord{}, err
	}
	if err := json.Unmarshal([]byte(checks), &c.CheckIDs); err != nil {
		return CandidateRecord{}, err
	}
	if assertion != "" {
		c.Assertion = &knowledge.Assertion{}
		if err := json.Unmarshal([]byte(assertion), c.Assertion); err != nil {
			return CandidateRecord{}, err
		}
	}
	c.CreatedAt, c.PurgedAt = parseStamp(created), parseStamp(purged)
	return c, nil
}

// Observations returns a candidate's observations, newest first.
func (s *Store) Observations(ctx context.Context, candidateID string) ([]ObservationRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, candidate_id, check_id, session_id, result_seq, episode_id, fingerprint, spec_digest,
		approval_epoch, outcome, scope, purpose, run_id, created_at FROM cognition_observations WHERE candidate_id = ? ORDER BY id DESC`, candidateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ObservationRecord{}
	for rows.Next() {
		var o ObservationRecord
		var created string
		if err := rows.Scan(&o.ID, &o.CandidateID, &o.CheckID, &o.SessionID, &o.ResultSeq, &o.EpisodeID, &o.Fingerprint, &o.SpecDigest,
			&o.ApprovalEpoch, &o.Outcome, &o.Scope, &o.Purpose, &o.RunID, &created); err != nil {
			return nil, err
		}
		o.CreatedAt = parseStamp(created)
		out = append(out, o)
	}
	return out, rows.Err()
}

// SessionPurpose returns the core-owned purpose of a session, "" for an ordinary one.
func (s *Store) SessionPurpose(ctx context.Context, sessionID string) (string, error) {
	var purpose string
	err := s.db.QueryRowContext(ctx, `SELECT CASE WHEN json_valid(metadata_json) THEN COALESCE(json_extract(metadata_json, '$.purpose'), '') ELSE '' END
		FROM sessions WHERE id = ?`,
		sessionID).Scan(&purpose)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return purpose, err
}

// enqueueSourceChange records the version of an eligible terminal source in the
// transaction that changes it. Post-run delivery and host writes must not leave
// only an obsolete dirty row that fixBatch would pass over.
func enqueueSourceChange(ctx context.Context, conn querier, sessionID string, now time.Time) error {
	var on int
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM maintenance_state WHERE id = 1)
		AND EXISTS(SELECT 1 FROM sessions WHERE id = ? AND status IN ('completed','failed'))`, sessionID).Scan(&on); err != nil || on == 0 {
		return err
	}
	var (
		purpose, runID string
		child, privacy int
	)
	err := conn.QueryRowContext(ctx, `SELECT CASE WHEN json_valid(s.metadata_json) THEN COALESCE(json_extract(s.metadata_json, '$.purpose'), '') ELSE '' END,
		EXISTS(SELECT 1 FROM session_relations r WHERE r.child_session_id = s.id),
		EXISTS(SELECT 1 FROM source_fences f WHERE f.scope_kind = 'privacy' AND f.scope_ref = s.id),
		`+sourceRunSQL+`
		FROM sessions s WHERE s.id = ?`, sessionID).Scan(&purpose, &child, &privacy, &runID)
	if err != nil {
		return err
	}
	if purpose == PurposeMaintenance || child == 1 {
		return nil
	}
	sensitivity := knowledge.SensitivitySession
	if privacy == 1 {
		sensitivity = knowledge.SensitivityPrivate
	}
	digest, err := sourceVersion(ctx, conn, sessionID)
	if err != nil {
		return err
	}
	var revision int64
	if err := conn.QueryRowContext(ctx, `UPDATE maintenance_state SET dirty_revision = dirty_revision + 1, updated_at = ? WHERE id = 1
		RETURNING dirty_revision`, stamp(now)).Scan(&revision); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO maintenance_sources(revision, kind, source_id, digest, scope, session_id, run_id,
		sensitivity, provenance, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, revision, knowledge.KindSession, sessionID, digest,
		SourceScopeProject, sessionID, runID, sensitivity, knowledge.ProvenanceRuntime, stamp(now))
	return err
}

// SourceVersion returns the version digest of a session source as it is now (sourceVersion),
// ErrNotFound when the session is gone.
func (s *Store) SourceVersion(ctx context.Context, sessionID string) (string, error) {
	return sourceVersion(ctx, s.db, sessionID)
}

// sourceVersion is the one definition of a session source's version: a digest of its status, newest
// message, newest Task State version and final answer. Every party compares this digest: the dirty
// row a terminal run writes, the batch that hands the source to a maintainer, the commit that re-checks
// it, and every reader of a candidate derived from it. A new run, a correction or a new answer in the
// session changes it; the version a candidate cites then no longer holds. Message seqs start at 0, so
// a session without messages is -1, not the seq of its first message.
func sourceVersion(ctx context.Context, q rowQuerier, sessionID string) (string, error) {
	var (
		status, final    string
		lastSeq, version int64
	)
	err := q.QueryRowContext(ctx, `SELECT s.status, COALESCE(s.final, ''),
		(SELECT COALESCE(MAX(seq), -1) FROM session_messages m WHERE m.session_id = s.id),
		(SELECT COALESCE(MAX(version), 0) FROM task_states t WHERE t.session_id = s.id)
		FROM sessions s WHERE s.id = ?`, sessionID).Scan(&status, &final, &lastSeq, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return knowledge.Digest(knowledge.KindSession, sessionID, status, fmt.Sprint(lastSeq), fmt.Sprint(version), final), nil
}

// SourceScopeProject is the scope of a session source: the project this database belongs to.
const SourceScopeProject = "project"

func newLeaseID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ml_" + hex.EncodeToString(b[:]), nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseStamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func clipText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
