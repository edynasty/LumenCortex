//go:build unix

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

const dapAgentFixture = `#include <stdio.h>
#include <unistd.h>
int scale(int count) {
  int multiplier = count - 3;
  int adjusted = multiplier * 14;
  return adjusted + 1;
}
int main(void) {
  int total = scale(3);
  printf("total=%d expected=43\n", total);
  for (int i = 0; i < 300; i++) {
    usleep(10000);
  }
  return total == 43 ? 0 : 2;
}
`

// dapScript drives the real Agent loop through the debug tools from visible
// tool results only; it issues no model request.
type dapScript struct {
	t          *testing.T
	step       int
	phase      string
	debugID    string
	epoch      uint64
	threadID   int
	frameID    int
	oldVarRef  int
	adapterPID int
	lastCall   string
	facts      map[string]any
	tools      []string
}

func (p *dapScript) Model() string { return "dap-e1-scripted" }

func (p *dapScript) tool(name string, args map[string]any) (protocol.ProviderResponse, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	id := fmt.Sprintf("c%d", p.step)
	p.lastCall = id
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: raw}}}, FinishReason: "tool_calls", Usage: protocol.Usage{Requests: 1}}, nil
}

func (p *dapScript) dbg(extra map[string]any) map[string]any {
	m := map[string]any{"debug_id": p.debugID, "epoch": p.epoch}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (p *dapScript) previous(req protocol.ProviderRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" && req.Messages[i].ToolCallID == p.lastCall {
			return req.Messages[i].Content
		}
	}
	return ""
}

func (p *dapScript) fail(format string, args ...any) (protocol.ProviderResponse, error) {
	return protocol.ProviderResponse{}, fmt.Errorf(format, args...)
}

