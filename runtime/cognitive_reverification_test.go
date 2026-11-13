package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCognitiveReverificationPreservesOriginalInvalidation(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "verify service", cogAnswer("observed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	a := portFact("9090")
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "recheck", Kind: "belief", Title: "Service port", Scope: scope, Assertion: &a}, 0)
	first, err := f.engine.VerifyCognitiveNode(ctx, owner, "recheck", 1)
	if err != nil || len(first.Evidence) != 1 || first.Evidence[0].Verification != "failed" {
		t.Fatalf("first failure: %+v %v", first, err)
	}
	second, err := f.engine.VerifyCognitiveNode(ctx, owner, "recheck", 2)
	if err != nil || second.Version != 3 || !second.Times.InvalidatedAt.Equal(first.Times.InvalidatedAt) || second.Evidence[0].Verification != "failed" {
		t.Fatalf("second failure: %+v %v", second, err)
	}
	writeFile(t, filepath.Join(f.root, "defaults.env"), "BASE=9090\n")
	approveChecks(t, f.engine)
	f.bindPorts(t)
	passed, err := f.engine.VerifyCognitiveNode(ctx, owner, "recheck", 3)
	if err != nil || passed.Evidence[0].Verification != "passed" || !passed.Times.InvalidatedAt.Equal(first.Times.InvalidatedAt) {
		t.Fatalf("pass after invalidation: %+v %v", passed, err)
	}
	writeFile(t, filepath.Join(f.root, "defaults.env"), "BASE=8080\n")
	approveChecks(t, f.engine)
	f.bindPorts(t)
	last, err := f.engine.VerifyCognitiveNode(ctx, owner, "recheck", 4)
	if err != nil || last.Version != 5 || last.Evidence[0].Verification != "failed" || !last.Times.InvalidatedAt.Equal(first.Times.InvalidatedAt) {
		t.Fatalf("last failure: %+v %v", last, err)
	}
	graph, _, err := f.engine.CognitiveGraphSnapshot(ctx)
	if err != nil || graph.Nodes["recheck"].Grade != "hypothesis" || graph.Nodes["recheck"].Metadata["useLimitation"] != "invalidated" {
		t.Fatalf("stale PASS retained: %+v %v", graph, err)
	}
	original, err := f.engine.ReadCognitiveNode(ctx, owner, "recheck", 2)
	if err != nil {
		t.Fatal(err)
	}
	if original.Evidence[0].ID != first.Evidence[0].ID || original.Evidence[0].Verification != "failed" {
		t.Fatal("original failed observation rewritten")
	}
	invalid := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "host-invalid", Kind: "belief", Title: "Invalid", Scope: scope, Status: "invalid"}, 0)
	invalid.Title = "Invalid revised"
	revised, err := f.engine.PutCognitiveNode(ctx, owner, invalid, 1)
	if err != nil || revised.Version != 2 || !revised.Times.InvalidatedAt.Equal(invalid.Times.InvalidatedAt) {
		t.Fatalf("host invalid revision: %+v %v", revised, err)
	}
}
