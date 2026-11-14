package knowledge

import "time"

// Application is a framework receipt for one explicit use through the ordinary
// tool dispatcher. The action's semantic quality remains a model judgment.
// Pending/unknown receipts are never replayed as actions or counted as success.
type Application struct {
	ID                string                  `json:"id"`
	ExperienceID      string                  `json:"experienceId"`
	AdoptionID        string                  `json:"adoptionId"`
	SessionID         string                  `json:"sessionId"`
	RunID             string                  `json:"runId"`
	DefinitionVersion int64                   `json:"definitionVersion"`
	Actions           []AppliedAction         `json:"actions"`
	Conditions        []ExperienceObservation `json:"conditions"`
	State             string                  `json:"state"`
	At                time.Time               `json:"at"`
	FinishedAt        time.Time               `json:"finishedAt,omitempty"`
}

type AppliedAction struct {
	Tool        string `json:"tool"`
	ArgsDigest  string `json:"argsDigest"`
	ExecutionID string `json:"executionId,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
}

// Executed reports whether the complete planned action sequence has settled
// execution identities. Tool failure is an observation, not non-execution.
// Denied, interrupted, pending and partially executed sequences stay unknown.
func (a Application) Executed() bool {
	if (a.State != "completed" && a.State != "failed") || len(a.Actions) == 0 {
		return false
	}
	for i, action := range a.Actions {
		if action.ExecutionID == "" || (action.Outcome != "completed" && action.Outcome != "failed") {
			return false
		}
		if a.State == "completed" && action.Outcome != "completed" {
			return false
		}
		for _, prior := range a.Actions[:i] {
			if prior.ExecutionID == action.ExecutionID {
				return false
			}
		}
	}
	return true
}

// Reproduced reports whether an executed application repeated the procedure its origin observed:
// every action settled with the outcome of the origin step it replays. An action that failed where
// the origin's completed (or the reverse) ran something else, so an expected outcome observed after
// it is not a result of the procedure and cannot be credited to it.
func (a Application) Reproduced(steps []ExperienceStep) bool {
	if !a.Executed() || len(steps) != len(a.Actions) {
		return false
	}
	for i, action := range a.Actions {
		if action.Outcome != steps[i].Outcome {
			return false
		}
	}
	return true
}
