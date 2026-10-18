package taskstate

import (
	"slices"
	"testing"
	"time"
)

// unitsState returns an open state whose single unit is completed with the given checks, each with
// the given evaluator status ("" leaves the unit without checks).
func unitsState(t *testing.T, status string) State {
	t.Helper()
	m := NewWorkUnitManager(nil)
	unit := WorkUnit{ID: "u", Goal: "g"}
	if status != "" {
		unit.Checks = []string{"go-test"}
	}
	if err := m.Seed([]WorkUnit{unit}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureActive(); err != nil {
		t.Fatal(err)
	}
	done := WorkCompleted
	patch := WorkUnitPatch{Status: &done}
	if status != "" {
		results := []UnitCheck{{Check: "go-test", Status: status, Provenance: "runtime", Seq: 7}}
		patch.CheckResults = &results
	}
	if _, err := m.Update("u", patch); err != nil && status == "passed" {
		t.Fatal(err)
	}
	s := State{Resolution: Resolution{State: ResolutionOpen}}
	s.SetUnits(m)
	return s
}

// ADR-0006 D2 with M4.1: done is latched by positive evidence only (a finishable workflow, every
// required check satisfied at the current fingerprint, or every unit completed with passing evaluator
// verdicts), and an unsatisfied required check is an obligation.
func TestEvidenceRuleAcceptanceSources(t *testing.T) {
	at := time.Unix(1, 0).UTC()
	sat := CheckEvidence{ID: "go-test", Satisfied: true, Ref: "check:go-test@eval:12"}
	unsat := CheckEvidence{ID: "lint"}
	cases := []struct {
		name        string
		state       State
		ev          Evidence
		done        bool
		obligations []string
		ready       bool
	}{
		{"nothing is no evidence", State{}, Evidence{Refs: []string{"head:abc"}}, false, nil, true},
		{"all required checks satisfied", State{}, Evidence{Checks: []CheckEvidence{sat}, Refs: []string{"head:abc"}}, true, nil, false},
		{"an unsatisfied required check is an obligation", State{}, Evidence{Checks: []CheckEvidence{sat, unsat}}, false, []string{ObligationChecks}, false},
		{"satisfied checks but work pending application", State{}, Evidence{Checks: []CheckEvidence{sat}, PendingApplication: true}, false, []string{ObligationPendingApplication}, false},
		{"units completed with passing verdicts", unitsState(t, "passed"), Evidence{}, true, nil, false},
		{"units completed without checks are the model's word", unitsState(t, ""), Evidence{}, false, nil, true},
		{"a cancelled unit blocks check acceptance", State{Resolution: Resolution{CancelledUnits: []string{"x"}}}, Evidence{Checks: []CheckEvidence{sat}}, false, nil, true},
		{"a running execution task is an obligation", State{}, Evidence{Checks: []CheckEvidence{sat}, Tasks: []TaskEvidence{{SessionID: "c", Status: "running"}}}, false, []string{ObligationTasks}, false},
		{"a task waiting at a gate is an obligation", State{}, Evidence{Tasks: []TaskEvidence{{SessionID: "c", Status: "waiting_gate"}}}, false, []string{ObligationTasks}, false},
		{"an interrupted task blocks acceptance", State{}, Evidence{Checks: []CheckEvidence{sat}, Tasks: []TaskEvidence{{SessionID: "c", Status: "interrupted"}}}, false, nil, true},
		{"a failed task blocks acceptance", State{}, Evidence{Checks: []CheckEvidence{sat}, Tasks: []TaskEvidence{{SessionID: "c", Status: "failed"}}}, false, nil, true},
		{"a completed task leaves check acceptance", State{}, Evidence{Checks: []CheckEvidence{sat}, Tasks: []TaskEvidence{{SessionID: "c", Status: "completed"}}}, true, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.state
			s.Resolution.State = ResolutionOpen
			if got := s.Obligations(c.ev); !slices.Equal(got, c.obligations) {
				t.Fatalf("obligations=%v, want %v", got, c.obligations)
			}
			if got := s.ReadyForReview(c.ev); got != c.ready {
				t.Fatalf("ready for review=%v, want %v", got, c.ready)
			}
			if got := s.AutoDone(c.ev, at); got != c.done {
				t.Fatalf("AutoDone=%v, want %v (resolution %+v)", got, c.done, s.Resolution)
			}
			if c.done && (s.Resolution.State != ResolutionDone || s.Resolution.By != ByEvidence) {
				t.Fatalf("resolution=%+v", s.Resolution)
			}
		})
	}

	// The latch records what it rests on: each satisfied check's result ref, then the workspace refs.
	s := State{Resolution: Resolution{State: ResolutionOpen}}
	s.AutoDone(Evidence{Checks: []CheckEvidence{sat}, Refs: []string{"head:abc"}}, at)
	if want := []string{"check:go-test@eval:12", "head:abc"}; !slices.Equal(s.Resolution.Refs, want) {
		t.Fatalf("refs=%v, want %v", s.Resolution.Refs, want)
	}
	// A resolved thread is never replaced.
	s.Resolution = Resolution{State: ResolutionDropped}
	if s.AutoDone(Evidence{Checks: []CheckEvidence{sat}}, at) {
		t.Fatal("AutoDone replaced a dropped thread")
	}
}
