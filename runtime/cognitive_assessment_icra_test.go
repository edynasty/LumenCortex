package runtime

import (
	"context"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// ICRA-003 (Part 1). When a belief's premise has its scope narrowed away from the reader or becomes
// invisible through a co-consumer's withdrawal (without fencing the reader itself), AssessCognition
// eagerly loaded the premise via CognitiveNode and aborted the entire assessment with ErrFenced,
// making the dependency_changed stale transition unreachable and blocking query/ID assessments.
func TestAssessmentMarksNarrowedOrWithdrawnPremiseStaleWithoutAborting(t *testing.T) {
	t.Run("premise scope narrowed to foreign session", func(t *testing.T) {
		f := newCognitionFixture(t, false)
		ctx := context.Background()
		owner := f.run(t, "premise owner", cogAnswer("premise"))
		reader := f.run(t, "dependent reader", cogAnswer("reader"))

		projectScope := cognitiveMustScope(t, f.engine, owner, "project")
		premise := cognitiveMustPut(t, f.engine, owner, CognitiveNode{
			ID:    "icra003-premise",
			Kind:  "belief",
			Title: "shared service topology",
			Scope: projectScope,
		}, 0)

		readerScope := cognitiveMustScope(t, f.engine, reader, "session")
		cognitiveMustPut(t, f.engine, reader, CognitiveNode{
			ID:           "icra003-conclusion",
			Kind:         "belief",
			Title:        "topology dependent routing rule",
			Scope:        readerScope,
			Dependencies: []knowledge.NodeRef{{ID: premise.ID, Version: premise.Version}},
		}, 0)
		cognitiveMustPut(t, f.engine, reader, CognitiveNode{
			ID:    "icra003-independent",
			Kind:  "belief",
			Title: "topology local fallback",
			Scope: readerScope,
		}, 0)

		// Owner narrows the premise from project scope to owner's private session scope.
		ownerSessionScope := cognitiveMustScope(t, f.engine, owner, "session")
		narrowed := premise
		narrowed.Scope = ownerSessionScope
		cognitiveMustPut(t, f.engine, owner, narrowed, premise.Version)

		byID, err := f.engine.AssessCognition(ctx, reader, "", []string{"icra003-conclusion", "icra003-independent"})
		if err != nil {
			t.Fatalf("AssessCognition by ID aborted on narrowed premise: %v", err)
		}
		if slices.ContainsFunc(byID.Items, func(it CognitiveAssessmentItem) bool { return it.ID == premise.ID }) {
			t.Fatalf("narrowed foreign premise leaked into reader's assessment: %+v", byID.Items)
		}
		idx := slices.IndexFunc(byID.Items, func(it CognitiveAssessmentItem) bool { return it.ID == "icra003-conclusion" })
		if idx < 0 || byID.Items[idx].Applicability != knowledge.Stale || byID.Items[idx].Reason != "dependency_changed" {
			t.Fatalf("conclusion item = %+v, want stale/dependency_changed", byID.Items)
		}

		byQuery, err := f.engine.AssessCognition(ctx, reader, "topology", nil)
		if err != nil {
			t.Fatalf("AssessCognition by query aborted on narrowed premise: %v", err)
		}
		if len(byQuery.Items) != 2 {
			t.Fatalf("query items = %+v, want 2 reader beliefs without foreign premise", byQuery.Items)
		}
	})

	t.Run("experience premise hidden by co-consumer withdrawal", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		ctx := context.Background()
		exp, _ := expValidated(t, f, "shared experience premise")
		reader := expMustRun(t, f, "reader depending on validated experience", cogAnswer("ok"))
		cognitiveMustPut(t, f.engine, reader, CognitiveNode{
			ID:           "icra003-exp-dependent",
			Kind:         "belief",
			Title:        "deployment note citing validated experience",
			Scope:        cognitiveMustScope(t, f.engine, reader, "project"),
			Dependencies: []knowledge.NodeRef{{ID: exp.ID, Version: exp.Version}},
		}, 0)

		coConsumer := expNewSession(t, f, "co-consumer later withdrawn")
		if _, err := f.engine.AdoptExperience(ctx, coConsumer, exp.ID, exp.Version); err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.RevokeSource(ctx, SourceSession, coConsumer, "withdrawn_consumer"); err != nil {
			t.Fatal(err)
		}

		got, err := f.engine.AssessCognition(ctx, reader, "", []string{"icra003-exp-dependent"})
		if err != nil {
			t.Fatalf("AssessCognition aborted when co-consumer hid Experience premise: %v", err)
		}
		if len(got.Items) != 1 || got.Items[0].Applicability != knowledge.Stale || got.Items[0].Reason != "dependency_changed" {
			t.Fatalf("items = %+v, want single stale/dependency_changed item", got.Items)
		}
	})
}

// ICRA-003 (Part 2). Query-mode AssessCognition matched tokens only against Title and Body, missing
// structured Assertion Target/Key/Value fields that name the actual configuration file or key.
func TestAssessmentQueryMatchesStructuredAssertionFields(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "verify gateway config", cogAnswer("verified"))
	f.write(t, "gateway.schema.json", `{"type":"object","properties":{"listen_port":{"const":"8080"}},"required":["listen_port"]}`)
	f.write(t, "deploy/gateway.json", `{"listen_port":"8080"}`)
	assertion := CognitiveAssertion{
		Kind:   knowledge.AssertConfigValue,
		Target: "deploy/gateway.json",
		Key:    "listen_port",
		Value:  "8080",
	}
	f.setChecks(t, `{"gw":{"kind":"schema","schema":"gateway.schema.json","files":["deploy/gateway.json"],"required":false}}`, map[string]knowledge.Assertion{"gw": assertion})
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	n := cognitiveMustPut(t, f.engine, owner, CognitiveNode{
		ID:        "icra003-assertion-only",
		Kind:      "belief",
		Title:     "service endpoint",
		Body:      "verified configuration",
		Scope:     scope,
		Assertion: &assertion,
	}, 0)
	if _, err := f.engine.VerifyCognitiveNode(ctx, owner, n.ID, n.Version); err != nil {
		t.Fatal(err)
	}

	got, err := f.engine.AssessCognition(ctx, owner, "gateway.json listen_port", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != n.ID || got.Items[0].Applicability != knowledge.Current {
		t.Fatalf("AssessCognition by assertion target/key = %+v, want current %s", got, n.ID)
	}
}
