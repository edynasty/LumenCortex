package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func TestCognitionDeliveryFinalizationKeepsSourceQueued(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	a, err := f.engine.NewSession(ctx, SessionOptions{Goal: "inspect service with delivery"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.engine.runAgentWith(ctx, a.ID, cogAnswer("8080"), AgentOptions{Policy: "workspace", MaxSteps: 2,
		Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "config.env"}}}})
	if err != nil || result.Status != "completed" {
		t.Fatalf("source run: %+v %v", result, err)
	}
	version, err := f.engine.store.SourceVersion(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := f.adoptFacts(t, a.ID, portFact("8080"))
	if r.Sources != 1 || r.Qualified != 1 || len(r.Inserted) != 1 {
		t.Fatalf("post-delivery source silently skipped: %+v", r)
	}
	if rec := f.candidate(t, r.Inserted[0]); !strings.Contains(strings.Join(rec.SourceRefs, " "), version) {
		t.Fatalf("candidate not at settled version: %+v", rec)
	}
}

func TestCognitionHostMutationRequeuesFinishedSource(t *testing.T) {
	f := newCognitionFixture(t, true)
	a := f.run(t, "inspect service", cogAnswer("8080"))
	first := f.adoptFacts(t, a, portFact("8080"))
	if first.Qualified != 1 {
		t.Fatalf("initial: %+v", first)
	}
	before := f.state(t)
	if _, err := f.engine.ResetContext(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if v := f.hostView(t, first.Inserted[0]); v.Applicability != knowledge.Stale {
		t.Fatalf("old source did not become stale: %+v", v)
	}
	if st := f.state(t); st.Dirty <= before.Dirty {
		t.Fatalf("host mutation lost source: before=%+v after=%+v", before, st)
	}
	second := f.adoptFacts(t, a, portFact("8080"))
	if second.Sources != 1 || second.Qualified != 1 || second.Duplicates != 0 {
		t.Fatalf("new support not processed: %+v", second)
	}
	if v := f.readOnce(t, second.Inserted[0]); v.Applicability != knowledge.Current {
		t.Fatalf("new support not current: %+v", v)
	}
	if v := f.hostView(t, first.Inserted[0]); v.Applicability != knowledge.Stale {
		t.Fatalf("old version revived: %+v", v)
	}
}
