package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// FFR-013. An application that names the origin's tools in another order is refused before any
// action runs. The refusal must say what the consumer got wrong: "not a qualified observation" is
// the answer for an origin trace that cannot be reproduced at all, and a model that reads it gives
// the record up instead of applying the procedure it adopted in the observed order.
func TestExperienceApplicationNamesAReorderedProcedure(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	expInstallChecks(t, f)
	writeFile(t, filepath.Join(f.root, "build/fixture"), "read procedure workspace")
	origin := expNewSession(t, f, "origin that reads then writes")
	result, err := f.engine.runAgentWith(ctx, origin, &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch step {
		case 0:
			return cogCall("read_file", `{"path":"build/fixture"}`), nil
		case 1:
			return cogCall("write_file", `{"path":"build/reused","content":"ready"}`), nil
		default:
			return cogFinal("origin completed"), nil
		}
	}}, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("origin %+v %v", result, err)
	}
	n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("ordered read then write")))
	if n.Experience.State != knowledge.ExperienceValidated || len(n.Experience.Origin.Trace.Steps) != 2 {
		t.Fatalf("fixture %s %+v", n.Experience.State, n.Experience.Origin.Trace)
	}
	consumer := expNewSession(t, f, "consumer that reorders the procedure")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	read := experienceAction{Tool: "read_file", Arguments: map[string]any{"path": "build/fixture"}}
	write := experienceAction{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}
	apply := func(id string, actions ...experienceAction) protocol.ProviderResponse {
		raw, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": adoption.ID, "actions": actions})
		response := cogCall("apply_experience", string(raw))
		response.Message.ToolCalls[0].ID = id
		return response
	}
	var refusal, receipt string
	got, err := f.engine.runAgentWith(ctx, consumer, &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		switch step {
		case 0:
			return apply("reordered", write, read), nil
		case 1:
			refusal = lastToolResult(req, "reordered")
			return apply("observed", read, write), nil
		default:
			receipt = lastToolResult(req, "observed")
			return cogFinal("applied in the observed order"), nil
		}
	}}, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || got.Status != "completed" {
		t.Fatalf("consumer %+v %v", got, err)
	}
	if !strings.Contains(refusal, "application action differs from the observed procedure") || strings.Contains(refusal, session.ErrNotQualified.Error()) {
		t.Fatalf("a reordered procedure was refused as %s", refusal)
	}
	var applied struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(receipt), &applied); err != nil || applied.State != "completed" {
		t.Fatalf("the observed order was not applied after the refusal: %s %v", receipt, err)
	}
	final, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fb := final.Experience.Uses[0].Feedback; fb == nil || fb.Outcome != "success" || !fb.Used {
		t.Fatalf("feedback %+v", fb)
	}
}
