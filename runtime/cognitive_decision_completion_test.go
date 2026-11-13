package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

func completionSchemaBelief(t *testing.T, f *cognitionFixture, owner, id string, a CognitiveAssertion) CognitiveNode {
	t.Helper()
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	n := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: id, Kind: "belief", Title: "service configuration", Scope: scope, Assertion: &a}, 0)
	n, err := f.engine.VerifyCognitiveNode(context.Background(), owner, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Evidence) != 1 || n.Evidence[0].Verification != "passed" {
		t.Fatalf("schema did not produce a real passing observation: %+v", n)
	}
	return n
}

func TestCompletionAssessmentCombinesEvidenceWithoutOrderDependence(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "observe service configuration", cogAnswer("observed"))
	f.write(t, "port.schema.json", `{"type":"object","properties":{"port":{"const":"8080"}},"required":["port"]}`)
	f.write(t, "port.data.json", `{"port":"8080","generation":1}`)
	a := CognitiveAssertion{Kind: knowledge.AssertConfigValue, Target: "port.data.json", Key: "port", Value: "8080"}
	f.setChecks(t, `{"schema":{"kind":"schema","schema":"port.schema.json","files":["port.data.json"],"required":false}}`, map[string]knowledge.Assertion{"schema": a})
	old := completionSchemaBelief(t, f, owner, "first-observation", a)
	f.write(t, "port.data.json", `{"port":"8080","generation":2}`)
	fresh := completionSchemaBelief(t, f, owner, "fresh-observation", a)
	for _, reverse := range []bool{false, true} {
		n := fresh
		n.ID = "combined-new-last"
		n.Version = 1
		n.Evidence = []CognitiveEvidence{old.Evidence[0], fresh.Evidence[0]}
		if reverse {
			n.ID = "combined-old-last"
			slices.Reverse(n.Evidence)
		}
		// Native persistence accepts only the actual qualified observations produced
		// above; this models a multi-observation record, not invented PASS evidence.
		if err := f.engine.store.PutCognitiveNode(ctx, n, 0, f.engine.projectID()); err != nil {
			t.Fatal(err)
		}
		got, err := f.engine.AssessCognition(ctx, owner, "configuration", []string{n.ID})
		if err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(got.Items, func(item CognitiveAssessmentItem) bool { return item.ID == n.ID })
		if index < 0 || got.Items[index].Applicability != knowledge.Current || got.Items[index].Kind != "fact" {
			t.Errorf("evidence order changed current support (reverse=%v): %+v", reverse, got)
		}
	}
}

func TestCompletionAssessmentIncludesConflictsTheModelDidNotSelect(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	owner := f.run(t, "conflicting service claims", cogAnswer("observed"))
	f.write(t, "object.schema.json", `{"type":"object"}`)
	f.write(t, "object.data.json", `{}`)
	f.setChecks(t, `{"left":{"kind":"schema","schema":"object.schema.json","files":["object.data.json"],"required":false},"right":{"kind":"schema","schema":"object.schema.json","files":["object.data.json"],"required":false}}`, map[string]knowledge.Assertion{"left": portFact("8080"), "right": portFact("9090")})
	left := completionSchemaBelief(t, f, owner, "left-claim", portFact("8080"))
	right := completionSchemaBelief(t, f, owner, "right-claim", portFact("9090"))
	reader := expNewSession(t, f, "decide service port")
	p := &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("assess_cognition", `{"query":"service port","ids":["left-claim"],"action":"continue"}`), nil
		}
		var got CognitiveAssessment
		if err := json.Unmarshal([]byte(cogToolResult(t, req)), &got); err != nil {
			t.Fatal(err)
		}
		if got.RequestedAction != "continue" || got.Recommendation != "gather_evidence" || got.Incomplete {
			t.Fatalf("unsupported continuation: %+v", got)
		}
		for _, id := range []string{left.ID, right.ID} {
			i := slices.IndexFunc(got.Items, func(item CognitiveAssessmentItem) bool { return item.ID == id })
			if i < 0 || got.Items[i].Applicability != knowledge.Conflict || got.Items[i].Kind == "fact" {
				t.Fatalf("conflicting proposition hidden or presented as fact: %+v", got)
			}
		}
		return cogFinal("the host's bindings conflict; gather independent evidence"), nil
	}}
	expMustRunSession(t, f, reader, p)
	if got := countRows(t, f.engine, `SELECT count(*) FROM cognition_decisions WHERE session_id=?`, reader); got != 1 {
		t.Fatalf("missing decision receipt: %d", got)
	}
	if got := countRows(t, f.engine, `SELECT count(*) FROM derived_origins WHERE session_id=? AND source_ref=?`, reader, owner); got == 0 {
		t.Fatal("decision omitted source ownership")
	}
	if _, err := f.engine.RevokeSource(ctx, SourceSession, owner, "conflict_source_withdrawn"); err != nil {
		t.Fatal(err)
	}
	if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, reader); err != nil || !withdrawn {
		t.Fatalf("decision reader survived source withdrawal: %v %v", withdrawn, err)
	}
}
