package agent

import (
	"strings"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// PresentationOptions is how a model's execution profile presents the same authority: a compact
// tool catalog and shorter failure feedback, or the detailed forms. It never changes a tool's
// permission, parameters, mutating flag or the run's policy: two models with different profiles can
// do exactly the same things.
type PresentationOptions struct {
	// ToolDescriptions is "", config.ProfileCompact or config.ProfileDetailed.
	ToolDescriptions string
	// Feedback is "", config.ProfileCompact or config.ProfileDetailed.
	Feedback string
}

// CompactDescriptionBytes bounds a compact tool description.
const CompactDescriptionBytes = 200

// CompactFeedbackBytes bounds compact failure feedback.
const CompactFeedbackBytes = 1200

// presentSpecs applies the profile's description variant. The schema, permission and mutating flag
// are copied unchanged: presentation is not authority.
func presentSpecs(specs []protocol.ToolSpec, variant string) []protocol.ToolSpec {
	if variant != config.ProfileCompact || len(specs) == 0 {
		return specs
	}
	out := make([]protocol.ToolSpec, len(specs))
	for i, spec := range specs {
		out[i] = spec
		out[i].Description = compactDescription(spec.Description)
	}
	return out
}

// compactDescription is the first sentence of a description, bounded.
func compactDescription(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if end := strings.IndexAny(text, ".\n"); end > 0 && end+1 <= len(text) {
		text = text[:end+1]
	}
	if len(text) > CompactDescriptionBytes {
		cut := text[:CompactDescriptionBytes]
		for len(cut) > 0 && !utf8Valid(cut) {
			cut = cut[:len(cut)-1]
		}
		text = cut + "…"
	}
	return text
}

// presentFeedback applies the profile's feedback variant. Compact keeps one line per failing item
// and drops output tails; the reason and the instruction stay.
func presentFeedback(text, variant string) string {
	if variant != config.ProfileCompact || text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" {
			continue
		}
		// A tail line is indented under its item; compact keeps the item line only.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		out = append(out, line)
	}
	compact := strings.Join(out, "\n")
	if len(compact) > CompactFeedbackBytes {
		cut := compact[:CompactFeedbackBytes]
		for len(cut) > 0 && !utf8Valid(cut) {
			cut = cut[:len(cut)-1]
		}
		compact = cut + "…"
	}
	return compact
}

// utf8Valid reports whether s is valid UTF-8.
func utf8Valid(s string) bool {
	return utf8.ValidString(s)
}