func (p *dapScript) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.step++
	if p.step > 60 {
		return p.fail("script bound exceeded")
	}
	prev := p.previous(req)
	switch p.phase {
	case "":
		for _, spec := range req.Tools {
			if strings.HasPrefix(spec.Name, "debug_") {
				p.tools = append(p.tools, spec.Name)
			}
		}
		if len(p.tools) == 0 {
			return p.fail("no debug tools offered to the agent")
		}
		p.phase = "reproduce"
		return p.tool("bash", map[string]any{"command": "./fixture; echo EXIT=$?"})
	case "reproduce":
		p.facts["reproduce"] = prev
		if !strings.Contains(prev, "total=1 expected=43") || !strings.Contains(prev, "EXIT=2") {
			return p.fail("fault not reproduced: %s", prev)
		}
		p.phase = "start"
		return p.tool("debug_start", map[string]any{"adapter_id": "lldb", "program": "fixture"})
	case "start":
		var snap struct {
			ID         string `json:"id"`
			State      string `json:"state"`
			Epoch      uint64 `json:"epoch"`
			AdapterPID int    `json:"adapterPid"`
		}
		if err := json.Unmarshal([]byte(prev), &snap); err != nil || snap.ID == "" {
			return p.fail("debug_start unusable: %s", prev)
		}
		p.debugID, p.epoch, p.adapterPID = snap.ID, snap.Epoch, snap.AdapterPID
		if snap.AdapterPID <= 1 {
			return p.fail("adapter pid not reported: %s", prev)
		}
		p.phase = "bp"
		return p.tool("debug_set_breakpoint", p.dbg(map[string]any{"path": "fixture.c", "breakpoints": []map[string]any{{"line": 4}, {"line": 9}}}))
	case "bp":
		if !strings.Contains(prev, `"verified":true`) {
			return p.fail("breakpoints not adapter-verified: %s", prev)
		}
		p.phase = "configure"
		return p.tool("debug_continue", p.dbg(map[string]any{"action": "configure"}))
	case "configure":
		// One call must carry the stop and its frames: E1-D1.
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil {
			return p.fail("continue did not return stop info: %s", prev)
		}
		if stop.Reason != "breakpoint" || stop.ThreadID <= 1 || stop.StopEpoch == 0 || len(stop.TopFrames) == 0 {
			return p.fail("continue stop info incomplete: %s", prev)
		}
		if stop.TopFrames[0].Name != "main" || stop.TopFrames[0].Line != 9 {
			return p.fail("wrong first stop: %s", prev)
		}
		p.threadID = stop.ThreadID
		p.facts["continue_stop"] = prev
		p.phase = "step_into"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_into", "thread_id": p.threadID}))
	case "step_into":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || len(stop.TopFrames) == 0 {
			return p.fail("step_into did not stop in scale: %s", prev)
		}
		if stop.TopFrames[0].Name != "scale" || stop.TopFrames[0].Line != 4 {
			return p.fail("step_into did not enter scale: %s", prev)
		}
		p.facts["step_into"] = stop.TopFrames[0]
		p.phase = "step_over"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_over", "thread_id": p.threadID}))
	case "step_over":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || stop.Reason != "step" || len(stop.TopFrames) == 0 {
			return p.fail("step_over did not stop: %s", prev)
		}
		if stop.TopFrames[0].Line != 5 {
			return p.fail("step_over landed on line %d, want 5: %s", stop.TopFrames[0].Line, prev)
		}
		p.frameID = stop.TopFrames[0].ID
		p.facts["step_over"] = stop.TopFrames[0]
		p.phase = "scopes"
		return p.tool("debug_stack", p.dbg(map[string]any{"kind": "scopes", "frame_id": p.frameID}))
	case "scopes":
		var body struct {
			Scopes []struct {
				Name string `json:"name"`
				Ref  int    `json:"variablesReference"`
			} `json:"scopes"`
		}
		if err := json.Unmarshal([]byte(prev), &body); err != nil || len(body.Scopes) == 0 {
			return p.fail("no scopes: %s", prev)
		}
		p.oldVarRef = body.Scopes[0].Ref
		p.phase = "vars"
		return p.tool("debug_variables", p.dbg(map[string]any{"variables_reference": p.oldVarRef}))
	case "vars":
		if !strings.Contains(prev, `"count"`) || !strings.Contains(prev, `"multiplier"`) {
			return p.fail("locals missing: %s", prev)
		}
		p.facts["locals"] = prev
		p.phase = "evaluate"
		return p.tool("debug_evaluate", p.dbg(map[string]any{"frame_id": p.frameID, "expression": "multiplier"}))
	case "evaluate":
		if !strings.Contains(prev, `"result":"0"`) {
			return p.fail("evaluate multiplier wrong: %s", prev)
		}
		p.phase = "evaluate_ungranted"
		return p.tool("debug_evaluate", p.dbg(map[string]any{"frame_id": p.frameID, "expression": "count * 14 + 1", "side_effects": true}))
	case "evaluate_ungranted":
		if !strings.Contains(prev, "not granted") {
			return p.fail("ungranted evaluation not distinct: %s", prev)
		}
		p.facts["evaluate_ungranted"] = prev
		p.phase = "step2"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_over", "thread_id": p.threadID}))
	case "step2":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || len(stop.TopFrames) == 0 {
			return p.fail("second step failed: %s", prev)
		}
		p.facts["step2_stop"] = stop.TopFrames[0]
		p.phase = "stale"
		return p.tool("debug_variables", p.dbg(map[string]any{"variables_reference": p.oldVarRef}))
	case "stale":
		if !strings.Contains(prev, "stale") && !strings.Contains(prev, "earlier stop") {
			return p.fail("old variables reference was not rejected: %s", prev)
		}
		p.facts["stale_reference"] = prev
		p.phase = "fresh_stack"
		return p.tool("debug_stack", p.dbg(map[string]any{"kind": "stackTrace", "thread_id": p.threadID, "count": 5}))
	case "fresh_stack":
		var body struct {
			Frames []struct {
				ID   int `json:"id"`
				Line int `json:"line"`
			} `json:"stackFrames"`
		}
		if err := json.Unmarshal([]byte(prev), &body); err != nil || len(body.Frames) == 0 {
			return p.fail("fresh stack missing: %s", prev)
		}
		p.frameID = body.Frames[0].ID
		p.phase = "fresh_scopes"
		return p.tool("debug_stack", p.dbg(map[string]any{"kind": "scopes", "frame_id": p.frameID}))
	case "fresh_scopes":
		var body struct {
			Scopes []struct {
				Ref int `json:"variablesReference"`
			} `json:"scopes"`
		}
		if err := json.Unmarshal([]byte(prev), &body); err != nil || len(body.Scopes) == 0 {
			return p.fail("fresh scopes missing: %s", prev)
		}
		p.phase = "fresh_vars"
		return p.tool("debug_variables", p.dbg(map[string]any{"variables_reference": body.Scopes[0].Ref}))
	case "fresh_vars":
		if !strings.Contains(prev, `"multiplier"`) {
			return p.fail("fresh variables missing: %s", prev)
		}
		p.facts["fresh_variables"] = prev
		p.phase = "step_out"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_out", "thread_id": p.threadID}))
	case "step_out":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || stop.Reason != "step" {
			return p.fail("step_out failed: %s", prev)
		}
		p.facts["step_out"] = stop.TopFrames
		p.phase = "resume"
		return p.tool("debug_continue", p.dbg(map[string]any{"action": "continue", "wait": false}))
	case "resume":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || stop.Reason != "pending" {
			return p.fail("async continue state missing: %s", prev)
		}
		p.phase = "pause"
		return p.tool("debug_pause", p.dbg(map[string]any{"thread_id": p.threadID, "timeout_sec": 10}))
	case "pause":
		// lldb-dap reports a pause as the signal stop it is: reason "exception"
		// with the interrupted frames. Either reason proves the target stopped.
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || (stop.Reason != "pause" && stop.Reason != "exception") || len(stop.TopFrames) == 0 {
			return p.fail("pause did not stop the target: %s", prev)
		}
		p.facts["pause"] = prev
		p.facts["pause_reason"] = stop.Reason
		p.phase = "stop"
		return p.tool("debug_stop", p.dbg(map[string]any{"terminate_debuggee": true}))
	case "stop":
		var result struct {
			State   string `json:"state"`
			Cleanup string `json:"cleanup"`
		}
		if err := json.Unmarshal([]byte(prev), &result); err != nil || result.State != "closed" {
			return p.fail("debug_stop did not close the session: %s", prev)
		}
		if result.Cleanup != "disconnect" {
			return p.fail("debug_stop did not disconnect gracefully: %s", prev)
		}
		p.facts["stop"] = prev
		p.phase = "fix"
		return p.tool("replace_in_file", map[string]any{"path": "fixture.c", "old_text": "int multiplier = count - 3;", "new_text": "int multiplier = count;"})
	case "fix":
		if !strings.Contains(prev, `"replaced":true`) {
			return p.fail("fix failed: %s", prev)
		}
		p.phase = "verify"
		return p.tool("bash", map[string]any{"command": "cc -g -O0 fixture.c -o fixture && ./fixture; echo EXIT=$?"})
	case "verify":
		p.facts["verify"] = prev
		if !strings.Contains(prev, "total=43 expected=43") || !strings.Contains(prev, "EXIT=0") {
			return p.fail("verification failed: %s", prev)
		}
		p.phase = "done"
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "Breakpoint showed multiplier=0 for count=3; fixed the offset; re-ran and got total=43."}, FinishReason: "stop", Usage: protocol.Usage{Requests: 1}}, nil
	}
	return p.fail("unexpected phase %q", p.phase)
}

