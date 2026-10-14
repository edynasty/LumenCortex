package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestResolveModelRefPrecedence(t *testing.T) {
	cases := []struct {
		name                   string
		requested, stored, def config.ModelRef
		want                   config.ModelRef
		rule                   string
		writes                 bool
	}{
		{"explicit change pins", "p/b", "p/a", "p/d", "p/b", RuleModelUser, true},
		{"explicit change on an unpinned session", "p/b", "", "p/d", "p/b", RuleModelUser, true},
		{"same as the pin keeps it", "p/a", "p/a", "p/d", "p/a", RuleModelSession, false},
		{"pin wins over the default", "", "p/a", "p/d", "p/a", RuleModelSession, false},
		{"unpinned session takes and pins the default", "", "", "p/d", "p/d", RuleModelDefault, true},
		{"nothing at all is empty, for the caller to refuse", "", "", "", "", RuleModelDefault, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rule := ResolveModelRef(c.requested, c.stored, c.def)
			if got != c.want || rule != c.rule || WritesModelRef(rule) != c.writes {
				t.Fatalf("got %q via %s (writes=%v), want %q via %s (writes=%v)", got, rule, WritesModelRef(rule), c.want, c.rule, c.writes)
			}
		})
	}
}

func provErr(class protocol.ErrorClass) error {
	return &protocol.ProviderError{Class: class, Err: errors.New("upstream " + string(class))}
}

func TestClassifyAndFailover(t *testing.T) {
	alive := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// Classify: the run context decides cancellation; everything else is the adapter's class.
	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want protocol.ErrorClass
	}{
		{"auth", alive, provErr(protocol.ClassAuth), protocol.ClassAuth},
		{"bad request", alive, provErr(protocol.ClassBadRequest), protocol.ClassBadRequest},
		{"capacity", alive, provErr(protocol.ClassCapacity), protocol.ClassCapacity},
		{"availability", alive, provErr(protocol.ClassAvailability), protocol.ClassAvailability},
		{"wrapped class is found", alive, fmt.Errorf("model p/a: %w", provErr(protocol.ClassAuth)), protocol.ClassAuth},
		{"run cancelled", cancelled, provErr(protocol.ClassAvailability), protocol.ClassCancelled},
		{"run cancelled even for an unclassified error", cancelled, errors.New("x"), protocol.ClassCancelled},
		// An adapter that saw a cancellation the run context does not show (an idle timeout built
		// from a derived context) must not stop the run (F2).
		{"derived-context cancel is availability", alive, provErr(protocol.ClassCancelled), protocol.ClassAvailability},
		{"bare context.Canceled while the run lives is availability", alive, context.Canceled, protocol.ClassAvailability},
		{"unclassified error is availability", alive, errors.New("boom"), protocol.ClassAvailability},
	} {
		if got := Classify(c.ctx, c.err); got != c.want {
			t.Errorf("Classify %s = %s, want %s", c.name, got, c.want)
		}
	}

	// Failover: availability moves on, capacity needs a larger context, nothing else moves.
	cands := []Candidate{
		{Ref: "p/a", Context: 8000}, {Ref: "p/b", Context: 8000}, {Ref: "p/c", Context: 64000}, {Ref: "p/d", Context: 128000},
	}
	for _, c := range []struct {
		name   string
		failed int
		class  protocol.ErrorClass
		want   int
	}{
		{"availability goes to the next", 0, protocol.ClassAvailability, 1},
		{"availability on the last stops", 3, protocol.ClassAvailability, -1},
		{"capacity skips equal contexts", 0, protocol.ClassCapacity, 2},
		{"capacity from the middle", 2, protocol.ClassCapacity, 3},
		{"capacity with no larger context stops", 3, protocol.ClassCapacity, -1},
		{"auth never fails over", 0, protocol.ClassAuth, -1},
		{"bad request never fails over", 0, protocol.ClassBadRequest, -1},
		{"cancelled never fails over", 0, protocol.ClassCancelled, -1},
		{"index out of range stops", 9, protocol.ClassAvailability, -1},
	} {
		if got := Failover(cands, c.failed, c.class); got != c.want {
			t.Errorf("Failover %s = %d, want %d", c.name, got, c.want)
		}
	}
}

