package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// Remediation R1 of the T5a independent review (bench/evidence/cognitive-core-sol-independent-review-
// 20261109): one regression test per finding, named after the review's reproducer, end to end through
// the real loop, store, checks, approvals and git with scripted models.

// propose sets the maintainer's answer to the entries build makes from the batch's refs.
func (f *cognitionFixture) propose(build func(refs []string) []string) {
	f.answer = func(refs []string) string { return candidates(build(refs)...) }
}

// adoptFacts runs one pass whose maintainer proposes each assertion as a fact derived from src.
func (f *cognitionFixture) adoptFacts(t *testing.T, src string, facts ...knowledge.Assertion) MaintenanceReport {
	t.Helper()
	f.propose(func(refs []string) []string {
		var out []string
		for _, a := range facts {
			out = append(out, factJSON(a.Subject(), []string{refOf(t, refs, src)}, nil, a))
		}
		return out
	})
	return f.maintain(t)
}

// setChecks replaces the project's checks, approves the new set and binds each assertion to its check.
func (f *cognitionFixture) setChecks(t *testing.T, checks string, binds map[string]knowledge.Assertion) {
	t.Helper()
	setProjectChecks(t, f.root, checks)
	approveChecks(t, f.engine)
	for id, a := range binds {
		f.bind(t, a, id)
	}
}

