package knowledge

import "testing"

// FFR-001. A receipt reproduces the origin only when every action settled with the outcome its origin
// step had. Executed alone (a complete sequence, the last action allowed to fail) is not enough to
// credit the strategy with an outcome.
func TestApplicationReproducedRequiresTheOriginStepOutcomes(t *testing.T) {
	steps := func(outcomes ...string) []ExperienceStep {
		out := make([]ExperienceStep, len(outcomes))
		for i, o := range outcomes {
			out[i] = ExperienceStep{Tool: "bash", Outcome: o}
		}
		return out
	}
	receipt := func(state string, outcomes ...string) Application {
		a := Application{State: state}
		for i, o := range outcomes {
			a.Actions = append(a.Actions, AppliedAction{Tool: "bash", ExecutionID: string(rune('a' + i)), Outcome: o})
		}
		return a
	}
	for _, tc := range []struct {
		name     string
		receipt  Application
		origin   []ExperienceStep
		executed bool
		want     bool
	}{
		{"same completed sequence", receipt("completed", "completed", "completed"), steps("completed", "completed"), true, true},
		{"origin's failing last step repeated", receipt("failed", "completed", "failed"), steps("completed", "failed"), true, true},
		{"action failed where the origin completed", receipt("failed", "failed"), steps("completed"), true, false},
		{"action completed where the origin failed", receipt("completed", "completed"), steps("failed"), true, false},
		{"another number of steps", receipt("completed", "completed"), steps("completed", "completed"), true, false},
		{"unknown receipt", receipt("unknown", "completed"), steps("completed"), false, false},
		{"pending receipt", receipt("pending", "completed"), steps("completed"), false, false},
		{"partial sequence", Application{State: "failed", Actions: []AppliedAction{{Tool: "bash", ExecutionID: "a", Outcome: "failed"}, {Tool: "bash"}}}, steps("failed", "completed"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.receipt.Executed(); got != tc.executed {
				t.Fatalf("Executed = %v, want %v", got, tc.executed)
			}
			if got := tc.receipt.Reproduced(tc.origin); got != tc.want {
				t.Fatalf("Reproduced = %v, want %v", got, tc.want)
			}
		})
	}
}
