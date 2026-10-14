package cognition

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Progress counts repeated failures across a run. It is the only part of the earlier routing
// control that remains: Task State replaces it in M3.1.
type Progress struct {
	SameFailureCount   int            `json:"sameFailureCount,omitempty"`
	MaxRepeatedFailure int            `json:"maxRepeatedFailure,omitempty"`
	NoProgressSteps    int            `json:"noProgressSteps,omitempty"`
	LastFailure        string         `json:"lastFailure,omitempty"`
	Failures           map[string]int `json:"failures,omitempty"`
}

func (p *Progress) ObserveTool(name string, ok bool, content string) {
	if ok {
		p.NoProgressSteps = 0
		return
	}
	if p.Failures == nil {
		p.Failures = map[string]int{}
	}
	p.NoProgressSteps++
	signature := FailureSignature(name, content)
	p.Failures[signature]++
	p.LastFailure = signature
	p.SameFailureCount = p.Failures[signature]
	if p.SameFailureCount > p.MaxRepeatedFailure {
		p.MaxRepeatedFailure = p.SameFailureCount
	}
}

var (
	failureLargeNumber = regexp.MustCompile(`\b\d{4,}\b`)
	failureHex         = regexp.MustCompile(`(?i)0x[0-9a-f]+`)
	failurePath        = regexp.MustCompile(`/(?:[^\s:]+/)*[^\s:]+`)
)

func FailureSignature(tool, content string) string {
	normalized := failureLargeNumber.ReplaceAllString(content, "#")
	normalized = failureHex.ReplaceAllString(normalized, "0x#")
	normalized = failurePath.ReplaceAllString(normalized, "/PATH")
	if len(normalized) > 1600 {
		normalized = normalized[:1600]
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(tool) + "\n" + normalized))
	return hex.EncodeToString(sum[:10])
}
