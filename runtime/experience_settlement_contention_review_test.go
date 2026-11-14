package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

// FFR-009. A record holds up to MaxExperienceUses uses, and every other consumer's adoption or
// settlement revises it. A settlement that ran its outcome check again on every conflict and gave up
// after four left the uses of a busy record open: a later run of the consumer could only close them as
// unknown, and the checked results were lost. The outcome check observes the consumer's workspace, not
// the record, so it runs once; only the commit is repeated, as often as the writers of a full record
// can conflict.
//
// Before each of the settlement's first commits the host re-tiers the shared record, as the adoptions
// and settlements of the other consumers would revise it.
func TestExperienceSettlementOutlastsEveryWriterOfAFullRecord(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	runs := filepath.Join(t.TempDir(), "runs")
	checks := strings.Replace(experienceChecks, `"command": "test -f build/reused"`, `"command": "echo run >> `+runs+`; test -f build/reused"`, 1)
	if checks == experienceChecks {
		t.Fatal("fixture: the outcome check was not replaced")
	}
	setProjectChecks(t, f.root, checks)
	approveChecks(t, f.engine)
	f.bindPorts(t)
	f.bind(t, expArtifactFact("present"), "reused")
	origin := expOrigin(t, f, "contended build marker procedure")
	n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("contended build marker procedure")))
	if n.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("fixture: state %s", n.Experience.State)
	}
	consumer := expNewSession(t, f, "consumer whose settlement meets every other writer")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	outcomeChecks := func() int {
		raw, err := os.ReadFile(runs)
		if err != nil {
			t.Fatalf("fixture: the outcome check never ran: %v", err)
		}
		return strings.Count(string(raw), "run\n")
	}
	before := outcomeChecks()

	// Every other use of a full record can conflict once as an adoption and once as a settlement.
	writers := 2*knowledge.MaxExperienceUses - 1
	conflicts := 0
	var writerErr error
	testHookBeforeExperienceSettlement = func() {
		if conflicts == writers || writerErr != nil {
			return
		}
		conflicts++
		tiers := cognition.GovernorTiers{Hot: []string{n.ID}}
		if conflicts%2 == 0 {
			tiers = cognition.GovernorTiers{Warm: []string{n.ID}}
		}
		current, err := f.engine.ReadExperience(ctx, origin, n.ID, 0)
		if err == nil {
			_, err = f.engine.GovernorApply(ctx, origin, GovernorPlan{Tiers: tiers}, map[string]int64{n.ID: current.Version}, "another writer revises the shared record")
		}
		writerErr = err
	}
	t.Cleanup(func() { testHookBeforeExperienceSettlement = nil })

	actions := []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}
	result, err := f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, actions), AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer run %+v %v", result, err)
	}
	if writerErr != nil {
		t.Fatalf("competing revision: %v", writerErr)
	}
	if result.Cognition == nil || result.Cognition.Feedback != 1 || len(result.Cognition.Errors) != 0 {
		t.Fatalf("%d of %d competing revisions dropped the checked outcome: %+v", conflicts, writers, result.Cognition)
	}
	if conflicts != writers {
		t.Fatalf("fixture: %d of %d competing revisions ran", conflicts, writers)
	}
	if ran := outcomeChecks() - before; ran != 1 {
		t.Fatalf("one settlement ran its outcome check %d times", ran)
	}
	settled, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled.Experience.Uses) != 1 {
		t.Fatalf("uses = %d", len(settled.Experience.Uses))
	}
	if fb := settled.Experience.Uses[0].Feedback; fb == nil || !fb.Used || fb.Outcome != "success" || fb.Observation == nil {
		t.Fatalf("feedback = %+v", fb)
	}
	if settled.Memory == nil || settled.Memory.Tier != "hot" {
		t.Fatalf("the settlement overwrote the last competing revision: %+v", settled.Memory)
	}
}

// FFR-009 under real concurrency: every consumer of a full record ends its run at the same time, and
// each checked result is recorded by the run that produced it.
func TestExperienceSettlesEveryConcurrentConsumerOfAFullRecord(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, _ := expValidated(t, f, "build marker procedure every consumer reuses")
	type consumer struct {
		id, adoption string
		result       AgentResult
		err          error
	}
	consumers := make([]consumer, knowledge.MaxExperienceUses)
	version := n.Version
	for i := range consumers {
		consumers[i].id = expNewSession(t, f, "consumer of a full record")
		adoption, err := f.engine.AdoptExperience(ctx, consumers[i].id, n.ID, version)
		if err != nil {
			t.Fatal(err)
		}
		consumers[i].adoption, version = adoption.ID, adoption.Version
	}
	actions := []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range consumers {
		wg.Add(1)
		go func(c *consumer) {
			defer wg.Done()
			apply, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": c.adoption, "actions": actions})
			provider := &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if step == 0 {
					return cogCall("apply_experience", string(apply)), nil
				}
				return cogFinal("applied"), nil
			}}
			<-start
			c.result, c.err = f.engine.runAgentWith(ctx, c.id, provider, AgentOptions{MaxSteps: 6, Policy: "workspace"})
		}(&consumers[i])
	}
	close(start)
	wg.Wait()
	for i, c := range consumers {
		if c.err != nil || c.result.Status != "completed" {
			t.Fatalf("consumer %d run %+v %v", i, c.result, c.err)
		}
		if c.result.Cognition == nil || c.result.Cognition.Feedback != 1 || len(c.result.Cognition.Errors) != 0 {
			t.Errorf("consumer %d left its checked outcome unrecorded: %+v", i, c.result.Cognition)
		}
		settled, err := f.engine.ReadExperience(ctx, c.id, n.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(settled.Experience.Uses) != 1 {
			t.Fatalf("consumer %d sees %d uses", i, len(settled.Experience.Uses))
		}
		if fb := settled.Experience.Uses[0].Feedback; fb == nil || !fb.Used || fb.Observation == nil || fb.ApplicationID == "" {
			t.Errorf("consumer %d feedback = %+v", i, fb)
		}
	}
}