func catalog(entries ...Candidate) map[config.ModelRef]Candidate {
	out := map[config.ModelRef]Candidate{}
	for _, e := range entries {
		out[e.Ref] = e
	}
	return out
}

func refs(cands []Candidate) []config.ModelRef {
	out := make([]config.ModelRef, len(cands))
	for i, c := range cands {
		out[i] = c.Ref
	}
	return out
}

func TestCandidatesFilterOrderAndDedup(t *testing.T) {
	models := catalog(
		Candidate{Ref: "p/a", Context: 32768, Output: 4096, ToolCalling: true},
		Candidate{Ref: "p/b", Context: 32768, Output: 4096, ToolCalling: true},
		Candidate{Ref: "p/notools", Context: 32768, Output: 4096, ToolCalling: false},
		Candidate{Ref: "p/small", Context: 2000, Output: 500, ToolCalling: true},
	)
	// Session model first, then failover in order; duplicates, unknown refs, models without tool
	// calling and models whose context cannot fit the request are dropped.
	got := Candidates("p/a", []config.ModelRef{"p/b", "p/a", "p/ghost", "p/notools", "p/small", "p/b"}, models, 3000, 0, nil)
	if want := []config.ModelRef{"p/a", "p/b"}; !slices.Equal(refs(got), want) {
		t.Fatalf("candidates=%v, want %v", refs(got), want)
	}
	// A session model that cannot take the request is itself dropped, and the list is ordered.
	got = Candidates("p/small", []config.ModelRef{"p/b", "p/a"}, models, 3000, 0, nil)
	if want := []config.ModelRef{"p/b", "p/a"}; !slices.Equal(refs(got), want) {
		t.Fatalf("candidates=%v, want %v", refs(got), want)
	}
	// Same inputs, same list.
	again := Candidates("p/small", []config.ModelRef{"p/b", "p/a"}, models, 3000, 0, nil)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("Candidates is not deterministic")
	}
	// Nothing fits: empty, never nil-with-surprises.
	if got := Candidates("p/a", nil, models, 100000, 0, nil); len(got) != 0 {
		t.Fatalf("an oversized request got candidates: %v", refs(got))
	}
}

// base + margin (5% of the model's own context) + output reservation must fit the model's own
// context, with per-model limits, and MaxTokens replaces the model's output limit.
func TestCandidatesFitPerModelOutput(t *testing.T) {
	models := catalog(
		Candidate{Ref: "p/wide", Context: 100000, Output: 8000, ToolCalling: true},
		Candidate{Ref: "p/tight", Context: 10000, Output: 4096, ToolCalling: true},
	)
	fail := []config.ModelRef{"p/tight"}
	// tight: margin 500 + output 4096 = 4596; a base of 5400 fits (5400+4596 = 9996) ...
	if got := Candidates("p/wide", fail, models, 5400, 0, nil); !slices.Equal(refs(got), []config.ModelRef{"p/wide", "p/tight"}) {
		t.Fatalf("base 5400: %v", refs(got))
	}
	// ... 5405 does not, by exactly one token over.
	if got := Candidates("p/wide", fail, models, 5405, 0, nil); !slices.Equal(refs(got), []config.ModelRef{"p/wide"}) {
		t.Fatalf("base 5405: %v", refs(got))
	}
	// A run MaxTokens of 1000 is the reservation for every model, so tight fits again.
	if got := Candidates("p/wide", fail, models, 8000, 1000, nil); !slices.Equal(refs(got), []config.ModelRef{"p/wide", "p/tight"}) {
		t.Fatalf("MaxTokens 1000: %v", refs(got))
	}
	// The wide model's own 8000 reservation is not applied to tight, nor tight's to wide.
	if OutputBudget(0, models["p/wide"]) != 8000 || OutputBudget(0, models["p/tight"]) != 4096 || OutputBudget(77, models["p/wide"]) != 77 {
		t.Fatal("OutputBudget does not use the candidate's own limit / the run's MaxTokens")
	}
	if Margin(10000) != 500 {
		t.Fatalf("Margin(10000)=%d, want 5%%", Margin(10000))
	}
}

