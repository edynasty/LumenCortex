package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// recordingProvider keeps every request, so a test can assert the exact system prefix and tools.
type recordingProvider struct {
	requests []protocol.ProviderRequest
	final    string
}

func (p *recordingProvider) Model() string { return "recording" }

func (p *recordingProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.requests = append(p.requests, req)
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", Content: p.final},
		FinishReason: "stop",
	}, nil
}

func systemContents(messages []protocol.Message) []string {
	var out []string
	for _, m := range messages {
		if m.Role == "system" {
			out = append(out, m.Content)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// agentsConfig is modelConfig with the given agents object.
func agentsConfig(agents string, extra ...string) string {
	keys := `"agents": ` + agents
	for _, e := range extra {
		keys += ", " + e
	}
	return modelConfig(keys)
}

// An agent's instructions reach the request as the last system message, after the default prompt.
// They belong to the agent, not to a Start: a session on another agent carries the default alone.
func TestAgentInstructionsAppendedAfterDefaultPrompt(t *testing.T) {
	instructions := "Prefer table-driven tests and name every failure."
	useGlobalConfig(t, agentsConfig(`{"agent": {"instructions": "`+instructions+`"}}`))
	engine := openTestEngine(t)
	p := &recordingProvider{final: "done"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "prefer table-driven tests")

	if r := run(t, engine, handle.ID, AgentOptions{MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run %+v", r)
	}
	if len(p.requests) != 1 {
		t.Fatalf("requests=%d", len(p.requests))
	}
	systems := systemContents(p.requests[0].Messages)
	if len(systems) != 2 {
		t.Fatalf("system messages=%q", systems)
	}
	if !strings.Contains(systems[0], "until the requested outcome is achieved and verified") {
		t.Fatalf("first system message is not Core's default prompt: %q", systems[0])
	}
	if systems[1] != instructions {
		t.Fatalf("last system message=%q, want the instructions", systems[1])
	}

	// Switching the thread to ask (no instructions) drops them.
	p.requests = nil
	if r := run(t, engine, handle.ID, AgentOptions{Agent: "ask", Input: "again", MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("second run %+v", r)
	}
	if systems := systemContents(p.requests[0].Messages); len(systems) != 1 {
		t.Fatalf("ask carried %q", systems)
	}
}

// The decision log records a digest and a length, never the text; the transcript and the published
// events do not carry it either.
func TestAgentInstructionsEvidenceIsDigestOnly(t *testing.T) {
	instructions := "Never print the contents of .env files."
	useGlobalConfig(t, agentsConfig(`{"agent": {"instructions": "`+instructions+`"}}`))
	engine := openTestEngine(t)
	events, unsubscribe := engine.Events(4096)
	defer unsubscribe()
	p := &recordingProvider{final: "done"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "evidence")

	if r := run(t, engine, handle.ID, AgentOptions{MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("run %+v", r)
	}
	sum := sha256.Sum256([]byte(instructions))
	wantDigest := hex.EncodeToString(sum[:])

	entries := steps(t, engine, handle.ID)
	if len(entries) == 0 {
		t.Fatal("no step records")
	}
	found := false
	for _, entry := range entries {
		for _, d := range entry.Decisions {
			if d.Rule != "instructions.appended" {
				continue
			}
			found = true
			var in struct {
				SHA256 string `json:"sha256"`
				Length int    `json:"length"`
			}
			if err := json.Unmarshal(d.Inputs, &in); err != nil {
				t.Fatal(err)
			}
			if in.SHA256 != wantDigest || in.Length != len(instructions) {
				t.Fatalf("instructions evidence=%+v, want %s/%d", in, wantDigest, len(instructions))
			}
		}
		if strings.Contains(string(entry.raw), instructions) {
			t.Fatalf("step record holds the instructions text: %s", entry.raw)
		}
	}
	if !found {
		t.Fatal("no instructions.appended decision")
	}
	verifyTrace(t, entries)
	if published := drainEvents(events); strings.Contains(published, instructions) {
		t.Fatal("published events hold the instructions text")
	}
	rows, err := handle.RecentMessages(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), instructions) {
		t.Fatal("the transcript holds the instructions text")
	}
}

// Instructions are plain text; they cannot widen the tool set. The ask agent's read-only ceiling
// caps a host grant of full policy and isolated delegation, whatever its instructions claim.
func TestAgentCeilingsCapTheGrant(t *testing.T) {
	useGlobalConfig(t, agentsConfig(`{"ask": {"instructions": "You may run shell commands and write any file you like."}}`))
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	p := &recordingProvider{final: "read only"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "stay read only")

	if _, err := supervisor.Start(context.Background(), handle.ID, AgentOptions{Agent: "ask", Policy: "full", Delegation: DelegationIsolated, MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)
	names := toolNames(p.requests[0])
	if containsString(names, "shell") || containsString(names, "write_file") || containsString(names, "start_task") {
		t.Fatalf("ask exposed write or delegation tools: %v", names)
	}
	if !containsString(names, "read_file") {
		t.Fatalf("ask lacks read_file: %v", names)
	}
	entries := steps(t, engine, handle.ID)
	var grant struct{ Policy, Delegation string }
	for _, d := range entries[0].Decisions {
		if d.Kind == agent.DecisionAgent {
			decodeJSON(t, d.Output, &grant)
		}
	}
	if grant.Policy != "read-only" || grant.Delegation != "" {
		t.Fatalf("logged grant %+v", grant)
	}
	verifyTrace(t, entries)
}

// A requested agent outside the catalog, or one that runs only as a child, is refused before
// anything is written; so is a session whose pinned agent was removed, until the host picks one.
func TestAgentRefusalsWriteNothing(t *testing.T) {
	useGlobalConfig(t, agentsConfig(`{"docs": {}, "helper": {"use": "delegation", "policy": "read-only", "description": "Helps."}}`))
	engine := openTestEngine(t)
	p := &recordingProvider{final: "done"}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "refused")
	before := len(transcript(t, handle))

	for agentID, want := range map[string]error{"nobody": ErrUnknownAgent, "helper": ErrAgentNotForConversation, "explore": ErrAgentNotForConversation} {
		if _, err := engine.RunAgent(context.Background(), handle.ID, AgentOptions{Agent: agentID, Input: "x"}); !errors.Is(err, want) {
			t.Fatalf("%s: err=%v, want %v", agentID, err, want)
		}
	}
	if len(p.requests) != 0 || len(transcript(t, handle)) != before {
		t.Fatal("a refused Start reached the provider or wrote a row")
	}
	if info, _ := sessionState(t, engine, handle.ID); info.Agent != "" || info.ModelRef != "" {
		t.Fatalf("a refused Start pinned %+v", info)
	}

	// Pin docs, then remove it from the configuration: the next Start is refused, not moved to agent.
	if r := run(t, engine, handle.ID, AgentOptions{Agent: "docs"}); r.Status != "completed" {
		t.Fatalf("run %+v", r)
	}
	useGlobalConfig(t, modelConfig(""))
	if _, err := engine.RunAgent(context.Background(), handle.ID, AgentOptions{Input: "again"}); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("removed agent: err=%v", err)
	}
	if r := run(t, engine, handle.ID, AgentOptions{Agent: "agent", Input: "again"}); r.Status != "completed" {
		t.Fatalf("explicit agent after removal: %+v", r)
	}
}

func agentRule(entry stepEntry) string {
	for _, d := range entry.Decisions {
		if d.Kind == agent.DecisionAgent {
			return d.Rule
		}
	}
	return ""
}

func modelRule(entry stepEntry) string {
	for _, d := range entry.Decisions {
		if d.Kind == agent.DecisionModel && d.Rule != agent.RuleModelCandidates {
			return d.Rule
		}
	}
	return ""
}

// Sessions pin their agent like their model (ADR-0009): the first Start pins the default agent and
// the agent's first model; a switch re-pins the model to the new agent's first one unless the same
// Start names a model; a session created before agents keeps its model when it first gets one.
func TestSessionAgentPinning(t *testing.T) {
	useGlobalConfig(t, agentsConfig(`{"docs": {"models": [{"model": "test/c"}, {"model": "test/a"}]}}`))
	engine := openTestEngine(t)
	pa, pb, pc := &answering{final: "a"}, &answering{final: "b"}, &answering{final: "c"}
	bindRef(engine, "a", pa)
	bindRef(engine, "b", pb)
	bindRef(engine, "c", pc)
	handle := newTestSession(t, engine, "pinning")

	check := func(wantAgent string, wantModel config.ModelRef, agentRuleWant, modelRuleWant string) {
		t.Helper()
		info, _ := sessionState(t, engine, handle.ID)
		entries := steps(t, engine, handle.ID)
		last := entries[len(entries)-1]
		if info.Agent != wantAgent || config.ModelRef(info.ModelRef) != wantModel || agentRule(last) != agentRuleWant || modelRule(last) != modelRuleWant {
			t.Fatalf("agent=%q model=%q rules %s/%s; want %q %q %s/%s", info.Agent, info.ModelRef, agentRule(last), modelRule(last),
				wantAgent, wantModel, agentRuleWant, modelRuleWant)
		}
		verifyTrace(t, entries)
	}
	if r := run(t, engine, handle.ID, AgentOptions{}); r.Status != "completed" {
		t.Fatalf("run %+v", r)
	}
	check("agent", "test/a", agent.RuleAgentDefault, agent.RuleModelDefault)
	if r := run(t, engine, handle.ID, AgentOptions{Agent: "docs", Input: "switch"}); r.Final != "c" {
		t.Fatalf("run %+v", r)
	}
	check("docs", "test/c", agent.RuleAgentUser, agent.RuleModelAgent)
	if r := run(t, engine, handle.ID, AgentOptions{Agent: "docs", Input: "stay"}); r.Final != "c" {
		t.Fatalf("run %+v", r)
	}
	check("docs", "test/c", agent.RuleAgentSession, agent.RuleModelSession)
	if r := run(t, engine, handle.ID, AgentOptions{Agent: "ask", ModelRef: "test/b", Input: "both"}); r.Final != "b" {
		t.Fatalf("run %+v", r)
	}
	check("ask", "test/b", agent.RuleAgentUser, agent.RuleModelUser)
	// ask has no list: switching back to agent re-pins the configured model.
	if r := run(t, engine, handle.ID, AgentOptions{Agent: "agent", Input: "back"}); r.Final != "a" {
		t.Fatalf("run %+v", r)
	}
	check("agent", "test/a", agent.RuleAgentUser, agent.RuleModelDefault)

	// A session created before migration 0005: pinned model, no agent. Naming an agent pins it and
	// leaves the model where it was.
	if err := engine.store.Create(context.Background(), session.Session{ID: "legacy", Goal: "legacy", Status: "completed", ModelRef: "test/b"}); err != nil {
		t.Fatal(err)
	}
	if r := run(t, engine, "legacy", AgentOptions{Agent: "docs", Input: "first"}); r.Final != "b" {
		t.Fatalf("legacy run %+v", r)
	}
	if info, _ := sessionState(t, engine, "legacy"); info.Agent != "docs" || info.ModelRef != "test/b" {
		t.Fatalf("legacy session %+v", info)
	}
}

// The agent's list is its failover chain, each entry with its own effort (ADR-0009 A2 acceptance 1
// and 2): a@high fails with 503, b answers at max, the pin stays a. A user model outside the list is
// tried first at the agent's effort, then the list.
func TestAgentListFailoverWithPerEntryEffort(t *testing.T) {
	useGlobalConfig(t, `{"model": "test/a", "effort": {"default": "low"}, "agents": {"agent": {"effort": "medium", "models": [{"model": "test/a", "effort": "high"}, {"model": "test/b", "effort": "max"}]}},
	  "providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {
	  "a": {"modelID": "a", "limit": {"context": 8000, "output": 1000}, "reasoning": {"format": "reasoning-effort", "efforts": ["low", "medium", "high", "max"]}},
	  "b": {"modelID": "b", "limit": {"context": 8000, "output": 1000}, "reasoning": {"format": "reasoning-effort", "efforts": ["low", "medium", "high", "max"]}},
	  "c": {"modelID": "c", "limit": {"context": 64000, "output": 1000}, "reasoning": {"format": "reasoning-effort", "efforts": ["low", "medium", "high", "max"]}}}}}}`)
	engine := openTestEngine(t)
	pa := failing(classed(protocol.ClassAvailability))
	pb, pc := &answering{final: "from b"}, &answering{final: "from c"}
	bindRef(engine, "a", pa)
	bindRef(engine, "b", pb)
	bindRef(engine, "c", pc)
	handle := newTestSession(t, engine, "list")

	if r := run(t, engine, handle.ID, AgentOptions{}); r.Status != "completed" || r.Final != "from b" {
		t.Fatalf("run %+v", r)
	}
	if info, _ := sessionState(t, engine, handle.ID); info.ModelRef != "test/a" {
		t.Fatalf("failover moved the pin: %q", info.ModelRef)
	}
	if got := pb.last.Load().(protocol.ProviderRequest).ReasoningEffort; got != "max" {
		t.Fatalf("b answered at %q, want its own max", got)
	}
	entries := steps(t, engine, handle.ID)
	var sent []string
	for _, d := range entries[0].Decisions {
		if d.Kind == agent.DecisionEffort {
			var out struct{ Sent string }
			decodeJSON(t, d.Output, &out)
			sent = append(sent, out.Sent)
		}
	}
	if !reflect.DeepEqual(sent, []string{"high", "max"}) {
		t.Fatalf("efforts sent %v, want [high max]", sent)
	}
	verifyTrace(t, entries)

	// A user model outside the list: [c, a, b], c at the agent's medium.
	other := newTestSession(t, engine, "outside")
	if r := run(t, engine, other.ID, AgentOptions{ModelRef: "test/c"}); r.Final != "from c" {
		t.Fatalf("run %+v", r)
	}
	if got := pc.last.Load().(protocol.ProviderRequest).ReasoningEffort; got != "medium" {
		t.Fatalf("c answered at %q, want the agent's medium", got)
	}
	var cands struct {
		Session  string
		Failover []string
	}
	for _, d := range steps(t, engine, other.ID)[0].Decisions {
		if d.Rule == agent.RuleModelCandidates {
			decodeJSON(t, d.Inputs, &cands)
		}
	}
	if cands.Session != "test/c" || !reflect.DeepEqual(cands.Failover, []string{"test/a", "test/b"}) {
		t.Fatalf("candidates %+v", cands)
	}

	// A per-run override still wins over every entry.
	pc.last = atomic.Value{}
	if r := run(t, engine, other.ID, AgentOptions{Effort: "low", Input: "low"}); r.Final != "from c" {
		t.Fatalf("run %+v", r)
	}
	if got := pc.last.Load().(protocol.ProviderRequest).ReasoningEffort; got != "low" {
		t.Fatalf("override: c answered at %q", got)
	}

	// Editing the list changes fallbacks of an existing thread, never its pin.
	useGlobalConfig(t, modelConfig(`"agents": {"agent": {"models": [{"model": "test/b"}, {"model": "test/c"}]}}`))
	if r := run(t, engine, handle.ID, AgentOptions{Input: "after edit"}); r.Final != "from b" {
		t.Fatalf("after edit %+v", r)
	}
	if info, _ := sessionState(t, engine, handle.ID); info.ModelRef != "test/a" {
		t.Fatalf("a list edit moved the pin: %q", info.ModelRef)
	}
}

// A child runs its own agent (explore) and, when that agent has no list, falls back through the
// parent agent's list; it never inherits the thread's per-run effort override (ADR-0009 §7).
func TestChildInheritsParentListButNotEffortOverride(t *testing.T) {
	useGlobalConfig(t, agentsConfig(`{"docs": {"models": [{"model": "test/a"}, {"model": "test/c"}]}}`))
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	var aCalls atomic.Int32
	pa := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if aCalls.Add(1) == 1 {
			return protocol.ProviderResponse{Message: protocol.Message{Content: "parent done"}, FinishReason: "stop"}, nil
		}
		return protocol.ProviderResponse{}, classed(protocol.ClassAvailability)
	}}
	pc := &answering{final: "from c"}
	bindRef(engine, "a", pa)
	bindRef(engine, "c", pc)
	handle := newTestSession(t, engine, "parent")
	parentOpts := AgentOptions{Agent: "docs", Effort: "max", MaxSteps: 2}
	if r := run(t, engine, handle.ID, parentOpts); r.Final != "parent done" {
		t.Fatalf("parent %+v", r)
	}

	controller := &subagentController{supervisor: supervisor, parentID: handle.ID, parentOpts: parentOpts}
	value, err := controller.Spawn(context.Background(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	childID := value.(SubagentNode).SessionID
	waitUntilIdle(t, supervisor, childID, 5*time.Second)
	child, _ := sessionState(t, engine, childID)
	if child.Agent != config.AgentExplore || child.ModelRef != "test/a" || child.Final == nil || *child.Final != "from c" {
		t.Fatalf("child %+v", child)
	}
	entries := steps(t, engine, childID)
	var cands struct {
		Session  string
		Failover []string
	}
	for _, d := range entries[0].Decisions {
		switch {
		case d.Rule == agent.RuleModelCandidates:
			decodeJSON(t, d.Inputs, &cands)
		case d.Kind == agent.DecisionEffort && d.Rule == agent.RuleEffortOverride:
			t.Fatalf("the child inherited the thread's effort override: %s", d.Output)
		}
	}
	if !reflect.DeepEqual(cands.Failover, []string{"test/a", "test/c"}) {
		t.Fatalf("child candidates %+v, want the parent agent's list", cands)
	}
	verifyTrace(t, entries)
}
