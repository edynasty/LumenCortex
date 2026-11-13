package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func completionApplyProvider(t *testing.T, n CognitiveNode, adoption string, actions []experienceAction) *cognitionProvider {
	t.Helper()
	return &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			raw, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": adoption, "actions": actions})
			return cogCall("apply_experience", string(raw)), nil
		}
		return cogFinal("procedure attempt finished"), nil
	}}
}

func TestCompletionLearningThroughOrdinaryTools(t *testing.T) {
	f := newCognitionFixture(t, true)
	expInstallChecks(t, f)
	ctx := context.Background()
	proposal := expProposal("build marker procedure")
	p := &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch step {
		case 0:
			return cogCall("write_file", `{"path":"build/reused","content":"ready"}`), nil
		case 1:
			raw, _ := json.Marshal(map[string]any{"proposal": proposal})
			return cogCall("propose_experience", string(raw)), nil
		default:
			return cogFinal("origin executed"), nil
		}
	}}
	origin, result, err := expRunProvider(t, f, "build marker procedure", p)
	if err != nil || result.Status != "completed" || result.Cognition == nil || result.Cognition.Extracted != 1 || len(result.Cognition.Errors) != 0 {
		t.Fatalf("origin completion %+v %v", result, err)
	}
	reader := expNewSession(t, f, "reuse build marker")
	matches, err := f.engine.RecallExperiences(ctx, reader, "build marker", 8)
	if err != nil || len(matches) != 1 {
		t.Fatalf("recall %+v %v", matches, err)
	}
	n, err := f.engine.ReadExperience(ctx, reader, matches[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n.Experience.Origin.Source.SessionID != origin || n.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("origin %+v", n.Experience)
	}
	if err := os.Remove(filepath.Join(f.root, "build/reused")); err != nil {
		t.Fatal(err)
	}
	consumer := &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch step {
		case 0:
			raw, _ := json.Marshal(map[string]any{"id": n.ID, "version": n.Version})
			return cogCall("adopt_experience", string(raw)), nil
		case 1:
			var adopted ExperienceAdoption
			if err := json.Unmarshal([]byte(cogToolResult(t, req)), &adopted); err != nil || adopted.ID == "" {
				t.Fatalf("adoption %s %v", cogToolResult(t, req), err)
			}
			raw, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": adopted.ID, "actions": []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}})
			response := cogCall("apply_experience", string(raw))
			response.Message.ToolCalls[0].ID = "apply_1"
			return response, nil
		default:
			var receipt struct {
				State string `json:"state"`
			}
			content := lastToolResult(req, "apply_1")
			if err := json.Unmarshal([]byte(content), &receipt); err != nil || receipt.State != "completed" {
				t.Fatalf("application %s %v", content, err)
			}
			return cogFinal("reused observed procedure"), nil
		}
	}}
	got, err := f.engine.runAgentWith(ctx, reader, consumer, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || got.Status != "completed" || got.Cognition == nil || got.Cognition.Feedback != 1 || len(got.Cognition.Errors) != 0 {
		t.Fatalf("consumer completion %+v %v", got, err)
	}
	final, err := f.engine.ReadExperience(ctx, reader, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	feedback := final.Experience.Uses[0].Feedback
	if feedback == nil || feedback.Outcome != "success" || !feedback.Used || !feedback.ConditionsHeld || feedback.ApplicationID == "" {
		t.Fatalf("feedback %+v", feedback)
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "build/reused")); err != nil || string(data) != "ready" {
		t.Fatalf("action not applied: %q %v", data, err)
	}
	receipt, err := f.engine.store.ExperienceApplication(ctx, reader, final.Experience.Uses[0].ID)
	if err != nil || receipt.State != "completed" || receipt.Actions[0].ExecutionID == "" {
		t.Fatalf("receipt %+v %v", receipt, err)
	}
	other := expNewSession(t, f, "independent reader")
	private, err := f.engine.ReadExperience(ctx, other, n.ID, 0)
	if err != nil || len(private.Experience.Uses) != 0 {
		t.Fatalf("consumer feedback leaked %+v %v", private, err)
	}
}