func TestCandidatesHealthIsAdvisory(t *testing.T) {
	models := catalog(
		Candidate{Ref: "p/a", Context: 32768, Output: 4096, ToolCalling: true},
		Candidate{Ref: "p/b", Context: 32768, Output: 4096, ToolCalling: true},
	)
	down := func(bad ...config.ModelRef) func(config.ModelRef) bool {
		return func(r config.ModelRef) bool { return !slices.Contains(bad, r) }
	}
	if got := Candidates("p/a", []config.ModelRef{"p/b"}, models, 100, 0, down("p/a")); !slices.Equal(refs(got), []config.ModelRef{"p/b"}) {
		t.Fatalf("unhealthy session model: %v", refs(got))
	}
	// When every candidate is marked down the registry is ignored: refusing to try is worse.
	if got := Candidates("p/a", []config.ModelRef{"p/b"}, models, 100, 0, down("p/a", "p/b")); !slices.Equal(refs(got), []config.ModelRef{"p/a", "p/b"}) {
		t.Fatalf("all unhealthy: %v", refs(got))
	}
}

func TestClampEffort(t *testing.T) {
	cases := []struct {
		name      string
		e         config.Effort
		supported []config.Effort
		want      config.Effort
	}{
		{"nothing to send is never changed", "", []config.Effort{"low"}, ""},
		{"no list leaves the request", "high", nil, "high"},
		{"listed level passes", "medium", []config.Effort{"low", "medium", "high"}, "medium"},
		{"lowest listed level not below the request", "medium", []config.Effort{"low", "high", "max"}, "high"},
		{"above everything clamps to the highest", "max", []config.Effort{"low", "high"}, "high"},
		{"unordered list", "low", []config.Effort{"max", "medium"}, "medium"},
		{"none is a level", "none", []config.Effort{"low", "high"}, "low"},
	}
	for _, c := range cases {
		if got := ClampEffort(c.e, c.supported); got != c.want {
			t.Errorf("%s: ClampEffort(%q, %v) = %q, want %q", c.name, c.e, c.supported, got, c.want)
		}
	}
	if !ValidEffort("") || !ValidEffort("max") || ValidEffort("extreme") {
		t.Fatal("ValidEffort accepts the wrong set")
	}
	if got, rule := EffortFor("low", "high"); got != "high" || rule != RuleEffortOverride {
		t.Fatalf("override: %q %s", got, rule)
	}
	if got, rule := EffortFor("low", ""); got != "low" || rule != RuleEffortDefault {
		t.Fatalf("default: %q %s", got, rule)
	}
}

func TestHealthRegistry(t *testing.T) {
	now := time.Unix(1000, 0)
	h := NewHealth(func() time.Time { return now })
	if !h.Available("p/a") {
		t.Fatal("a fresh registry marks nothing")
	}
	h.Fail("p/a", 0)
	h.Fail("p/b", time.Hour)
	if h.Available("p/a") || h.Available("p/b") {
		t.Fatal("failed models stay available")
	}
	if got := h.Unavailable([]config.ModelRef{"p/b", "p/a", "p/c", "p/a"}); !slices.Equal(got, []config.ModelRef{"p/a", "p/b"}) {
		t.Fatalf("Unavailable=%v, want sorted and deduplicated", got)
	}
	now = now.Add(DefaultUnhealthyFor)
	if !h.Available("p/a") || h.Available("p/b") {
		t.Fatal("the default window did not end, or the Retry-After window ended early")
	}
	h.Success("p/b")
	if !h.Available("p/b") {
		t.Fatal("Success must clear the mark")
	}
}

