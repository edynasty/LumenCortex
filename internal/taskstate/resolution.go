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
	ObligationPendingApplication = "pending_application"
)

// Evidence is what the rules read beyond the state itself: the workflow runtime's verdicts and the
// workspace, which only the runtime can inspect.
type Evidence struct {
	// Workflow: a host workflow is active. WorkflowCanFinish and GatePending are its verdicts.
	Workflow          bool
	WorkflowCanFinish bool
	GatePending       bool
	// PendingApplication: the session's owned worktree is dirty or has commits not yet applied.
	PendingApplication bool
	// Refs are the workspace refs a latch rests on, e.g. "head:<sha>".
	Refs []string
}

// Obligations lists what keeps the thread open, in a stable order. M4.1 adds required checks; execution
// tasks arrive with C2.
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
	if e.PendingApplication {
		out = append(out, ObligationPendingApplication)
	}
	return out
}

// accepted reports positive acceptance evidence. Before M4.1 the only evidence is a host workflow that
// can finish; a model's cancellation since the last reopen never counts.
func (s State) accepted(e Evidence) bool {
	return e.Workflow && e.WorkflowCanFinish && len(s.Resolution.CancelledUnits) == 0
}

// AutoDone latches done (by evidence) when the thread is open, no obligation remains and there is
// positive acceptance evidence. It never replaces a done or dropped thread. It reports whether s changed.
func (s *State) AutoDone(e Evidence, at time.Time) bool {
	if s.Resolution.State != ResolutionOpen || len(s.Obligations(e)) > 0 || !s.accepted(e) {
		return false
	}
	refs := slices.Clone(e.Refs)
	if s.Workflow != nil && s.Workflow.Definition != nil {
		refs = append([]string{fmt.Sprintf("workflow:%s@%s", s.Workflow.Definition.ID, s.Workflow.CurrentAction)}, refs...)
	}
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
