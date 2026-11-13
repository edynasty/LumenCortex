package runtime

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// One project crosses the original six boundaries rather than replacing them
// with independent happy paths: correction, dynamic failure, withdrawal,
// independent B/C support, repeat dedupe, privacy, and a byte-bounded backlog.
func TestCognitionR2CompoundRecovery(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	t.Setenv("LC_R1_PORT", "8080")
	f.setChecks(t, freshChecks, map[string]knowledge.Assertion{"port": portFact("8080")})
	a := f.run(t, "source A", cogAnswer("8080"))
	first := f.adoptFacts(t, a, portFact("8080"))
	if first.Qualified != 1 {
		t.Fatalf("A: %+v", first)
	}
	old := first.Inserted[0]
	if got := f.readOnce(t, old); got.Applicability != knowledge.Current {
		t.Fatalf("initial A: %+v", got)
	}
	initialObservations := f.observations(t, old)
	if _, err := f.engine.runAgentWith(ctx, a, cogAnswer("correction: port is 9090"), AgentOptions{Policy: "workspace", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if got := f.readOnce(t, old); got.Applicability != knowledge.Stale {
		t.Fatalf("corrected A: %+v", got)
	}
	wrong := f.adoptFacts(t, a, portFact("9090"))
	if wrong.Qualified != 0 || wrong.CandidateOnly != 1 {
		t.Fatalf("wrong claim accepted by unrelated 8080 check: %+v", wrong)
	}
	if _, err := f.engine.RevokeSource(ctx, SourceSession, a, "owner_withdrawal"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"B", "C"} {
		source := f.run(t, "independent source "+name, cogAnswer("8080"))
		r := f.adoptFacts(t, source, portFact("8080"))
		if r.Qualified != 1 || r.Duplicates != 0 || len(r.Inserted) != 1 {
			t.Fatalf("%s support lost to old dedupe: %+v", name, r)
		}
		id := r.Inserted[0]
		if got := f.readOnce(t, id); got.Applicability != knowledge.Current {
			t.Fatalf("%s current: %+v", name, got)
		}
		if _, err := f.engine.ResetContext(ctx, source); err != nil {
			t.Fatal(err)
		}
		replaced := f.adoptFacts(t, source, portFact("8080"))
		if replaced.Qualified != 1 || replaced.Duplicates != 0 {
			t.Fatalf("%s version support: %+v", name, replaced)
		}
		if got := f.hostView(t, id); got.Applicability != knowledge.Stale {
			t.Fatalf("old %s revived: %+v", name, got)
		}
		// A repeated event of exactly the same terminal version may advance the
		// dirty counter, but must create no second support or observation.
		status := "completed"
		if err := f.engine.store.Update(ctx, source, session.Patch{Status: &status}); err != nil {
			t.Fatal(err)
		}
		if repeat := f.maintain(t); len(repeat.Inserted) != 0 {
			t.Fatalf("same %s version adopted twice: %+v", name, repeat)
		}
		if name == "B" {
			t.Setenv("LC_R1_PORT", "9090")
			if got := f.readOnce(t, replaced.Inserted[0]); got.Applicability != knowledge.Stale {
				t.Fatalf("B reused old PASS: %+v", got)
			}
			t.Setenv("LC_R1_PORT", "8080")
			if got := f.readOnce(t, replaced.Inserted[0]); got.Applicability != knowledge.Current {
				t.Fatalf("B failed to recover: %+v", got)
			}
		}
		if _, err := f.engine.RevokeSource(ctx, SourceSession, source, "owner_withdrawal"); err != nil {
			t.Fatal(err)
		}
		if got := f.hostView(t, replaced.Inserted[0]); got.Applicability != knowledge.Withdrawn {
			t.Fatalf("%s not withdrawn: %+v", name, got)
		}
	}
	if got := f.hostView(t, old); got.Applicability != knowledge.Withdrawn {
		t.Fatalf("A history revived: %+v", got)
	}
	observations := f.observations(t, old)
	for _, prior := range initialObservations {
		if !slices.Contains(observations, prior) {
			t.Fatalf("A observation overwritten: %+v", prior)
		}
	}
	const private = "R2-COMPOUND-PRIVATE-d89eb49"
	p := f.run(t, "private source", cogAnswer("private note "+private))
	f.propose(func(refs []string) []string {
		return []string{candidateJSON("procedure", "private note", private, refOf(t, refs, p), nil)}
	})
	privateReport := f.maintain(t)
	f.run(t, "read private note", reader(privateReport.Inserted[0]))
	if _, err := f.engine.DeleteSession(ctx, p, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if found := textAtRest(t, f.engine.store.Path(), private); len(found) != 0 {
		t.Fatalf("private recovery bytes: %v", found)
	}
	f.answer = func([]string) string { return `{"candidates":[]}` }
	large := f.largeBacklog(t, 9)
	for i := 0; ; i++ {
		state := f.state(t)
		if state.Cursor == state.Dirty {
			break
		}
		if i > len(large)+2 {
			t.Fatalf("compound backlog stuck: %+v", state)
		}
		f.maintain(t)
	}
	for _, input := range f.batchInputs() {
		withinCap(t, input)
	}
	f.committedOnce(t, large)
	f.supportedOnly(t)
	if got := f.hostView(t, privateReport.Inserted[0]); got.Applicability != knowledge.Withdrawn || strings.Contains(got.Claim, private) {
		t.Fatalf("deleted candidate revived: %+v", got)
	}
	t.Log("A correction + dynamic failure + B/C independent support + repeated-version dedupe + privacy + bounded cursor drain; unsupported adoption=0")
}