func (f *cognitionFixture) candidate(t *testing.T, id string) session.CandidateRecord {
	t.Helper()
	rec, err := f.engine.store.Candidate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

var indexEntry = regexp.MustCompile(`(kc_[0-9a-f]+) \((\w+), (\w+)`)

// startApplicability is the applicability the cognition index of req gives candidate id, "" when the
// index does not name it.
func startApplicability(req protocol.ProviderRequest, id string) knowledge.Applicability {
	for _, m := range indexEntry.FindAllStringSubmatch(indexRow(req), -1) {
		if m[1] == id {
			return knowledge.Applicability(m[3])
		}
	}
	return ""
}

// scripted answers request n with steps[n], then with a final answer.
func scripted(steps ...func(protocol.ProviderRequest) protocol.ProviderResponse) *cognitionProvider {
	return &cognitionProvider{respond: func(n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n < len(steps) {
			return steps[n](req), nil
		}
		return cogFinal("done"), nil
	}}
}

func readStep(callID, ref string) func(protocol.ProviderRequest) protocol.ProviderResponse {
	return func(protocol.ProviderRequest) protocol.ProviderResponse {
		return cogStep(callID, "read_knowledge", `{"ref":"`+ref+`"}`)
	}
}

// viewIn decodes the read_knowledge result of call callID that request req carries.
func viewIn(t *testing.T, req protocol.ProviderRequest, callID string) KnowledgeView {
	t.Helper()
	out := lastToolResult(req, callID)
	if out == "" {
		t.Fatalf("no result of %s in %+v", callID, req.Messages)
	}
	var v KnowledgeView
	decodeJSON(t, []byte(out), &v)
	return v
}

// freshChecks: "port" holds while the configuration yields 8080 and the external dependency
// LC_R1_PORT, which no workspace fingerprint covers, is 8080 too.
const freshChecks = `{"port": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 8080 && test \"$LC_R1_PORT\" = 8080", "required": false}}`

// P1-01: a qualified observation is fresh only for the reader its freshness class allows. A process
// check may read anything (here an environment variable) its fingerprint does not cover, so its PASS is
// fresh only for the run that made it: every new run establishes its own within the Start budget, an
// unrun or failed revalidation is unknown or stale, and an old PASS is never current elsewhere.
func TestIndependentFreshAtUse(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, true)
	t.Setenv("LC_R1_PORT", "8080")
	f.setChecks(t, freshChecks, map[string]knowledge.Assertion{"port": portFact("8080")})
	a := f.run(t, "inspect the external configuration", cogAnswer("the service listens on 8080"))
	report := f.adoptFacts(t, a, portFact("8080"))
	if report.Qualified != 1 || len(report.Inserted) != 1 {
		t.Fatalf("adoption %+v", report)
	}
	id := report.Inserted[0]
	adoption := f.observations(t, id)
	if len(adoption) != 1 || adoption[0].Outcome != evaluator.StatusPassed || adoption[0].Purpose != session.ObservationAdoption {
		t.Fatalf("adoption observations %+v", adoption)
	}
	fingerprint := adoption[0].Fingerprint

	t.Run("an old PASS is not current for another reader", func(t *testing.T) {
		v := f.hostView(t, id)
		if v.Applicability != knowledge.Unknown || len(v.Checks) != 1 || v.Checks[0].Freshness != knowledge.FreshnessRun {
			t.Fatalf("host view of a run-bound PASS %+v", v)
		}
	})

	t.Run("a new run revalidates and succeeds", func(t *testing.T) {
		p := scripted(readStep("r1", id))
		sid := f.run(t, "use the service port", p)
		if app := startApplicability(p.request(t, 0), id); app != knowledge.Current {
			t.Fatalf("index %s: %q", app, indexRow(p.request(t, 0)))
		}
		if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Current {
			t.Fatalf("view in the run %+v", v)
		}
		obs := f.observations(t, id)
		last := obs[0] // newest first
		if len(obs) != 2 || last.Purpose != session.ObservationRevalidation || last.SessionID != sid || last.RunID == "" ||
			last.Outcome != evaluator.StatusPassed || last.Fingerprint != fingerprint {
			t.Fatalf("observations %+v", obs)
		}
		if v := f.hostView(t, id); v.Applicability != knowledge.Unknown {
			t.Fatalf("after the run ended its PASS is current for the host: %+v", v)
		}
	})

	t.Run("an external change at the same fingerprint fails the new run's check", func(t *testing.T) {
		t.Setenv("LC_R1_PORT", "9090")
		p := scripted(readStep("r1", id))
		f.run(t, "use the service port after the change", p)
		if app := startApplicability(p.request(t, 0), id); app != knowledge.Stale {
			t.Fatalf("index %s: %q", app, indexRow(p.request(t, 0)))
		}
		if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Stale {
			t.Fatalf("view in the run %+v", v)
		}
		obs := f.observations(t, id)
		if last := obs[0]; last.Outcome != evaluator.StatusFailed || last.Fingerprint != fingerprint {
			t.Fatalf("the failing revalidation %+v (the change is outside the fingerprint %s)", last, fingerprint)
		}
	})

	t.Run("a revalidation that does not complete leaves it unknown", func(t *testing.T) {
		old := revalidationBudget
		revalidationBudget = time.Nanosecond
		t.Cleanup(func() { revalidationBudget = old })
		before := len(f.observations(t, id))
		p := scripted(readStep("r1", id))
		f.run(t, "use the service port without a revalidation", p)
		if app := startApplicability(p.request(t, 0), id); app != knowledge.Unknown {
			t.Fatalf("index %s", app)
		}
		if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Unknown {
			t.Fatalf("view %+v", v)
		}
		if n := len(f.observations(t, id)); n != before {
			t.Fatalf("observations %d -> %d without a completed revalidation", before, n)
		}
	})

	t.Run("the same run reuses its own fresh result", func(t *testing.T) {
		before := len(f.observations(t, id))
		p := scripted(readStep("r1", id), readStep("r2", id))
		f.run(t, "use the service port twice", p)
		if app := startApplicability(p.request(t, 0), id); app != knowledge.Current {
			t.Fatalf("index %s", app)
		}
		for i, call := range []string{"r1", "r2"} {
			if v := viewIn(t, p.request(t, i+1), call); v.Applicability != knowledge.Current {
				t.Fatalf("read %s %+v", call, v)
			}
		}
		if n := len(f.observations(t, id)); n != before+1 {
			t.Fatalf("observations %d -> %d: a read appended or the Start did not", before, n)
		}
	})

	t.Run("in the same run a mutation or the reuse window ends the reuse", func(t *testing.T) {
		// The dependency changed through a tool of the run: a completed side effect ends the reuse.
		p := scripted(readStep("r1", id), func(protocol.ProviderRequest) protocol.ProviderResponse {
			return cogStep("w1", "write_file", `{"path":"notes.txt","content":"changed\n"}`)
		}, readStep("r2", id))
		f.run(t, "change something, then use the port", p)
		if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Current {
			t.Fatalf("before the mutation %+v", v)
		}
		if v := viewIn(t, p.request(t, 3), "r2"); v.Applicability != knowledge.Unknown {
			t.Fatalf("after the mutation %+v", v)
		}
		// The dependency changed outside the run, which Core cannot observe: the reuse ends with
		// RunReuseWindow.
		var offset atomic.Int64
		f.engine.clock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
		t.Cleanup(func() { f.engine.clock = nil })
		q := scripted(readStep("r1", id), func(protocol.ProviderRequest) protocol.ProviderResponse {
			offset.Store(int64(knowledge.RunReuseWindow + time.Minute))
			return cogStep("r2", "read_knowledge", `{"ref":"`+id+`"}`)
		})
		f.run(t, "use the port, then again much later", q)
		if v := viewIn(t, q.request(t, 1), "r1"); v.Applicability != knowledge.Current {
			t.Fatalf("within the window %+v", v)
		}
		if v := viewIn(t, q.request(t, 2), "r2"); v.Applicability != knowledge.Unknown {
			t.Fatalf("after the window %+v", v)
		}
	})

	t.Run("a result in another scope is not shared", func(t *testing.T) {
		f.engine.clock = nil
		runGitIn(t, f.root, "add", "lumencortex.json")
		runGitIn(t, f.root, "commit", "-m", "checks")
		handle, err := f.engine.NewSession(ctx, SessionOptions{Goal: "work in a worktree"})
		if err != nil {
			t.Fatal(err)
		}
		wt, err := f.engine.AttachWorktree(ctx, handle.ID, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		// In the worktree the configuration yields 9090: the same check fails there.
		writeFile(t, filepath.Join(wt.Path, "defaults.env"), "BASE=9090\n")
		p := scripted(readStep("r1", id))
		if _, err := f.engine.runAgentWith(ctx, handle.ID, p, AgentOptions{MaxSteps: 4, Policy: "workspace"}); err != nil {
			t.Fatal(err)
		}
		if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Stale {
			t.Fatalf("worktree reader %+v", v)
		}
		obs := f.observations(t, id)
		if last := obs[0]; last.Scope != wt.Path || last.Outcome != evaluator.StatusFailed {
			t.Fatalf("worktree revalidation %+v (worktree %s)", last, wt.Path)
		}
		root := scripted(readStep("r1", id))
		f.run(t, "use the port in the main workspace", root)
		if v := viewIn(t, root.request(t, 1), "r1"); v.Applicability != knowledge.Current {
			t.Fatalf("main-workspace reader %+v", v)
		}
		if newest := f.observations(t, id)[0]; newest.Scope != f.root || newest.Outcome != evaluator.StatusPassed {
			t.Fatalf("main revalidation %+v", newest)
		}
	})

	t.Run("a Start runs at most MaxRevalidations checks", func(t *testing.T) {
		g := newCognitionFixture(t, true)
		services := []string{"alpha", "beta", "gamma"}
		var checks []string
		binds := map[string]knowledge.Assertion{}
		var facts []knowledge.Assertion
		for _, s := range services {
			checks = append(checks, `"`+s+`": {"kind": "test", "command": "test \"$LC_R1_PORT\" = 8080", "required": false}`)
			a := knowledge.Assertion{Kind: knowledge.AssertPortValue, Target: s, Value: "8080"}
			binds[s], facts = a, append(facts, a)
		}
		g.setChecks(t, "{"+strings.Join(checks, ",")+"}", binds)
		src := g.run(t, "inspect three services", cogAnswer("all three listen on 8080"))
		r := g.adoptFacts(t, src, facts...)
		if r.Qualified != 3 {
			t.Fatalf("adoption %+v", r)
		}
		before := 0
		for _, id := range r.Inserted {
			before += len(g.observations(t, id))
		}
		p := scripted()
		g.run(t, "use the services", p)
		counts := map[knowledge.Applicability]int{}
		after := 0
		for _, id := range r.Inserted {
			counts[startApplicability(p.request(t, 0), id)]++
			after += len(g.observations(t, id))
		}
		if after-before != knowledge.MaxRevalidations || counts[knowledge.Current] != knowledge.MaxRevalidations || counts[knowledge.Unknown] != 1 {
			t.Fatalf("revalidations %d, index %v: %q", after-before, counts, indexRow(p.request(t, 0)))
		}
	})

	t.Run("a Start never waits past its budget", func(t *testing.T) {
		g := newCognitionFixture(t, true)
		g.setChecks(t, `{"port": {"kind": "test", "command": "sleep 3; test \"$LC_R1_PORT\" = 8080", "required": false}}`,
			map[string]knowledge.Assertion{"port": portFact("8080")})
		src := g.run(t, "inspect the slow configuration", cogAnswer("8080"))
		r := g.adoptFacts(t, src, portFact("8080"))
		if r.Qualified != 1 {
			t.Fatalf("adoption %+v", r)
		}
		old := revalidationBudget
		revalidationBudget = 300 * time.Millisecond
		t.Cleanup(func() { revalidationBudget = old })
		p := scripted()
		began := time.Now()
		g.run(t, "use the slow service", p)
		if took := time.Since(began); took >= 3*time.Second {
			t.Fatalf("the run waited %v for a revalidation over its budget", took)
		}
		if app := startApplicability(p.request(t, 0), r.Inserted[0]); app != knowledge.Unknown {
			t.Fatalf("index %s", app)
		}
	})
}

// P1-05: a candidate is qualified only through the host-approved binding of its exact structured
// assertion, by the check that binding names, run by Core. Nothing the maintainer writes picks a
// check or a status, a free-text claim is never supported by a passing check, and a binding of another
// value, scope or definition supports nothing.
func TestAuditFalseClaimPassingUnrelatedCheck(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, true)
	a := f.run(t, "inspect the service", cogAnswer("the service listens on 8080"))
	b := f.run(t, "inspect the service again", cogAnswer("it is 8080"))
	configKey := knowledge.Assertion{Kind: knowledge.AssertConfigValue, Target: "config.env", Key: "PORT", Value: "8080"}
	f.bindAt(t, configKey, "port", "another-project")
	withField := func(entry, field string) string {
		return strings.TrimSuffix(entry, "}") + `,` + field + `}`
	}
	// Each entry's subject names it, so its record is found whatever order Core keeps them in.
	type entry struct {
		name   string
		build  func(subject, ra, rb string) string
		status string
	}
	entries := []entry{
		{"correct claim and check", func(s, ra, _ string) string { return factJSON(s, []string{ra}, nil, portFact("8080")) }, knowledge.StatusQualified},
		{"wrong free-text claim with an unrelated PASS", func(s, ra, _ string) string {
			return candidateJSON("fact", s, "The service listens on port 9090.", ra, nil)
		}, knowledge.StatusCandidateOnly},
		{"wrong value whose binding names another check", func(s, ra, _ string) string {
			return factJSON(s, []string{ra}, nil, portFact("9090"))
		}, knowledge.StatusCandidateOnly},
		{"wrong value without a binding", func(s, ra, _ string) string { return factJSON(s, []string{ra}, nil, portFact("7070")) }, knowledge.StatusCandidateOnly},
		{"binding approved for another scope", func(s, ra, _ string) string { return factJSON(s, []string{ra}, nil, configKey) }, knowledge.StatusCandidateOnly},
		{"another expected predicate", func(s, ra, _ string) string {
			raw := factJSON(s, []string{ra}, nil, portFact("8080"))
			return strings.Replace(raw, `"value":"8080"`, `"value":"8080","predicate":"not_equals"`, 1)
		}, knowledge.StatusRejected},
		{"candidate picks its check", func(s, ra, _ string) string {
			return withField(candidateJSON("fact", s, "The service listens on port 9090.", ra, nil), `"checkIds":["port"]`)
		}, knowledge.StatusRejected},
		{"candidate forges qualified", func(s, ra, _ string) string {
			return withField(factJSON(s, []string{ra}, nil, portFact("9090")), `"status":"qualified"`)
		}, knowledge.StatusRejected},
		{"no binding for the kind", func(s, ra, _ string) string {
			return factJSON(s, []string{ra}, nil, knowledge.Assertion{Kind: knowledge.AssertDependencyVersion, Target: "go.mod", Key: "example.com/x", Value: "v1.0.0"})
		}, knowledge.StatusCandidateOnly},
		{"multi-source correct claim", func(s, ra, rb string) string {
			return factJSON(s, []string{ra, rb}, nil, portFact("8080"))
		}, knowledge.StatusQualified},
		{"multi-source wrong claim", func(s, ra, rb string) string {
			return factJSON(s, []string{ra, rb}, nil, portFact("9090"))
		}, knowledge.StatusCandidateOnly},
	}
	subject := func(i int) string { return fmt.Sprintf("entry-%02d", i) }
	build := func(from, to int, x, y string) {
		f.propose(func(refs []string) []string {
			rx, ry := refOf(t, refs, x), refOf(t, refs, y)
			var out []string
			for i := from; i < to; i++ {
				out = append(out, entries[i].build(subject(i), rx, ry))
			}
			return out
		})
	}
	build(0, 8, a, b)
	first := f.maintain(t)
	if len(first.Inserted) != 8 || !slices.Equal(first.Checks, []string{"port", "port9090"}) {
		t.Fatalf("first pass %+v", first)
	}
	// The rest needs fresh sources: one batch commits at most MaxCandidates entries.
	c := f.run(t, "inspect the service a third time", cogAnswer("8080 again"))
	d := f.run(t, "inspect the service a fourth time", cogAnswer("still 8080"))
	build(8, len(entries), c, d)
	second := f.maintain(t)
	if len(second.Inserted) != len(entries)-8 {
		t.Fatalf("second pass %+v", second)
	}
	recs, err := f.engine.store.Candidates(ctx, session.CandidateFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	byEntry := map[int]session.CandidateRecord{}
	for _, rec := range recs {
		for i := range entries {
			if rec.Subject == subject(i) || strings.Contains(rec.Claim, `"`+subject(i)+`"`) {
				byEntry[i] = rec
			}
		}
	}
	var port8080 session.ApprovalRecord
	bindings, err := f.engine.store.ActiveConstraintBindings(ctx, f.engine.projectID())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bindings {
		if b.CheckID == "port" && b.ConstraintDigest == portFact("8080").ValueDigest() {
			port8080 = b
		}
	}
	var qualified []string
	for i, e := range entries {
		rec, ok := byEntry[i]
		if !ok {
			t.Errorf("%s: no record", e.name)
			continue
		}
		if rec.Status != e.status {
			t.Errorf("%s: %s (%s), want %s", e.name, rec.Status, rec.StatusReason, e.status)
		}
		if rec.Status != knowledge.StatusQualified {
			continue
		}
		qualified = append(qualified, rec.ID)
		if rec.Assertion == nil || *rec.Assertion != portFact("8080") || rec.BindingDigest == "" || rec.BindingDigest != port8080.BindingDigest ||
			!slices.Equal(rec.CheckIDs, []string{"port"}) || rec.Claim != "port_value(service) = 8080" {
			t.Errorf("%s: qualified without its exact binding: %+v", e.name, rec)
		}
		var adoption []session.ObservationRecord
		for _, o := range f.observations(t, rec.ID) {
			if o.Purpose == session.ObservationAdoption {
				adoption = append(adoption, o)
			}
		}
		if len(adoption) != 1 || adoption[0].CheckID != "port" || adoption[0].Outcome != evaluator.StatusPassed || adoption[0].SessionID != rec.MaintenanceSessionID {
			t.Errorf("%s: qualified without one passing adoption observation of its bound check: %+v", e.name, adoption)
		}
	}
	if len(qualified) != 2 {
		t.Fatalf("qualified %v", qualified)
	}
	all, err := f.engine.store.Candidates(ctx, session.CandidateFilter{Statuses: []string{knowledge.StatusQualified}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(qualified) {
		t.Errorf("unsupported adoption: %d qualified, %d supported", len(all), len(qualified))
	}

	// Lifecycle of the host's approvals, on a project with one qualified candidate and nothing
	// contesting it: the support follows the workspace, the binding and the check definition.
	g := newCognitionFixture(t, true)
	src := g.run(t, "inspect the service", cogAnswer("8080"))
	adopted := g.adoptFacts(t, src, portFact("8080"))
	if adopted.Qualified != 1 {
		t.Fatalf("adoption %+v", adopted)
	}
	gid := adopted.Inserted[0]
	binding := g.candidate(t, gid).BindingDigest
	t.Run("a check fingerprint change is not covered by the old PASS", func(t *testing.T) {
		g.write(t, "defaults.env", "BASE=9090\n")
		t.Cleanup(func() { g.write(t, "defaults.env", "BASE=8080\n") })
		p := scripted(readStep("r1", gid))
		g.run(t, "use the port after the workspace changed", p)
		if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Stale {
			t.Fatalf("after the change %+v", v)
		}
	})
	t.Run("a revoked binding supports nothing", func(t *testing.T) {
		if _, err := g.engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: binding}); err != nil {
			t.Fatal(err)
		}
		if v := g.hostView(t, gid); v.Applicability != knowledge.CandidateOnly {
			t.Fatalf("after the binding was revoked %+v", v)
		}
		e := g.run(t, "inspect once more", cogAnswer("8080"))
		if r := g.adoptFacts(t, e, portFact("8080")); r.Qualified != 0 || r.CandidateOnly != 1 {
			t.Fatalf("adoption without the binding %+v", r)
		}
		g.bind(t, portFact("8080"), "port")
	})
	t.Run("an unapproved check set supports nothing", func(t *testing.T) {
		e := g.run(t, "inspect before the approval is revoked", cogAnswer("8080"))
		if _, err := g.engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeChecks}); err != nil {
			t.Fatal(err)
		}
		g.propose(func(refs []string) []string {
			return []string{factJSON("service port", []string{refOf(t, refs, e)}, nil, portFact("8080"))}
		})
		before := g.state(t)
		r, err := g.engine.MaintainCognition(ctx)
		if r.Qualified != 0 {
			t.Fatalf("adoption with an unapproved check %+v %v", r, err)
		}
		t.Logf("pass under an unapproved check set: qualified=%d candidateOnly=%d err=%v", r.Qualified, r.CandidateOnly, err)
		if err != nil && g.state(t).Cursor != before.Cursor {
			t.Fatalf("a failed pass moved the cursor: %+v", g.state(t))
		}
		approveChecks(t, g.engine)
	})
	t.Run("a binding of an older check definition supports nothing", func(t *testing.T) {
		setProjectChecks(t, g.root, strings.Replace(cognitionChecks, `= 8080"`, `= 8080 && true"`, 1))
		approveChecks(t, g.engine)
		if v := g.hostView(t, gid); v.Applicability != knowledge.CandidateOnly {
			t.Fatalf("after the definition changed %+v", v)
		}
		e := g.run(t, "inspect after the redefinition", cogAnswer("8080"))
		r := g.adoptFacts(t, e, portFact("8080"))
		if r.Qualified != 0 || len(r.Inserted) != 1 {
			t.Fatalf("adoption under a stale binding %+v", r)
		}
		if rec := g.candidate(t, r.Inserted[0]); rec.Status != knowledge.StatusCandidateOnly || !strings.Contains(rec.StatusReason, "definition changed") {
			t.Fatalf("adoption under a stale binding %+v", rec)
		}
	})
	if all, err := g.engine.store.Candidates(ctx, session.CandidateFilter{Statuses: []string{knowledge.StatusQualified}, Limit: 100}); err != nil || len(all) != 1 || all[0].ID != gid {
		t.Errorf("unsupported adoption in the lifecycle: %+v %v", all, err)
	}
}

