package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// Approval kinds, approvers and states of check_approvals (ADR-0017). An approval is host state: only
// a host's explicit user decision, a team decision it adopted, or a frozen experiment contract writes
// one; no tool reaches these methods.
const (
	ApprovalCheckSet   = "check_set"
	ApprovalCheckSpec  = "check_spec"
	ApprovalBinding    = "constraint_binding"
	ApprovalActive     = "active"
	ApprovalRevoked    = "revoked"
	ApprovedByUser     = "user"
	ApprovedByTrust    = "trust-flow"
	ApprovedByTeam     = "team-decision"
	ApprovedByFreeze   = "experiment-freeze"
	maxApprovalDigest  = 128
	maxApprovalRef     = 512
	maxApprovalActor   = 128
	maxApprovalCheckID = 128
)

// ErrApprovalInvalid reports an approval request the table cannot hold or the rules refuse.
var ErrApprovalInvalid = errors.New("invalid approval")

// ApprovedSpec is one check definition of an approved set: its id and spec digest.
type ApprovedSpec struct {
	CheckID    string `json:"checkId"`
	SpecDigest string `json:"specDigest"`
}

// CheckSetApproval is one approval of a whole effective check set: its digest and every definition in
// it (none for the empty set).
type CheckSetApproval struct {
	ProjectID   string
	SetDigest   string
	Specs       []ApprovedSpec
	ApprovedBy  string // user, trust-flow or experiment-freeze
	Actor       string
	DecisionRef string
}

// BindingApproval is one approval of a ConstraintBinding: the constraint (ref and digest) the check
// definition (id and spec digest) may satisfy, identified by BindingDigest, which the caller computed
// over all of them and the binding's scope.
type BindingApproval struct {
	ProjectID        string
	ConstraintRef    string
	ConstraintDigest string
	CheckID          string
	SpecDigest       string
	BindingDigest    string
	ApprovedBy       string // user, team-decision or experiment-freeze
	Actor            string
	DecisionRef      string
}

// ApprovalRecord is one row of check_approvals.
type ApprovalRecord struct {
	ID               int64     `json:"id"`
	Kind             string    `json:"kind"`
	CheckID          string    `json:"checkId,omitempty"`
	SpecDigest       string    `json:"specDigest,omitempty"`
	SetDigest        string    `json:"setDigest,omitempty"`
	ConstraintRef    string    `json:"constraintRef,omitempty"`
	ConstraintDigest string    `json:"constraintDigest,omitempty"`
	BindingDigest    string    `json:"bindingDigest,omitempty"`
	Epoch            int64     `json:"epoch"`
	ApprovedBy       string    `json:"approvedBy"`
	Actor            string    `json:"actor"`
	DecisionRef      string    `json:"decisionRef,omitempty"`
	State            string    `json:"state"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

func bounded(name, v string, max int, required bool) error {
	if v == "" && !required {
		return nil
	}
	if v == "" || len(v) > max || strings.ContainsAny(v, "\r\n\x00") {
		return fmt.Errorf("%w: %s must be 1-%d bytes on one line", ErrApprovalInvalid, name, max)
	}
	return nil
}

func validApprover(by string, allowed ...string) error {
	for _, a := range allowed {
		if by == a {
			return nil
		}
	}
	return fmt.Errorf("%w: approved by %q (want one of %s)", ErrApprovalInvalid, by, strings.Join(allowed, ", "))
}

func (a CheckSetApproval) validate() error {
	if err := validApprover(a.ApprovedBy, ApprovedByUser, ApprovedByTrust, ApprovedByFreeze); err != nil {
		return err
	}
	for _, f := range []struct {
		name, v string
		max     int
		req     bool
	}{
		{"project", a.ProjectID, 256, true}, {"set digest", a.SetDigest, maxApprovalDigest, true},
		{"actor", a.Actor, maxApprovalActor, true}, {"decision ref", a.DecisionRef, maxApprovalRef, false},
	} {
		if err := bounded(f.name, f.v, f.max, f.req); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(a.Specs))
	for _, s := range a.Specs {
		if err := bounded("check id", s.CheckID, maxApprovalCheckID, true); err != nil {
			return err
		}
		if err := bounded("spec digest", s.SpecDigest, maxApprovalDigest, true); err != nil {
			return err
		}
		if seen[s.CheckID] {
			return fmt.Errorf("%w: check %q appears twice", ErrApprovalInvalid, s.CheckID)
		}
		seen[s.CheckID] = true
	}
	return nil
}

func (b BindingApproval) validate() error {
	if err := validApprover(b.ApprovedBy, ApprovedByUser, ApprovedByTeam, ApprovedByFreeze); err != nil {
		return err
	}
	for _, f := range []struct {
		name, v string
		max     int
		req     bool
	}{
		{"project", b.ProjectID, 256, true}, {"constraint ref", b.ConstraintRef, maxApprovalRef, true},
		{"constraint digest", b.ConstraintDigest, maxApprovalDigest, true}, {"check id", b.CheckID, maxApprovalCheckID, true},
		{"spec digest", b.SpecDigest, maxApprovalDigest, true}, {"binding digest", b.BindingDigest, maxApprovalDigest, true},
		{"actor", b.Actor, maxApprovalActor, true}, {"decision ref", b.DecisionRef, maxApprovalRef, false},
	} {
		if err := bounded(f.name, f.v, f.max, f.req); err != nil {
			return err
		}
	}
	return nil
}

// nextApprovalEpoch is the project's next approval epoch: one above every epoch it ever had, revoked
// ones included, so an epoch is never reused and a restored definition never revives an old approval.
func nextApprovalEpoch(ctx context.Context, q querier, projectID string) (int64, error) {
	var epoch int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(approval_epoch), 0) + 1 FROM check_approvals WHERE project_id = ?`, projectID).Scan(&epoch)
	return epoch, err
}

