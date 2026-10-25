package taskstate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// ConstraintUse statuses (ADR-0006 amendment, ADR-0017, plan T4-S3). A constraint the host selected
// for the thread is satisfied only by a check the host bound to it (an approved ConstraintBinding),
// whose definition is approved, passing in a closed episode run in the reader's current scope, or by a
// deviation the user accepted. Nothing the model says sets any of them.
const (
	// ConstraintSelected: selected, not yet decided by a run.
	ConstraintSelected = "selected"
	// ConstraintSatisfied: the bound check passed in a closed episode of run RunID.
	ConstraintSatisfied = "satisfied"
	// ConstraintFailed: the bound check ran in a closed episode and did not pass.
	ConstraintFailed = "failed"
	// ConstraintUnbound: no active approved binding matches the constraint, its digest, its scope and
	// the bound check's current definition.
	ConstraintUnbound = "unbound"
	// ConstraintSpecUnapproved: a binding matches, but the host has not approved the check as it is now.
	ConstraintSpecUnapproved = "spec_unapproved"
	// ConstraintUnknown: not decided in this run: over the revalidation cap or budget, the episode did
	// not close, the session is fenced, or the check could not run.
	ConstraintUnknown = "unknown"
	// ConstraintDeviation: the user accepted not meeting the constraint for this task.
	ConstraintDeviation = "deviation"
)

// MaxConstraints bounds the constraints a thread can carry.
const MaxConstraints = 16

const (
	maxConstraintRef    = 512
	maxConstraintDigest = 128
	maxConstraintScope  = 256
	maxDeviationReason  = 512
)

// Constraint edit actions (ConstraintEdit.Action).
const (
	ConstraintSelect    = "select"
	ConstraintRemove    = "remove"
	ConstraintDeviate   = "deviation"
	constraintActionSet = "select, remove or deviation"
)

// ConstraintUse is the visible execution record of one constraint selected for the thread: what it is
// (Ref and Digest, the scope its binding was approved for), how a run last decided it, and on what. A
// satisfied use names the binding, the approved definition and the closed episode that decided it. It
// is a record of run RunID, never a pass a later run takes over: every Start decides it again.
type ConstraintUse struct {
	Ref        string    `json:"constraintRef"`
	Digest     string    `json:"constraintDigest"`
	Scope      string    `json:"scope"`
	SelectedBy string    `json:"selectedBy"`
	SelectedAt time.Time `json:"selectedAt"`
	ConstraintDecision
	RunID     string     `json:"runId,omitempty"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`

	Deviation *ConstraintDeviationAcceptance `json:"deviation,omitempty"`
}

// ConstraintDecision is how a run decided one use. The runtime fills it from the approved binding and
// the check episode, never from a model.
type ConstraintDecision struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// The decision's basis, set when an approved binding matched.
	BindingDigest string `json:"bindingDigest,omitempty"`
	Check         string `json:"check,omitempty"`
	SpecDigest    string `json:"specDigest,omitempty"`
	ApprovalEpoch int64  `json:"approvalEpoch,omitempty"`
	// The episode that decided it, set when the check ran.
	EpisodeID       string `json:"episodeId,omitempty"`
	ResultSeq       int64  `json:"resultSeq,omitempty"`
	PreFingerprint  string `json:"preFingerprint,omitempty"`
	PostFingerprint string `json:"postFingerprint,omitempty"`
}

