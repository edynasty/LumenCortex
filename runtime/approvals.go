package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/session"
)

// ErrSpecUnapproved reports a check whose current definition the host has not approved (ADR-0017): it
// is not run by the completion gate or a work unit, cannot be accepted and satisfies nothing.
var ErrSpecUnapproved = evaluator.ErrSpecUnapproved

// ErrApprovalStale refuses an approval whose digests are not those of the current configuration: the
// user confirmed something other than what would be approved.
var ErrApprovalStale = errors.New("approval does not match the current check definitions")

// ErrApprovalInvalid refuses an approval request with a missing, oversized or unknown field.
var ErrApprovalInvalid = session.ErrApprovalInvalid

// Approvers of a check set or a constraint binding (ADR-0017). A check set is approved by the user, by
// a trust flow that showed the user the whole set, or by a frozen experiment contract; a binding by
// the user, an adopted team decision, or a frozen experiment contract. Agents only ever propose.
const (
	ApprovedByUser       = session.ApprovedByUser
	ApprovedByTrustFlow  = session.ApprovedByTrust
	ApprovedByTeam       = session.ApprovedByTeam
	ApprovedByExperiment = session.ApprovedByFreeze
)

// Check approval changes of one check against the newest approved set.
const (
	CheckUnchanged = "unchanged"
	CheckAdded     = "added"
	CheckChanged   = "changed"
)

// ApprovalRecord is one stored approval: a check set, one of its definitions, or a constraint binding.
type ApprovalRecord = session.ApprovalRecord

// DefinitionWrite is the newest write of a check definition file by a run, since the newest approval.
type DefinitionWrite = session.DefinitionWrite

// CheckApprovalItem is one configured check against the newest approved set: its definition, digest,
// whether it is approved now, and how it differs from the approved definition.
type CheckApprovalItem struct {
	Spec       CheckSpec `json:"spec"`
	SpecDigest string    `json:"specDigest"`
	Approved   bool      `json:"approved"`
	Change     string    `json:"change"`
}

// CheckApprovalState is the project's effective check set and its approval, the one view CLI and
// Desktop show before a user approves (ADR-0017). ConfigScope is where the definitions are read from:
// the Engine workspace's effective configuration, never a session worktree. Removed are definitions
// the newest approved set had and the configuration no longer has. Previous is that approval, active or
// revoked; LastWrite the newest write of a definition file by a run since it.
type CheckApprovalState struct {
	ProjectID   string              `json:"projectId"`
	ConfigScope string              `json:"configScope"`
	SetDigest   string              `json:"setDigest"`
	Approved    bool                `json:"approved"`
	Epoch       int64               `json:"epoch,omitempty"`
	Checks      []CheckApprovalItem `json:"checks"`
	Removed     []ApprovalRecord    `json:"removed,omitempty"`
	Previous    *ApprovalRecord     `json:"previous,omitempty"`
	LastWrite   *DefinitionWrite    `json:"lastWrite,omitempty"`
	Bindings    []ApprovalRecord    `json:"bindings"`
	Warnings    []config.Warning    `json:"warnings,omitempty"`
}

// ApproveChecksRequest approves the current check set. SetDigest is the digest the user was shown
// (CheckApprovalState.SetDigest); the approval fails with ErrApprovalStale when the configuration no
// longer has it. The empty set is approved like any other.
type ApproveChecksRequest struct {
	SetDigest   string `json:"setDigest"`
	ApprovedBy  string `json:"approvedBy"`
	Actor       string `json:"actor"`
	DecisionRef string `json:"decisionRef,omitempty"`
}

// ApproveBindingRequest approves a ConstraintBinding: the constraint (ref and digest) the configured
// check may satisfy, for scope. SpecDigest is the check definition the user was shown; it must be the
// current one. A check approval never approves a binding, nor the other way round.
type ApproveBindingRequest struct {
	ConstraintRef    string `json:"constraintRef"`
	ConstraintDigest string `json:"constraintDigest"`
	CheckID          string `json:"checkId"`
	SpecDigest       string `json:"specDigest"`
	Scope            string `json:"scope"`
	ApprovedBy       string `json:"approvedBy"`
	Actor            string `json:"actor"`
	DecisionRef      string `json:"decisionRef,omitempty"`
}

// RevokeApprovalRequest revokes the project's check set approval (Kind "checks") or one constraint
// binding (Kind "constraint_binding", by BindingDigest).
type RevokeApprovalRequest struct {
	Kind          string `json:"kind"`
	BindingDigest string `json:"bindingDigest,omitempty"`
}

// Revocation kinds.
const (
	RevokeChecks  = "checks"
	RevokeBinding = "constraint_binding"
)

