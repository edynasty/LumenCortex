package agent

import (
	"context"
	"encoding/json"
	"github.com/edynasty/LumenCortex/protocol"
	"testing"
)

// Execution completion does not assert delivery verification.
func TestDeliveryDoesNotGateAgentCompletion(t *testing.T) {
	store := newState("delivery")
	if err := json.Unmarshal([]byte(`{"delivery":{"files":[{"path":"result.txt"}]}}`), &store.ts); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{answer("The implementation is not finished.")}}
	result, _ := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "delivery", Options{Session: testRef, MaxSteps: 1})
	if result.Status != StatusCompleted {
		t.Fatalf("delivery infrastructure blocked Agent completion: %+v", result)
	}
}