// I7: every logged decision recomputes from its recorded inputs alone.
func TestDecisionReplay(t *testing.T) {
	models := catalog(
		Candidate{Ref: "p/a", Context: 8000, Output: 1000, ToolCalling: true},
		Candidate{Ref: "p/b", Context: 64000, Output: 4096, ToolCalling: true},
		Candidate{Ref: "p/notools", Context: 64000, Output: 4096, ToolCalling: false},
	)
	_, _, model := ModelDecision("", "p/a", "p/d", "sha256:cfg")
	cands, candidates := CandidatesDecision("p/a", []config.ModelRef{"p/b", "p/notools"}, models, 2000, 0, []config.ModelRef{})
	_, effort := EffortDecision("low", "high", []config.Effort{"medium", "max"})
	next, failover := FailoverDecision(cands, 0, protocol.ClassCapacity)
	if next != 1 {
		t.Fatalf("next=%d", next)
	}
	for _, d := range []Decision{model, candidates, effort, failover} {
		if err := ReplayDecision(d); err != nil {
			t.Errorf("%s: %v", d.Rule, err)
		}
	}
	// A logged output that the inputs do not produce is caught.
	forged := candidates
	forged.Output = json.RawMessage(`{"candidates":["p/b"]}`)
	if ReplayDecision(forged) == nil {
		t.Fatal("a forged candidate list replayed clean")
	}
	forged = effort
	forged.Output = json.RawMessage(`{"requested":"high","sent":"high"}`)
	if ReplayDecision(forged) == nil {
		t.Fatal("a forged effort replayed clean")
	}
	forged = failover
	forged.Rule = RuleFailoverPrefix + string(protocol.ClassAvailability)
	if ReplayDecision(forged) == nil {
		t.Fatal("a failover logged under the wrong class replayed clean")
	}
	if ReplayDecision(Decision{Kind: "mystery", Rule: "x"}) == nil {
		t.Fatal("an unknown decision kind replayed clean")
	}
	// A decision carries no hidden reasoning: only observable inputs and the chosen output.
	raw, _ := json.Marshal([]Decision{model, candidates, effort, failover})
	for _, forbidden := range []string{"thinking", "reasoning_content", "scratch", "chain"} {
		if json.Valid(raw) && containsFold(string(raw), forbidden) {
			t.Errorf("decision log mentions %q: %s", forbidden, raw)
		}
	}
}

// I14: a step record passes VerifyStep only when the model that answered is reached from the Start's
// model and the logged candidate list through logged failover moves.
func TestVerifyStepEnforcesTheI14Trace(t *testing.T) {
	models := catalog(
		Candidate{Ref: "p/a", Context: 8000, Output: 1000, ToolCalling: true},
		Candidate{Ref: "p/b", Context: 8000, Output: 1000, ToolCalling: true},
		Candidate{Ref: "p/c", Context: 64000, Output: 1000, ToolCalling: true},
		Candidate{Ref: "p/x", Context: 64000, Output: 1000, ToolCalling: true},
	)
	_, _, model := ModelDecision("", "p/a", "p/d", "sha256:cfg")
	cands, candidates := CandidatesDecision("p/a", []config.ModelRef{"p/b"}, models, 100, 0, nil)
	_, effort := EffortDecision("", "", nil)
	_, failover := FailoverDecision(cands, 0, protocol.ClassAvailability)
	// A failover over a list the step never logged: p/x was not a candidate.
	_, offList := FailoverDecision([]Candidate{models["p/a"], models["p/x"]}, 0, protocol.ClassAvailability)
	// Candidates computed from a model the Start did not resolve.
	_, wrongSession := CandidatesDecision("p/c", nil, models, 100, 0, nil)
	// The same move logged twice: the second leaves p/a while the run is already on p/b.
	fromA := failover
	forged := candidates
	forged.Output = json.RawMessage(`{"candidates":["p/b"]}`)

	record := func(answered config.ModelRef, ds ...Decision) json.RawMessage {
		raw, err := json.Marshal(stepRecord{Step: 1, ModelRef: answered, Decisions: ds})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	cases := []struct {
		name string
		raw  json.RawMessage
		ok   bool
	}{
		{"first candidate answers", record("p/a", model, candidates, effort), true},
		{"logged failover answers", record("p/b", model, candidates, effort, failover, effort), true},
		{"later step without the Start decision", record("p/a", candidates, effort), true},
		{"record without modelRef is not checked", record(""), true},
		{"answered by a model without a logged move", record("p/b", model, candidates, effort), false},
		{"failover over an unlogged candidate list", record("p/x", model, candidates, effort, offList, effort), false},
		{"candidates from a model the Start did not resolve", record("p/c", model, wrongSession, effort), false},
		{"failover from a model the run was not on", record("p/b", model, candidates, effort, failover, effort, fromA, effort), false},
		{"failover before any candidate list", record("p/b", model, failover, effort), false},
		{"no candidate list", record("p/a", model, effort), false},
		{"forged decision", record("p/b", model, forged, effort), false},
	}
	for _, c := range cases {
		err := VerifyStep(c.raw)
		if (err == nil) != c.ok {
			t.Errorf("%s: VerifyStep = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if equalFold(s[i:i+len(sub)], sub) {
				return true
			}
		}
		return false
	})()
}

func equalFold(a, b string) bool {
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
