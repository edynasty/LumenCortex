package runtime

import (
	"context"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func TestCompletionAttentionSelectsTaskBeforeLocatorBudget(t *testing.T) {
	f := newCognitionFixture(t, true)
	source := f.run(t, "inspect service port", cogAnswer("service listens on 8080"))
	adopted := f.adoptFacts(t, source, portFact("8080"))
	if adopted.Qualified != 1 {
		t.Fatalf("origin not qualified: %+v", adopted)
	}
	unrelated := cogAnswer("unrelated variable renamed")
	f.run(t, "rename a symbol in an unrelated module", unrelated)
	if row := indexRow(unrelated.request(t, 0)); row != "" {
		t.Fatalf("unrelated knowledge selected: %q", row)
	}
	related := cogAnswer("service port available")
	f.run(t, "use the service port", related)
	if got := startApplicability(related.request(t, 0), adopted.Inserted[0]); got != knowledge.Current {
		t.Fatalf("relevant checked knowledge missing: %s", got)
	}
	// The same principle applies to explicit Experience recall. Shared tokens
	// select the observed procedure without requiring its whole original title.
	n, _ := expValidated(t, f, "build marker procedure")
	reader := expNewSession(t, f, "reuse marker")
	matches, err := f.engine.RecallExperiences(context.Background(), reader, "marker build", 8)
	if err != nil || len(matches) != 1 || matches[0].ID != n.ID {
		t.Fatalf("related experience not recalled: %+v %v", matches, err)
	}
	matches, err = f.engine.RecallExperiences(context.Background(), reader, "unrelated encryption policy", 8)
	if err != nil || len(matches) != 0 {
		t.Fatalf("unrelated experience recalled: %+v %v", matches, err)
	}
}