// textAtRest reports the database files (main file, WAL, shared memory) whose bytes hold text.
func textAtRest(t *testing.T, dbPath, text string) []string {
	t.Helper()
	var found []string
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), text) {
			found = append(found, filepath.Base(path))
		}
	}
	return found
}

// privacyWorld is a project in which the content of one source, A, reached every place cognition can
// copy it: A's own transcript and answer; the batch input and the answer of the maintenance session M
// that read A together with B; M's candidates (a fact derived from A, a free-text claim from A, a claim
// derived from A and B together, a claim derived from B only); a reader R whose read_knowledge result
// holds a candidate's text; and a second maintenance session M2 that read R.
type privacyWorld struct {
	f                         *cognitionFixture
	sentinel                  string
	a, b, m, r, m2            string
	fact, free, multi, bOnly  string
	readerRows, bRows, m2Rows int
}

func newPrivacyWorld(t *testing.T) *privacyWorld {
	t.Helper()
	f := newCognitionFixture(t, true)
	w := &privacyWorld{f: f, sentinel: fmt.Sprintf("T5A-R1-PRIVATE-%d", time.Now().UnixNano())}
	w.a = f.run(t, "investigate the service", cogAnswer("The service listens on 8080. Its codename is "+w.sentinel+"."))
	w.b = f.run(t, "check the service again", cogAnswer("confirmed: the service listens on 8080"))
	f.propose(func(refs []string) []string {
		ra, rb := refOf(t, refs, w.a), refOf(t, refs, w.b)
		multi, _ := json.Marshal(map[string]any{"claimType": "fact", "subject": "codename and port", "claim": "Both runs name " + w.sentinel + " on 8080.",
			"rationale": "A and B agree", "sourceRefs": []string{ra, rb}})
		return []string{
			factJSON("port of "+w.sentinel, []string{ra}, nil, portFact("8080")),
			candidateJSON("fact", "codename", "The codename is "+w.sentinel+".", ra, nil),
			string(multi),
			candidateJSON("procedure", "restart", "Restart the service after a configuration change.", rb, nil),
		}
	})
	report := f.maintain(t)
	if len(report.Inserted) != 4 || report.Qualified != 1 {
		t.Fatalf("fixture maintenance %+v", report)
	}
	w.m = report.MaintenanceSessionID
	for _, id := range report.Inserted {
		switch rec := f.candidate(t, id); rec.Subject {
		case "port of " + w.sentinel:
			w.fact = id
		case "codename":
			w.free = id
		case "codename and port":
			w.multi = id
		case "restart":
			w.bOnly = id
		}
	}
	p := reader(w.free)
	w.r = f.run(t, "use the project's cognition", p)
	if !strings.Contains(cogToolResult(t, p.request(t, 1)), w.sentinel) {
		t.Fatal("fixture: the reader's result does not hold the candidate's text")
	}
	f.answer = func([]string) string { return `{"candidates":[]}` }
	second := f.maintain(t)
	if second.Sources != 1 {
		t.Fatalf("fixture: the reader was not maintained %+v", second)
	}
	w.m2 = second.MaintenanceSessionID
	if !strings.Contains(fmt.Sprint(f.maintainer.request(t, len(f.maintainer.requests)-1).Messages), w.sentinel) {
		t.Fatal("fixture: the second maintenance batch does not hold the reader's copy")
	}
	if len(textAtRest(t, f.engine.store.Path(), w.sentinel)) == 0 {
		t.Fatal("fixture: the sentinel is not at rest")
	}
	w.readerRows = w.rows(t, w.r)
	w.bRows = w.rows(t, w.b)
	w.m2Rows = w.rows(t, w.m2)
	return w
}

