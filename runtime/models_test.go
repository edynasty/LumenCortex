package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider/openai"
)

// modelConfig is a global configuration with three catalog models a, b (context 8000) and c
// (context 64000), default model a, and the given extra top-level keys (`"failover": [...]`).
func modelConfig(extra string) string {
	if extra != "" {
		extra += ","
	}
	return fmt.Sprintf(`{"model": "test/a", %s "providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {
	  "a": {"modelID": "a", "limit": {"context": 8000, "output": 1000}},
	  "b": {"modelID": "b", "limit": {"context": 8000, "output": 1000}},
	  "c": {"modelID": "c", "limit": {"context": 64000, "output": 1000}}}}}}`, extra)
}

// answering returns a provider that records its calls and answers final.
type answering struct {
	calls atomic.Int32
	final string
	last  atomic.Value // protocol.ProviderRequest
}

func (p *answering) Model() string { return "answering" }
func (p *answering) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls.Add(1)
	p.last.Store(req)
	return protocol.ProviderResponse{Message: protocol.Message{Content: p.final}, FinishReason: "stop"}, nil
}

// failing returns a provider that fails every request with the given error.
func failing(err error) *exitProvider {
	return &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{}, err
	}}
}

func classed(class protocol.ErrorClass) error {
	return &protocol.ProviderError{Class: class, Err: errors.New("upstream " + string(class))}
}

type stepEntry struct {
	Step      int64            `json:"step"`
	ModelRef  config.ModelRef  `json:"modelRef"`
	Decisions []agent.Decision `json:"decisions"`
	raw       json.RawMessage
}

