package dap

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edynasty/LumenCortex/evalruntime"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in integration exercises the production transport, not the P0 Python probe.
func TestRealLLDBTransport(t *testing.T) {
	if os.Getenv("LC_TEST_LLDB") == "" {
		t.Skip("set LC_TEST_LLDB for installed LLDB integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := testLLDB(t)
	root := t.TempDir()
	source := filepath.Join(root, "fixture.c")
	program := filepath.Join(root, "fixture")
	if err := os.WriteFile(source, []byte("#include <stdio.h>\nint main(void) {\n int value = 41;\n value += 1;\n printf(\"%d\\n\", value);\n return value == 42 ? 0 : 1;\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "cc", "-g", "-O0", source, "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile: %v %s", err, out)
	}
	cmd, err := evalruntime.ProgramCommand(ctx, evalruntime.ProgramOptions{Debug: true, Sandbox: testSandbox(t), Executable: path, Workspace: root, PrivateDir: t.TempDir(), ReadPaths: testReadPaths(path), Env: []string{"PATH=/usr/bin:/bin", "HOME=" + root, "TMPDIR=" + root}, CPUSeconds: 20, FileBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	request := func(name string, args any, out any) {
		t.Helper()
		if err := p.Client.Request(ctx, name, args, out); err != nil {
			var rejection *ResponseError
			if errors.As(err, &rejection) {
				t.Logf("adapter rejection: %s", rejection.Body)
			}
			for len(p.Client.events) > 0 {
				m := <-p.Client.events
				t.Logf("adapter event: %s %s", m.Event, m.Body)
			}
			t.Fatalf("%s: %v stderr=%+v", name, err, p.Stderr.Snapshot())
		}
	}
	event := func(name string) Message {
		t.Helper()
		for {
			select {
			case m, ok := <-p.Client.Events():
				if !ok {
					t.Fatal(p.Client.Err())
				}
				if m.Event == name {
					return m
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	request("initialize", map[string]any{"adapterID": "lldb", "pathFormat": "path", "linesStartAt1": true, "columnsStartAt1": true}, nil)
	request("launch", map[string]any{"program": program, "cwd": root, "disableASLR": false}, nil)
	event("initialized")
	request("setBreakpoints", map[string]any{"source": map[string]string{"path": source}, "breakpoints": []map[string]int{{"line": 5}}}, nil)
	request("configurationDone", struct{}{}, nil)
	stopped := event("stopped")
	var stop struct {
		ThreadID int `json:"threadId"`
	}
	json.Unmarshal(stopped.Body, &stop)
	var stack struct {
		Frames []struct {
			ID   int `json:"id"`
			Line int `json:"line"`
		} `json:"stackFrames"`
	}
	request("stackTrace", map[string]int{"threadId": stop.ThreadID, "levels": 4}, &stack)
	if len(stack.Frames) == 0 || stack.Frames[0].Line != 5 {
		t.Fatalf("wrong stop %+v", stack)
	}
	var scopes struct {
		Scopes []struct {
			Ref int `json:"variablesReference"`
		} `json:"scopes"`
	}
	request("scopes", map[string]int{"frameId": stack.Frames[0].ID}, &scopes)
	if len(scopes.Scopes) == 0 {
		t.Fatal("no scopes")
	}
	var variables struct {
		Variables []struct{ Name, Value string } `json:"variables"`
	}
	request("variables", map[string]int{"variablesReference": scopes.Scopes[0].Ref}, &variables)
	found := false
	for _, v := range variables.Variables {
		if v.Name == "value" && v.Value == "42" {
			found = true
		}
	}
	if !found {
		t.Fatalf("initialized variable incorrect %+v", variables)
	}
	request("next", map[string]int{"threadId": stop.ThreadID}, nil)
	event("stopped")
	request("continue", map[string]int{"threadId": stop.ThreadID}, nil)
	exited := event("exited")
	var exit struct {
		Code int `json:"exitCode"`
	}
	json.Unmarshal(exited.Body, &exit)
	if exit.Code != 0 {
		t.Fatalf("exit %d", exit.Code)
	}
	request("disconnect", map[string]bool{"terminateDebuggee": true}, nil)
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("LLDB_REAL: initialized value=42, line=5, next/continue, exit=0, managed cleanup complete")
}