// ConstraintDeviationAcceptance is the user's acceptance of not meeting a constraint in this task.
type ConstraintDeviationAcceptance struct {
	Reason string    `json:"reason"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

// DecideConstraint records run runID's decision on the use at index i. A deviation the user accepted is
// kept as it is: no run's decision replaces the user's acceptance.
func (s *State) DecideConstraint(i int, runID string, d ConstraintDecision, at time.Time) {
	if i < 0 || i >= len(s.Constraints) || s.Constraints[i].Status == ConstraintDeviation {
		return
	}
	u := &s.Constraints[i]
	u.ConstraintDecision, u.RunID = d, runID
	at = at.UTC()
	u.DecidedAt = &at
}

// ConstraintEdit is the host's edit of the thread's constraints: select one (Ref, Digest, Scope), remove
// it, or accept a deviation from it (Reason). Actor is the user making the edit. There is no action
// that marks a constraint satisfied.
type ConstraintEdit struct {
	Action string `json:"action"`
	Ref    string `json:"constraintRef"`
	Digest string `json:"constraintDigest"`
	Scope  string `json:"scope"`
	Reason string `json:"reason,omitempty"`
	Actor  string `json:"actor"`
}

// ErrUnknownConstraint: a remove or deviation names a constraint the thread has not selected.
var ErrUnknownConstraint = errors.New("constraint is not selected for this thread")

func (c ConstraintEdit) validate() error {
	switch c.Action {
	case ConstraintSelect, ConstraintRemove, ConstraintDeviate:
	default:
		return fmt.Errorf("constraint action must be %s", constraintActionSet)
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"constraint ref", c.Ref, maxConstraintRef}, {"constraint digest", c.Digest, maxConstraintDigest}, {"scope", c.Scope, maxConstraintScope}} {
		v := strings.TrimSpace(f.v)
		if v == "" || len(v) > f.max || !utf8.ValidString(v) || strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("%s must be 1-%d bytes on one line", f.name, f.max)
		}
	}
	if err := validActor(c.Actor); err != nil {
		return err
	}
	if c.Action == ConstraintDeviate {
		r := strings.TrimSpace(c.Reason)
		if r == "" || len(r) > maxDeviationReason || !utf8.ValidString(r) {
			return fmt.Errorf("a deviation needs a reason of 1-%d bytes", maxDeviationReason)
		}
	}
	return nil
}

func (s State) constraintIndex(ref, digest, scope string) int {
	for i, u := range s.Constraints {
		if u.Ref == ref && u.Digest == digest && u.Scope == scope {
			return i
		}
	}
	return -1
}

// CheckConstraintEdit reports whether the edit applies to s: a remove or deviation needs the constraint
// selected, and a select needs room for one more.
func (s State) CheckConstraintEdit(c ConstraintEdit) error {
	if err := c.validate(); err != nil {
		return err
	}
	i := s.constraintIndex(strings.TrimSpace(c.Ref), strings.TrimSpace(c.Digest), strings.TrimSpace(c.Scope))
	switch {
	case c.Action == ConstraintSelect && i < 0 && len(s.Constraints) >= MaxConstraints:
		return fmt.Errorf("a thread carries at most %d constraints", MaxConstraints)
	case c.Action != ConstraintSelect && i < 0:
		return ErrUnknownConstraint
	}
	return nil
}

// setConstraint applies a validated edit; one that no longer applies (the constraint was removed
// meanwhile, the list is full) changes nothing.
func (s *State) setConstraint(c ConstraintEdit, at time.Time) {
	ref, digest, scope := strings.TrimSpace(c.Ref), strings.TrimSpace(c.Digest), strings.TrimSpace(c.Scope)
	i := s.constraintIndex(ref, digest, scope)
	actor := strings.TrimSpace(c.Actor)
	switch c.Action {
	case ConstraintSelect:
		if i >= 0 || len(s.Constraints) >= MaxConstraints {
			return
		}
		s.Constraints = append(s.Constraints, ConstraintUse{Ref: ref, Digest: digest, Scope: scope, SelectedBy: actor, SelectedAt: at,
			ConstraintDecision: ConstraintDecision{Status: ConstraintSelected}})
	case ConstraintRemove:
		if i >= 0 {
			s.Constraints = slices.Concat(s.Constraints[:i], s.Constraints[i+1:])
		}
	case ConstraintDeviate:
		if i >= 0 {
			u := &s.Constraints[i]
			u.Status, u.Reason = ConstraintDeviation, ""
			u.Deviation = &ConstraintDeviationAcceptance{Reason: strings.TrimSpace(c.Reason), Actor: actor, At: at}
		}
	}
}
