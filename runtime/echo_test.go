package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// The §4.6 regression set (design §4.6, AGENTS.md reasoning boundary), through the real Engine, SQLite
// store and loop. Item 1 is protocol.TestEchoNeverSerialized; item 5 is internal/migrate's S1 test 14.

const echoSentinel = "SENTINEL-ECHO-7d41"

// echoStep is one scripted answer: a final text or one read_file call, with the echo the adapter
// returns, or an error.
type echoStep struct {
	final string
	call  string
	echo  string
	err   error
}

// seenRequest is what a request carried: its messages, and the echo bytes aligned with them.
type seenRequest struct {
	messages []protocol.Message
	echo     []string
}

func (s seenRequest) echoAt() map[int]string {
	out := map[int]string{}
	for i, e := range s.echo {
		if e != "" {
			out[i] = e
		}
	}
	return out
}

type echoProvider struct {
	mu     sync.Mutex
	script []echoStep
	seen   []seenRequest
}

func (p *echoProvider) Model() string { return "echo" }

func (p *echoProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(req.Echo) != 0 && len(req.Echo) != len(req.Messages) {
		return protocol.ProviderResponse{}, fmt.Errorf("echo has %d entries for %d messages", len(req.Echo), len(req.Messages))
	}
	seen := seenRequest{messages: append([]protocol.Message(nil), req.Messages...), echo: make([]string, len(req.Messages))}
	for i, e := range req.Echo {
		seen.echo[i] = string(e.Data())
	}
	p.seen = append(p.seen, seen)
	if len(p.script) == 0 {
		return protocol.ProviderResponse{}, fmt.Errorf("no scripted answer")
	}
	step := p.script[0]
	p.script = p.script[1:]
	if step.err != nil {
		return protocol.ProviderResponse{}, step.err
	}
	resp := protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: step.final}, FinishReason: "stop"}
	if step.call != "" {
		resp.Message.ToolCalls = []protocol.ToolCall{{ID: step.call, Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)}}
		resp.FinishReason = "tool_calls"
	}
	if step.echo != "" {
		resp.Echo = protocol.NewEcho("reasoning_content", []byte(step.echo), 12)
	}
	return resp, nil
}

func echoWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// assertNoSentinelAtRest checks every table of the closed database, its WAL and shared memory, and
// every backup, for the sentinel.
func assertNoSentinelAtRest(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	tables.Close()
	scanned := 0
	for _, name := range names {
		rows, err := db.Query(`SELECT * FROM "` + name + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			scanned++
			if text := fmt.Sprint(values...); strings.Contains(text, echoSentinel) {
				t.Errorf("table %s holds the echo sentinel: %s", name, text)
			}
			for _, v := range values {
				if b, ok := v.([]byte); ok && bytes.Contains(b, []byte(echoSentinel)) {
					t.Errorf("table %s holds the echo sentinel", name)
				}
			}
		}
		rows.Close()
	}
	db.Close()
	if scanned == 0 {
		t.Fatal("no rows scanned")
	}
	files, _ := filepath.Glob(dbPath + "*")
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "backups", "*"))
	for _, f := range append(files, backups...) {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if bytes.Contains(raw, []byte(echoSentinel)) {
			t.Errorf("%s holds the echo sentinel", filepath.Base(f))
		}
	}
}

func drainEvents(events <-chan Event) string {
	var out []string
	for len(events) > 0 {
		raw, _ := json.Marshal(<-events)
		out = append(out, string(raw))
	}
	return strings.Join(out, "\n")
}

// §4.6 regression 2: inside a run, every request carries the echo of exactly the current turn's
// assistant messages; a new turn (a later run's Input) carries none; afterwards no table, file or
// published event holds the echo.
func TestEchoStaysInRunMemory(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, err := Open(Options{Workspace: echoWorkspace(t)})
	if err != nil {
		t.Fatal(err)
	}
	events, unsubscribe := engine.Events(4096)
	p := &echoProvider{script: []echoStep{
		{call: "c1", echo: echoSentinel + "-1"},
		{call: "c2", echo: echoSentinel + "-2"},
		{final: "done", echo: echoSentinel + "-3"},
		{final: "done again"},
	}}
	bindRef(engine, "a", p)
	handle := newTestSession(t, engine, "read the readme twice")
	if r := run(t, engine, handle.ID, AgentOptions{MaxSteps: 5}); r.Status != "completed" {
		t.Fatalf("first run %+v", r)
	}
	if r := run(t, engine, handle.ID, AgentOptions{Input: "again", MaxSteps: 5}); r.Status != "completed" {
		t.Fatalf("second run %+v", r)
	}
	if len(p.seen) != 4 {
		t.Fatalf("requests=%d", len(p.seen))
	}
	assistantIndex := func(req seenRequest, call string) int {
		for i, m := range req.messages {
			if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == call {
				return i
			}
		}
		t.Fatalf("assistant %s not in the request", call)
		return -1
	}
	if got := p.seen[0].echoAt(); len(got) != 0 {
		t.Fatalf("request 1 echo %v", got)
	}
	if got, want := p.seen[1].echoAt(), map[int]string{assistantIndex(p.seen[1], "c1"): echoSentinel + "-1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("request 2 echo %v, want %v", got, want)
	}
	if got, want := p.seen[2].echoAt(), map[int]string{assistantIndex(p.seen[2], "c1"): echoSentinel + "-1", assistantIndex(p.seen[2], "c2"): echoSentinel + "-2"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("request 3 echo %v, want %v", got, want)
	}
	if got := p.seen[3].echoAt(); len(got) != 0 {
		t.Fatalf("the next run's request carries echo %v", got)
	}
	for i, req := range p.seen {
		raw, _ := json.Marshal(req.messages)
		if bytes.Contains(raw, []byte(echoSentinel)) {
			t.Fatalf("request %d messages hold the echo: %s", i, raw)
		}
	}
	verifyTrace(t, steps(t, engine, handle.ID))
	unsubscribe()
	if published := drainEvents(events); strings.Contains(published, echoSentinel) || published == "" {
		t.Fatalf("published events hold the echo (or none were published): %d bytes", len(published))
	}
	path := engine.store.Path()
	engine.Close()
	assertNoSentinelAtRest(t, path)
}

