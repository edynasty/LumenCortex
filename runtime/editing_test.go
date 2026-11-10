package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

func resultOf(req protocol.ProviderRequest, id string) string {
	for _, message := range req.Messages {
		if message.Role == "tool" && message.ToolCallID == id {
			return message.Content
		}
	}
	return ""
}

// The Agent loop drives Hashline editing end to end: read with anchors, edit a line, then a second edit
// computed from the stale revision is rejected with the code and hint the model needs.
func TestAgentLoopEditsLinesAndRejectsAStaleEdit(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, root := gitEngine(t)
	target := filepath.Join(root, "demo.go")
	if err := os.WriteFile(target, []byte(hashlineFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The revision and anchor are only known at run time, so the scripted provider derives them from
	// the read_file result the loop hands back.
	dynamic := &scriptedEditProvider{engine: engine, root: root}

	bindRef(engine, "a", dynamic)
	session := newTestSession(t, engine, "edit a line with anchors")
	result, runErr := engine.RunAgent(ctx, session.ID, AgentOptions{MaxSteps: 8, Policy: "full"})
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	if result.Status != "completed" {
		t.Fatalf("run = %+v", result)
	}

	content := readTestWorkspaceFile(t, target)
	if !strings.Contains(content, "// concurrent") {
		t.Fatalf("the concurrent change was lost:\n%s", content)
	}
	if !strings.Contains(content, "return a + b") {
		t.Fatalf("the first edit was lost:\n%s", content)
	}
	if strings.Contains(content, "return a * b") {
		t.Fatalf("the stale edit was applied:\n%s", content)
	}
	// The stale refusal reached the model with a code and a remediation.
	stale := dynamic.refusal
	if stale == "" || !strings.Contains(stale, "stale") || !strings.Contains(stale, "re-read") {
		t.Fatalf("stale refusal = %q", stale)
	}
	if _, err := engine.WriteLeases(ctx); err != nil {
		t.Fatal(err)
	}
}

const hashlineFixture = "package demo\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"

// scriptedEditProvider runs the four-step edit flow, deriving the anchor and revision from the tool
// result the loop hands back.
type scriptedEditProvider struct {
	engine   *Engine
	root     string
	step     int
	rev      string
	anchor   string
	refusal  string
	reloaded bool
}

func (p *scriptedEditProvider) Model() string { return "scripted-editing" }

func (p *scriptedEditProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	switch p.step {
	case 0:
		p.step++
		return toolCalls(tc("read", "read_file", `{"path":"demo.go","anchors":true}`)), nil
	case 1:
		read := resultOf(req, "read")
		p.rev, p.anchor = parseAnchorHeader(read)
		if p.rev == "" || p.anchor == "" {
			return protocol.ProviderResponse{}, errNoAnchor
		}
		p.step++
		return toolCalls(tc("edit", "edit_lines", mustJSON(map[string]any{
			"path": "demo.go", "rev": p.rev,
			"ops": []any{map[string]any{"op": "replace", "start": p.anchor, "text": "\treturn a + b"}},
		}))), nil
	case 2:
		// Another writer changes the file after the first edit landed; the second edit still names the
		// revision read at step 0, so it must be refused.
		path := filepath.Join(p.root, "demo.go")
		raw, err := os.ReadFile(path)
		if err != nil {
			return protocol.ProviderResponse{}, err
		}
		if err := os.WriteFile(path, append(raw, []byte("// concurrent\n")...), 0o644); err != nil {
			return protocol.ProviderResponse{}, err
		}
		p.step++
		return toolCalls(tc("stale", "edit_lines", mustJSON(map[string]any{
			"path": "demo.go", "rev": p.rev,
			"ops": []any{map[string]any{"op": "replace", "start": p.anchor, "text": "\treturn a * b"}},
		}))), nil
	default:
		p.refusal = resultOf(req, "stale")
		p.step++
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
	}
}

var errNoAnchor = &editTestError{"the read result carried no anchored revision"}

type editTestError struct{ message string }

func (e *editTestError) Error() string { return e.message }

// parseAnchorHeader returns the revision of the header and the anchor of the first row.
func parseAnchorHeader(rendered string) (rev, anchor string) {
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "@@ ") {
			for _, field := range strings.Fields(line) {
				if strings.HasPrefix(field, "rev=") {
					rev = strings.TrimPrefix(field, "rev=")
				}
			}
			continue
		}
		if strings.Contains(line, "|") && anchor == "" {
			candidate, _, _ := strings.Cut(line, "|")
			if strings.Contains(candidate, ":") {
				anchor = candidate
			}
		}
	}
	if anchor != "" {
		// The fixture's first row is package demo; the edited line is the one naming the expression.
		for _, line := range strings.Split(rendered, "\n") {
			index := strings.Index(line, "|")
			if index < 0 || !strings.Contains(line, "return a - b") {
				continue
			}
			anchor = line[:index]
		}
	}
	return rev, anchor
}

func readTestWorkspaceFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A write lease taken through the Engine's host API refuses another session's write tool and, once
// released, lets it through.
func TestWriteLeaseBlocksAnotherSessionsWriteTool(t *testing.T) {
	engine, root := gitEngine(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "demo.go"), []byte(hashlineFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	lease, err := engine.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "demo.go", Holder: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "session-a" || lease.Prefix {
		t.Fatalf("lease = %+v", lease)
	}
	listed, err := engine.WriteLeases(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("leases = %+v, %v", listed, err)
	}
	other, err := toolset.New(toolset.Options{Workspace: root, Policy: toolset.PolicyFull,
		SessionID: "session-b", Editing: engine.editing})
	if err != nil {
		t.Fatal(err)
	}
	result, execErr := other.Execute(ctx, "write_file", map[string]any{"path": "demo.go", "content": "package demo\n"}, nil)
	if execErr != nil {
		t.Fatal(execErr)
	}
	if result.OK {
		t.Fatalf("a leased file was written: %s", result.Content)
	}
	var refusal struct {
		Code   string `json:"code"`
		Holder string `json:"holder"`
	}
	if err := json.Unmarshal([]byte(result.Content), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "lease_conflict" || refusal.Holder != "session-a" {
		t.Fatalf("refusal = %s", result.Content)
	}
	if err := engine.ReleaseWriteLease(ctx, WriteLeaseRequest{Path: "demo.go", Holder: "session-a"}); err != nil {
		t.Fatal(err)
	}
	result, execErr = other.Execute(ctx, "write_file", map[string]any{"path": "demo.go", "content": "package demo\n"}, nil)
	if execErr != nil || !result.OK {
		t.Fatalf("write after release = %s, %v", result.Content, execErr)
	}
}

// A whole-file write computed from an old read is refused once the file changed (E3 S7b), through the
// real tool registry the Agent loop uses.
func TestWriteFileRefusesAStaleReadInTheRunRegistry(t *testing.T) {
	engine, root := gitEngine(t)
	ctx := context.Background()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := toolset.New(toolset.Options{Workspace: root, Policy: toolset.PolicyFull,
		SessionID: "session-1", Editing: engine.editing,
		Events: func(kind, sid string, payload map[string]any) { engine.events.publish(kind, sid, payload) }})
	if err != nil {
		t.Fatal(err)
	}
	if result, execErr := set.Execute(ctx, "read_file", map[string]any{"path": "notes.txt"}, nil); execErr != nil || !result.OK {
		t.Fatalf("read = %s, %v", result.Content, execErr)
	}
	if err := os.WriteFile(path, []byte("alpha\nconcurrent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, execErr := set.Execute(ctx, "write_file", map[string]any{"path": "notes.txt", "content": "rewritten\n"}, nil)
	if execErr != nil {
		t.Fatal(execErr)
	}
	if result.OK {
		t.Fatalf("a stale whole-file write succeeded: %s", result.Content)
	}
	var refusal struct {
		Code string `json:"code"`
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(result.Content), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Code != "conflict" || !strings.Contains(refusal.Hint, "re-read") {
		t.Fatalf("refusal = %s", result.Content)
	}
	if got := readTestWorkspaceFile(t, path); got != "alpha\nconcurrent\n" {
		t.Fatalf("the concurrent write was lost: %q", got)
	}
}

// A commit publishes edit.committed on the run's event bus with the path and both revisions, and a
// refused edit publishes edit.conflict with its code.
func TestEditEventsReachSubscribers(t *testing.T) {
	engine, root := gitEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := newTestSession(t, engine, "watch edits")
	events, unsubscribe, err := engine.SubscribeRuntimeEvents(ctx, session.ID, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	set, err := toolset.New(toolset.Options{Workspace: root, Policy: toolset.PolicyFull, SessionID: session.ID,
		Editing: engine.editing,
		Events:  func(kind, sid string, payload map[string]any) { engine.events.publish(kind, sid, payload) }})
	if err != nil {
		t.Fatal(err)
	}
	if result, execErr := set.Execute(ctx, "write_file", map[string]any{"path": "a.txt", "content": "alpha\n"}, nil); execErr != nil || !result.OK {
		t.Fatalf("write = %s, %v", result.Content, execErr)
	}
	// A read then an external change makes the next whole-file write a conflict.
	if result, execErr := set.Execute(ctx, "read_file", map[string]any{"path": "a.txt"}, nil); execErr != nil || !result.OK {
		t.Fatalf("read = %s, %v", result.Content, execErr)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\nother\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if result, execErr := set.Execute(ctx, "write_file", map[string]any{"path": "a.txt", "content": "beta\n"}, nil); execErr != nil || result.OK {
		t.Fatalf("stale write = %s, %v", result.Content, execErr)
	}

	seen := map[string]map[string]any{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case event := <-events:
			var payload map[string]any
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatalf("%s payload: %v", event.Type, err)
			}
			seen[event.Type] = payload
		case <-deadline:
			t.Fatalf("events seen: %v", seen)
		}
	}
	committed := seen["edit.committed"]
	if committed["path"] != "a.txt" || committed["tool"] != "write_file" || committed["actor"] != session.ID {
		t.Fatalf("edit.committed = %v", committed)
	}
	if committed["newRev"] == "" || committed["newRev"] == committed["oldRev"] {
		t.Fatalf("edit.committed revisions = %v", committed)
	}
	conflict := seen["edit.conflict"]
	if conflict["code"] != "conflict" || conflict["path"] != "a.txt" {
		t.Fatalf("edit.conflict = %v", conflict)
	}
}
