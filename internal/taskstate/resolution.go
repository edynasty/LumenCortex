package taskstate

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Resolution states and authors (ADR-0006 D2). Resolution is independent of run status, archival and
// worktree cleanup.
const (
	ResolutionOpen    = "open"
	ResolutionDone    = "done"
	ResolutionDropped = "dropped"

	ByEvidence = "evidence"
	ByUser     = "user"
	ByReopen   = "reopen"
)

// Resolution is whether the user's objective is finished. Only the user (through the inbox), the
// evidence rule and a reopen write it.
type Resolution struct {
	State string     `json:"state"`
	By    string     `json:"by,omitempty"`
	Actor string     `json:"actor,omitempty"`
	At    *time.Time `json:"at,omitempty"`
	// Refs are what an evidence decision rested on, e.g. "workflow:<id>@<action>" and "head:<sha>".
	// Later changes to them do not reopen the thread.
	Refs []string `json:"refs,omitempty"`
	// CancelledUnits are the work units the model cancelled since the thread was last opened. A
	// cancellation discharges an obligation but is never acceptance, so while this is non-empty the
	// evidence rule does not latch done.
	CancelledUnits []string `json:"cancelledUnits,omitempty"`
}

// Obligation names; each keeps a thread open.
const (
	ObligationWorkUnits          = "work_units"
	ObligationGate               = "gate"
	ObligationWorkflow           = "workflow"
	ObligationChecks             = "checks"
	ObligationPendingApplication = "pending_application"
)

// Evidence is what the rules read beyond the state itself: the workflow runtime's verdicts, the
// evaluator's stored results and the workspace, which only the runtime can inspect.
type Evidence struct {
	// Workflow: a host workflow is active. WorkflowCanFinish and GatePending are its verdicts.
	Workflow          bool
	WorkflowCanFinish bool
	GatePending       bool
	// Checks are the configured required checks and whether each has a satisfying stored result at
	// the workspace's current fingerprint (design §4.7). Outside git none is ever satisfied.
	Checks []CheckEvidence
	// PendingApplication: the session's owned worktree is dirty or has commits not yet applied.
	PendingApplication bool
	// Refs are the workspace refs a latch rests on, e.g. "head:<sha>".
	Refs []string
}

// CheckEvidence is one required check. Ref names the result that satisfies it, e.g.
// "check:go-test@eval:12", and is empty when none does.
type CheckEvidence struct {
	ID        string `json:"id"`
	Satisfied bool   `json:"satisfied"`
	Ref       string `json:"ref,omitempty"`
}

// Obligations lists what keeps the thread open, in a stable order. Execution tasks arrive with C2.
func (s State) Obligations(e Evidence) []string {
	var out []string
	if s.WorkUnits != nil && len(s.Units().Incomplete()) > 0 {
		out = append(out, ObligationWorkUnits)
	}
	if e.GatePending {
		out = append(out, ObligationGate)
	}
	if e.Workflow && !e.WorkflowCanFinish {
		out = append(out, ObligationWorkflow)
	}
	for _, c := range e.Checks {
		if !c.Satisfied {
			out = append(out, ObligationChecks)
			break
		}
	}
	if e.PendingApplication {
		out = append(out, ObligationPendingApplication)
	}
	return out
}

// accepted reports positive acceptance evidence (ADR-0006 D2): a host workflow that can finish; every
// required check satisfied at the current fingerprint; or every work unit completed with passing
// evaluator verdicts. A model's cancellation since the last reopen never counts, and neither does
// applying changes on its own.
func (s State) accepted(e Evidence) bool {
	if len(s.Resolution.CancelledUnits) > 0 {
		return false
	}
	return (e.Workflow && e.WorkflowCanFinish) || checksAccepted(e.Checks) || s.unitsAccepted()
}

func checksAccepted(checks []CheckEvidence) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if !c.Satisfied {
			return false
		}
	}
	return true
}

// unitsAccepted: there are units, every one completed, and each completed with checks that all have a
// passed evaluator verdict. A unit completed without checks rests on the model's word alone.
func (s State) unitsAccepted() bool {
	if s.WorkUnits == nil {
		return false
	}
	units := s.Units().List()
	if len(units) == 0 {
		return false
	}
	for _, u := range units {
		if u.Status != WorkCompleted || len(u.Checks) == 0 || len(u.FailingChecks()) > 0 {
			return false
		}
	}
	return true
}

// AutoDone latches done (by evidence) when the thread is open, no obligation remains and there is
// positive acceptance evidence. It never replaces a done or dropped thread. It reports whether s changed.
func (s *State) AutoDone(e Evidence, at time.Time) bool {
	if s.Resolution.State != ResolutionOpen || len(s.Obligations(e)) > 0 || !s.accepted(e) {
		return false
	}
	var refs []string
	if s.Workflow != nil && s.Workflow.Definition != nil {
		refs = append(refs, fmt.Sprintf("workflow:%s@%s", s.Workflow.Definition.ID, s.Workflow.CurrentAction))
	}
	for _, c := range e.Checks {
		refs = append(refs, c.Ref)
	}
	refs = append(refs, e.Refs...)
	s.Resolution = Resolution{State: ResolutionDone, By: ByEvidence, Actor: "runtime", At: &at, Refs: refs}
	return true
}

// ReadyForReview is the derived "ready for review" value: open, nothing left to do, but no evidence the
// rule accepts. It is never stored.
func (s State) ReadyForReview(e Evidence) bool {
	return s.Resolution.State == ResolutionOpen && len(s.Obligations(e)) == 0 && !s.accepted(e)
}

// Reopen sets a done or dropped thread open again because a new obligation appeared (a unit created, a
// successful mutating tool). It reports whether s changed.
func (s *State) Reopen(at time.Time) bool {
	if s.Resolution.State == ResolutionOpen {
		return false
	}
	s.Resolution = Resolution{State: ResolutionOpen, By: ByReopen, Actor: "runtime", At: &at}
	return true
}

// NoteCancelled records a work unit the model cancelled, so the evidence rule does not latch on it.
func (s *State) NoteCancelled(unitID string) {
	if !slices.Contains(s.Resolution.CancelledUnits, unitID) {
		s.Resolution.CancelledUnits = append(s.Resolution.CancelledUnits, unitID)
	}
}

// resolve is the user's manual resolution (ResolveThread).
func (s *State) resolve(state, actor string, at time.Time) {
	r := Resolution{State: state, By: ByUser, Actor: actor, At: &at}
	if state != ResolutionOpen {
		r.CancelledUnits = s.Resolution.CancelledUnits
	}
	s.Resolution = r
}

func validResolution(state string) bool {
	return state == ResolutionOpen || state == ResolutionDone || state == ResolutionDropped
}

func validActor(actor string) error {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 128 {
		return fmt.Errorf("actor must be 1-128 bytes")
	}
	return nil
}