func TestCompletionApplicationRefusesSubstitutionAndChangedConditions(t *testing.T) {
	for _, mode := range []string{"substitution", "conditions", "policy"} {
		t.Run(mode, func(t *testing.T) {
			f := newCognitionFixture(t, true)
			n, _ := expValidated(t, f, "build marker procedure")
			ctx := context.Background()
			consumer := expNewSession(t, f, "apply build marker")
			adopted, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.root, "build/reused")); err != nil {
				t.Fatal(err)
			}
			actions := []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}
			if mode == "substitution" {
				actions = []experienceAction{{Tool: "read_file", Arguments: map[string]any{"path": "config.env"}}}
			}
			if mode == "conditions" {
				writeFile(t, filepath.Join(f.root, "config.env"), "PORT=9090\n")
				approveChecks(t, f.engine)
				f.bindPorts(t)
				f.bind(t, expArtifactFact("present"), "reused")
			}
			opts := AgentOptions{MaxSteps: 6, Policy: "workspace"}
			if mode == "policy" {
				opts.ToolDenylist = []string{"write_file"}
			}
			_, err = f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adopted.ID, actions), opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(f.root, "build/reused")); !os.IsNotExist(err) {
				t.Fatalf("refused action ran: %v", err)
			}
			got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if fb := got.Experience.Uses[0].Feedback; fb == nil || fb.Outcome != "unknown" || fb.Used {
				t.Fatalf("refused use counted: %+v", fb)
			}
		})
	}
}

func TestCompletionMemoryRetentionDependencyAndLifecycle(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "memory maintenance", cogAnswer("observed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	a := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "premise", Kind: "belief", Title: "service premise", Scope: scope}, 0)
	b := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "dependent", Kind: "belief", Title: "derived service hypothesis", Scope: scope, Dependencies: []CognitiveNodeRef{{ID: a.ID, Version: a.Version}}}, 0)
	if _, err := f.engine.PutCognitiveNode(ctx, owner, CognitiveNode{ID: a.ID, Kind: "belief", Title: "cycle", Scope: scope, Dependencies: []CognitiveNodeRef{{ID: b.ID, Version: b.Version}}}, a.Version); err == nil {
		t.Fatal("dependency cycle accepted")
	}
	a.Title = "revised service premise"
	a, err := f.engine.PutCognitiveNode(ctx, owner, a, a.Version)
	if err != nil {
		t.Fatal(err)
	}
	assessed, err := f.engine.AssessCognition(ctx, owner, "service", []string{b.ID})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(assessed.Items, func(x CognitiveAssessmentItem) bool { return x.ID == b.ID })
	if i < 0 || assessed.Items[i].Applicability != knowledge.Stale || assessed.Recommendation == "continue" {
		t.Fatalf("stale dependency trusted: %+v", assessed)
	}
	for _, n := range []CognitiveNode{
		{ID: "cold", Kind: "entity", Title: "dormant duplicate", Status: "dormant", Scope: scope},
		{ID: "safety", Kind: "belief", Title: "rare safety constraint", Status: "dormant", Scope: scope, Memory: &CognitiveMemoryPolicy{Retention: "safety"}},
		{ID: "negative", Kind: "negative", Title: "failed counterexample", Status: "stale", Scope: scope},
	} {
		cognitiveMustPut(t, f.engine, owner, n, 0)
	}
	maintenance, err := f.engine.MaintainCognitiveMemory(ctx, owner, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(maintenance.Archived, []string{"cold"}) || !maintenance.Pressure || maintenance.Protected < 2 {
		t.Fatalf("retention %+v", maintenance)
	}
	if _, err := f.engine.SetCognitiveLifecycle(ctx, owner, "safety", 1, "archive", "test retention"); err == nil {
		t.Fatal("safety archived")
	}
	restored, err := f.engine.SetCognitiveLifecycle(ctx, owner, "cold", 2, "restore", "host restores record")
	if err != nil || restored.Status != "active" {
		t.Fatalf("restore %+v %v", restored, err)
	}
	revoked, err := f.engine.SetCognitiveLifecycle(ctx, owner, "cold", 3, "revoke", "host withdraws record")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.SetCognitiveLifecycle(ctx, owner, "cold", revoked.Version, "restore", "cannot revive"); err == nil {
		t.Fatal("revoked identity revived")
	}
	if _, err := f.engine.SetCognitiveLifecycle(ctx, owner, "cold", 1, "archive", "stale CAS"); !errors.Is(err, session.ErrCognitiveRevision) {
		t.Fatalf("stale write = %v", err)
	}
}