func steps(t *testing.T, engine *Engine, id string) []stepEntry {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT json FROM agent_steps WHERE session_id = ? ORDER BY step`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []stepEntry
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var entry stepEntry
		decodeJSON(t, []byte(raw), &entry)
		entry.raw = json.RawMessage(raw)
		out = append(out, entry)
	}
	return out
}

func rules(entry stepEntry) []string {
	var out []string
	for _, d := range entry.Decisions {
		out = append(out, d.Rule)
	}
	return out
}

// verifyTrace checks every step record with agent.VerifyStep: each logged decision recomputes from
// its recorded inputs (I7), and the model that answered is reached through logged moves (I14).
func verifyTrace(t *testing.T, entries []stepEntry) {
	t.Helper()
	for _, entry := range entries {
		if err := agent.VerifyStep(entry.raw); err != nil {
			t.Errorf("step %d: %v\n%s", entry.Step, err, entry.raw)
		}
	}
}

func run(t *testing.T, engine *Engine, id string, opts AgentOptions) AgentResult {
	t.Helper()
	if opts.MaxSteps == 0 {
		opts.MaxSteps = 3
	}
	result, _ := engine.RunAgent(context.Background(), id, opts)
	return result
}

// An older session has no model_ref. Its first Start pins the configured default; later changes of
// the default do not move it; only an explicit ModelRef does, and failover never writes the pin.
func TestSessionPinningAcrossConfigurationChanges(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine := openTestEngine(t)
	pa, pb := &answering{final: "from a"}, &answering{final: "from b"}
	bindRef(engine, "a", pa)
	bindRef(engine, "b", pb)
	handle := newTestSession(t, engine, "pinning")
	if info, _ := sessionState(t, engine, handle.ID); info.ModelRef != "" {
		t.Fatalf("a new session is already pinned: %q", info.ModelRef)
	}

	if r := run(t, engine, handle.ID, AgentOptions{}); r.Status != "completed" || pa.calls.Load() != 1 {
		t.Fatalf("first run %+v, calls on a=%d", r, pa.calls.Load())
	}
	info, _ := sessionState(t, engine, handle.ID)
	if info.ModelRef != "test/a" || info.Provider != "test" || info.Model != "a" {
		t.Fatalf("first Start did not pin the default: %+v", info)
	}
	first := steps(t, engine, handle.ID)
	if got := rules(first[0]); got[0] != agent.RuleModelDefault {
		t.Fatalf("first decision %v, want %s", got, agent.RuleModelDefault)
	}

	// The global default moves to b. The session stays on a.
	useGlobalConfig(t, strings.Replace(modelConfig(""), `"model": "test/a"`, `"model": "test/b"`, 1))
	if r := run(t, engine, handle.ID, AgentOptions{Input: "again"}); r.Status != "completed" {
		t.Fatalf("second run %+v", r)
	}
	if info, _ := sessionState(t, engine, handle.ID); info.ModelRef != "test/a" || pb.calls.Load() != 0 || pa.calls.Load() != 2 {
		t.Fatalf("a changed default moved the session: %+v (a=%d b=%d)", info, pa.calls.Load(), pb.calls.Load())
	}
	all := steps(t, engine, handle.ID)
	if got := rules(all[len(all)-1])[0]; got != agent.RuleModelSession {
		t.Fatalf("second run's model rule = %s, want %s", got, agent.RuleModelSession)
	}

	// An explicit model re-pins.
	if r := run(t, engine, handle.ID, AgentOptions{Input: "switch", ModelRef: "test/b"}); r.Status != "completed" {
		t.Fatalf("third run %+v", r)
	}
	if info, _ := sessionState(t, engine, handle.ID); info.ModelRef != "test/b" || info.Model != "b" || pb.calls.Load() != 1 {
		t.Fatalf("explicit model did not re-pin: %+v", info)
	}
	all = steps(t, engine, handle.ID)
	if got := rules(all[len(all)-1])[0]; got != agent.RuleModelUser {
		t.Fatalf("third run's model rule = %s, want %s", got, agent.RuleModelUser)
	}
	verifyTrace(t, all)
}

// Availability failover answers from the next candidate, records every decision, leaves the pinned
// model_ref alone, and a child spawned afterwards inherits the pinned model, not the one that
// answered.
func TestAvailabilityFailoverKeepsPinAndChildInheritsIt(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"failover": ["test/b"]`))
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	var aCalls atomic.Int32
	pa := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if aCalls.Add(1) == 1 {
			return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, RetryAfter: time.Hour, Err: errors.New("overloaded")}
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "a back"}, FinishReason: "stop"}, nil
	}}
	pb := &answering{final: "from b"}
	bindRef(engine, "a", pa)
	bindRef(engine, "b", pb)
	handle := newTestSession(t, engine, "failover")

	started := time.Now()
	r := run(t, engine, handle.ID, AgentOptions{})
	if r.Status != "completed" || r.Final != "from b" {
		t.Fatalf("result %+v", r)
	}
	// Retry-After 3600 is recorded, not waited for.
	if time.Since(started) > 10*time.Second {
		t.Fatalf("the run waited %v for a one-hour Retry-After", time.Since(started))
	}
	if engine.health.Available("test/a") {
		t.Fatal("the failed model is still available in the Engine's registry")
	}
	info, _ := sessionState(t, engine, handle.ID)
	if info.ModelRef != "test/a" || info.Model != "a" {
		t.Fatalf("failover moved the session pin: %+v", info)
	}
	entries := steps(t, engine, handle.ID)
	if len(entries) != 1 || entries[0].ModelRef != "test/b" {
		t.Fatalf("steps=%+v, want one step answered by test/b", entries)
	}
	want := []string{agent.RuleModelDefault, agent.RuleModelCandidates, agent.RuleEffortDefault, agent.RuleFailoverPrefix + "availability", agent.RuleEffortDefault}
	if got := rules(entries[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("decision trace %v, want %v", got, want)
	}
	verifyTrace(t, entries)

	// The child inherits sessions.model_ref (a), not the model that answered the parent (b).
	controller := &subagentController{supervisor: supervisor, parentID: handle.ID, parentOpts: AgentOptions{}}
	value, err := controller.Spawn(context.Background(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	childID := value.(SubagentNode).SessionID
	waitUntilIdle(t, supervisor, childID, 5*time.Second)
	child, _ := sessionState(t, engine, childID)
	if child.ModelRef != "test/a" {
		t.Fatalf("child model_ref=%q, want the parent's pin test/a", child.ModelRef)
	}
	childSteps := steps(t, engine, childID)
	if len(childSteps) == 0 || rules(childSteps[0])[0] != agent.RuleModelUser {
		t.Fatalf("child steps=%+v, want its model logged as %s", childSteps, agent.RuleModelUser)
	}
	verifyTrace(t, childSteps)
}

// subagentModel overrides the parent's pin for children.
func TestChildUsesSubagentModelWhenConfigured(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"subagentModel": "test/c"`))
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	bindRef(engine, "a", &answering{final: "parent"})
	bindRef(engine, "c", &answering{final: "child"})
	handle := newTestSession(t, engine, "parent")
	if r := run(t, engine, handle.ID, AgentOptions{}); r.Status != "completed" {
		t.Fatalf("parent %+v", r)
	}
	controller := &subagentController{supervisor: supervisor, parentID: handle.ID, parentOpts: AgentOptions{Effort: "low"}}
	value, err := controller.Spawn(context.Background(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	childID := value.(SubagentNode).SessionID
	waitUntilIdle(t, supervisor, childID, 5*time.Second)
	child, _ := sessionState(t, engine, childID)
	if child.ModelRef != "test/c" || child.Model != "c" {
		t.Fatalf("child %+v, want subagentModel test/c", child)
	}
	if parent, _ := sessionState(t, engine, handle.ID); parent.ModelRef != "test/a" {
		t.Fatalf("spawning moved the parent's pin: %q", parent.ModelRef)
	}
}

// A capacity error needs a candidate with a larger context: an equal one is skipped, a larger one
// answers, and with none the run fails as failed/provider:capacity.
func TestCapacityFailoverNeedsALargerContext(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"failover": ["test/b", "test/c"]`))
	engine := openTestEngine(t)
	pa := failing(classed(protocol.ClassCapacity))
	pb := &answering{final: "from b"}
	pc := &answering{final: "from c"}
	bindRef(engine, "a", pa)
	bindRef(engine, "b", pb)
	bindRef(engine, "c", pc)
	handle := newTestSession(t, engine, "capacity")
	r := run(t, engine, handle.ID, AgentOptions{})
	if r.Status != "completed" || r.Final != "from c" || pb.calls.Load() != 0 {
		t.Fatalf("result %+v, b was called %d times (same context, must be skipped)", r, pb.calls.Load())
	}
	entries := steps(t, engine, handle.ID)
	if got := rules(entries[0]); !strings.Contains(strings.Join(got, ","), agent.RuleFailoverPrefix+"capacity") {
		t.Fatalf("trace %v lacks the capacity failover", got)
	}
	verifyTrace(t, entries)

	// Nothing larger: the run fails and says why.
	useGlobalConfig(t, modelConfig(`"failover": ["test/b"]`))
	other := newTestSession(t, engine, "capacity, no larger model")
	r = run(t, engine, other.ID, AgentOptions{})
	info, cause := sessionState(t, engine, other.ID)
	if r.Status != "failed" || info.Status != "failed" || cause == nil || cause.Kind != agent.KindProviderCapacity {
		t.Fatalf("result %+v session %s cause %+v", r, info.Status, cause)
	}
}

