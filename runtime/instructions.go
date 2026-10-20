package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/edynasty/LumenCortex/internal/agent"
)

// instructionsDecision is the one evidence entry an agent's instructions leave: digest and length,
// never the text. It replays through agent.ReplayDecision like every other decision. The text itself
// is configuration (ADR-0009), validated when the configuration loads.
func instructionsDecision(text string) agent.Decision {
	sum := sha256.Sum256([]byte(text))
	inputs, _ := json.Marshal(struct {
		SHA256 string `json:"sha256"`
		Length int    `json:"length"`
	}{hex.EncodeToString(sum[:]), len(text)})
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
