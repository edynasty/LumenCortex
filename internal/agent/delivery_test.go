package agent

import (
	"context"
	"encoding/json"
	"github.com/edynasty/LumenCortex/protocol"
	"testing"
)

// A declared deliverable cannot be discharged by a stop reply or a notebook.
func TestDeliveryStopIsNotProof(t *testing.T) {
	store := newState("delivery")
	if err := json.Unmarshal([]byte(`{"delivery":{"files":[{"path":"result.txt"}]}}`), &store.ts); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{answer("The implementation is not finished.")}}
	result, _ := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "delivery", Options{Session: testRef, MaxSteps: 1})
	if result.Status == StatusCompleted {
		t.Fatalf("unverified delivery completed: %+v", result)
	}
}
