package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// FFR-007. One record holds every consumer's use of an Experience, so another writer (a second
// consumer's adoption or settlement, a host lifecycle change) can revise it while a consumer's
// outcome check runs. That conflict is benign. The settlement must record the checked outcome
// against the newer revision: left open, a later run of the consumer can only close the use as
// unknown, and the real result, a counterexample included, is lost.
//
// The outcome check blocks while a file outside the workspace exists and reports that it started,
// so the competing revision is committed exactly between the settlement's read and its commit.
func TestExperienceSettlementSurvivesAConcurrentRevision(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	dir := t.TempDir()
	hold, started := filepath.Join(dir, "hold"), filepath.Join(dir, "started")
	checks := strings.Replace(experienceChecks, `"command": "test -f build/reused"`,
		`"command": "touch `+started+`; while test -f `+hold+`; do sleep 0.05; done; test -f build/reused"`, 1)
	if checks == experienceChecks {
		t.Fatal("fixture: the outcome check was not replaced")
	}
	setProjectChecks(t, f.root, checks)
	approveChecks(t, f.engine)
	f.bindPorts(t)
	f.bind(t, expArtifactFact("present"), "reused")
	origin := expOrigin(t, f, "shared build marker procedure")
	n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("shared build marker procedure")))
	if n.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("fixture: state %s", n.Experience.State)
	}
	consumer := expNewSession(t, f, "consumer whose settlement meets another writer")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, hold, "held")
	if err := os.Remove(started); err != nil {
		t.Fatalf("fixture: the outcome check did not run during validation: %v", err)
	}
	competed := make(chan error, 1)
	go func() {
		defer os.Remove(hold)
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				break
			}
			if time.Now().After(deadline) {
				competed <- errors.New("the settlement never ran its outcome check")
				return
			}
		}
		// The host re-tiers the shared record while the consumer's outcome check is running.
		current, err := f.engine.ReadExperience(ctx, origin, n.ID, 0)
		if err == nil {
			_, err = f.engine.GovernorApply(ctx, origin, GovernorPlan{Tiers: cognition.GovernorTiers{Hot: []string{n.ID}}},
				map[string]int64{n.ID: current.Version}, "host re-tiers the shared record")
		}
		competed <- err
	}()
	actions := []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}
	result, err := f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, actions), AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer run %+v %v", result, err)
	}
	if err := <-competed; err != nil {
		t.Fatalf("competing revision: %v", err)
	}
	if result.Cognition == nil || result.Cognition.Feedback != 1 || len(result.Cognition.Errors) != 0 {
		t.Fatalf("a benign concurrent revision dropped the checked outcome: %+v", result.Cognition)
	}
	settled, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled.Experience.Uses) != 1 {
		t.Fatalf("uses = %d", len(settled.Experience.Uses))
	}
	fb := settled.Experience.Uses[0].Feedback
	if fb == nil || !fb.Used || fb.Outcome != "success" || fb.Observation == nil {
		t.Fatalf("feedback = %+v", fb)
	}
	if settled.Memory == nil || settled.Memory.Tier != "hot" {
		t.Fatalf("the settlement overwrote the competing revision: %+v", settled.Memory)
	}
}
