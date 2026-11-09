package agent

import (
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestThreadEvidenceCarriesUnlistedWorkerSources(t *testing.T) {
	// A graph can select a worker outside the legacy 20-child display window.
	// It must still be a source of the result, so revocation can withhold it.
	sources, complete := resultSessions(toolReadThread, "reader", protocol.ToolResult{OK: true, Content: `{"id":"root","children":[{"id":"listed"}],"evidence":{"sources":["unlisted-worker","listed","reader"]}}`})
	if !complete || !slices.Equal(sources, []string{"root", "listed", "unlisted-worker"}) {
		t.Fatalf("lost or duplicated evidence sources: %v %v", sources, complete)
	}
}