// checkApproval is the approval the current effective check set runs under: epoch is 0 unless the
// newest set approval is active and covers exactly the current definitions.
type checkApproval struct {
	setDigest string
	epoch     int64
	set       *session.ApprovalRecord
	specs     []session.ApprovalRecord
}

// projectID is the key of the project's approvals: the canonical workspace the state is bound to.
func (e *Engine) projectID() string {
	if e.state.Workspace != "" {
		return e.state.Workspace
	}
	return e.workspace
}

// approvedChecks re-reads the effective check set from the Engine workspace (configScope) and the
// project's newest approval, and returns the definitions with the approval epoch they run under (0 for
// all of them unless the whole set is approved as it is now).
func (e *Engine) approvedChecks(ctx context.Context) ([]evaluator.Spec, checkApproval, []config.Warning, error) {
	specs, warnings, err := e.effectiveChecks()
	if err != nil {
		return nil, checkApproval{}, nil, err
	}
	a := checkApproval{setDigest: evaluator.SetDigest(specs)}
	if a.set, a.specs, err = e.store.LatestCheckSetApproval(ctx, e.projectID()); err != nil {
		return nil, checkApproval{}, nil, err
	}
	if a.set != nil && a.set.State == session.ApprovalActive && a.set.SetDigest == a.setDigest && coversExactly(a.specs, specs) {
		a.epoch = a.set.Epoch
	}
	for i := range specs {
		specs[i].ApprovalEpoch = a.epoch
	}
	return specs, a, warnings, nil
}

// coversExactly: the approved definitions are the current ones, each active with the same digest.
func coversExactly(approved []session.ApprovalRecord, specs []evaluator.Spec) bool {
	if len(approved) != len(specs) {
		return false
	}
	byID := make(map[string]session.ApprovalRecord, len(approved))
	for _, a := range approved {
		byID[a.CheckID] = a
	}
	for _, s := range specs {
		a, ok := byID[s.ID]
		if !ok || a.State != session.ApprovalActive || a.SpecDigest != evaluator.SpecDigest(s) {
			return false
		}
	}
	return true
}

// CheckApprovalState reports the project's effective check set against its approvals. It reads only.
func (e *Engine) CheckApprovalState(ctx context.Context) (CheckApprovalState, error) {
	specs, a, warnings, err := e.approvedChecks(ctx)
	if err != nil {
		return CheckApprovalState{}, err
	}
	st := CheckApprovalState{ProjectID: e.projectID(), ConfigScope: e.workspace, SetDigest: a.setDigest,
		Approved: a.epoch > 0, Epoch: a.epoch, Checks: make([]CheckApprovalItem, 0, len(specs)), Previous: a.set, Warnings: warnings}
	approved := make(map[string]session.ApprovalRecord, len(a.specs))
	for _, r := range a.specs {
		approved[r.CheckID] = r
	}
	for _, s := range specs {
		item := CheckApprovalItem{Spec: s, SpecDigest: evaluator.SpecDigest(s), Approved: a.epoch > 0, Change: CheckAdded}
		if r, ok := approved[s.ID]; ok {
			item.Change = CheckChanged
			if r.SpecDigest == item.SpecDigest {
				item.Change = CheckUnchanged
			}
			delete(approved, s.ID)
		}
		st.Checks = append(st.Checks, item)
	}
	for _, r := range a.specs {
		if _, gone := approved[r.CheckID]; gone {
			st.Removed = append(st.Removed, r)
		}
	}
	var since time.Time
	if a.set != nil {
		since = a.set.CreatedAt
	}
	if st.LastWrite, err = e.store.LastDefinitionWrite(ctx, since); err != nil {
		return CheckApprovalState{}, err
	}
	if st.Bindings, err = e.store.ActiveConstraintBindings(ctx, e.projectID()); err != nil {
		return CheckApprovalState{}, err
	}
	return st, nil
}

// ApproveChecks approves the current effective check set, as the user was shown it, under a new epoch:
// the set, every definition in it, and nothing else. A previous approval is revoked by it. Results
// decided under an older epoch satisfy nothing afterwards; the checks run again.
func (e *Engine) ApproveChecks(ctx context.Context, req ApproveChecksRequest) (CheckApprovalState, error) {
	specs, a, _, err := e.approvedChecks(ctx)
	if err != nil {
		return CheckApprovalState{}, err
	}
	if strings.TrimSpace(req.SetDigest) != a.setDigest {
		return CheckApprovalState{}, fmt.Errorf("%w: the check set is %s now, not %s", ErrApprovalStale, a.setDigest, strings.TrimSpace(req.SetDigest))
	}
	approval := session.CheckSetApproval{ProjectID: e.projectID(), SetDigest: a.setDigest, ApprovedBy: req.ApprovedBy,
		Actor: strings.TrimSpace(req.Actor), DecisionRef: strings.TrimSpace(req.DecisionRef)}
	for _, s := range specs {
		approval.Specs = append(approval.Specs, session.ApprovedSpec{CheckID: s.ID, SpecDigest: evaluator.SpecDigest(s)})
	}
	epoch, err := e.store.ApproveCheckSet(ctx, approval)
	if err != nil {
		return CheckApprovalState{}, err
	}
	e.events.publish("checks.approved", "", map[string]any{"epoch": epoch, "setDigest": a.setDigest, "approvedBy": req.ApprovedBy, "actor": approval.Actor})
	return e.CheckApprovalState(ctx)
}

