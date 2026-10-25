package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// e2eProvider is the main model of TestRealEngineEndToEndProvenance. The parent (its requests offer
// spawn_subagent) writes a file, derives a second with shell, spawns a child, waits for it and answers;
// the child (no spawn_subagent) answers at once. A second turn is answered directly. It keeps every
// request it received, per session role.
type e2eProvider struct {
	mu      sync.Mutex
	parent  []protocol.ProviderRequest
	child   []protocol.ProviderRequest
	childID string
}

func (p *e2eProvider) Model() string { return "e2e" }

func (p *e2eProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !hasTool(req, "spawn_subagent") {
		p.child = append(p.child, req)
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child evidence"}, FinishReason: "stop"}, nil
	}
	p.parent = append(p.parent, req)
	switch len(p.parent) {
	case 1:
		return toolCalls(tc("w", "write_file", `{"path":"a.txt","content":"alpha\n"}`)), nil
	case 2:
		return toolCalls(tc("s", "shell", `{"command":"cat a.txt a.txt > b.txt"}`)), nil
	case 3:
		return toolCalls(tc("spawn", "spawn_subagent", `{"goal":"inspect a.txt"}`)), nil
	case 4:
		var node SubagentNode
		_ = json.Unmarshal([]byte(lastToolResult(req, "spawn")), &node)
		p.childID = node.SessionID
		return toolCalls(tc("wait", "wait_subagent", `{"child_session_id":"`+node.SessionID+`","timeout_seconds":10}`)), nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: fmt.Sprintf("parent answer %d", len(p.parent))}, FinishReason: "stop"}, nil
}

