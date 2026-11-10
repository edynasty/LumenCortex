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

const delveAgentFixture = `package main

import "fmt"

func scale(count int) int {
	multiplier := count - 3
	adjusted := multiplier * 14
	return adjusted + 1
}

func main() {
	total := scale(3)
	fmt.Printf("total=%d expected=43\n", total)
	for i := 0; i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
	}
}
`

// delveScript runs the same diagnostic loop as the LLDB test against Delve.
type delveScript struct {
	step     int
	phase    string
	debugID  string
	epoch    uint64
	threadID int
	frameID  int
	oldRef   int
	lastCall string
	facts    map[string]any
}

func (p *delveScript) Model() string { return "delve-e1-scripted" }

func (p *delveScript) tool(name string, args map[string]any) (protocol.ProviderResponse, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	id := fmt.Sprintf("d%d", p.step)
	p.lastCall = id
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: raw}}}}, nil
}

func (p *delveScript) dbg(extra map[string]any) map[string]any {
	m := map[string]any{"debug_id": p.debugID, "epoch": p.epoch}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (p *delveScript) prev(req protocol.ProviderRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" && req.Messages[i].ToolCallID == p.lastCall {
			return req.Messages[i].Content
		}
	}
	return ""
}

func (p *delveScript) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.step++
	if p.step > 60 {
		return protocol.ProviderResponse{}, fmt.Errorf("script bound exceeded")
	}
	prev := p.prev(req)
	switch p.phase {
	case "":
		p.phase = "run"
		return p.tool("bash", map[string]any{"command": "./fixture; echo EXIT=$?"})
	case "run":
		p.facts["run"] = prev
		if !strings.Contains(prev, "total=1 expected=43") {
			return protocol.ProviderResponse{}, fmt.Errorf("fault not reproduced: %s", prev)
		}
		p.phase = "start"
		return p.tool("debug_start", map[string]any{"adapter_id": "delve", "program": "fixture"})
	case "start":
		var snap struct {
			ID    string `json:"id"`
			Epoch uint64 `json:"epoch"`
		}
		if err := json.Unmarshal([]byte(prev), &snap); err != nil || snap.ID == "" {
			return protocol.ProviderResponse{}, fmt.Errorf("debug_start unusable: %s", prev)
		}
		p.debugID, p.epoch = snap.ID, snap.Epoch
		p.phase = "bp"
		return p.tool("debug_set_breakpoint", p.dbg(map[string]any{"path": "main.go", "breakpoints": []map[string]any{{"line": 9}, {"line": 15}}}))
	case "bp":
		p.phase = "configure"
		return p.tool("debug_continue", p.dbg(map[string]any{"action": "configure"}))
	case "configure":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || stop.Reason != "breakpoint" || len(stop.TopFrames) == 0 {
			return protocol.ProviderResponse{}, fmt.Errorf("delve continue stop missing: %s", prev)
		}
		if stop.TopFrames[0].Name != "main" || stop.TopFrames[0].Line != 15 {
			return protocol.ProviderResponse{}, fmt.Errorf("wrong first delve stop: %s", prev)
		}
		p.threadID = stop.ThreadID
		p.facts["stop"] = prev
		p.phase = "step_into"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_into", "thread_id": p.threadID}))
	case "step_into":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || len(stop.TopFrames) == 0 || stop.TopFrames[0].Name != "scale" {
			return protocol.ProviderResponse{}, fmt.Errorf("step_into did not enter scale: %s", prev)
		}
		p.facts["step_into"] = stop.TopFrames[0]
		p.phase = "step_over"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_over", "thread_id": p.threadID}))
	case "step_over":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || len(stop.TopFrames) == 0 {
			return protocol.ProviderResponse{}, fmt.Errorf("step_over failed: %s", prev)
		}
		p.frameID = stop.TopFrames[0].ID
		p.facts["step_over"] = stop.TopFrames[0]
		p.phase = "scopes"
		return p.tool("debug_stack", p.dbg(map[string]any{"kind": "scopes", "frame_id": p.frameID}))
	case "scopes":
		var body struct {
			Scopes []struct {
				Ref int `json:"variablesReference"`
			} `json:"scopes"`
		}
		if err := json.Unmarshal([]byte(prev), &body); err != nil || len(body.Scopes) == 0 {
			return protocol.ProviderResponse{}, fmt.Errorf("no scopes: %s", prev)
		}
		p.oldRef = body.Scopes[0].Ref
		p.phase = "vars"
		return p.tool("debug_variables", p.dbg(map[string]any{"variables_reference": p.oldRef}))
	case "vars":
		p.facts["vars"] = prev
		p.phase = "evaluate"
		return p.tool("debug_evaluate", p.dbg(map[string]any{"frame_id": p.frameID, "expression": "multiplier"}))
	case "evaluate":
		if !strings.Contains(prev, `"result"`) {
			return protocol.ProviderResponse{}, fmt.Errorf("evaluate failed: %s", prev)
		}
		p.facts["evaluate"] = prev
		p.phase = "step"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_over", "thread_id": p.threadID}))
	case "step":
		var stop stopShape
		if err := json.Unmarshal([]byte(prev), &stop); err != nil || stop.Reason != "step" {
			return protocol.ProviderResponse{}, fmt.Errorf("step failed: %s", prev)
		}
		p.facts["step"] = prev
		p.phase = "stale"
		return p.tool("debug_variables", p.dbg(map[string]any{"variables_reference": p.oldRef}))
	case "stale":
		if !strings.Contains(prev, "stale") && !strings.Contains(prev, "earlier stop") {
			return protocol.ProviderResponse{}, fmt.Errorf("old reference not rejected: %s", prev)
		}
		p.facts["stale"] = prev
		p.phase = "step_out"
		return p.tool("debug_step", p.dbg(map[string]any{"action": "step_out", "thread_id": p.threadID}))
	case "step_out":
		p.facts["step_out"] = prev
		p.phase = "resume"
		return p.tool("debug_continue", p.dbg(map[string]any{"action": "continue", "wait": false}))
	case "resume":
		p.phase = "pause"
		return p.tool("debug_pause", p.dbg(map[string]any{"thread_id": p.threadID, "timeout_sec": 10}))
	case "pause":
		p.facts["pause"] = prev
		p.phase = "stop"
		return p.tool("debug_stop", p.dbg(map[string]any{"terminate_debuggee": true}))
	case "stop":
		p.facts["stop"] = prev
		var result struct {
			State   string `json:"state"`
			Cleanup string `json:"cleanup"`
		}
		if err := json.Unmarshal([]byte(prev), &result); err != nil || result.State != "closed" {
			return protocol.ProviderResponse{}, fmt.Errorf("stop did not close: %s", prev)
		}
		p.phase = "done"
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "Delve reached the breakpoint, inspected locals, stepped, and stopped cleanly."}, FinishReason: "stop"}, nil
	}
	return protocol.ProviderResponse{}, fmt.Errorf("unexpected phase %q", p.phase)
}

