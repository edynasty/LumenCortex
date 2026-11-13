package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestCompletionReopenFinishesMemoryWithoutReplayingActions(t *testing.T) {
	engine, root := cognitiveOpenUpgradeProject(t)
	f := &cognitionFixture{engine: engine, root: root}
	t.Cleanup(func() { _ = f.engine.Close() })
	ctx := context.Background()
	n, _ := expValidated(t, f, "recover applied build marker")
	if err := engine.EnableCognition(ctx); err != nil {
		t.Fatal(err)
	}
	consumer := expNewSession(t, f, "consumer interrupted during memory settlement")
	adoption, err := engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	faultSQL(t, engine, `CREATE TRIGGER completion_fault BEFORE INSERT ON cognition_nodes WHEN NEW.kind='experience' AND json_type(NEW.json,'$.experience.uses[0].feedback')='object' BEGIN SELECT RAISE(ABORT,'completion feedback fault'); END`)
	result, err := engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}), AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" || result.Cognition == nil || len(result.Cognition.Errors) == 0 {
		t.Fatalf("memory fault masked or changed task result %+v %v", result, err)
	}
	before := countRows(t, engine, `SELECT COUNT(*) FROM derived_origins WHERE session_id=? AND kind='execution'`, consumer)
	faultSQL(t, engine, `DROP TRIGGER completion_fault`)
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	f.engine = reopened
	recovered, err := reopened.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fb := recovered.Experience.Uses[0].Feedback; fb == nil || fb.Outcome != "success" || !fb.Used {
		t.Fatalf("missing recovered feedback %+v", fb)
	}
	if after := countRows(t, reopened, `SELECT COUNT(*) FROM derived_origins WHERE session_id=? AND kind='execution'`, consumer); after != before {
		t.Fatalf("recovery replayed action: %d -> %d", before, after)
	}
}

func TestCompletionPendingApplicationStaysUnknownAcrossReopen(t *testing.T) {
	engine, root := cognitiveOpenUpgradeProject(t)
	f := &cognitionFixture{engine: engine, root: root}
	t.Cleanup(func() { _ = f.engine.Close() })
	ctx := context.Background()
	n, _ := expValidated(t, f, "pending application is never replayed")
	if err := engine.EnableCognition(ctx); err != nil {
		t.Fatal(err)
	}
	consumer := expNewSession(t, f, "pending application")
	adoption, err := engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	faultSQL(t, engine, `CREATE TRIGGER application_fault BEFORE UPDATE ON cognition_applications BEGIN SELECT RAISE(ABORT,'application settlement fault'); END`)
	result, err := engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}), AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Cognition == nil || len(result.Cognition.Errors) == 0 {
		t.Fatalf("application fault hidden %+v %v", result, err)
	}
	before := countRows(t, engine, `SELECT COUNT(*) FROM derived_origins WHERE session_id=? AND kind='execution'`, consumer)
	faultSQL(t, engine, `DROP TRIGGER application_fault`)
	if err := os.Remove(filepath.Join(root, "build/reused")); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	f.engine = reopened
	receipt, err := reopened.store.ExperienceApplication(ctx, consumer, adoption.ID)
	if err != nil || receipt.State != "unknown" {
		t.Fatalf("pending receipt %+v %v", receipt, err)
	}
	got, err := reopened.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fb := got.Experience.Uses[0].Feedback; fb == nil || fb.Used || fb.Outcome != "unknown" {
		t.Fatalf("pending became success %+v", fb)
	}
	if _, err := os.Stat(filepath.Join(root, "build/reused")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery repeated effect: %v", err)
	}
	if after := countRows(t, reopened, `SELECT COUNT(*) FROM derived_origins WHERE session_id=? AND kind='execution'`, consumer); after != before {
		t.Fatal("recovery admitted an action")
	}
}

func TestCompletionRecallTelemetryIsNotSharedEvidence(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, _ := expValidated(t, f, "build marker procedure")
	reader := expNewSession(t, f, "recall build marker")
	other := expNewSession(t, f, "another reader")
	p := &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("recall_experiences", `{"query":"build marker"}`), nil
		}
		var view struct {
			Matches []ExperienceMatch `json:"matches"`
		}
		if err := json.Unmarshal([]byte(cogToolResult(t, req)), &view); err != nil || len(view.Matches) != 1 {
			t.Fatalf("recall result %+v %v", view, err)
		}
		return cogFinal("candidate read only"), nil
	}}
	expMustRunSession(t, f, reader, p)
	info, err := f.engine.CognitiveMemory(ctx, reader, n.ID)
	if err != nil || info.Access.Count != 1 || info.Access.LastAccess.IsZero() || info.Outcomes.Adoptions != 0 {
		t.Fatalf("access became use %+v %v", info, err)
	}
	private, err := f.engine.CognitiveMemory(ctx, other, n.ID)
	if err != nil || private.Access.Count != 0 || private.Outcomes.Successes != 0 {
		t.Fatalf("private usage leaked %+v %v", private, err)
	}
	before, err := f.engine.ReadExperience(ctx, reader, n.ID, 0)
	if err != nil || before.Experience.State != knowledge.ExperienceValidated || before.Version != n.Version {
		t.Fatalf("attention mutated evidence %+v %v", before, err)
	}
}
