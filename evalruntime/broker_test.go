package evalruntime

import (
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

func TestBrokerBudgetCannotWidenHostPolicy(t *testing.T) {
	ceiling := DefaultBudget()
	for _, tc := range []struct {
		name   string
		budget evalapi.Budget
		denied bool
	}{
		{"inherit", evalapi.Budget{}, false},
		{"tighten", evalapi.Budget{WallSeconds: 1, MemoryBytes: 128 << 20}, false},
		{"unlimited-memory", evalapi.Budget{MemoryBytes: -1}, true},
		{"longer-wall", evalapi.Budget{WallSeconds: ceiling.WallSeconds + 1}, true},
		{"more-workers", evalapi.Budget{MaxWorkers: ceiling.MaxWorkers + 1}, true},
		{"unlimited-callbacks", evalapi.Budget{MaxCallbacks: -1}, true},
		{"deeper-callbacks", evalapi.Budget{MaxDepth: ceiling.MaxDepth + 1}, true},
		{"larger-artifact", evalapi.Budget{ArtifactBytes: ceiling.ArtifactBytes + 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBrokerBudget(tc.budget, ceiling)
			if errors.Is(err, evalapi.ErrDenied) != tc.denied {
				t.Fatalf("policy result=%v denied=%v", err, tc.denied)
			}
		})
	}
}