func (w *privacyWorld) rows(t *testing.T, sessionID string) int {
	t.Helper()
	return countRows(t, w.f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ?`, sessionID)
}

// liveText counts the rows of the live database that hold the sentinel in any text column cognition or
// a transcript keeps, whatever the files still hold in free pages.
func (w *privacyWorld) liveText(t *testing.T) int {
	t.Helper()
	like := "%" + w.sentinel + "%"
	return countRows(t, w.f.engine, `SELECT
		(SELECT count(*) FROM cognition_candidates WHERE subject LIKE ?1 OR proposed_claim LIKE ?1 OR rationale LIKE ?1 OR status_reason LIKE ?1 OR assertion_json LIKE ?1)
		+ (SELECT count(*) FROM session_messages WHERE json LIKE ?1)
		+ (SELECT count(*) FROM sessions WHERE final LIKE ?1 OR goal LIKE ?1 OR error_json LIKE ?1)`, like)
}

// readsBlocked: every candidate of M reads as withdrawn, without its text, to the host and to a run.
func (w *privacyWorld) readsBlocked(t *testing.T) {
	t.Helper()
	for _, id := range []string{w.fact, w.free, w.multi, w.bOnly} {
		if v := w.f.hostView(t, id); v.Applicability != knowledge.Withdrawn || strings.Contains(fmt.Sprint(v), w.sentinel) {
			t.Fatalf("host read of %s %+v", id, v)
		}
	}
}

// assertPurged: the deletion of A is complete. No file of the database holds the sentinel; M's
// candidates, the one derived from B only included (M read A and B together, so no part of its answer
// is separable from A), are content-free tombstones; M keeps its row without content; R keeps its rows
// with its copy replaced; M2, which read R's copy, lost its content too; B, which the deletion does not
// cover, keeps all of its own; and recall finds nothing.
func (w *privacyWorld) assertPurged(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f := w.f
	if found := textAtRest(t, f.engine.store.Path(), w.sentinel); len(found) != 0 {
		t.Fatalf("the sentinel is still in %v", found)
	}
	if n := w.liveText(t); n != 0 {
		t.Fatalf("%d live rows hold the sentinel", n)
	}
	for _, id := range []string{w.fact, w.free, w.multi, w.bOnly} {
		rec := f.candidate(t, id)
		if rec.PurgedAt.IsZero() || rec.Subject != "" || rec.Claim != "" || rec.Rationale != "" || rec.Assertion != nil || rec.StatusReason != "" {
			t.Fatalf("candidate %s after the deletion %+v", id, rec)
		}
	}
	w.readsBlocked(t)
	if pending, err := f.engine.store.PendingPrivacyDeletion(ctx, w.a); err != nil || pending != nil {
		t.Fatalf("deletion still pending %+v %v", pending, err)
	}
	if _, err := f.engine.store.Get(ctx, w.a); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the deleted source %v", err)
	}
	if m, err := f.engine.store.Get(ctx, w.m); err != nil || m.Final != nil || w.rows(t, w.m) != 0 {
		t.Fatalf("maintenance session after the deletion %+v %v (rows %d)", m, err, w.rows(t, w.m))
	}
	if n := w.rows(t, w.r); n != w.readerRows {
		t.Fatalf("reader rows %d -> %d", w.readerRows, n)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ? AND json_extract(json, '$.content') = ?`, w.r, session.RemovedContent); n == 0 {
		t.Fatal("the reader's copy was not replaced")
	}
	if b, err := f.engine.store.Get(ctx, w.b); err != nil || b.Final == nil || *b.Final != "confirmed: the service listens on 8080" || w.rows(t, w.b) != w.bRows {
		t.Fatalf("source B after the deletion %+v %v", b, err)
	}
	hits, err := f.engine.Recall(ctx, w.sentinel, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(hits)
	for _, h := range hits {
		if h.ID == w.a || h.ID == w.m || h.ID == w.m2 {
			t.Fatalf("recall returns %s: %s", h.ID, raw)
		}
	}
	if strings.Contains(string(raw), w.sentinel) {
		t.Fatalf("recall returns the sentinel: %s", raw)
	}
}

// P1-02: deleting a source purges every text cognition derived from it, not only the source's own
// rows: candidate text, maintenance input and output, derived maintenance, copies in readers, from the
// live database, its WAL and its free pages. The fence blocks every read at once; a deletion that
// cannot finish reports pending, resumes and completes only once no byte is left.
func TestIndependentPrivacyDeletion(t *testing.T) {
	ctx := context.Background()
	t.Run("a deletion removes every derived copy", func(t *testing.T) {
		w := newPrivacyWorld(t)
		if _, err := w.f.engine.DeleteSession(ctx, w.a, WorktreeCleanup{}); err != nil {
			t.Fatal(err)
		}
		w.assertPurged(t)
		if n := w.rows(t, w.m2); n != 0 {
			t.Fatalf("the maintenance session that read the reader's copy kept %d rows (had %d)", n, w.m2Rows)
		}
		// The same content from a new, legitimate source is adopted again: no purged row makes it a
		// duplicate.
		c := w.f.run(t, "investigate the service anew", cogAnswer("The service listens on 8080. Its codename is "+w.sentinel+"."))
		r := w.f.adoptFacts(t, c, portFact("8080"))
		if len(r.Inserted) != 1 || r.Qualified != 1 || r.Duplicates != 0 {
			t.Fatalf("re-adoption from a new source %+v", r)
		}
	})
	t.Run("a WAL reader keeps it pending until it can finish", func(t *testing.T) {
		shortCheckpointWait(t)
		w := newPrivacyWorld(t)
		path := w.f.engine.store.Path()
		held := holdWALReader(t, path)
		if _, err := w.f.engine.DeleteSession(ctx, w.a, WorktreeCleanup{}); !errors.Is(err, ErrDeletionPending) {
			t.Fatalf("delete while a reader blocks the WAL truncation: %v, want ErrDeletionPending", err)
		}
		if pending, err := w.f.engine.store.PendingPrivacyDeletion(ctx, w.a); err != nil || pending == nil {
			t.Fatalf("pending %+v %v", pending, err)
		}
		w.readsBlocked(t)
		if n := w.liveText(t); n != 0 {
			t.Fatalf("%d live rows hold the sentinel while the deletion is pending", n)
		}
		held.release()
		if _, err := w.f.engine.DeleteSession(ctx, w.a, WorktreeCleanup{}); err != nil {
			t.Fatalf("resumed delete: %v", err)
		}
		w.assertPurged(t)
	})
	t.Run("an interrupted deletion is recovered by another Engine", func(t *testing.T) {
		w := newPrivacyWorld(t)
		cctx, cancel := context.WithCancel(ctx)
		testHookPurgeStep = cancel
		t.Cleanup(func() { testHookPurgeStep = nil })
		if _, err := w.f.engine.DeleteSession(cctx, w.a, WorktreeCleanup{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted delete: %v", err)
		}
		testHookPurgeStep = nil
		if pending, err := w.f.engine.store.PendingPrivacyDeletion(ctx, w.a); err != nil || pending == nil {
			t.Fatalf("pending %+v %v", pending, err)
		}
		// The candidates' text went in the first step: whatever the rest owes, no row holds it.
		w.readsBlocked(t)
		if n := countRows(t, w.f.engine, `SELECT count(*) FROM cognition_candidates WHERE subject LIKE ?1 OR proposed_claim LIKE ?1`, "%"+w.sentinel+"%"); n != 0 {
			t.Fatalf("%d candidates hold the sentinel after the first step", n)
		}
		// Open finishes the deletions an earlier Engine left pending; afterwards the source is gone.
		second, err := Open(Options{Workspace: w.f.root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { second.Close() })
		if pending, err := second.store.PendingPrivacyDeletion(ctx, w.a); err != nil || pending != nil {
			t.Fatalf("pending after another Engine opened %+v %v", pending, err)
		}
		if _, err := second.DeleteSession(ctx, w.a, WorktreeCleanup{}); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("delete after the recovery: %v", err)
		}
		w.assertPurged(t)
	})
}

// P1-03: a candidate holds only for the version of its sources it was derived from. A new run in the
// source session (a correction, a new answer) makes the old candidate stale to every reader; a change
// while a batch is with the maintainer refuses the commit, which leaves the cursor; the next pass
// maintains the new version, and the candidates it derives cite that version.
func TestIndependentSourceVersion(t *testing.T) {
	ctx := context.Background()
	t.Run("a new run in the source makes the old candidate stale", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "initial decision", cogAnswer("the user selected port 8080"))
		first := f.adoptFacts(t, a, portFact("8080"))
		if first.Qualified != 1 {
			t.Fatalf("adoption %+v", first)
		}
		id := first.Inserted[0]
		old := f.candidate(t, id)
		oldDirty := f.state(t).Dirty
		history := f.observations(t, id)
		if _, err := f.engine.runAgentWith(ctx, a, cogAnswer("The earlier decision is withdrawn; the user now selected 9090."),
			AgentOptions{Input: "replace the previous decision", Policy: "workspace", MaxSteps: 2}); err != nil {
			t.Fatal(err)
		}
		// The correction is a new revision and a new version of the same session.
		if now, err := f.engine.store.SourceVersion(ctx, a); err != nil || strings.Contains(strings.Join(old.SourceRefs, " "), now) {
			t.Fatalf("the corrected source kept the cited version %s: %v", now, err)
		}
		if st := f.state(t); st.Dirty <= oldDirty {
			t.Fatalf("the correction did not advance the dirty revision %+v", st)
		}
		if v := f.hostView(t, id); v.Applicability != knowledge.Stale {
			t.Fatalf("host view after the source changed %+v", v)
		}
		p := scripted(readStep("r1", id))
		f.run(t, "use the decision", p)
		if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Stale {
			t.Fatalf("reader view after the source changed %+v", v)
		}
		if got := startApplicability(p.request(t, 0), id); got == knowledge.Current {
			t.Fatalf("the Start index calls the old candidate current")
		}
		f.propose(func(refs []string) []string {
			return []string{candidateJSON("decision", "selected port", "The user selected port 9090.", refOf(t, refs, a), nil)}
		})
		second := f.maintain(t)
		if second.Sources < 1 || len(second.Inserted) != 1 {
			t.Fatalf("the new version was not maintained %+v", second)
		}
		now, err := f.engine.store.SourceVersion(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if rec := f.candidate(t, second.Inserted[0]); rec.SourceDigest == old.SourceDigest || !strings.Contains(strings.Join(rec.SourceRefs, " "), now) {
			t.Fatalf("the new candidate does not cite the new version %s: %+v", now, rec)
		}
		if rec := f.candidate(t, id); rec.Status != old.Status || rec.Claim != old.Claim || rec.SourceDigest != old.SourceDigest {
			t.Fatalf("the old candidate was rewritten %+v", rec)
		}
		// Both versions' candidates exist side by side, and the old one keeps its observations.
		if obs := f.observations(t, id); len(obs) < len(history) || obs[len(obs)-1] != history[len(history)-1] {
			t.Fatalf("the old candidate's observations %+v (had %+v)", obs, history)
		}
		if live, err := f.engine.store.Candidates(ctx, session.CandidateFilter{Limit: 10}); err != nil || len(live) != 2 {
			t.Fatalf("candidates of both versions %d %v", len(live), err)
		}
	})
	t.Run("a change while the maintainer reads the batch refuses its commit", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "initial decision", cogAnswer("the user selected port 8080"))
		changed := false
		f.answer = func(refs []string) string {
			if !changed {
				changed = true
				if _, err := f.engine.runAgentWith(ctx, a, cogAnswer("the user now selected 9090"),
					AgentOptions{Input: "replace the previous decision", Policy: "workspace", MaxSteps: 2}); err != nil {
					t.Error(err)
				}
			}
			return candidates(factJSON("service port", []string{refOf(t, refs, a)}, nil, portFact("8080")))
		}
		before := f.state(t)
		report, err := f.engine.MaintainCognition(ctx)
		if !errors.Is(err, session.ErrSourceChanged) {
			t.Fatalf("commit over a source changed in flight: %v (%+v)", err, report)
		}
		if live, _ := f.engine.store.Candidates(ctx, session.CandidateFilter{Limit: 10}); len(live) != 0 {
			t.Fatalf("a refused commit wrote candidates %+v", live)
		}
		if st := f.state(t); st.Cursor != before.Cursor || st.Dirty <= before.Dirty {
			t.Fatalf("state after the refused commit %+v (before %+v)", st, before)
		}
		next := f.maintain(t)
		if len(next.Inserted) != 1 {
			t.Fatalf("the next pass %+v", next)
		}
		now, err := f.engine.store.SourceVersion(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if rec := f.candidate(t, next.Inserted[0]); !strings.Contains(strings.Join(rec.SourceRefs, " "), now) {
			t.Fatalf("the next pass's candidate does not cite version %s: %+v", now, rec)
		}
		if st := f.state(t); st.Cursor != st.Dirty {
			t.Fatalf("the backlog was not drained %+v", st)
		}
	})
	t.Run("a delayed maintainer of the old version cannot commit", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "initial decision", cogAnswer("the user selected port 8080"))
		stale, err := f.engine.store.SourceVersion(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		release, entered := make(chan struct{}), make(chan struct{}, 1)
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		f.maintainer.respond = func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			entered <- struct{}{}
			<-release
			return cogFinal(candidates(factJSON("service port", []string{refOf(t, batchRefs(req), a)}, nil, portFact("8080")))), nil
		}
		type outcome struct {
			report MaintenanceReport
			err    error
		}
		delayed := make(chan outcome, 1)
		go func() {
			r, err := f.engine.MaintainCognition(ctx)
			delayed <- outcome{r, err}
		}()
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("the first pass did not reach its maintainer")
		}
		// While the old version is with its maintainer, the source is corrected.
		if _, err := f.engine.runAgentWith(ctx, a, cogAnswer("the user now selected 9090"),
			AgentOptions{Input: "replace the previous decision", Policy: "workspace", MaxSteps: 2}); err != nil {
			t.Fatal(err)
		}
		once.Do(func() { close(release) })
		var got outcome
		select {
		case got = <-delayed:
		case <-time.After(20 * time.Second):
			t.Fatal("the delayed pass did not return")
		}
		if !errors.Is(got.err, session.ErrSourceChanged) || len(got.report.Inserted) != 0 {
			t.Fatalf("the delayed maintainer of the old version: %+v %v", got.report, got.err)
		}
		f.maintainer.respond = func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			return cogFinal(f.answer(batchRefs(req))), nil
		}
		f.propose(func(refs []string) []string {
			return []string{candidateJSON("decision", "selected port", "The user selected port 9090.", refOf(t, refs, a), nil)}
		})
		next := f.maintain(t)
		if len(next.Inserted) != 1 {
			t.Fatalf("the pass over the new version %+v", next)
		}
		live, err := f.engine.store.Candidates(ctx, session.CandidateFilter{Limit: 10})
		if err != nil || len(live) != 1 || strings.Contains(strings.Join(live[0].SourceRefs, " "), stale) {
			t.Fatalf("a candidate cites the old version %s: %+v %v", stale, live, err)
		}
	})
}