// Authentication and ordinary bad-request errors never trigger availability failover: the
// transcript is not sent to a second endpoint, and the session fails with the class as its cause.
func TestAuthAndBadRequestDoNotFailOver(t *testing.T) {
	for class, kind := range map[protocol.ErrorClass]string{
		protocol.ClassAuth:       agent.KindProviderAuth,
		protocol.ClassBadRequest: agent.KindProviderBadRequest,
	} {
		t.Run(string(class), func(t *testing.T) {
			useGlobalConfig(t, modelConfig(`"failover": ["test/b", "test/c"]`))
			engine := openTestEngine(t)
			pb, pc := &answering{final: "b"}, &answering{final: "c"}
			bindRef(engine, "a", failing(classed(class)))
			bindRef(engine, "b", pb)
			bindRef(engine, "c", pc)
			handle := newTestSession(t, engine, "refused")
			r := run(t, engine, handle.ID, AgentOptions{})
			info, cause := sessionState(t, engine, handle.ID)
			if r.Status != "failed" || info.Status != "failed" || cause == nil || cause.Kind != kind {
				t.Fatalf("result %+v session %s cause %+v, want failed/%s", r, info.Status, cause, kind)
			}
			if pb.calls.Load() != 0 || pc.calls.Load() != 0 {
				t.Fatalf("the transcript reached a failover model: b=%d c=%d", pb.calls.Load(), pc.calls.Load())
			}
			if !engine.health.Available("test/a") {
				t.Fatalf("a %s error must not mark the model unavailable", class)
			}
		})
	}
}

