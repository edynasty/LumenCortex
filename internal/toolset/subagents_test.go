package toolset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
)

type spawnRecorder struct{ agents []string }

func (r *spawnRecorder) Spawn(_ context.Context, _ string, agentID string) (any, error) {
	r.agents = append(r.agents, agentID)
	return map[string]string{"agent": agentID}, nil
}
func (r *spawnRecorder) List(context.Context) (any, error)                        { return nil, nil }
func (r *spawnRecorder) Wait(context.Context, string, time.Duration) (any, error) { return nil, nil }

// spawn_subagent runs only an agent of its enum: omitted means explore, anything else outside the
// list is an error before the controller is asked for a child (ADR-0009 AG5).
func TestSpawnSubagentRunsOnlyEligibleAgents(t *testing.T) {
	recorder := &spawnRecorder{}
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyReadOnly, Subagents: recorder,
		SubagentAgents: []config.AgentInfo{{ID: config.AgentExplore}, {ID: "audit"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"worker", "agent", "nobody"} {
		_, err := set.Execute(context.Background(), "spawn_subagent", map[string]any{"goal": "look", "agent": id}, nil)
		if err == nil || !strings.Contains(err.Error(), "choose one of: explore, audit") {
			t.Fatalf("%s: err=%v", id, err)
		}
	}
	for _, args := range []map[string]any{{"goal": "look"}, {"goal": "look", "agent": "audit"}} {
		if _, err := set.Execute(context.Background(), "spawn_subagent", args, nil); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(recorder.agents, ",") != "explore,audit" {
		t.Fatalf("spawned %v", recorder.agents)
	}
}

// Acceptance 6 (ADR-0009 R.3): the catalog text a delegation tool's description carries is bounded
// by the catalog limits. A full catalog of custom read-only delegation agents with maximal ids and
// descriptions is loaded through config and measured.
func TestAgentCatalogLinesAreBounded(t *testing.T) {
	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "lumencortex"), 0o755); err != nil {
		t.Fatal(err)
	}
	const maxID = 40
	var agents []string
	for i := 0; i < config.MaxCustomAgents; i++ {
		id := fmt.Sprintf("a%02d", i) + strings.Repeat("x", maxID-3)
		agents = append(agents, fmt.Sprintf(`%q: {"use": "delegation", "policy": "read-only", "description": %q}`,
			id, strings.Repeat("d", config.MaxAgentDescriptionSize)))
	}
	content := `{"agents": {` + strings.Join(agents, ",") + `}}`
	if err := os.WriteFile(filepath.Join(xdg, "lumencortex", config.FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eligible := cfg.Effective().DelegationAgents(config.AgentInfo.SpawnEligible)
	if len(eligible) != config.MaxCustomAgents+1 {
		t.Fatalf("eligible %d", len(eligible))
	}
	explore := len(AgentCatalogLines(eligible[:1]))
	got := len(AgentCatalogLines(eligible))
	// Each custom line is "\n- " + id + ": " + description.
	bound := explore + config.MaxCustomAgents*(len("\n- ")+maxID+len(": ")+config.MaxAgentDescriptionSize)
	if got != bound {
		t.Fatalf("catalog text %d bytes, bound %d", got, bound)
	}
	if got > 6<<10 {
		t.Fatalf("catalog text %d bytes exceeds the 6 KiB a request may spend on it", got)
	}
}