// P1-04: withdrawing a candidate's source does not make its claim unadoptable. An independent source
// that states the same claim is new support: it is adopted and qualified on its own, while the
// withdrawn candidate stays withdrawn and unchanged.
func TestIndependentDedupeAfterWithdrawal(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, true)
	a := f.run(t, "first source", cogAnswer("8080"))
	first := f.adoptFacts(t, a, portFact("8080"))
	if first.Qualified != 1 {
		t.Fatalf("first adoption %+v", first)
	}
	old := f.candidate(t, first.Inserted[0])
	if _, err := f.engine.RevokeSource(ctx, SourceSession, a, "user_withdrawal"); err != nil {
		t.Fatal(err)
	}
	b := f.run(t, "independent new source", cogAnswer("8080"))
	// B states the claim twice in one answer: one support, adopted once.
	f.propose(func(refs []string) []string {
		rb := refOf(t, refs, b)
		return []string{factJSON("service port", []string{rb}, nil, portFact("8080")), factJSON("the port again", []string{rb}, nil, portFact("8080"))}
	})
	second := f.maintain(t)
	if second.Qualified != 1 || len(second.Inserted) != 1 {
		t.Fatalf("adoption from an independent source after the withdrawal %+v", second)
	}
	// B's support was established by Core running the bound check itself in B's maintenance pass.
	if obs := f.observations(t, second.Inserted[0]); len(obs) != 1 || obs[0].Purpose != session.ObservationAdoption ||
		obs[0].Outcome != evaluator.StatusPassed || obs[0].SessionID != second.MaintenanceSessionID {
		t.Fatalf("the new support's observation %+v", obs)
	}
	// A run revalidates the new candidate's run-bound check in its Start and reads it current.
	p := scripted(readStep("r1", second.Inserted[0]), readStep("r2", old.ID))
	f.run(t, "use the port", p)
	if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Current {
		t.Fatalf("the new candidate to a reader %+v", v)
	}
	if v := viewIn(t, p.request(t, 2), "r2"); v.Applicability != knowledge.Withdrawn || v.Claim != "" {
		t.Fatalf("the withdrawn candidate to a reader %+v", v)
	}
	if v := f.hostView(t, old.ID); v.Applicability != knowledge.Withdrawn {
		t.Fatalf("the withdrawn candidate %+v", v)
	}
	if rec := f.candidate(t, old.ID); rec.Status != old.Status || rec.Claim != old.Claim || rec.SupportDigest != old.SupportDigest {
		t.Fatalf("the withdrawn candidate was rewritten %+v", rec)
	}
	if rec := f.candidate(t, second.Inserted[0]); rec.SupportDigest == old.SupportDigest || rec.ClaimDigest != old.ClaimDigest {
		t.Fatalf("the new candidate's identity %+v (old %+v)", rec, old)
	}
}

