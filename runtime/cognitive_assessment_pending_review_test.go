package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// FFR-005. Knowledge that becomes valid tomorrow is not usable today, but it is not stale: nothing
// about it expired or was contradicted. The assessment a model decides from must report it as
// unknown (pending), the way the evidence predicate and the Governor projection already do; an
// expired record stays stale.
func TestAssessmentReportsNotYetValidKnowledgeAsPendingNotStale(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "seed", cogAnswer("seed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	now := time.Now().UTC()
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "pending", Kind: "belief", Title: "Valid from tomorrow", Scope: scope,
		Times: CognitiveTimes{Valid: CognitiveValidTime{From: now.Add(24 * time.Hour)}}}, 0)
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "expired", Kind: "belief", Title: "Expired yesterday", Scope: scope,
		Times: CognitiveTimes{Valid: CognitiveValidTime{From: now.Add(-48 * time.Hour), Until: now.Add(-24 * time.Hour)}}}, 0)
	assessed, err := f.engine.AssessCognition(ctx, owner, "", []string{"pending", "expired"})
	if err != nil || len(assessed.Items) != 2 {
		t.Fatalf("assessment %+v %v", assessed, err)
	}
	pending, expired := assessed.Items[0], assessed.Items[1]
	if pending.Applicability != knowledge.Unknown || pending.Reason != "not_yet_valid" {
		t.Fatalf("not-yet-valid knowledge assessed as %s (%s), want unknown (not_yet_valid)", pending.Applicability, pending.Reason)
	}
	if expired.Applicability != knowledge.Stale || expired.Reason != "expired" {
		t.Fatalf("expired knowledge assessed as %s (%s), want stale (expired)", expired.Applicability, expired.Reason)
	}
	if assessed.Recommendation == "continue" {
		t.Fatal("pending knowledge must not support continuing")
	}
}
