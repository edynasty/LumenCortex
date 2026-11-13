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
	Completed   bool   `json:"completed"`
}

// Executed reports whether the complete planned action sequence has settled
// execution identities. Tool failure is an observation, not non-execution.
// Denied, interrupted, pending and partially executed sequences stay unknown.
func (a Application) Executed() bool {
	if (a.State != "completed" && a.State != "failed") || len(a.Actions) == 0 {
		return false
	}
	for _, action := range a.Actions {
		if action.ExecutionID == "" {
			return false
		}
	}
	return true
}
