package evaluator

import (
	"context"
	"errors"
	"testing"
)

// S5 TestSatisfies (design verify/go/contracts): only a passed runtime or human result at the current,
// non-empty fingerprint satisfies; editing the check changes the fingerprint; outside git there is none.
// ADR-0017: the result must also carry the approved definition's digest and the current approval
// epoch, and an unapproved spec is never satisfied. It must come from a closed episode whose
// fingerprint did not move, and a runtime pass counts only within the run that closed it (inRun).
func TestSatisfies(t *testing.T) {
	env := Env{Head: "abc", DirtyHash: "", WorktreeID: "local"}
	spec := Spec{ID: "go-test", Kind: KindTest, Command: "go test ./...", ApprovalEpoch: 3}
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
	digest := SpecDigest(spec)
	approved := func(r Result) Result {
		r.SpecDigest, r.ApprovalEpoch = digest, 3
		r.EpisodeID, r.EpisodeStatus, r.PreFingerprint, r.PostFingerprint = "ep_1", EpisodeClosed, r.Fingerprint, r.Fingerprint
		return r
	}
	episode := func(r Result, edit func(*Result)) Result { r = approved(r); edit(&r); return r }
	pass := Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: now}
	human := Result{Status: StatusPassed, Provenance: "human:desktop-user", Fingerprint: now}
	unapproved := spec
	unapproved.ApprovalEpoch = 0
	cases := []struct {
		name    string
		r       Result
		current string
		spec    Spec
		inRun   bool
		want    bool
	}{
		{"in-run runtime pass", approved(pass), now, spec, true, true},
		{"human acceptance", approved(human), now, spec, true, true},
		{"llm pass", approved(Result{Status: StatusPassed, Provenance: "llm:deepseek/chat", Fingerprint: now}), now, spec, true, false},
		{"old fingerprint", approved(Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: "old+@local#00"}), now, spec, true, false},
		{"edited check", approved(pass), edited, spec, true, false},
		{"failed", approved(Result{Status: StatusFailed, Provenance: ProvenanceRuntime, Fingerprint: now}), now, spec, true, false},
		{"model provenance", approved(Result{Status: StatusPassed, Provenance: "model", Fingerprint: now}), now, spec, true, false},
		{"human without id", approved(Result{Status: StatusPassed, Provenance: "human:", Fingerprint: now}), now, spec, true, false},
		{"outside git", approved(Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: ""}), "", spec, true, false},
		// ADR-0017: a result from before approvals, of an older epoch, of another definition, or for a
		// spec that is not approved now never satisfies.
		{"no approval", Result{Status: StatusPassed, Provenance: ProvenanceRuntime, Fingerprint: now}, now, spec, true, false},
		{"older epoch", episode(human, func(r *Result) { r.ApprovalEpoch = 2 }), now, spec, true, false},
		{"other definition", episode(pass, func(r *Result) { r.SpecDigest = "sha256:other" }), now, spec, true, false},
		{"spec unapproved now", approved(pass), now, unapproved, true, false},
		// Episodes: a runtime pass from another run, a row from before episodes, an episode that did not
		// close, and one whose fingerprint moved never satisfy; a person's acceptance holds across runs.
		{"runtime pass from another run", approved(pass), now, spec, false, false},
		{"human acceptance across runs", approved(human), now, spec, false, true},
		{"no episode", episode(pass, func(r *Result) { r.EpisodeID, r.EpisodeStatus = "", "" }), now, spec, true, false},
		{"episode left open", episode(pass, func(r *Result) { r.EpisodeStatus = EpisodeOpen }), now, spec, true, false},
		{"scope changed", episode(pass, func(r *Result) { r.EpisodeStatus = EpisodeScopeChanged }), now, spec, true, false},
		{"spec changed in the episode", episode(pass, func(r *Result) { r.EpisodeStatus = EpisodeSpecUnapproved }), now, spec, true, false},
		{"interrupted", episode(human, func(r *Result) { r.EpisodeStatus = EpisodeInterrupted }), now, spec, true, false},
		{"post fingerprint moved", episode(pass, func(r *Result) { r.PostFingerprint = "abc+dirty@local#00" }), now, spec, true, false},
		{"pre fingerprint differs", episode(human, func(r *Result) { r.PreFingerprint = "" }), now, spec, true, false},
	}
	for _, c := range cases {
		if got := Satisfies(c.r, c.current, c.spec, c.inRun); got != c.want {
			t.Errorf("%s: Satisfies=%v want %v", c.name, got, c.want)
		}
	}
}

