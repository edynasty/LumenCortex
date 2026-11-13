package runtime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func TestCognitionR2StateTransitionsPreserveHistory(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	t.Setenv("LC_R1_PORT", "8080")
	f.setChecks(t, freshChecks, map[string]knowledge.Assertion{"port": portFact("8080")})
	a := f.run(t, "source", cogAnswer("8080"))
	f.propose(func(refs []string) []string {
		return []string{factJSON("port", []string{refOf(t, refs, a)}, []string{"config.env"}, portFact("8080"))}
	})
	r := f.maintain(t)
	if r.Qualified != 1 {
		t.Fatalf("qualified adoption: %+v", r)
	}
	id := r.Inserted[0]
	initial := f.candidate(t, id)
	first := f.observations(t, id)[0]
	if got := f.hostView(t, id); got.Status != knowledge.StatusQualified || got.Applicability != knowledge.Unknown {
		t.Fatalf("qualified != current outside run: %+v", got)
	}
	if got := f.readOnce(t, id); got.Applicability != knowledge.Current {
		t.Fatalf("unknown -> current: %+v", got)
	}
	t.Setenv("LC_R1_PORT", "9090")
	if got := f.readOnce(t, id); got.Applicability != knowledge.Stale {
		t.Fatalf("current -> stale: %+v", got)
	}
	t.Setenv("LC_R1_PORT", "8080")
	if got := f.readOnce(t, id); got.Applicability != knowledge.Current {
		t.Fatalf("stale -> current: %+v", got)
	}
	path := filepath.Join(f.root, "config.env")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := f.hostView(t, id); got.Applicability != knowledge.SourceMissing {
		t.Fatalf("current -> source_missing: %+v", got)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.readOnce(t, id); got.Applicability != knowledge.Current {
		t.Fatalf("source_missing -> current after file restoration: %+v", got)
	}
	if _, err := f.engine.RevokeSource(ctx, SourceSession, a, "owner_withdrawal"); err != nil {
		t.Fatal(err)
	}
	if got := f.readOnce(t, id); got.Applicability != knowledge.Withdrawn || got.Claim != "" {
		t.Fatalf("withdrawn -> current forbidden: %+v", got)
	}
	if got := f.candidate(t, id); got.Status != initial.Status || got.Claim != initial.Claim {
		t.Fatalf("observation rewrote historical adoption: %+v", got)
	}
	observations := f.observations(t, id)
	if len(observations) < 4 || !slices.Contains(observations, first) {
		t.Fatalf("observations not append-only: %+v", observations)
	}
	b := f.run(t, "independent free-text source", cogAnswer("8080"))
	f.propose(func(refs []string) []string {
		return []string{candidateJSON("fact", "port", "8080 is probably right", refOf(t, refs, b), nil)}
	})
	free := f.maintain(t)
	if free.CandidateOnly != 1 {
		t.Fatalf("free text: %+v", free)
	}
	if got := f.readOnce(t, free.Inserted[0]); got.Applicability != knowledge.CandidateOnly {
		t.Fatalf("candidate_only -> current without support forbidden: %+v", got)
	}
	t.Log("qualified/unknown/current/stale/source_missing/withdrawn/candidate_only transitions; conflict covered by TestCognitionConflictAndWithdrawal; historical observations retained")
}
