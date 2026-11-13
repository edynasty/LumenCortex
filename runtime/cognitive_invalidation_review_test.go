package runtime

import (
	"context"
	"testing"
	"time"
)

// P0-REVIEW-F03: the framework stamps the first invalidation; a caller can neither backdate or future-date
// one on a record it leaves active, nor replace it. Handing the stored instant back in another
// display zone is the same instant.
func TestCognitiveInvalidationTimeIsFrameworkAssigned(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "seed", cogAnswer("seed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

	if got, err := f.engine.PutCognitiveNode(ctx, owner, CognitiveNode{ID: "backdated", Kind: "belief", Title: "Backdated", Scope: scope,
		Times: CognitiveTimes{InvalidatedAt: past}}, 0); err == nil {
		t.Fatalf("created %s record with a caller invalidation time %s", got.Status, got.Times.InvalidatedAt)
	}
	active := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "active", Kind: "belief", Title: "Active", Scope: scope}, 0)
	scheduled := active
	scheduled.Times.InvalidatedAt = time.Now().UTC().Add(240 * time.Hour)
	if _, err := f.engine.PutCognitiveNode(ctx, owner, scheduled, active.Version); err == nil {
		t.Fatal("an active record accepted a caller invalidation time")
	}

	before := time.Now().UTC()
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "invalid", Kind: "belief", Title: "Invalid", Scope: scope, Status: "invalid"}, 0)
	stored, err := f.engine.ReadCognitiveNode(ctx, owner, "invalid", 0)
	if err != nil || stored.Times.InvalidatedAt.Before(before) || stored.Times.InvalidatedAt.After(time.Now().UTC()) {
		t.Fatalf("framework invalidation stamp = %s, %v", stored.Times.InvalidatedAt, err)
	}
	first := stored.Times.InvalidatedAt

	replaced := stored
	replaced.Times.InvalidatedAt = past
	if _, err := f.engine.PutCognitiveNode(ctx, owner, replaced, stored.Version); err == nil {
		t.Fatal("the first invalidation time was replaced")
	}
	zoned := stored
	zoned.Body = "revised with the same instant shown in UTC+8"
	zoned.Times.InvalidatedAt = first.In(time.FixedZone("UTC+8", 8*3600))
	if _, err := f.engine.PutCognitiveNode(ctx, owner, zoned, stored.Version); err != nil {
		t.Fatalf("the same instant in another display zone was refused: %v", err)
	}
	revised, err := f.engine.ReadCognitiveNode(ctx, owner, "invalid", 0)
	if err != nil || revised.Version != stored.Version+1 || !revised.Times.InvalidatedAt.Equal(first) || revised.Body != zoned.Body {
		t.Fatalf("revision = version %d, invalidatedAt %s, %v", revised.Version, revised.Times.InvalidatedAt, err)
	}
}