// largeBacklog runs n sources whose projections are each several KiB, so that together they exceed
// MaxBatchInputBytes several times.
func (f *cognitionFixture) largeBacklog(t *testing.T, n int) []string {
	t.Helper()
	var sources []string
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("finding %d: ", i) + strings.Repeat(fmt.Sprintf("observation-%d ", i), 600)
		sources = append(sources, f.run(t, fmt.Sprintf("investigate area %d %s", i, strings.Repeat("g", 200)), cogAnswer(body)))
	}
	return sources
}

// batchInputs are the maintenance batch messages the maintainer received, in order.
func (f *cognitionFixture) batchInputs() []string {
	f.maintainer.mu.Lock()
	defer f.maintainer.mu.Unlock()
	var out []string
	for _, req := range f.maintainer.requests {
		for _, m := range req.Messages {
			if m.Role == "user" && strings.HasPrefix(m.Content, "Maintenance batch.") {
				out = append(out, m.Content)
			}
		}
	}
	return out
}

// committedOnce fails unless each source has exactly one dedupe record that is not a skip: it was
// committed by exactly one pass, neither lost nor processed twice.
func (f *cognitionFixture) committedOnce(t *testing.T, sources []string) {
	t.Helper()
	for _, id := range sources {
		if n := countRows(t, f.engine, `SELECT count(*) FROM maintenance_dedupe WHERE source_ref LIKE ? AND outcome != 'skipped'`, "%:"+id+"@%"); n != 1 {
			t.Errorf("source %s committed %d times", id, n)
		}
	}
}

// withinCap fails for a batch input over MaxBatchInputBytes or whose JSON document does not decode.
func withinCap(t *testing.T, input string) {
	t.Helper()
	if len(input) > knowledge.MaxBatchInputBytes {
		t.Errorf("batch input %d bytes over the %d cap", len(input), knowledge.MaxBatchInputBytes)
	}
	_, doc, ok := strings.Cut(input, "\n")
	if !ok || !json.Valid([]byte(doc)) {
		t.Errorf("batch input is not an instruction line and one JSON document: %.200q", input)
	}
}

