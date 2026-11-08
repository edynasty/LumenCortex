package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

type hubFixtureProvider struct {
	mu      sync.Mutex
	arrived chan string
	release chan struct{}
	started map[string]bool
}

func (p *hubFixtureProvider) Model() string { return "hub-fixture" }
func (p *hubFixtureProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	goal := ""
	for _, m := range req.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "HUB:") {
			goal = m.Content
			break
		}
	}
	if goal == "" {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "root"}, FinishReason: "stop"}, nil
	}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "finished " + goal}, FinishReason: "stop"}, nil
		}
	}
	p.mu.Lock()
	first := !p.started[goal]
	p.started[goal] = true
	p.mu.Unlock()
	if first && goal != "HUB:integration" {
		p.arrived <- goal
		select {
		case <-ctx.Done():
			return protocol.ProviderResponse{}, ctx.Err()
		case <-p.release:
		}
	}
	body, _ := json.Marshal(map[string]string{"path": "result.txt", "content": goal})
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
		call("write", "write_file", string(body)), call("unit", "work_unit_update", `{"id":"task","status":"completed","summary":"wrote result.txt"}`),
	}}, FinishReason: "tool_calls"}, nil
}
func TestHubFourWorkersAndDependencyAdmission(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	root, _ := taskRepo(t)
	e, s := openTaskEngine(t, root)
	defer e.Close()
	defer s.Close()
	p := &hubFixtureProvider{arrived: make(chan string, 4), release: make(chan struct{}), started: map[string]bool{}}
	parent := newParentSession(t, e, p)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	specs := []HubTaskSpec{}
	for _, id := range []string{"a", "b", "c", "d"} {
		specs = append(specs, HubTaskSpec{ID: id, Goal: "HUB:" + id})
	}
	specs = append(specs, HubTaskSpec{ID: "integration", Goal: "HUB:integration", DependsOn: []string{"a", "b", "c", "d"}})
	if err := e.ConfigureHub(ctx, parent, specs); err != nil {
		t.Fatal(err)
	}
	hub, err := s.Hub(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartProposedTask(ctx, parent, hub.Tasks[4].TaskID); err == nil {
		t.Fatal("unmet dependencies admitted")
	}
	children := []ExecutionTask{}
	for i := 0; i < 4; i++ {
		child, err := s.StartProposedTask(ctx, parent, hub.Tasks[i].TaskID)
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	seen := map[string]bool{}
	for range 4 {
		select {
		case id := <-p.arrived:
			seen[id] = true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if len(seen) != 4 {
		t.Fatalf("missing concurrent workers: %v", seen)
	}
	paths := map[string]bool{}
	for _, child := range children {
		if !s.Active(child.SessionID) || paths[child.Runtime.Path] || child.Runtime.Path == root {
			t.Fatalf("not isolated/live: %+v", child)
		}
		paths[child.Runtime.Path] = true
	}
	if _, err = s.StartProposedTask(ctx, parent, hub.Tasks[0].TaskID); err == nil {
		t.Fatal("duplicate assignment started")
	}
	m, err := e.SendWorkerMessage(ctx, parent, children[0].SessionID, children[1].SessionID, "message-1", "artifact data: value=42")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.SendWorkerMessage(ctx, parent, children[0].SessionID, children[1].SessionID, "message-1", "artifact data: value=42")
	if err != nil || again.Seq != m.Seq {
		t.Fatalf("duplicate message: %+v %v", again, err)
	}
	if _, err = e.SendWorkerMessage(ctx, parent, children[0].SessionID, children[1].SessionID, "message-1", "altered"); err == nil {
		t.Fatal("message id reused for different content")
	}
	inbox, err := e.WorkerMessages(ctx, parent, children[1].SessionID, 0, 20, true)
	if err != nil || len(inbox.Messages) != 1 || inbox.Messages[0].Body != "artifact data: value=42" || inbox.Messages[0].ReceivedAt == nil {
		t.Fatalf("delivery: %+v %v", inbox, err)
	}
	foreign := newParentSession(t, e, p)
	if _, err = e.SendWorkerMessage(ctx, parent, foreign, children[0].SessionID, "escape", "bad"); err == nil {
		t.Fatal("foreign sender admitted")
	}
	if _, err = e.WorkerMessages(ctx, parent, foreign, 0, 20, false); err == nil {
		t.Fatal("foreign inbox read admitted")
	}
	if _, err = s.StartProposedTask(ctx, parent, hub.Tasks[4].TaskID); err == nil {
		t.Fatal("running dependency counted complete")
	}
	close(p.release)
	for _, child := range children {
		waitInactive(t, s, child.SessionID)
	}
	hub, err = s.Hub(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if hub.Tasks[i].State != "completed" {
			t.Fatalf("worker result: %+v", hub.Tasks[i])
		}
		data, err := os.ReadFile(filepath.Join(children[i].Runtime.Path, "result.txt"))
		if err != nil || string(data) != "HUB:"+specs[i].ID {
			t.Fatalf("worktree content %q %v", data, err)
		}
	}
	if _, err = os.Stat(filepath.Join(root, "result.txt")); !os.IsNotExist(err) {
		t.Fatal("worker wrote parent checkout")
	}
	if hub.Tasks[4].State != "ready" {
		t.Fatalf("dependency graph: %+v", hub.Tasks)
	}
	final, err := s.StartProposedTask(ctx, parent, hub.Tasks[4].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	waitInactive(t, s, final.SessionID)
	result, err := s.Hub(ctx, parent)
	if err != nil || result.Tasks[4].State != "completed" {
		t.Fatalf("final: %+v %v", result, err)
	}
	t.Log(fmt.Sprintf("HUB_REAL: four concurrent Core workers, %d isolated worktrees, four writes, dependency denial/ready/start, message dedup and foreign denial; external model requests=0", len(paths)))
}