type stopShape struct {
	Reason    string `json:"reason"`
	ThreadID  int    `json:"threadId"`
	StopEpoch uint64 `json:"stopEpoch"`
	TopFrames []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Line int    `json:"line"`
	} `json:"topFrames"`
}

// agentDAPEngine opens an Engine whose workspace is trusted and whose provider
// is the scripted one.
func agentDAPEngine(t *testing.T, workspace string, provider protocol.Provider, debug *DebugOptions) *Engine {
	t.Helper()
	xdg := t.TempDir()
	dir := filepath.Join(xdg, "lumencortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"model":"test/b0","trustedWorkspaces":[%q],"providers":{"test":{"settings":{"baseURL":"http://127.0.0.1:9/v1"},"models":{"b0":{"modelID":"b0"}}}}}`, workspace)
	if err := os.WriteFile(filepath.Join(dir, "lumencortex.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	engine, err := Open(Options{Workspace: workspace, Debug: debug, ProviderFactory: func(config.ResolvedModel) (protocol.Provider, error) {
		return provider, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

// TestAgentLoopDebugsRealLLDB converts the E1 experiment into a regression
// test: the real Agent loop drives a real lldb-dap through the debug tools.
func TestAgentLoopDebugsRealLLDB(t *testing.T) {
	if os.Getenv("LC_TEST_LLDB") == "" {
		t.Skip("set LC_TEST_LLDB for the real adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.c"), []byte(dapAgentFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	compile := exec.CommandContext(ctx, "cc", "-g", "-O0", "fixture.c", "-o", "fixture")
	compile.Dir = root
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile %v %s", err, out)
	}
	adapter := testLLDB(t)
	script := &dapScript{t: t, facts: map[string]any{}}
	engine := agentDAPEngine(t, root, script, &DebugOptions{Sandbox: testSandbox(t), Adapters: []DebugAdapter{{ID: "lldb", Command: adapter, ReadPaths: testReadPaths(adapter), Launch: map[string]any{"disableASLR": false}}}})
	session, err := engine.NewSession(ctx, SessionOptions{Goal: "find the wrong total with the debugger, fix it, re-verify"})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	result, runErr := engine.RunAgent(ctx, session.ID, AgentOptions{Policy: "full", MaxSteps: 60})
	if runErr != nil {
		t.Fatalf("agent run: %v facts=%v", runErr, script.facts)
	}
	if result.Status != "completed" {
		t.Fatalf("run status %q facts=%v", result.Status, script.facts)
	}
	live, err := engine.DebugSessions(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("live debug sessions after run: %+v", live)
	}
	final, err := os.ReadFile(filepath.Join(root, "fixture.c"))
	if err != nil || !strings.Contains(string(final), "int multiplier = count;") {
		t.Fatalf("fix not applied: %s %v", final, err)
	}
	page, err := engine.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: session.ID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	modules := 0
	for _, event := range page.Events {
		if event.Type == "debug.module" {
			modules++
		}
	}
	if modules != 0 {
		t.Fatalf("%d debug.module events entered durable history", modules)
	}
	if script.adapterPID > 1 && processAlive(script.adapterPID) {
		t.Fatalf("adapter process %d outlived the run", script.adapterPID)
	}
	t.Logf("LLDB_AGENT_LOOP: tools=%v stop=%v step_in=%v step_out=%v pause=%v cleanup=disconnect",
		script.tools, script.facts["continue_stop"], script.facts["step_into"], script.facts["step_out"], script.facts["pause"])
}