// Cancellation of the run ends it as interrupted/cancelled even though the provider reports an
// availability error, and no candidate is tried after it.
func TestRunCancellationIsNotFailedOverOrTimedOut(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"failover": ["test/b"]`))
	engine := openTestEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	pa := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		cancel()
		return protocol.ProviderResponse{}, classed(protocol.ClassAvailability)
	}}
	pb := &answering{final: "b"}
	bindRef(engine, "a", pa)
	bindRef(engine, "b", pb)
	handle := newTestSession(t, engine, "cancel")
	engine.RunAgent(ctx, handle.ID, AgentOptions{MaxSteps: 2})
	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "interrupted" || cause == nil || cause.Kind != agent.KindCancelled {
		t.Fatalf("session %s cause %+v, want interrupted/cancelled", info.Status, cause)
	}
	if pb.calls.Load() != 0 {
		t.Fatal("a cancelled run tried the failover model")
	}
	if !engine.health.Available("test/a") {
		t.Fatal("cancellation marked the model unavailable")
	}
}

// Health is per Engine and not persisted.
func TestHealthRegistryIsPerEngineAndNotPersisted(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"failover": ["test/b"]`))
	workspace := t.TempDir()
	first, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	bindRef(first, "a", failing(classed(protocol.ClassAvailability)))
	bindRef(first, "b", &answering{final: "b"})
	handle, err := first.NewSession(context.Background(), SessionOptions{Goal: "health"})
	if err != nil {
		t.Fatal(err)
	}
	if r := run(t, first, handle.ID, AgentOptions{}); r.Status != "completed" {
		t.Fatalf("result %+v", r)
	}
	if first.health.Available("test/a") {
		t.Fatal("failure was not recorded")
	}
	second := openEngineAt(t, workspace)
	if !second.health.Available("test/a") {
		t.Fatal("a second Engine sees the first one's health state")
	}
	first.Close()
	reopened := openEngineAt(t, workspace)
	if !reopened.health.Available("test/a") {
		t.Fatal("health survived a restart")
	}
}