// P2-06: MaxBatchInputBytes bounds the whole first message a maintainer receives as serialized: the
// instruction line, the policy, every source projection with its ref and the catalog of supportable
// assertions. A backlog over the cap is split across passes at whole sources; the sources left out
// stay dirty, the cursor never passes them, and a failed or concurrent pass loses or repeats none. A
// first source that cannot fit is deferred, never sent.
func TestIndependentBatchCap(t *testing.T) {
	ctx := context.Background()
	t.Run("the cap counts the whole serialized input to the byte", func(t *testing.T) {
		catalog := []knowledge.Assertion{portFact("8080"), {Kind: knowledge.AssertConfigValue, Target: "config.env", Key: "PORT"}}
		source := func(filler int) batchSource {
			return batchSource{projection: json.RawMessage(`{"ref":"session:x@sha256:0","thread":"` + strings.Repeat("x", filler) + `"}`)}
		}
		empty, err := maintenanceInput([]batchSource{source(0)}, catalog)
		if err != nil {
			t.Fatal(err)
		}
		room := knowledge.MaxBatchInputBytes - len(empty)
		exact, _ := maintenanceInput([]batchSource{source(room)}, catalog)
		over, _ := maintenanceInput([]batchSource{source(room + 1)}, catalog)
		if len(exact) != knowledge.MaxBatchInputBytes || !batchFits(exact) {
			t.Fatalf("an input of exactly %d bytes (%d) does not fit", knowledge.MaxBatchInputBytes, len(exact))
		}
		if len(over) != knowledge.MaxBatchInputBytes+1 || batchFits(over) {
			t.Fatalf("an input of %d bytes fits", len(over))
		}
		withinCap(t, exact)
		// The instruction line, the policy and the catalog are counted: the same source with one more
		// catalog entry, or the wrapper alone, is part of the length batchFits measures.
		wider, _ := maintenanceInput([]batchSource{source(room)}, append(slices.Clone(catalog), knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: "go.mod"}))
		if batchFits(wider) {
			t.Fatalf("one more catalog entry kept the input (%d bytes) within the cap", len(wider))
		}
		wrapper, _ := maintenanceInput(nil, nil)
		if !strings.HasPrefix(wrapper, "Maintenance batch.") || !strings.Contains(wrapper, knowledge.PolicyVersion) {
			t.Fatalf("the wrapper %q", wrapper)
		}
		t.Logf("wrapper %d bytes, exact %d, over %d, wider %d", len(wrapper), len(exact), len(over), len(wider))
	})
	t.Run("the review's four 8179-byte projections do not fit together", func(t *testing.T) {
		// The reproducer of the review: four projections of the largest kind a source yields, with a
		// catalog. Serialized together they are over the cap, so a batch never holds all four.
		included := make([]batchSource, 4)
		for i := range included {
			included[i].projection = json.RawMessage(`{"thread":"` + strings.Repeat("x", 8179) + `"}`)
		}
		catalog := []knowledge.Assertion{portFact("8080")}
		all, err := maintenanceInput(included, catalog)
		if err != nil {
			t.Fatal(err)
		}
		if batchFits(all) {
			t.Fatalf("four projections (%d bytes) fit", len(all))
		}
		three, _ := maintenanceInput(included[:3], catalog)
		if !batchFits(three) {
			t.Fatalf("three projections (%d bytes) do not fit", len(three))
		}
		withinCap(t, three)
		t.Logf("four %d bytes (refused), three %d bytes (sent)", len(all), len(three))
	})
	t.Run("a backlog over the cap is split across passes without loss", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		sources := f.largeBacklog(t, 10)
		passes := 0
		for st := f.state(t); st.Cursor != st.Dirty; st = f.state(t) {
			if passes++; passes > len(sources)+1 {
				t.Fatalf("the backlog does not drain: %+v", st)
			}
			r := f.maintain(t)
			after := f.state(t)
			if !r.Ran || r.Sources == 0 || after.Cursor <= st.Cursor || after.Cursor > after.Dirty || after.Dirty != st.Dirty {
				t.Fatalf("pass %d: %+v, state %+v -> %+v", passes, r, st, after)
			}
			if passes == 1 && after.Cursor == after.Dirty {
				t.Fatal("fixture: the whole backlog fit in one pass; the cap was not exercised")
			}
		}
		for _, in := range f.batchInputs() {
			withinCap(t, in)
		}
		f.committedOnce(t, sources)
		t.Logf("%d sources in %d passes", len(sources), passes)
	})
	t.Run("a failed pass after a cut loses no source", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		sources := f.largeBacklog(t, 8)
		f.maintain(t)
		mid := f.state(t)
		if mid.Cursor == mid.Dirty {
			t.Fatal("fixture: no cut")
		}
		f.answer = func([]string) string { return "" } // an empty reply interrupts the run
		if r, err := f.engine.MaintainCognition(ctx); err == nil {
			t.Fatalf("the interrupted pass %+v", r)
		}
		if st := f.state(t); st.Cursor != mid.Cursor || st.Dirty != mid.Dirty {
			t.Fatalf("the failed pass moved the state %+v -> %+v", mid, st)
		}
		f.answer = func([]string) string { return `{"candidates":[]}` }
		for st := f.state(t); st.Cursor != st.Dirty; st = f.state(t) {
			f.maintain(t)
		}
		for _, in := range f.batchInputs() {
			withinCap(t, in)
		}
		f.committedOnce(t, sources)
	})
	t.Run("a catalog that leaves no room defers the sources", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		src := f.run(t, "inspect the service", cogAnswer("8080 "+strings.Repeat("detail ", 900)))
		var bindings []string
		for i := 0; len(bindings) < 64; i++ {
			a := knowledge.Assertion{Kind: knowledge.AssertConfigValue, Target: fmt.Sprintf("conf/%03d/%s.env", i, strings.Repeat("d", 300)), Key: strings.Repeat("K", 120), Value: "1"}
			bindings = append(bindings, f.bind(t, a, "port").BindingDigest)
		}
		before := f.state(t)
		requests := len(f.batchInputs())
		r, err := f.engine.MaintainCognition(ctx)
		if !errors.Is(err, errBatchOverhead) {
			t.Fatalf("a pass whose catalog leaves no room: %+v %v", r, err)
		}
		if len(f.batchInputs()) != requests {
			t.Fatal("an input over the cap reached the maintainer")
		}
		if st := f.state(t); st.Cursor != before.Cursor || st.Dirty != before.Dirty {
			t.Fatalf("the deferred pass moved the state %+v -> %+v", before, st)
		}
		for _, b := range bindings {
			if _, err := f.engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: b}); err != nil {
				t.Fatal(err)
			}
		}
		if r := f.maintain(t); r.Sources != 1 {
			t.Fatalf("the pass after the catalog shrank %+v", r)
		}
		for _, in := range f.batchInputs() {
			withinCap(t, in)
		}
		f.committedOnce(t, []string{src})
	})
	t.Run("two engines drain a large backlog concurrently", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		sources := f.largeBacklog(t, 12)
		second, err := Open(Options{Workspace: f.root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { second.Close() })
		second.cognition = &CognitionOptions{Maintainer: bind(second, f.maintainer)}
		var held atomic.Int64
		drain := func(e *Engine) error {
			for i := 0; i < 200; i++ {
				st, err := e.CognitionState(ctx)
				if err != nil {
					return err
				}
				if st.Cursor == st.Dirty {
					return nil
				}
				r, err := e.MaintainCognition(ctx)
				if err != nil {
					return err
				}
				if r.Held {
					held.Add(1)
					time.Sleep(5 * time.Millisecond)
				}
			}
			return errors.New("the backlog does not drain")
		}
		errs := make(chan error, 2)
		go func() { errs <- drain(f.engine) }()
		go func() { errs <- drain(second) }()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		for _, in := range f.batchInputs() {
			withinCap(t, in)
		}
		f.committedOnce(t, sources)
		t.Logf("%d batches, %d passes found the lease held", len(f.batchInputs()), held.Load())
	})
}

// readOnce runs one reader that opens candidate id with read_knowledge and returns what it saw.
func (f *cognitionFixture) readOnce(t *testing.T, id string) KnowledgeView {
	t.Helper()
	p := scripted(readStep("r1", id))
	f.run(t, "use the project's cognition", p)
	return viewIn(t, p.request(t, 1), "r1")
}