// TestAgentLoopDebugsRealDelve covers the same loop with Delve and records the
// panic-stop capability honestly.
func TestAgentLoopDebugsRealDelve(t *testing.T) {
	dlv := os.Getenv("LC_TEST_DELVE")
	if dlv == "" {
		t.Skip("set LC_TEST_DELVE for the real adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	root := t.TempDir()
	python, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain missing")
	}
	source := strings.ReplaceAll(delveAgentFixture, `import "fmt"`, "import (\n\t\"fmt\"\n\t\"time\"\n)")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, python, "build", "-gcflags=all=-N -l", "-o", "fixture", "main.go")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %v %s", err, out)
	}
	script := &delveScript{facts: map[string]any{}}
	engine := agentDAPEngine(t, root, script, &DebugOptions{Sandbox: testSandbox(t), Adapters: []DebugAdapter{{ID: "delve", Command: dlv, Args: []string{"dap", "--listen=unix:{socket}"}, Transport: "unix", Loopback: true, Launch: map[string]any{"mode": "exec"}, ReadPaths: []string{"/Library/Developer/CommandLineTools/Library/PrivateFrameworks/LLDB.framework"}}}})
	session, err := engine.NewSession(ctx, SessionOptions{Goal: "debug the Go total with Delve"})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	result, runErr := engine.RunAgent(ctx, session.ID, AgentOptions{Policy: "full", MaxSteps: 60})
	if runErr != nil {
		t.Fatalf("delve agent run: %v facts=%v", runErr, script.facts)
	}
	if result.Status != "completed" {
		t.Fatalf("delve run %q facts=%v", result.Status, script.facts)
	}
	if live, _ := engine.DebugSessions(ctx, session.ID); len(live) != 0 {
		t.Fatalf("delve sessions still live: %+v", live)
	}
	t.Logf("DELVE_AGENT_LOOP: stop=%v vars=%v step=%v pause=%v", script.facts["stop"], script.facts["vars"], script.facts["step"], script.facts["pause"])
}

// TestDelveExceptionStop records whether Delve reports a Go panic as an
// exception stop with exceptionInfo.
func TestDelveExceptionStop(t *testing.T) {
	dlv := os.Getenv("LC_TEST_DELVE")
	if dlv == "" {
		t.Skip("set LC_TEST_DELVE for the real adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	root := t.TempDir()
	source := "package main\n\nfunc main() {\n\tvar values []int\n\t_ = values[1]\n}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, "go", "build", "-gcflags=all=-N -l", "-o", "fixture", "main.go")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %v %s", err, out)
	}
	engine := agentDAPEngine(t, root, &dapScript{t: t, facts: map[string]any{}}, &DebugOptions{Sandbox: testSandbox(t), Adapters: []DebugAdapter{{ID: "delve", Command: dlv, Args: []string{"dap", "--listen=unix:{socket}"}, Transport: "unix", Loopback: true, Launch: map[string]any{"mode": "exec"}}}})
	session, err := engine.NewSession(ctx, SessionOptions{Goal: "panic stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	snap, err := engine.DebugStart(ctx, DebugStartOptions{SessionID: session.ID, AdapterID: "delve", Program: filepath.Join(root, "fixture")})
	if err != nil {
		t.Fatal(err)
	}
	c := DebugControl{SessionID: session.ID, DebugID: snap.ID, Epoch: snap.Epoch}
	c.Action = "configure"
	_, stop, err := engine.DebugControlAndWait(ctx, c, DebugWaitOptions{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("DELVE_EXCEPTION_STOP: reason=%q exception=%+v", stop.Reason, stop.Exception)
	if stop.Reason == "exception" {
		if stop.Exception == nil {
			t.Fatal("exception stop without exception info")
		}
	} else {
		t.Logf("Delve reported %q for a Go panic, not an exception stop: recorded, not asserted", stop.Reason)
	}
	if _, _, err = engine.DebugDisconnect(ctx, c, true); err != nil {
		t.Fatal(err)
	}
}

var _ = config.ResolvedModel{}