// Every rejection happens before the Start transaction: the session row, the transcript and the
// provider are exactly as before, and the provider factory is the only thing that was consulted.
func TestRejectedStartsLeaveTheSessionUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		opts    AgentOptions
		want    string // substring of the error
		wantErr error
		// real builds the clients with the default factory (openai.FromModel) instead of the fakes.
		real bool
	}{
		{name: "unknown model", config: modelConfig(""), opts: AgentOptions{ModelRef: "test/zzz"}, wantErr: ErrUnknownModel},
		{name: "malformed model", config: modelConfig(""), opts: AgentOptions{ModelRef: "nonsense"}, wantErr: ErrUnknownModel},
		{name: "invalid effort", config: modelConfig(""), opts: AgentOptions{Effort: "extreme"}, wantErr: ErrInvalidEffort},
		{
			name: "no tool calling", wantErr: ErrModelWithoutToolCalling,
			config: strings.Replace(modelConfig(""), `"a": {"modelID": "a",`, `"a": {"modelID": "a", "toolCalling": false,`, 1),
		},
		{
			name: "missing credential", want: "NO_SUCH_KEY_VAR", real: true,
			config: strings.Replace(modelConfig(""), `"baseURL": "http://127.0.0.1:9/v1"`, `"baseURL": "http://127.0.0.1:9/v1", "apiKey": "{env:NO_SUCH_KEY_VAR}"`, 1),
		},
		{
			name: "missing endpoint", wantErr: openai.ErrNoEndpoint, real: true,
			config: strings.Replace(modelConfig(""), `"settings": {"baseURL": "http://127.0.0.1:9/v1"}`, `"settings": {}`, 1),
		},
		{name: "unknown failover model", config: modelConfig(`"failover": ["test/missing"]`), want: "not in the provider catalog"},
		{name: "failover model without credential", want: "NO_SUCH_KEY_VAR", real: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("NO_SUCH_KEY_VAR", "")
			cfg := c.config
			if c.name == "failover model without credential" {
				// b's provider is a second provider whose key variable is unset.
				cfg = `{"model": "test/a", "failover": ["other/b"], "providers": {
				  "test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {"a": {"modelID": "a"}}},
				  "other": {"settings": {"baseURL": "http://127.0.0.1:9/v1", "apiKey": "{env:NO_SUCH_KEY_VAR}"}, "models": {"b": {"modelID": "b"}}}}}`
			}
			useGlobalConfig(t, cfg)
			engine := openTestEngine(t)
			p := &answering{final: "never"}
			bindRef(engine, "a", p)
			bindRef(engine, "b", p)
			if c.real {
				engine.providerFactory = defaultProviderFactory
			}
			handle := newTestSession(t, engine, "rejected")
			before := snapshotStart(t, engine, handle)
			_, err := engine.RunAgent(context.Background(), handle.ID, c.opts.withInput("follow-up"))
			if err == nil {
				t.Fatal("the Start was accepted")
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) && !strings.Contains(err.Error(), c.wantErr.Error()) {
				t.Fatalf("err=%v, want %v", err, c.wantErr)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%v, want it to contain %q", err, c.want)
			}
			if after := snapshotStart(t, engine, handle); !reflect.DeepEqual(after, before) {
				t.Fatalf("a rejected Start changed the session:\nbefore %+v\nafter  %+v", before, after)
			}
			if p.calls.Load() != 0 {
				t.Fatal("the provider was called for a rejected Start")
			}
		})
	}
}

func (o AgentOptions) withInput(s string) AgentOptions {
	o.Input = s
	return o
}

// A provider answers with reasoning in every shape the OpenAI-compatible wire uses. None of it may
// reach SQLite, the event stream or the next request.
func TestHiddenReasoningIsNeverPersisted(t *testing.T) {
	const secret = "SECRET-HIDDEN-REASONING-7f3a"
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"role":"assistant","reasoning_content":"`+secret+`","reasoning":"`+secret+`","reasoning_details":[{"text":"`+secret+`"}]},"finish_reason":null}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"visible answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	useGlobalConfig(t, fmt.Sprintf(`{"model": "test/a", "providers": {"test": {"settings": {"baseURL": %q},
	  "models": {"a": {"modelID": "a", "reasoning": {"format": "deepseek", "efforts": ["low", "high"], "echo": "reasoning_content"}}}}}}`, server.URL))
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := engine.Events(256)
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "think hard"})
	if err != nil {
		t.Fatal(err)
	}
	r := run(t, engine, handle.ID, AgentOptions{Effort: "high"})
	if r.Status != "completed" || r.Final != "visible answer" {
		t.Fatalf("result %+v", r)
	}
	if r2 := run(t, engine, handle.ID, AgentOptions{Input: "and again"}); r2.Status != "completed" {
		t.Fatalf("second result %+v", r2)
	}
	unsubscribe()
	var eventJSON []string
	for len(events) > 0 {
		raw, _ := json.Marshal(<-events)
		eventJSON = append(eventJSON, string(raw))
	}
	if strings.Contains(strings.Join(eventJSON, "\n"), secret) {
		t.Fatal("hidden reasoning reached the event stream")
	}
	if strings.Contains(strings.Join(bodies, "\n"), secret) {
		t.Fatal("hidden reasoning was replayed into a later request")
	}
	store := engine.store.Path()
	engine.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		raw, err := os.ReadFile(store + suffix)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("hidden reasoning is in %s", filepath.Base(store+suffix))
		}
	}
	// The effort the run asked for did reach the wire, and the second request's window carried the
	// first answer, so the test exercised the request and window paths.
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"reasoning_effort":"high"`) || !strings.Contains(bodies[1], "visible answer") {
		t.Fatalf("request bodies: %v", bodies)
	}
}

