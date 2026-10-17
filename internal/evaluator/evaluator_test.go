package evaluator

import (
	"context"
	"errors"
	"testing"
)

// S5 TestSatisfies (design verify/go/contracts): only a passed runtime or human result at the current,
// non-empty fingerprint satisfies; editing the check changes the fingerprint; outside git there is none.
func TestSatisfies(t *testing.T) {
	env := Env{Head: "abc", DirtyHash: "", WorktreeID: "local"}
	spec := Spec{ID: "go-test", Kind: KindTest, Command: "go test ./..."}
	now := Fingerprint(env, spec)
	edited := Fingerprint(env, Spec{ID: "go-test", Kind: KindTest, Command: "go test -race ./..."})
	if now == "" || edited == now {
		t.Fatalf("fingerprints: now=%q edited=%q", now, edited)
	}
	if fp := Fingerprint(Env{WorktreeID: "local"}, spec); fp != "" {
		t.Fatalf("outside git: %q, want no reusable fingerprint", fp)
	}
	if other := Fingerprint(Env{Head: "abc", WorktreeID: "/wt/s1"}, spec); other == now {
		t.Fatal("a result for the main checkout is valid in a worktree")
	}
	cases := []struct {
		r       Result
		current string
		want    bool
	}{
		{Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: now}, now, true},
		{Result{Status: StatusPassed, Provenance: "human:desktop-user", Fingerprint: now}, now, true},
		{Result{Status: StatusPassed, Provenance: "llm:deepseek/chat", Fingerprint: now}, now, false},
		{Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: "old+@local#00"}, now, false},
		{Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: now}, edited, false},
		{Result{Status: StatusFailed, Provenance: ProvenanceRuntime, Fingerprint: now}, now, false},
		{Result{Status: StatusPassed, Provenance: "model", Fingerprint: now}, now, false},
		{Result{Status: StatusPassed, Provenance: "human:", Fingerprint: now}, now, false},
		{Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: ""}, "", false},
	}
	for i, c := range cases {
		if got := Satisfies(c.r, c.current); got != c.want {
			t.Errorf("case %d: Satisfies=%v want %v", i, got, c.want)
		}
	}
}

// S5 TestCheckVerdict: a satisfying stored result decides without running; anything else runs the check
// and the fresh runtime result decides, with or without a fingerprint.
func TestCheckVerdict(t *testing.T) {
	const now = "abc+@local#01"
	runs := 0
	run := func(status string) func() (Result, error) {
		return func() (Result, error) {
			runs++
			return Result{Status: status, Provenance: ProvenanceRuntime, Fingerprint: now}, nil
		}
	}
	cases := []struct {
		name     string
		stored   *Result
		current  string
		fresh    string
		want     bool
		wantRuns int
	}{
		{"human acceptance is reused", &Result{Status: StatusPassed, Provenance: "human:desktop-user", Fingerprint: now}, now, StatusFailed, true, 0},
		{"runtime pass is reused", &Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: now}, now, StatusFailed, true, 0},
		{"stored failure reruns", &Result{Status: StatusFailed, Provenance: ProvenanceRuntime, Fingerprint: now}, now, StatusPassed, true, 1},
		{"stored pass for an old fingerprint reruns", &Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: "old"}, now, StatusFailed, false, 1},
		{"llm pass never counts", &Result{Status: StatusPassed, Provenance: "llm:p/m", Fingerprint: now}, now, StatusFailed, false, 1},
		{"outside git a fresh pass counts", nil, "", StatusPassed, true, 1},
		{"outside git a fresh failure blocks", nil, "", StatusFailed, false, 1},
		{"fresh error blocks", nil, now, StatusError, false, 1},
	}
	for _, c := range cases {
		runs = 0
		if _, ok, err := CheckVerdict(c.stored, c.current, run(c.fresh)); err != nil || ok != c.want || runs != c.wantRuns {
			t.Errorf("%s: ok=%v runs=%d err=%v, want ok=%v runs=%d", c.name, ok, runs, err, c.want, c.wantRuns)
		}
	}
	// The caller's cancellation decides nothing.
	if _, ok, err := CheckVerdict(nil, now, func() (Result, error) { return Result{}, context.Canceled }); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run: ok=%v err=%v", ok, err)
	}
}
