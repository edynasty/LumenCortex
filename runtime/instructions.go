package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/agent"
)

// MaxInstructionsBytes re-exports the bound AgentOptions.Instructions must respect, so a host can
// validate before Start instead of discovering the refusal on the bridge.
const MaxInstructionsBytes = agent.MaxInstructionsBytes

// Reasons an InstructionsError carries.
const (
	InstructionsTooLarge    = "too_large"
	InstructionsInvalidUTF8 = "invalid_utf8"
)

// Sentinels for callers that only care that the Start was refused.
var (
	ErrInstructionsTooLarge    = errors.New("agent instructions exceed the 16 KiB limit")
	ErrInstructionsInvalidUTF8 = errors.New("agent instructions are not valid UTF-8")
)

// InstructionsError reports why AgentOptions.Instructions was refused. It never carries the text.
type InstructionsError struct {
	Reason string
	Length int
}

func (e *InstructionsError) Error() string {
	switch e.Reason {
	case InstructionsInvalidUTF8:
		return fmt.Sprintf("%v: %d bytes", ErrInstructionsInvalidUTF8, e.Length)
	default:
		return fmt.Sprintf("%v: %d bytes", ErrInstructionsTooLarge, e.Length)
	}
}

func (e *InstructionsError) Unwrap() error {
	if e.Reason == InstructionsInvalidUTF8 {
		return ErrInstructionsInvalidUTF8
	}
	return ErrInstructionsTooLarge
}

// validateInstructions refuses text over the bound or invalid UTF-8 and returns the digest and
// length the decision log records. Whitespace-only text is no instructions: length 0, no decision.
func validateInstructions(text string) (digest string, length int, err error) {
	length = len(text)
	if length > MaxInstructionsBytes {
		return "", length, &InstructionsError{Reason: InstructionsTooLarge, Length: length}
	}
	if !utf8.ValidString(text) {
		return "", length, &InstructionsError{Reason: InstructionsInvalidUTF8, Length: length}
	}
	if strings.TrimSpace(text) == "" {
		return "", 0, nil
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), length, nil
}

// instructionsDecision is the one evidence entry the instructions leave: digest and length, never
// the text. It replays through agent.ReplayDecision like every other decision.
func instructionsDecision(digest string, length int) agent.Decision {
	inputs, _ := json.Marshal(struct {
		SHA256 string `json:"sha256"`
		Length int    `json:"length"`
	}{digest, length})
	output, _ := json.Marshal(struct {
		Appended bool `json:"appended"`
	}{true})
	return agent.Decision{
		Kind:   agent.DecisionInstructions,
		Inputs: inputs,
		Rule:   agent.RuleInstructionsAppended,
		Output: output,
	}
}
