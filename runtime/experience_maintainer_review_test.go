package runtime

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// A durable admission is not proof of a completed operation: the path guard refuses
// this write before it acts. The origin's outcome cannot be credited to the consumer.
func TestExperienceFeedbackRefusedWriteIsNotExecution(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	node, _ := expValidated(t, f, "denied execution cannot reuse an experience")
	consumer := expNewSession(t, f, "B adopts but its only write is refused")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, node.ID, node.Version)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() + "/consumer-ran"
	p := expWriteThenFinal(outside, "not permitted", "write was refused")
	expMustRunSession(t, f, consumer, p)
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("refused write created its sentinel or could not be checked: %v", err)
	}
	t.Logf("consumer tool result: %s", cogToolResult(t, p.request(t, 1)))
	origin, err := f.engine.store.ExperienceOrigin(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if len(origin.Trace.Steps) != 1 || origin.Trace.Steps[0].Tool != "write_file" || origin.Trace.Steps[0].Outcome == "completed" {
		t.Fatalf("expected one uncompleted write admission: %+v", origin.Trace)
	}
	fed, err := f.engine.RecordExperienceFeedback(ctx, consumer, node.ID, adoption.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback := fed.Experience.Uses[0].Feedback
	if feedback == nil || feedback.Outcome != "unknown" || feedback.Used || feedback.Observation != nil || feedback.RevisionNeeded {
		t.Fatalf("refused write counted as execution: %+v", feedback)
	}
	if fed.Experience.State != node.Experience.State || fed.Experience.Statistics().Successes != 0 {
		t.Fatalf("refused consumer changed experience state or success count: %+v", fed.Experience)
	}
}

// Read-only procedures need their own executed origin and explicit application;
// reading an existing marker does not prove application of a write procedure.
func TestExperienceFeedbackRecognizesReadOnlyExecution(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	expInstallChecks(t, f)
	writeFile(t, f.root+"/build/reused", "ready")
	origin := expMustRun(t, f, "inspect marker", &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("read_file", `{"path":"build/reused"}`), nil
		}
		return cogFinal("marker inspected"), nil
	}})
	proposal := expProposal("consumer inspects an existing marker")
	proposal.Strategy = []string{"Read build/reused and check the existing marker before deciding."}
	node := expValidate(t, f, origin, expExtract(t, f, origin, proposal))
	consumer := expNewSession(t, f, "B reads the marker")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, node.ID, node.Version)
	if err != nil {
		t.Fatal(err)
	}
	expMustRunSession(t, f, consumer, completionApplyProvider(t, node, adoption.ID, []experienceAction{{Tool: "read_file", Arguments: map[string]any{"path": "build/reused"}}}))
	receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adoption.ID)
	if err != nil || receipt.State != "completed" || receipt.Actions[0].ExecutionID == "" {
		t.Fatalf("read application: %+v %v", receipt, err)
	}
	fed, err := f.engine.RecordExperienceFeedback(ctx, consumer, node.ID, adoption.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback := fed.Experience.Uses[0].Feedback
	if feedback == nil || feedback.Outcome != "success" || !feedback.Used || feedback.Observation == nil || feedback.RevisionNeeded {
		t.Fatalf("real read-only execution was lost: %+v", feedback)
	}
	if !reflect.DeepEqual(fed.Experience.Origin, node.Experience.Origin) || !reflect.DeepEqual(fed.Experience.Validation, node.Experience.Validation) {
		t.Fatal("consumer feedback rewrote the origin or its original validation")
	}
}

// A successful earlier consumer run must not qualify a later answer-only run.
func TestExperienceFeedbackDoesNotReusePreviousRunExecution(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	node, _ := expValidated(t, f, "earlier execution is not the adopted run")
	consumer := expNewSession(t, f, "B executed before adoption")
	expMustRunSession(t, f, consumer, expWriteThenFinal("build/prior", "earlier", "old run done"))
	adoption, err := f.engine.AdoptExperience(ctx, consumer, node.ID, node.Version)
	if err != nil {
		t.Fatal(err)
	}
	expMustRunSession(t, f, consumer, cogAnswer("no new execution"))
	fed, err := f.engine.RecordExperienceFeedback(ctx, consumer, node.ID, adoption.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback := fed.Experience.Uses[0].Feedback
	if feedback == nil || feedback.Outcome != "unknown" || feedback.Used || feedback.Observation != nil || feedback.RevisionNeeded {
		t.Fatalf("an earlier run qualified the answer-only consumer: %+v", feedback)
	}
	if stats := fed.Experience.Statistics(); stats.Adoptions != 1 || stats.Unknown != 1 || stats.Successes != 0 || stats.Failures != 0 {
		t.Fatalf("answer-only feedback changed reuse counts: %+v", stats)
	}
}
