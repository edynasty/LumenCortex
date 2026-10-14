package window

import (
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestEstimateFormulaAndDeterminism(t *testing.T) {
	// {"role":"user","content":"abc"} is 31 bytes → (31+2)/3 + 4 = 15
	m := protocol.Message{Role: "user", Content: "abc"}
	raw, _ := json.Marshal(m)
	if want := (len(raw)+2)/3 + 4; Estimate(m) != want || Estimate(m) != Estimate(m) {
		t.Fatalf("Estimate=%d want %d", Estimate(m), want)
	}
	if Estimate([]protocol.Message{m, m}) <= Estimate(m) {
		t.Fatal("a longer request must not estimate lower")
	}
}

// A stored assistant row whose arguments are not valid JSON must still estimate, not become 0.
func TestEstimateSurvivesInvalidToolArguments(t *testing.T) {
	m := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "1", Name: "x", Arguments: json.RawMessage(`{broken`)}}}
	if n := Estimate(m); n <= 4 {
		t.Fatalf("Estimate=%d for a message with a broken argument", n)
	}
	if Estimate([]protocol.Message{m}) <= 4 {
		t.Fatal("slice estimate lost the message")
	}
}