// ApproveCheckSet records the approval of a whole check set under a new epoch, in one transaction: the
// project's active set and spec approvals are revoked, and the set row and one row per definition are
// written active. It returns the epoch. The caller compares the digest with the current configuration
// before calling; this method reads no file.
func (s *Store) ApproveCheckSet(ctx context.Context, a CheckSetApproval) (int64, error) {
	if err := a.validate(); err != nil {
		return 0, err
	}
	var epoch int64
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		var err error
		if epoch, err = nextApprovalEpoch(ctx, conn, a.ProjectID); err != nil {
			return err
		}
		at := now()
		if _, err := revokeChecks(ctx, conn, a.ProjectID, at); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO check_approvals(project_id, kind, check_set_digest, approval_epoch,
			approved_by, actor, decision_ref, state, created_at, updated_at) VALUES(?, 'check_set', ?, ?, ?, ?, ?, 'active', ?, ?)`,
			a.ProjectID, a.SetDigest, epoch, a.ApprovedBy, a.Actor, nullString(a.DecisionRef), at, at); err != nil {
			return err
		}
		for _, spec := range a.Specs {
			if _, err := conn.ExecContext(ctx, `INSERT INTO check_approvals(project_id, kind, check_id, spec_digest, check_set_digest,
				approval_epoch, approved_by, actor, decision_ref, state, created_at, updated_at)
				VALUES(?, 'check_spec', ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
				a.ProjectID, spec.CheckID, spec.SpecDigest, a.SetDigest, epoch, a.ApprovedBy, a.Actor, nullString(a.DecisionRef), at, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

func revokeChecks(ctx context.Context, q querier, projectID, at string) (int64, error) {
	res, err := q.ExecContext(ctx, `UPDATE check_approvals SET state = 'revoked', updated_at = ?
		WHERE project_id = ? AND kind IN ('check_set', 'check_spec') AND state = 'active'`, at, projectID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevokeCheckApprovals revokes the project's active check set and its definitions. It returns the
// number of rows revoked; nothing active is not an error.
func (s *Store) RevokeCheckApprovals(ctx context.Context, projectID string) (int64, error) {
	var n int64
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		var err error
		n, err = revokeChecks(ctx, conn, projectID, now())
		return err
	})
	return n, err
}

// LatestCheckSetApproval returns the project's newest check set approval, active or revoked, and the
// definitions approved with it, or nil when the project never had one.
func (s *Store) LatestCheckSetApproval(ctx context.Context, projectID string) (*ApprovalRecord, []ApprovalRecord, error) {
	set, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM check_approvals
		WHERE project_id = ? AND kind = 'check_set' ORDER BY approval_epoch DESC, id DESC LIMIT 1`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	specs, err := s.approvals(ctx, `SELECT `+approvalColumns+` FROM check_approvals
		WHERE project_id = ? AND kind = 'check_spec' AND approval_epoch = ? ORDER BY check_id`, projectID, set.Epoch)
	if err != nil {
		return nil, nil, err
	}
	return &set, specs, nil
}

// ApproveConstraintBinding records a binding under a new epoch, superseding (revoking) the project's
// active binding of the same constraint to the same check. It returns the epoch.
func (s *Store) ApproveConstraintBinding(ctx context.Context, b BindingApproval) (int64, error) {
	if err := b.validate(); err != nil {
		return 0, err
	}
	var epoch int64
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		var err error
		if epoch, err = nextApprovalEpoch(ctx, conn, b.ProjectID); err != nil {
			return err
		}
		at := now()
		if _, err := conn.ExecContext(ctx, `UPDATE check_approvals SET state = 'revoked', updated_at = ?
			WHERE project_id = ? AND kind = 'constraint_binding' AND state = 'active' AND constraint_ref = ? AND check_id = ?`,
			at, b.ProjectID, b.ConstraintRef, b.CheckID); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO check_approvals(project_id, kind, check_id, spec_digest, constraint_ref,
			constraint_digest, binding_digest, approval_epoch, approved_by, actor, decision_ref, state, created_at, updated_at)
			VALUES(?, 'constraint_binding', ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
			b.ProjectID, b.CheckID, b.SpecDigest, b.ConstraintRef, b.ConstraintDigest, b.BindingDigest, epoch,
			b.ApprovedBy, b.Actor, nullString(b.DecisionRef), at, at)
		return err
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// RevokeConstraintBinding revokes the project's active binding with the digest. It returns the number
// of rows revoked.
func (s *Store) RevokeConstraintBinding(ctx context.Context, projectID, bindingDigest string) (int64, error) {
	var n int64
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE check_approvals SET state = 'revoked', updated_at = ?
			WHERE project_id = ? AND kind = 'constraint_binding' AND state = 'active' AND binding_digest = ?`, now(), projectID, bindingDigest)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// ActiveConstraintBindings returns the project's active bindings, oldest first.
func (s *Store) ActiveConstraintBindings(ctx context.Context, projectID string) ([]ApprovalRecord, error) {
	return s.approvals(ctx, `SELECT `+approvalColumns+` FROM check_approvals
		WHERE project_id = ? AND kind = 'constraint_binding' AND state = 'active' ORDER BY approval_epoch, id`, projectID)
}

const approvalColumns = `id, kind, COALESCE(check_id, ''), COALESCE(spec_digest, ''), COALESCE(check_set_digest, ''),
	COALESCE(constraint_ref, ''), COALESCE(constraint_digest, ''), COALESCE(binding_digest, ''), approval_epoch, approved_by,
	actor, COALESCE(decision_ref, ''), state, created_at, updated_at`

func scanApproval(r interface{ Scan(...any) error }) (ApprovalRecord, error) {
	var (
		a                ApprovalRecord
		created, updated string
	)
	if err := r.Scan(&a.ID, &a.Kind, &a.CheckID, &a.SpecDigest, &a.SetDigest, &a.ConstraintRef, &a.ConstraintDigest,
		&a.BindingDigest, &a.Epoch, &a.ApprovedBy, &a.Actor, &a.DecisionRef, &a.State, &created, &updated); err != nil {
		return ApprovalRecord{}, err
	}
	a.CreatedAt, a.UpdatedAt = parseTime(created), parseTime(updated)
	return a, nil
}

func (s *Store) approvals(ctx context.Context, query string, args ...any) ([]ApprovalRecord, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApprovalRecord{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CheckDefinitions are the workspace-relative paths of the files that define a project's checks.
// WatchCheckDefinitions names them for the project this store serves (the runtime does, once, right
// after Open and before any run): a committed tool result that changed one of them, in any session or
// worktree of the project, revokes the project's check approvals in its own transaction (ADR-0017). A
// store nobody told watches nothing.
func (s *Store) WatchCheckDefinitions(projectID string, paths ...string) {
	s.checkProject = projectID
	s.checkDefinitions = make([]string, 0, len(paths))
	for _, p := range paths {
		s.checkDefinitions = append(s.checkDefinitions, path.Clean(p))
	}
}

// definitionWritten reports whether a derived file edge ("<path>@<blob>") names a check definition
// file. The comparison ignores case: on a case-insensitive file system another spelling is the same
// file, and an extra revocation only costs a re-approval.
func (s *Store) definitionWritten(e DerivedEdge) (string, bool) {
	if e.Kind != "file" || len(s.checkDefinitions) == 0 {
		return "", false
	}
	at := strings.LastIndex(e.TargetRef, "@")
	if at <= 0 {
		return "", false
	}
	p := path.Clean(e.TargetRef[:at])
	for _, d := range s.checkDefinitions {
		if strings.EqualFold(p, d) {
			return p, true
		}
	}
	return "", false
}

func (s *Store) revokeOnDefinitionWrite(ctx context.Context, q querier, edges []DerivedEdge) error {
	for _, e := range edges {
		if _, ok := s.definitionWritten(e); ok {
			_, err := revokeChecks(ctx, q, s.checkProject, now())
			return err
		}
	}
	return nil
}

// DefinitionWrite is the newest recorded write of a check definition file by a run.
type DefinitionWrite struct {
	SessionID   string    `json:"sessionId"`
	RunID       string    `json:"runId,omitempty"`
	ExecutionID string    `json:"executionId,omitempty"`
	Path        string    `json:"path"`
	At          time.Time `json:"at"`
}

// LastDefinitionWrite returns the newest derived file edge, at or after since, that names a watched
// check definition file by the rule revocation uses, or nil. It reads only; a host shows it as where a
// change came from. The GLOB on each file name narrows the scan; definitionWritten decides.
func (s *Store) LastDefinitionWrite(ctx context.Context, since time.Time) (*DefinitionWrite, error) {
	if len(s.checkDefinitions) == 0 {
		return nil, nil
	}
	var (
		where []string
		args  []any
	)
	for _, d := range s.checkDefinitions {
		where = append(where, `lower(target_ref) GLOB ?`)
		args = append(args, "*"+strings.ToLower(globEscape(path.Base(d)))+"@*")
	}
	// Rows are newest first by id (insertion order); created_at strings of varying precision do not
	// compare as text, so the bound is checked on parsed times.
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, run_id, execution_id, target_ref, created_at FROM derived_origins
		WHERE kind = 'file' AND (`+strings.Join(where, " OR ")+`) ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			w                DefinitionWrite
			ref, at          string
			runID, execution sql.NullString
		)
		if err := rows.Scan(&w.SessionID, &runID, &execution, &ref, &at); err != nil {
			return nil, err
		}
		w.RunID, w.ExecutionID, w.At = runID.String, execution.String, parseTime(at)
		if w.At.Before(since) {
			return nil, nil
		}
		if _, ok := s.definitionWritten(DerivedEdge{Kind: "file", TargetRef: ref}); !ok {
			continue
		}
		w.Path = ref[:strings.LastIndex(ref, "@")]
		return &w, nil
	}
	return nil, rows.Err()
}

// globEscape quotes GLOB's special characters in a literal path.
func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '*', '?', '[':
			b.WriteByte('[')
			b.WriteRune(r)
			b.WriteByte(']')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