// max_tokens is sent only when the run or the model's configuration sets it, and the reservation
// for capacity checks does not turn into a wire value.
func TestMaxTokensIsSentOnlyWhenRequestedOrConfigured(t *testing.T) {
	configured := strings.Replace(modelConfig(""), `"a": {"modelID": "a", "limit": {"context": 8000, "output": 1000}}`,
		`"a": {"modelID": "a", "limit": {"context": 8000, "output": 1000}}, "d": {"modelID": "d", "limit": {"context": 8000}}`, 1)
	useGlobalConfig(t, configured)
	engine := openTestEngine(t)
	pa, pd := &answering{final: "a"}, &answering{final: "d"}
	bindRef(engine, "a", pa)
	bindRef(engine, "d", pd)
	sent := func(p *answering) int { return p.last.Load().(protocol.ProviderRequest).MaxTokens }

	h := newTestSession(t, engine, "max tokens")
	run(t, engine, h.ID, AgentOptions{})
	if sent(pa) != 1000 {
		t.Fatalf("configured output limit sent as %d, want 1000", sent(pa))
	}
	run(t, engine, h.ID, AgentOptions{Input: "x", MaxTokens: 77})
	if sent(pa) != 77 {
		t.Fatalf("explicit MaxTokens sent as %d, want 77", sent(pa))
	}
	h2 := newTestSession(t, engine, "no output configured")
	run(t, engine, h2.ID, AgentOptions{ModelRef: "test/d"})
	if sent(pd) != 0 {
		t.Fatalf("an unconfigured output limit was sent as max_tokens=%d", sent(pd))
	}
	// Effort never scales it.
	run(t, engine, h2.ID, AgentOptions{Input: "y", Effort: "max"})
	if sent(pd) != 0 {
		t.Fatalf("effort scaled max_tokens to %d", sent(pd))
	}
}

// Options.ProviderFactory is how a host injects clients: it is called once per model the Start may
// use, with the resolved settings, before the run transaction.
func TestOptionsProviderFactoryBuildsEveryRequiredClientBeforeTheStart(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"failover": ["test/b", "test/c"]`))
	var built []config.ModelRef
	p := &answering{final: "ok"}
	engine, err := Open(Options{Workspace: t.TempDir(), ProviderFactory: func(m config.ResolvedModel) (protocol.Provider, error) {
		built = append(built, m.Ref)
		return p, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	handle := newTestSession(t, engine, "factory")
	if len(built) != 0 {
		t.Fatalf("clients built before a Start: %v", built)
	}
	if r := run(t, engine, handle.ID, AgentOptions{}); r.Status != "completed" {
		t.Fatalf("result %+v", r)
	}
	if want := []config.ModelRef{"test/a", "test/b", "test/c"}; !reflect.DeepEqual(built, want) {
		t.Fatalf("built %v, want the session model then the failover list %v", built, want)
	}
	// A factory error rejects the Start and nothing is written.
	boom := errors.New("factory refused")
	rejecting, err := Open(Options{Workspace: t.TempDir(), ProviderFactory: func(config.ResolvedModel) (protocol.Provider, error) { return nil, boom }})
	if err != nil {
		t.Fatal(err)
	}
	defer rejecting.Close()
	h := newTestSession(t, rejecting, "rejected by the factory")
	before := snapshotStart(t, rejecting, h)
	if _, err := rejecting.RunAgent(context.Background(), h.ID, AgentOptions{Input: "x"}); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if after := snapshotStart(t, rejecting, h); !reflect.DeepEqual(after, before) {
		t.Fatalf("a factory failure changed the session: %+v -> %+v", before, after)
	}
}