// T1-S7 item 1: one multi-turn task on the real Engine and SQLite with Auto effort on: write_file,
// shell, a child session and a second turn. Afterwards the stored provenance matches what happened:
// one answered main envelope per request each model call received, carrying the digest of the tools
// that request offered; one answered auto_effort envelope per judge call; a completed, lineage-complete
// execution for each side-effect call under the request that issued it; file edges naming the blobs
// now on disk; the child derived from the spawning request; the wait result derived from the child.
func TestRealEngineEndToEndProvenance(t *testing.T) {
	workspace := t.TempDir()
	gitInit(t, workspace)
	engine, err := Open(Options{Workspace: workspace, Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	main, judge := &e2eProvider{}, &judgeProvider{level: "high"}
	bindRef(engine, "b0", main)
	bindRef(engine, "b1", judge)
	useGlobalConfig(t, autoConfig("test/b0", "test/b1", "high", ""))
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	handle := newTestSession(t, engine, "write a.txt, derive b.txt, have a child inspect it")
	parent := handle.ID

	for _, input := range []string{"", "what did the child find?"} {
		if _, err := supervisor.Start(context.Background(), parent, AgentOptions{Input: input, Effort: config.EffortAuto, Policy: "full", MaxSteps: 8}); err != nil {
			t.Fatal(err)
		}
		waitInactive(t, supervisor, parent)
		if info, cause := sessionState(t, engine, parent); info.Status != "completed" {
			t.Fatalf("turn %q ended %s (%+v)", input, info.Status, cause)
		}
	}
	if main.childID == "" {
		t.Fatal("the parent never saw its child")
	}
	waitInactive(t, supervisor, main.childID)
	if len(main.parent) != 6 || len(main.child) != 1 {
		t.Fatalf("model calls: parent %d, child %d; want 6 and 1", len(main.parent), len(main.child))
	}

	// Envelopes against the requests the models received.
	judged := 0
	for _, s := range []struct {
		id   string
		reqs []protocol.ProviderRequest
	}{{parent, main.parent}, {main.childID, main.child}} {
		var mains []envRow
		for _, e := range envelopeRows(t, engine, s.id) {
			if e.State != "answered" || e.Lineage != 1 {
				t.Errorf("session %s envelope %+v, want answered and complete", s.id, e)
			}
			switch e.Purpose {
			case session.PurposeMain:
				mains = append(mains, e)
			case session.PurposeAutoEffort:
				judged++
			default:
				t.Errorf("session %s envelope purpose %q", s.id, e.Purpose)
			}
		}
		if len(mains) != len(s.reqs) {
			t.Fatalf("session %s: %d main envelopes for %d model requests", s.id, len(mains), len(s.reqs))
		}
		for i, e := range mains {
			if want := specsDigest(t, s.reqs[i].Tools); e.Digest != want {
				t.Errorf("session %s request %d: envelope digest %s, the tools sent hash to %s", s.id, i+1, e.Digest, want)
			}
		}
	}
	if calls := int(judge.calls.Load()); calls == 0 || calls != judged {
		t.Fatalf("judge calls %d, answered auto_effort envelopes %d", calls, judged)
	}
	// A judge envelope does not name the stored input row it classified (T1-S3 OPEN 1). The row is
	// still recoverable: a main envelope of the same run covers it. Each parent turn's input row is the
	// user row holding that turn's text.
	inputSeq := map[string]int64{}
	if err := engine.store.EachMessageFrom(context.Background(), parent, 0, func(m session.Message) error {
		var msg protocol.Message
		if err := json.Unmarshal(m.JSON, &msg); err != nil {
			return err
		}
		if msg.Role == "user" {
			inputSeq[msg.Content] = m.Seq
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	turnInputs := []string{"write a.txt, derive b.txt, have a child inspect it", "what did the child find?"}
	turn := 0
	parentEnvs := envelopeRows(t, engine, parent)
	for _, j := range parentEnvs {
		if j.Purpose != session.PurposeAutoEffort {
			continue
		}
		if turn >= len(turnInputs) {
			t.Fatalf("more parent judge envelopes than turns: %+v", parentEnvs)
		}
		seq, ok := inputSeq[turnInputs[turn]]
		if !ok {
			t.Fatalf("turn %d input %q is not a user row", turn+1, turnInputs[turn])
		}
		covered := false
		for _, m := range parentEnvs {
			if m.Purpose == session.PurposeMain && m.RunID == j.RunID && m.First.Valid && m.Last.Valid && m.First.Int64 <= seq && seq <= m.Last.Int64 {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("judge envelope %s (run %s): no main envelope of its run covers the turn's input row %d", j.ID, j.RunID, seq)
		}
		turn++
	}
	if turn != len(turnInputs) {
		t.Fatalf("%d parent judge envelopes for %d turns", turn, len(turnInputs))
	}

	// Executions and file edges against the workspace.
	var mains []envRow
	for _, e := range envelopeRows(t, engine, parent) {
		if e.Purpose == session.PurposeMain {
			mains = append(mains, e)
		}
	}
	execs, edges := readProvenance(t, engine, parent)
	byTool := map[string]execRow{}
	for _, x := range execs {
		if x.Outcome != "completed" || x.Lineage != 1 {
			t.Errorf("execution %+v, want completed and complete", x)
		}
		byTool[x.Tool] = x
	}
	if x, ok := byTool["write_file"]; !ok || x.Request != mains[0].ID {
		t.Errorf("write_file execution %+v, want under request 1 (%s)", x, mains[0].ID)
	}
	if x, ok := byTool["shell"]; !ok || x.Request != mains[1].ID {
		t.Errorf("shell execution %+v, want under request 2 (%s)", x, mains[1].ID)
	}
	if raw, _ := os.ReadFile(filepath.Join(workspace, "b.txt")); string(raw) != "alpha\nalpha\n" {
		t.Fatalf("b.txt = %q", raw)
	}
	var got []string
	for _, e := range edges {
		if e.SourceKind != "request" || e.Origin != "agent_generated" || e.Lineage != 1 {
			t.Errorf("file edge %+v", e)
		}
		got = append(got, e.Target+" <- "+e.Source)
	}
	sort.Strings(got)
	want := []string{
		"a.txt@" + gitBlob(t, workspace, "a.txt") + " <- " + mains[0].ID,
		"b.txt@" + gitBlob(t, workspace, "b.txt") + " <- " + mains[1].ID,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("file edges:\n got %v\nwant %v", got, want)
	}

	// The child and the wait result.
	var spawn, wait *messageEdge
	msgEdges := messageEdges(t, engine, parent)
	for i := range msgEdges {
		switch {
		case msgEdges[i].Kind == "child_session" && msgEdges[i].Target == "session:"+main.childID:
			spawn = &msgEdges[i]
		case msgEdges[i].Kind == "message" && strings.HasSuffix(msgEdges[i].Target, ":wait"):
			wait = &msgEdges[i]
		}
	}
	if spawn == nil || spawn.SourceKind != "request" || spawn.Source != mains[2].ID || spawn.Lineage != 1 {
		t.Fatalf("child edge %+v, want from request 3 (%s); edges %+v", spawn, mains[2].ID, msgEdges)
	}
	if wait == nil || wait.SourceKind != "session" || wait.Source != main.childID || wait.Lineage != 1 {
		t.Fatalf("wait edge %+v; edges %+v", wait, msgEdges)
	}
	assertPaired(t, transcriptRows(t, engine, parent))
}