// bindingDigest is the digest a ConstraintBinding is approved and used by: "sha256:<hex>" of the
// canonical JSON of the constraint ref and digest, the check id and spec digest, and the scope.
func bindingDigest(constraintRef, constraintDigest, checkID, specDigest, scope string) string {
	raw, _ := json.Marshal([]string{constraintRef, constraintDigest, checkID, specDigest, scope})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ApproveConstraintBinding approves one binding of a constraint to a configured check definition, as
// the user was shown it. It supersedes an active binding of the same constraint to the same check. It
// does not approve the check definition, which must be approved on its own before the binding can
// satisfy the constraint.
func (e *Engine) ApproveConstraintBinding(ctx context.Context, req ApproveBindingRequest) (ApprovalRecord, error) {
	scope := strings.TrimSpace(req.Scope)
	if scope == "" || len(scope) > 256 {
		return ApprovalRecord{}, fmt.Errorf("%w: scope must be 1-256 bytes", ErrApprovalInvalid)
	}
	specs, _, err := e.effectiveChecks()
	if err != nil {
		return ApprovalRecord{}, err
	}
	spec, ok := findSpec(specs, strings.TrimSpace(req.CheckID))
	if !ok {
		return ApprovalRecord{}, fmt.Errorf("%w: %q", ErrUnknownCheck, req.CheckID)
	}
	if current := evaluator.SpecDigest(spec); strings.TrimSpace(req.SpecDigest) != current {
		return ApprovalRecord{}, fmt.Errorf("%w: check %s is %s now", ErrApprovalStale, spec.ID, current)
	}
	b := session.BindingApproval{ProjectID: e.projectID(), ConstraintRef: strings.TrimSpace(req.ConstraintRef),
		ConstraintDigest: strings.TrimSpace(req.ConstraintDigest), CheckID: spec.ID, SpecDigest: evaluator.SpecDigest(spec),
		ApprovedBy: req.ApprovedBy, Actor: strings.TrimSpace(req.Actor), DecisionRef: strings.TrimSpace(req.DecisionRef)}
	b.BindingDigest = bindingDigest(b.ConstraintRef, b.ConstraintDigest, b.CheckID, b.SpecDigest, scope)
	if _, err := e.store.ApproveConstraintBinding(ctx, b); err != nil {
		return ApprovalRecord{}, err
	}
	bindings, err := e.store.ActiveConstraintBindings(ctx, e.projectID())
	if err != nil {
		return ApprovalRecord{}, err
	}
	for _, r := range bindings {
		if r.BindingDigest == b.BindingDigest {
			e.events.publish("checks.binding_approved", "", map[string]any{"epoch": r.Epoch, "bindingDigest": r.BindingDigest, "checkId": r.CheckID})
			return r, nil
		}
	}
	return ApprovalRecord{}, fmt.Errorf("binding %s was not stored", b.BindingDigest)
}

// RevokeApproval revokes the check set approval or one binding and returns the rows revoked. Nothing
// active to revoke is not an error.
func (e *Engine) RevokeApproval(ctx context.Context, req RevokeApprovalRequest) (int64, error) {
	var (
		n   int64
		err error
	)
	switch req.Kind {
	case RevokeChecks:
		n, err = e.store.RevokeCheckApprovals(ctx, e.projectID())
	case RevokeBinding:
		if strings.TrimSpace(req.BindingDigest) == "" {
			return 0, fmt.Errorf("%w: a binding revocation names its binding digest", ErrApprovalInvalid)
		}
		n, err = e.store.RevokeConstraintBinding(ctx, e.projectID(), strings.TrimSpace(req.BindingDigest))
	default:
		return 0, fmt.Errorf("%w: revocation kind %q (want %s or %s)", ErrApprovalInvalid, req.Kind, RevokeChecks, RevokeBinding)
	}
	if err != nil {
		return 0, err
	}
	if n > 0 {
		e.events.publish("checks.revoked", "", map[string]any{"kind": req.Kind, "rows": n})
	}
	return n, nil
}
