package taskstate

// Progress preserves historical state fields only. Runtime no longer infers progress or repetition
// from tool outcomes; old records are retained without driving execution or effort.
type Progress struct {
	SameFailureCount   int            `json:"sameFailureCount,omitempty"`
	MaxRepeatedFailure int            `json:"maxRepeatedFailure,omitempty"`
	NoProgressSteps    int            `json:"noProgressSteps,omitempty"`
	LastFailure        string         `json:"lastFailure,omitempty"`
	Failures           map[string]int `json:"failures,omitempty"`
}
