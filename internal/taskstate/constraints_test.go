package taskstate

import (
	"testing"
	"time"
)

// No run's decision replaces the user's accepted deviation, whatever the decision; other uses take it.
func TestDecideConstraintKeepsTheUsersDeviation(t *testing.T) {
	at := time.Unix(100, 0)
	var s State
	for _, e := range []ConstraintEdit{
		{Action: ConstraintSelect, Ref: "a", Digest: "d", Scope: "p", Actor: "u"},
		{Action: ConstraintSelect, Ref: "b", Digest: "d", Scope: "p", Actor: "u"},
		{Action: ConstraintDeviate, Ref: "a", Digest: "d", Scope: "p", Reason: "not here", Actor: "u"},
	} {
		s.Apply([]QueuedEdit{{At: at, Edit: Edit{Constraint: &e}}})
	}
	for i := range s.Constraints {
		s.DecideConstraint(i, "run1", ConstraintDecision{Status: ConstraintSatisfied, EpisodeID: "ep"}, at)
	}
	if a := s.Constraints[0]; a.Status != ConstraintDeviation || a.Deviation == nil || a.RunID != "" || a.EpisodeID != "" {
		t.Fatalf("the deviation was decided over: %+v", a)
	}
	if b := s.Constraints[1]; b.Status != ConstraintSatisfied || b.RunID != "run1" {
		t.Fatalf("the other use: %+v", b)
	}
}
