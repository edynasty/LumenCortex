package runtime

import (
	"context"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestCompletionFeedbackRequiresActualExperienceApplication(t *testing.T) {
	f := newCognitionFixture(t, false)
	n, _ := expValidated(t, f, "build marker procedure")
	consumer := expNewSession(t, f, "unrelated configuration inspection")
	adoption, err := f.engine.AdoptExperience(context.Background(), consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	p := &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("read_file", `{"path":"config.env"}`), nil
		}
		return cogFinal("configuration inspected"), nil
	}}
	expMustRunSession(t, f, consumer, p)
	got, err := f.engine.RecordExperienceFeedback(context.Background(), consumer, n.ID, adoption.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback := got.Experience.Uses[0].Feedback
	if feedback == nil || feedback.Used || feedback.Outcome != "unknown" {
		t.Fatalf("unapplied procedure counted as reuse: %+v", feedback)
	}
}
