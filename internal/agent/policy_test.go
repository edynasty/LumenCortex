package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestResolveModelRefPrecedence(t *testing.T) {
	cases := []struct {
		name                               string
		requested, stored, agentFirst, def config.ModelRef
		switched                           bool
		want                               config.ModelRef
		rule                               string
		writes                             bool
	}{
		{"explicit change pins", "p/b", "p/a", "", "p/d", false, "p/b", RuleModelUser, true},
		{"explicit change on an unpinned session", "p/b", "", "", "p/d", false, "p/b", RuleModelUser, true},
		{"same as the pin keeps it", "p/a", "p/a", "", "p/d", false, "p/a", RuleModelSession, false},
		{"pin wins over the default", "", "p/a", "", "p/d", false, "p/a", RuleModelSession, false},
		{"unpinned session takes and pins the default", "", "", "", "p/d", false, "p/d", RuleModelDefault, true},
		{"nothing at all is empty, for the caller to refuse", "", "", "", "", false, "", RuleModelDefault, true},
		// ADR-0009: the agent's list.
		{"pin wins over the agent's list", "", "p/a", "p/l", "p/d", false, "p/a", RuleModelSession, false},
		{"unpinned session takes the agent's first model", "", "", "p/l", "p/d", false, "p/l", RuleModelAgent, true},
		{"agent switch re-pins to its first model", "", "p/a", "p/l", "p/d", true, "p/l", RuleModelAgent, true},
		{"agent switch to one without a list re-pins the default", "", "p/a", "", "p/d", true, "p/d", RuleModelDefault, true},
		{"agent switch with no list and no default keeps the pin", "", "p/a", "", "", true, "p/a", RuleModelSession, false},
		{"explicit model wins over an agent switch", "p/b", "p/a", "p/l", "p/d", true, "p/b", RuleModelUser, true},
		{"explicit model equal to the pin still wins over a switch", "p/a", "p/a", "p/l", "p/d", true, "p/a", RuleModelUser, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rule := ResolveModelRef(c.requested, c.stored, c.agentFirst, c.def, c.switched)
			if got != c.want || rule != c.rule || WritesModelRef(rule) != c.writes {
				t.Fatalf("got %q via %s (writes=%v), want %q via %s (writes=%v)", got, rule, WritesModelRef(rule), c.want, c.rule, c.writes)
			}
			// The decision replays, including from a record that carries the agent inputs.
			_, _, d := ModelDecision(c.requested, c.stored, c.agentFirst, c.def, c.switched, "sha256:cfg")
			if err := ReplayDecision(d); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveAgentAndCeilings(t *testing.T) {
	for _, c := range []struct {
		requested, stored, def, want, rule string
		switched                           bool
	}{
		{"", "", "agent", "agent", RuleAgentDefault, false},
		{"ask", "", "agent", "ask", RuleAgentUser, false}, // a legacy session gets an agent without moving its pin
		{"ask", "ask", "agent", "ask", RuleAgentSession, false},
		{"", "ask", "agent", "ask", RuleAgentSession, false},
		{"docs", "ask", "agent", "docs", RuleAgentUser, true},
	} {
		id, rule := ResolveAgent(c.requested, c.stored, c.def)
		if id != c.want || rule != c.rule || AgentSwitched(c.stored, id) != c.switched {
			t.Errorf("%+v: got %s via %s switched=%v", c, id, rule, AgentSwitched(c.stored, id))
		}
	}
	for _, c := range [][3]string{
		{"full", "read-only", "read-only"}, {"read-only", "full", "read-only"}, {"workspace", "workspace", "workspace"},
		{"full", "", "full"}, {"full", "workspace", "workspace"},
	} {
		if got := CapPolicy(c[0], c[1]); got != c[2] {
			t.Errorf("CapPolicy(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	for _, c := range [][3]string{
		{DelegationIsolated, "none", DelegationNone}, {DelegationIsolated, "propose", DelegationPropose},
		{DelegationPropose, "isolated", DelegationPropose}, {DelegationNone, "isolated", DelegationNone}, {DelegationIsolated, "", DelegationIsolated},
	} {
		if got := CapDelegation(c[0], c[1]); got != c[2] {
			t.Errorf("CapDelegation(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	lookup := func(id string) (AgentCeilings, bool) {
		if id == "ask" {
			return AgentCeilings{Policy: "read-only", Delegation: "none", Digest: "sha256:x"}, true
		}
		return AgentCeilings{}, false
	}
	_, _, grant, d, err := AgentDecision("ask", "", "agent", AgentGrant{Policy: "full", Delegation: DelegationIsolated}, lookup)
	if err != nil || grant != (AgentGrant{Policy: "read-only", Delegation: DelegationNone}) {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	if err := ReplayDecision(d); err != nil {
		t.Fatal(err)
	}
	// A tampered record does not replay.
	d.Output = []byte(`{"agent":"ask","write":true,"policy":"full","delegation":"isolated"}`)
	if err := ReplayDecision(d); err == nil {
		t.Fatal("a widened logged grant replayed")
	}
	if _, _, _, _, err := AgentDecision("", "gone", "agent", AgentGrant{}, lookup); err == nil {
		t.Fatal("an agent missing from the catalog resolved")
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
}

// Design §4.5 (S5): override wins; a stall escalates the default one level at each threshold and two
// at twice it, never above escalation.max; no default or no escalation means no escalation.
func TestEffortFor(t *testing.T) {
	esc := &config.Escalation{SameFailure: 2, NoProgressSteps: 6, Max: config.EffortHigh}
	cases := []struct {
		name          string
		def, override config.Effort
		esc           *config.Escalation
		stall         Stall
		want          config.Effort
		rule          string
	}{
		{"override", "low", "max", esc, Stall{SameFailure: 9}, "max", RuleEffortOverride},
		{"default", "low", "", esc, Stall{}, "low", RuleEffortDefault},
		{"below threshold", "low", "", esc, Stall{SameFailure: 1, NoProgressSteps: 5}, "low", RuleEffortDefault},
		{"same failure", "low", "", esc, Stall{SameFailure: 2}, "medium", RuleEffortEscalated},
		{"no progress", "low", "", esc, Stall{NoProgressSteps: 6}, "medium", RuleEffortEscalated},
		{"twice the threshold", "low", "", esc, Stall{SameFailure: 4}, "high", RuleEffortEscalated},
		{"capped by max", "medium", "", esc, Stall{SameFailure: 4}, "high", RuleEffortEscalated},
		{"already at max", "high", "", esc, Stall{SameFailure: 4}, "high", RuleEffortCapped},
		{"no escalation configured", "low", "", nil, Stall{SameFailure: 4}, "low", RuleEffortDefault},
		{"nothing to send stays nothing", "", "", esc, Stall{SameFailure: 4}, "", RuleEffortDefault},
		{"disabled threshold", "low", "", &config.Escalation{SameFailure: 0, NoProgressSteps: 6, Max: "high"}, Stall{SameFailure: 9}, "low", RuleEffortDefault},
	}
	for _, c := range cases {
		got, rule := EffortFor(c.def, c.override, c.esc, c.stall)
		if got != c.want || rule != c.rule {
			t.Errorf("%s: EffortFor = %q/%s, want %q/%s", c.name, got, rule, c.want, c.rule)
		}
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
	_, _, model := ModelDecision("", "p/a", "", "p/d", false, "sha256:cfg")
	cands, candidates := CandidatesDecision("p/a", []config.ModelRef{"p/b", "p/notools"}, models, 2000, 0, []config.ModelRef{})
	_, effort := EffortDecision("low", "", &config.Escalation{SameFailure: 2, NoProgressSteps: 6, Max: "max"}, Stall{SameFailure: 2}, 7, []config.Effort{"medium", "max"}, "")
	next, failover := FailoverDecision(cands, 0, protocol.ClassCapacity)
	if next != 1 {
		t.Fatalf("next=%d", next)
	}
	defaultContext := DefaultContextDecision("p/a", config.DefaultContext)
	for _, d := range []Decision{model, candidates, effort, failover, defaultContext} {
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
	forged = defaultContext
	forged.Output = json.RawMessage(`{"context":8000}`)
	if ReplayDecision(forged) == nil {
		t.Fatal("a default context other than config.DefaultContext replayed clean")
	}
	forged = effort
	forged.Output = json.RawMessage(`{"requested":"low","sent":"medium"}`)
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

// §4.6 turn rule, the reference cases of design verify/go/contracts: a turn never spans runs or
// models. Producer "" is an assistant row this run did not produce (an earlier run's).
func TestNeedsTurnBoundary(t *testing.T) {
	const a, b config.ModelRef = "p/a", "p/b"
	u, tl := TurnRow{Role: "user"}, TurnRow{Role: "tool"}
	asst := func(m config.ModelRef) TurnRow { return TurnRow{Role: "assistant", Producer: m} }
	cases := []struct {
		name   string
		rows   []TurnRow
		target config.ModelRef
		want   bool
	}{
		{"empty", nil, a, false},
		{"newest is user", []TurnRow{u}, a, false},
		{"same model, same run", []TurnRow{u, asst(a), tl, asst(a), tl}, a, false},
		{"failover mid-turn", []TurnRow{u, asst(a), tl}, b, true},
		{"resume after tool rows", []TurnRow{u, asst(""), tl}, a, true},
		{"resume after a final answer", []TurnRow{u, asst("")}, a, true},
		{"other model only in an earlier turn", []TurnRow{u, asst(b), u, asst(a), tl}, a, false},
		{"boundary already appended", []TurnRow{u, asst(a), tl, u}, b, false},
	}
	for _, c := range cases {
		if got := NeedsTurnBoundary(c.rows, c.target); got != c.want {
			t.Errorf("%s: NeedsTurnBoundary=%v want %v", c.name, got, c.want)
		}
	}
	// The decision records a "turn" row (the "resume" row is Start's) and replays; a decision logged
	// for rows that need no boundary does not.
	due, d := TurnBoundary([]TurnRow{u, asst(a), tl}, b)
	if !due {
		t.Fatal("failover mid-turn: no boundary")
	}
	if err := ReplayDecision(d); err != nil {
		t.Fatal(err)
	}
	forged := d
	forged.Inputs = json.RawMessage(`{"rows":[{"role":"user"},{"role":"assistant","producer":"p/b"},{"role":"tool"}],"target":"p/b"}`)
	if ReplayDecision(forged) == nil {
		t.Fatal("a turn decision for rows that need no boundary replayed clean")
	}
	if due, d := TurnBoundary([]TurnRow{u, asst(a), tl}, a); due || d.Rule != "" {
		t.Fatalf("no boundary due, got %v %+v", due, d)
	}
}

// Run memory (§4.6): a request carries echo only for current-turn assistant rows its target
// produced in this run, aligned with the request's messages; a user row ends the turn, drops its echo
// and stops counting it toward the window.
func TestRunMemoryEchoScope(t *testing.T) {
	const a, b config.ModelRef = "p/a", "p/b"
	// A window of n messages (system and Task State first) whose transcript rows start at first.
	built := func(n int, first int64) window.Result {
		return window.Result{Messages: make([]protocol.Message, n), FirstSeq: first}
	}
	r := &run{}
	r.appendRow(1, protocol.Message{Role: "user", Content: "q"})
	call := protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "c", Name: "read"}}}
	r.appendAssistant(2, call, a, protocol.NewEcho("k", []byte("E"), 7))
	r.appendRow(3, protocol.Message{Role: "tool", ToolCallID: "c", Content: "r"})

	// All three rows: system, Task State, user, assistant, tool.
	echo := r.requestEcho(built(5, 1), a)
	if len(echo) != 5 || string(echo[3].Data()) != "E" || !echo[2].IsZero() || !echo[4].IsZero() {
		t.Fatalf("echo to the producing model misaligned: %v", echo)
	}
	// A window that dropped the user row: system, Task State, assistant, tool.
	if echo := r.requestEcho(built(4, 2), a); len(echo) != 4 || string(echo[2].Data()) != "E" {
		t.Fatalf("echo misaligned in a window from seq 2: %v", echo)
	}
	if got := r.requestEcho(built(5, 1), b); got != nil {
		t.Fatalf("another model received the echo: %v", got)
	}
	if r.transcript[1].EchoTokens != 7 {
		t.Fatalf("echo tokens %d, want 7 while the row is in the current turn", r.transcript[1].EchoTokens)
	}

	r.appendRow(4, protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: "not done"})
	if got := r.requestEcho(built(6, 1), a); got != nil {
		t.Fatalf("a request after a user row carries the previous turn's echo: %v", got)
	}
	if r.transcript[1].EchoTokens != 0 {
		t.Fatalf("the previous turn's echo still counts %d tokens", r.transcript[1].EchoTokens)
	}
	if rows := r.turnRows(); len(rows) != 0 {
		t.Fatalf("turn rows after a user row: %v", rows)
	}
}

// Long run memory (§4.6): one turn of many tool steps keeps echo only for rows still in memory; the
// groups no window can include take their echo with them, so memory is bounded by the window.
func TestRunMemoryBoundedByWindow(t *testing.T) {
	r := &run{}
	r.appendRow(0, protocol.Message{Role: "user", Content: "q"})
	big := strings.Repeat("x", 3000)
	for i := int64(1); i < 400; i += 2 {
		id := fmt.Sprint("c", i)
		r.appendAssistant(i, protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: "read"}}},
			"p/a", protocol.NewEcho("k", []byte(big), 1000))
		r.appendRow(i+1, protocol.Message{Role: "tool", ToolCallID: id, Content: big})
	}
	if _, err := r.groups(8000); err != nil {
		t.Fatal(err)
	}
	if r.trimmed == 0 {
		t.Fatal("nothing was trimmed; the test does not exercise the bound")
	}
	assistants := 0
	for _, e := range r.transcript {
		if e.Msg.Role == "assistant" {
			assistants++
		}
	}
	if len(r.turn) != assistants {
		t.Fatalf("run memory holds %d echoes for %d assistant rows in memory", len(r.turn), assistants)
	}
	for seq := range r.turn {
		if seq < r.transcript[0].Seq {
			t.Fatalf("run memory holds echo of trimmed row %d", seq)
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
	_, _, model := ModelDecision("", "p/a", "", "p/d", false, "sha256:cfg")
	cands, candidates := CandidatesDecision("p/a", []config.ModelRef{"p/b"}, models, 100, 0, nil)
	_, effort := EffortDecision("", "", nil, Stall{}, 0, nil, "")
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
