package knowledge

import (
	"errors"
	"math"
	"slices"
)

// NodeRef pins a dependency to the revision actually read. New versions invalidate
// applicability; they never silently replace the premise of an earlier decision.
type NodeRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// MemoryPolicy is host-owned retention metadata, not evidence of correctness.
// Access counts are kept per reader in the existing store, outside immutable facts.
type MemoryPolicy struct {
	Tier       string   `json:"tier,omitempty"`
	Retention  string   `json:"retention,omitempty"`
	Importance float64  `json:"importance,omitempty"`
	Canonical  *NodeRef `json:"canonical,omitempty"`
}

func (m MemoryPolicy) Validate() error {
	if !slices.Contains([]string{"", "hot", "warm", "cold"}, m.Tier) || !slices.Contains([]string{"", "normal", "safety", "counterexample"}, m.Retention) || math.IsNaN(m.Importance) || math.IsInf(m.Importance, 0) || m.Importance < 0 || m.Importance > 1 {
		return errors.New("invalid memory policy")
	}
	if m.Canonical != nil && (!plainText(m.Canonical.ID, 128) || m.Canonical.Version < 1) {
		return errors.New("invalid canonical revision")
	}
	return nil
}

func (n Node) MemoryClass() string {
	switch n.Kind {
	case "task":
		return "working"
	case "experience":
		return "procedural"
	case "evidence", "negative":
		return "episodic"
	default:
		return "semantic"
	}
}

// ProtectedMemory keeps low-frequency warnings and counterexamples. High access
// frequency never promotes a failed observation or makes it safe to delete.
func (n Node) ProtectedMemory() bool {
	if n.Kind == "negative" || n.Kind == "task" || (n.Memory != nil && (n.Memory.Retention == "safety" || n.Memory.Retention == "counterexample" || n.Memory.Importance >= 0.8)) {
		return true
	}
	for _, e := range n.Evidence {
		if e.Verification == "failed" || len(e.ContradictingClaims) > 0 {
			return true
		}
	}
	if x := n.Experience; x != nil {
		if x.Origin.Outcome == "failed" || x.Definition.ExpectedResult == "failed" || x.State == ExperienceNeedsRevision {
			return true
		}
		for _, u := range x.Uses {
			if u.Feedback != nil && u.Feedback.Outcome == "failure" {
				return true
			}
		}
	}
	return false
}

func (n Node) ValidateLifecycle() error {
	if !slices.Contains([]string{"", "fact", "inference", "hypothesis", "decision", "unknown", "experience"}, n.Epistemic) {
		return errors.New("invalid epistemic class")
	}
	if n.ModelConfidence != nil && (math.IsNaN(*n.ModelConfidence) || math.IsInf(*n.ModelConfidence, 0) || *n.ModelConfidence < 0 || *n.ModelConfidence > 1) {
		return errors.New("invalid model confidence")
	}
	if n.RevisionReason != "" && !SafeExperienceText(n.RevisionReason, 512) {
		return errors.New("invalid revision reason")
	}
	if len(n.Dependencies) > 16 {
		return errors.New("too many cognitive dependencies")
	}
	seen := make(map[string]bool, len(n.Dependencies))
	for _, ref := range n.Dependencies {
		if !plainText(ref.ID, 128) || ref.ID == n.ID || ref.Version < 1 || seen[ref.ID] {
			return errors.New("invalid cognitive dependency")
		}
		seen[ref.ID] = true
	}
	if n.Memory != nil {
		return n.Memory.Validate()
	}
	return nil
}
