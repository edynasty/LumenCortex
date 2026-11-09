package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestDebugBoundaryPreservesFenceAndStopsFurtherExecution(t *testing.T) {
	for _, point := range []string{"BeforeModelCall", "AfterModelCall", "BeforeToolCall", "AfterToolCall", "BeforeDelivery"} {
		t.Run(point, func(t *testing.T) {
			store := envStore("debug-fenced")
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{
				toolStep(call("write", "bash", `{"command":"echo once"}`)), answerOf("done"),
			}}
			tools := &tierTools{}
			loop := Loop{Models: testModels(provider), Store: store, Tools: tools,
				DebugBoundary: func(_ context.Context, p DebugBoundary) error {
					if p.Event == point {
						return session.ErrFenced
					}
					return nil
				},
			}
			result, err := loop.Run(context.Background(), "debug-fenced", Options{Session: testRef, MaxSteps: 3})
			if !errors.Is(err, session.ErrFenced) {
				t.Fatalf("fence lost: %v", err)
			}
			assertExit(t, store, result, StatusFailed, KindFenced)
			wantRequests, wantExecutions := 1, 0
			switch point {
			case "BeforeModelCall":
				wantRequests = 0
			case "AfterToolCall":
				wantExecutions = 1
			case "BeforeDelivery":
				wantRequests, wantExecutions = 2, 1
			}
			if len(provider.seen) != wantRequests || len(store.executions) != wantExecutions {
				t.Fatalf("continued across fence: requests=%d executions=%d", len(provider.seen), len(store.executions))
			}
		})
	}
}