// supportedOnly fails for any qualified candidate without a passing adoption observation of the check
// its host-approved binding names (unsupported adoption). A purged tombstone carries no claim and
// reads as withdrawn; it is not an adoption anyone can use.
func (f *cognitionFixture) supportedOnly(t *testing.T) {
	t.Helper()
	all, err := f.engine.store.Candidates(context.Background(), session.CandidateFilter{Statuses: []string{knowledge.StatusQualified}, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range all {
		if !rec.PurgedAt.IsZero() {
			if rec.Claim != "" || rec.Assertion != nil {
				t.Errorf("a purged tombstone keeps its claim %+v", rec)
			}
			continue
		}
		if rec.Assertion == nil || rec.BindingDigest == "" || len(rec.CheckIDs) != 1 {
			t.Errorf("unsupported adoption %+v", rec)
			continue
		}
		var passed bool
		for _, o := range f.observations(t, rec.ID) {
			passed = passed || (o.Purpose == session.ObservationAdoption && o.Outcome == evaluator.StatusPassed && o.CheckID == rec.CheckIDs[0])
		}
		if !passed {
			t.Errorf("adoption without a passing observation of its bound check %+v", rec)
		}
	}
}

// The six remediations interact; each pair the remediation names is exercised together here.
func TestCrossFindingRegression(t *testing.T) {
	ctx := context.Background()
	correct := func(t *testing.T, f *cognitionFixture, id, answer string) {
		t.Helper()
		if _, err := f.engine.runAgentWith(ctx, id, cogAnswer(answer), AgentOptions{Input: "correct the earlier answer", Policy: "workspace", MaxSteps: 2}); err != nil {
			t.Fatal(err)
		}
	}

	// P1-01 x P1-03: a corrected source ends the old candidate's PASS: no new run reads it current, and
	// the new version's own claim is decided by its own check.
	t.Run("freshness and source version", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "find the port", cogAnswer("8080"))
		old := f.adoptFacts(t, a, portFact("8080")).Inserted[0]
		if v := f.readOnce(t, old); v.Applicability != knowledge.Current {
			t.Fatalf("before the correction %+v", v)
		}
		correct(t, f, a, "correction: the service now listens on 9090")
		if v := f.readOnce(t, old); v.Applicability != knowledge.Stale {
			t.Fatalf("after the correction %+v", v)
		}
		r := f.adoptFacts(t, a, portFact("9090"))
		if len(r.Inserted) != 1 || r.Qualified != 0 {
			t.Fatalf("the new version's claim against a configuration of 8080 %+v", r)
		}
		if v := f.readOnce(t, r.Inserted[0]); v.Applicability == knowledge.Current {
			t.Fatalf("the new version's failed claim %+v", v)
		}
		f.supportedOnly(t)
	})

	// P1-02 x P1-04: a deleted source leaves no readable text, and an independent source can state the
	// same claim again.
	t.Run("privacy deletion and dedupe", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		sentinel := fmt.Sprintf("XF-PRIVATE-%d", time.Now().UnixNano())
		a := f.run(t, "find the port", cogAnswer("8080, codename "+sentinel))
		f.propose(func(refs []string) []string {
			ra := refOf(t, refs, a)
			return []string{factJSON("port of "+sentinel, []string{ra}, nil, portFact("8080")), candidateJSON("fact", "codename", "codename "+sentinel, ra, nil)}
		})
		first := f.maintain(t)
		if first.Qualified != 1 {
			t.Fatalf("adoption %+v", first)
		}
		if _, err := f.engine.DeleteSession(ctx, a, WorktreeCleanup{}); err != nil {
			t.Fatal(err)
		}
		if found := textAtRest(t, f.engine.store.Path(), sentinel); len(found) != 0 {
			t.Fatalf("the sentinel is still in %v", found)
		}
		b := f.run(t, "find the port independently", cogAnswer("8080"))
		second := f.adoptFacts(t, b, portFact("8080"))
		if second.Qualified != 1 || second.Duplicates != 0 {
			t.Fatalf("the independent source after the deletion %+v", second)
		}
		if v := f.readOnce(t, second.Inserted[0]); v.Applicability != knowledge.Current {
			t.Fatalf("the new candidate %+v", v)
		}
		for _, id := range first.Inserted {
			if v := f.readOnce(t, id); v.Applicability != knowledge.Withdrawn || strings.Contains(fmt.Sprint(v), sentinel) {
				t.Fatalf("the deleted source's candidate %+v", v)
			}
		}
		f.supportedOnly(t)
	})

	// P1-05 x P1-04: a wrong claim never qualifies, however often it is proposed; a candidate_only
	// record of the right claim does not keep a later supported source out.
	t.Run("adoption and dedupe", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		binding := f.bind(t, portFact("8080"), "port").BindingDigest // supersedes the fixture's own
		if _, err := f.engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: binding}); err != nil {
			t.Fatal(err)
		}
		wrong := func(ref string) []string {
			return []string{
				candidateJSON("fact", "service port", "The service listens on 9090.", ref, nil),
				factJSON("other port", []string{ref}, nil, portFact("7070")),
			}
		}
		a := f.run(t, "find the port", cogAnswer("8080"))
		f.propose(func(refs []string) []string {
			ra := refOf(t, refs, a)
			return append(wrong(ra), factJSON("service port", []string{ra}, nil, portFact("8080")))
		})
		if r := f.maintain(t); r.Qualified != 0 || r.CandidateOnly != 3 {
			t.Fatalf("adoption without the binding %+v", r)
		}
		f.bind(t, portFact("8080"), "port")
		b := f.run(t, "find the port again", cogAnswer("8080"))
		f.propose(func(refs []string) []string {
			rb := refOf(t, refs, b)
			return append(wrong(rb), factJSON("service port", []string{rb}, nil, portFact("8080")))
		})
		r := f.maintain(t)
		if r.Qualified != 1 || r.CandidateOnly != 2 || r.Duplicates != 0 {
			t.Fatalf("adoption from the supported source %+v", r)
		}
		f.supportedOnly(t)
	})

	// P1-01 x invalidation: at an unchanged fingerprint an external change makes the knowledge stale,
	// its restoration makes it current again for a new run, and a revocation overrides any PASS.
	t.Run("freshness and invalidation", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		t.Setenv("LC_R1_PORT", "8080")
		f.setChecks(t, freshChecks, map[string]knowledge.Assertion{"port": portFact("8080")})
		a := f.run(t, "find the port", cogAnswer("8080"))
		id := f.adoptFacts(t, a, portFact("8080")).Inserted[0]
		t.Setenv("LC_R1_PORT", "9090")
		if v := f.readOnce(t, id); v.Applicability != knowledge.Stale {
			t.Fatalf("after the external change %+v", v)
		}
		t.Setenv("LC_R1_PORT", "8080")
		if v := f.readOnce(t, id); v.Applicability != knowledge.Current {
			t.Fatalf("after the external change was undone %+v", v)
		}
		if _, err := f.engine.RevokeSource(ctx, SourceSession, a, "user_withdrawal"); err != nil {
			t.Fatal(err)
		}
		if v := f.readOnce(t, id); v.Applicability != knowledge.Withdrawn {
			t.Fatalf("after the revocation %+v", v)
		}
	})

	// P2-06 x P1-03: a source corrected while it waits behind a cut is maintained at its new version
	// only; the cursor drains the backlog without losing or repeating a source.
	t.Run("batch cap, cursor and source version", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		sources := f.largeBacklog(t, 8)
		f.maintain(t)
		mid := f.state(t)
		if mid.Cursor == mid.Dirty {
			t.Fatal("fixture: no cut")
		}
		last := sources[len(sources)-1]
		correct(t, f, last, "correction: "+strings.Repeat("revised ", 300))
		for st := f.state(t); st.Cursor != st.Dirty; st = f.state(t) {
			f.maintain(t)
		}
		for _, in := range f.batchInputs() {
			withinCap(t, in)
		}
		f.committedOnce(t, sources)
		now, err := f.engine.store.SourceVersion(ctx, last)
		if err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, f.engine, `SELECT count(*) FROM maintenance_dedupe WHERE source_ref LIKE ? AND outcome != 'skipped'`, "%:"+last+"@"+now); n != 1 {
			t.Fatalf("the corrected source was not committed at its new version %s", now)
		}
	})

	// P1-03 x lease: a maintainer whose lease was taken over while its source was corrected commits
	// nothing; the new holder commits the new version.
	t.Run("source correction and lease takeover", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "find the port", cogAnswer("8080"))
		stale, err := f.engine.store.SourceVersion(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		propose := func(req protocol.ProviderRequest) protocol.ProviderResponse {
			return cogFinal(candidates(factJSON("service port", []string{refOf(t, batchRefs(req), a)}, nil, portFact("8080"))))
		}
		second, err := Open(Options{Workspace: f.root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { second.Close() })
		second.cognition = &CognitionOptions{Maintainer: bind(second, &cognitionProvider{respond: func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			return propose(req), nil
		}})}
		release, entered := make(chan struct{}), make(chan struct{}, 1)
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		f.maintainer.respond = func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			entered <- struct{}{}
			<-release
			return propose(req), nil
		}
		type outcome struct {
			report MaintenanceReport
			err    error
		}
		first := make(chan outcome, 1)
		go func() {
			r, err := f.engine.MaintainCognition(ctx)
			first <- outcome{r, err}
		}()
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("the first pass did not reach its maintainer")
		}
		correct(t, f, a, "correction: still 8080, confirmed by the user")
		second.clock = func() time.Time { return time.Now().Add(maintenanceLeaseTTL + time.Minute) }
		taken, err := second.MaintainCognition(ctx)
		if err != nil || !taken.Ran || taken.Qualified != 1 {
			t.Fatalf("takeover %+v %v", taken, err)
		}
		once.Do(func() { close(release) })
		var old outcome
		select {
		case old = <-first:
		case <-time.After(20 * time.Second):
			t.Fatal("the first pass did not return")
		}
		if old.err == nil || len(old.report.Inserted) != 0 {
			t.Fatalf("the old holder committed: %+v %v", old.report, old.err)
		}
		live, err := f.engine.store.Candidates(ctx, session.CandidateFilter{Limit: 10})
		if err != nil || len(live) != 1 || strings.Contains(strings.Join(live[0].SourceRefs, " "), stale) {
			t.Fatalf("candidates after the takeover %+v %v (old version %s)", live, err, stale)
		}
		if st := f.state(t); st.Cursor != st.Dirty {
			t.Fatalf("state %+v", st)
		}
		f.supportedOnly(t)
	})
}