// §4.6 regression 3: a run interrupted mid-turn leaves its echo nowhere; a second run on a new Engine
// starts with exactly one resume row after the tool row and never sends the echo.
func TestResumeOnFreshEngineNeverSendsEcho(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	workspace := echoWorkspace(t)
	first, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	p1 := &echoProvider{script: []echoStep{{call: "c1", echo: echoSentinel}}}
	bindRef(first, "a", p1)
	handle := newTestSession(t, first, "inspect")
	if r := run(t, first, handle.ID, AgentOptions{MaxSteps: 1}); r.Status != "interrupted" {
		t.Fatalf("first run %+v", r)
	}
	id := handle.ID
	first.Close()

	second, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	p2 := &echoProvider{script: []echoStep{{final: "resumed"}}}
	bindRef(second, "a", p2)
	if r := run(t, second, id, AgentOptions{MaxSteps: 2}); r.Status != "completed" || r.Final != "resumed" {
		t.Fatalf("resumed run %+v", r)
	}
	handle2, _, err := second.Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	rows := transcript(t, handle2)
	var roles []string
	for _, m := range rows {
		roles = append(roles, m.Role+":"+m.Origin)
	}
	if want := "user: assistant: tool: user:resume assistant:"; strings.Join(roles, " ") != want {
		t.Fatalf("transcript %v, want %s", roles, want)
	}
	if len(p2.seen) != 1 {
		t.Fatalf("requests=%d", len(p2.seen))
	}
	req := p2.seen[0]
	if len(req.echoAt()) != 0 {
		t.Fatal("the resumed run sent echo")
	}
	if last := req.messages[len(req.messages)-1]; last.Origin != protocol.OriginResume || last.Content != protocol.ResumeContent {
		t.Fatalf("the resumed request ends with %+v, want the resume row", last)
	}
	raw, _ := json.Marshal(req.messages)
	if bytes.Contains(raw, []byte(echoSentinel)) {
		t.Fatalf("resumed request holds the echo: %s", raw)
	}
	path := second.store.Path()
	second.Close()
	assertNoSentinelAtRest(t, path)
}

// §4.6 regression 4: a failover in the middle of a turn appends exactly one turn row, the new model
// never receives the old model's echo, and it does receive its own within the new turn. The turn
// decision replays (I7).
func TestFailoverMidTurnAppendsOneTurnRow(t *testing.T) {
	useGlobalConfig(t, modelConfig(`"failover": ["test/b"]`))
	engine, err := Open(Options{Workspace: echoWorkspace(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	pa := &echoProvider{script: []echoStep{
		{call: "a1", echo: echoSentinel + "-a"},
		{err: classed(protocol.ClassAvailability)},
	}}
	pb := &echoProvider{script: []echoStep{
		{call: "b1", echo: echoSentinel + "-b"},
		{final: "done"},
	}}
	bindRef(engine, "a", pa)
	bindRef(engine, "b", pb)
	handle := newTestSession(t, engine, "failover mid-turn")
	if r := run(t, engine, handle.ID, AgentOptions{MaxSteps: 5}); r.Status != "completed" || r.Final != "done" {
		t.Fatalf("run %+v", r)
	}
	rows := transcript(t, handle)
	var roles []string
	turns := 0
	for _, m := range rows {
		roles = append(roles, m.Role+":"+m.Origin)
		if m.Origin == protocol.OriginTurn {
			turns++
		}
	}
	if want := "user: assistant: tool: user:turn assistant: tool: assistant:"; strings.Join(roles, " ") != want || turns != 1 {
		t.Fatalf("transcript %v, want %s", roles, want)
	}
	if len(pa.seen) != 2 || len(pb.seen) != 2 {
		t.Fatalf("requests a=%d b=%d", len(pa.seen), len(pb.seen))
	}
	if got := pa.seen[1].echoAt(); len(got) != 1 {
		t.Fatalf("model a's second request echo %v, want its own echo once", got)
	}
	if got := pb.seen[0].echoAt(); len(got) != 0 {
		t.Fatalf("model b received echo %v right after the failover", got)
	}
	if last := pb.seen[0].messages[len(pb.seen[0].messages)-1]; last.Content != protocol.TurnContent {
		t.Fatalf("model b's first request ends with %+v, want the turn row", last)
	}
	for i, req := range pb.seen {
		for _, e := range req.echo {
			if strings.HasSuffix(e, "-a") {
				t.Fatalf("model b's request %d carries model a's echo", i)
			}
		}
	}
	if got := pb.seen[1].echoAt(); len(got) != 1 || !strings.HasSuffix(fmt.Sprint(got), "-b]") {
		t.Fatalf("model b's second request echo %v, want its own", got)
	}
	trace := steps(t, engine, handle.ID)
	verifyTrace(t, trace)
	if !strings.Contains(strings.Join(rules(trace[1]), " "), "turn.boundary") {
		t.Fatalf("step 2 decisions %v, want turn.boundary", rules(trace[1]))
	}
}