// ADR-0017 digests: a definition's digest covers every field that changes what runs or whether it is
// required, and not the approval epoch; the set digest is order-independent and the empty set has one
// that no other set shares.
func TestSpecAndSetDigests(t *testing.T) {
	base := Spec{ID: "go-test", Kind: KindTest, Command: "go test ./...", Timeout: 60e9, Required: true, Source: "workspace"}
	d := SpecDigest(base)
	if len(d) != len("sha256:")+64 || d[:7] != "sha256:" {
		t.Fatalf("digest %q", d)
	}
	withEpoch := base
	withEpoch.ApprovalEpoch = 9
	if SpecDigest(withEpoch) != d {
		t.Fatal("the approval epoch changed the definition digest")
	}
	edits := map[string]func(*Spec){
		"command":  func(s *Spec) { s.Command = "true" },
		"kind":     func(s *Spec) { s.Kind = KindBuild },
		"schema":   func(s *Spec) { s.Schema = "x.json" },
		"files":    func(s *Spec) { s.Files = []string{"a"} },
		"timeout":  func(s *Spec) { s.Timeout = 1e9 },
		"required": func(s *Spec) { s.Required = false },
		"source":   func(s *Spec) { s.Source = "global" },
		"id":       func(s *Spec) { s.ID = "go-vet" },
	}
	for name, edit := range edits {
		s := base
		edit(&s)
		if SpecDigest(s) == d {
			t.Errorf("editing %s kept the digest", name)
		}
	}
	other := Spec{ID: "a-lint", Kind: KindBuild, Command: "lint"}
	if SetDigest([]Spec{base, other}) != SetDigest([]Spec{other, base}) {
		t.Fatal("the set digest depends on order")
	}
	empty := SetDigest(nil)
	if empty == SetDigest([]Spec{base}) || empty != SetDigest([]Spec{}) {
		t.Fatal("the empty set digest is not its own")
	}
	optional := base
	optional.Required = false
	if SetDigest([]Spec{base}) == SetDigest([]Spec{optional}) {
		t.Fatal("making the only required check optional kept the set digest")
	}
}

// S5 TestCheckVerdict: a satisfying stored result decides without running; anything else runs the check
// and the fresh runtime result decides, with or without a fingerprint. An unapproved spec never runs.
// A stored runtime pass is reused only in the run that closed its episode.
func TestCheckVerdict(t *testing.T) {
	const now = "abc+@local#01"
	spec := Spec{ID: "go-test", Kind: KindTest, Command: "go test ./...", ApprovalEpoch: 1}
	digest := SpecDigest(spec)
	runs := 0
	run := func(status string) func() (Result, error) {
		return func() (Result, error) {
			runs++
			return Result{Status: status, Provenance: ProvenanceRuntime, Fingerprint: now}, nil
		}
	}
	closed := func(status, provenance, fingerprint string) *Result {
		return &Result{Status: status, Provenance: provenance, Fingerprint: fingerprint, SpecDigest: digest, ApprovalEpoch: 1,
			EpisodeID: "ep_1", EpisodeStatus: EpisodeClosed, PreFingerprint: fingerprint, PostFingerprint: fingerprint}
	}
	cases := []struct {
		name     string
		stored   *Result
		current  string
		inRun    bool
		fresh    string
		want     bool
		wantRuns int
	}{
		{"human acceptance is reused", closed(StatusPassed, "human:desktop-user", now), now, false, StatusFailed, true, 0},
		{"in-run runtime pass is reused", closed(StatusPassed, ProvenanceRuntime, now), now, true, StatusFailed, true, 0},
		{"another run's runtime pass reruns", closed(StatusPassed, ProvenanceRuntime, now), now, false, StatusFailed, false, 1},
		{"stored failure reruns", closed(StatusFailed, ProvenanceRuntime, now), now, true, StatusPassed, true, 1},
		{"stored pass for an old fingerprint reruns", closed(StatusPassed, ProvenanceRuntime, "old"), now, true, StatusFailed, false, 1},
		{"llm pass never counts", closed(StatusPassed, "llm:p/m", now), now, true, StatusFailed, false, 1},
		{"outside git a fresh pass counts", nil, "", false, StatusPassed, true, 1},
		{"outside git a fresh failure blocks", nil, "", false, StatusFailed, false, 1},
		{"fresh error blocks", nil, now, false, StatusError, false, 1},
	}
	for _, c := range cases {
		runs = 0
		if _, ok, err := CheckVerdict(c.stored, c.current, spec, c.inRun, run(c.fresh)); err != nil || ok != c.want || runs != c.wantRuns {
			t.Errorf("%s: ok=%v runs=%d err=%v, want ok=%v runs=%d", c.name, ok, runs, err, c.want, c.wantRuns)
		}
	}
	// The caller's cancellation decides nothing.
	if _, ok, err := CheckVerdict(nil, now, spec, false, func() (Result, error) { return Result{}, context.Canceled }); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run: ok=%v err=%v", ok, err)
	}
	// ADR-0017: an unapproved spec is not run and does not pass, whatever is stored.
	runs = 0
	unapproved := spec
	unapproved.ApprovalEpoch = 0
	if r, ok, err := CheckVerdict(closed(StatusPassed, ProvenanceRuntime, now), now, unapproved, true, run(StatusPassed)); err != nil || ok || runs != 0 || r.Status != StatusSpecUnapproved {
		t.Fatalf("unapproved: status=%s ok=%v runs=%d err=%v", r.Status, ok, runs, err)
	}
}
