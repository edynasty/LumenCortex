package debugger

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
)

// Opt-in deterministic capability comparison; measures observed local state, not
// model reasoning accuracy. The no-DAP arm runs the identical faulty program.
func TestCapabilityFourLanguageFaults(t *testing.T) {
	if os.Getenv("LC_CAPABILITY_EXPERIMENT") != "1" {
		t.Skip("explicit local adapter experiment")
	}
	python, node := os.Getenv("LC_TEST_PYTHON"), os.Getenv("LC_TEST_NODE")
	pyAdapter, jsAdapter := os.Getenv("LC_TEST_DEBUGPY"), os.Getenv("LC_TEST_JS_DEBUG")
	lldb := testLLDB(t)
	cases := []struct {
		name, extension, source string
		line                    int
		adapter                 Adapter
		runner                  string
	}{
		{"c", "c", "#include <stdio.h>\nint main(void) {\n int input=41;\n int value=input-1;\n puts(value==42 ? \"PASS\" : \"FAIL\");\n return value==42?0:1;\n}\n", 5, Adapter{ID: "lldb", Command: lldb, ReadPaths: xcodeReadPaths(lldb), Launch: map[string]any{"disableASLR": false}}, ""},
		{"go", "go", "package main\nimport (\"fmt\";\"os\")\nfunc main(){\n input:=41\n value:=input-1\n if value!=42 {fmt.Println(\"FAIL\");os.Exit(1)}\n fmt.Println(\"PASS\")\n}\n", 6, Adapter{ID: "dlv", Command: os.Getenv("LC_TEST_DELVE"), Args: []string{"dap", "--listen=unix:{socket}"}, ReadPaths: []string{"/Library/Developer/CommandLineTools/Library/PrivateFrameworks/LLDB.framework"}, Transport: "unix", Loopback: true, Launch: map[string]any{"mode": "exec"}}, ""},
		{"python", "py", "input=41\nvalue=input-1\nprint('PASS' if value==42 else 'FAIL')\nraise SystemExit(0 if value==42 else 1)\n", 3, Adapter{ID: "debugpy", Command: python, Args: []string{"-I", pyAdapter}, ReadPaths: []string{filepath.Dir(filepath.Dir(python)), filepath.Dir(filepath.Dir(pyAdapter))}, Loopback: true, Launch: map[string]any{"python": []string{python}, "justMyCode": true}}, python},
		{"javascript", "js", "let input=41;\nlet value=input-1;\nconsole.log(value===42 ? 'PASS' : 'FAIL');\nprocess.exit(value===42 ? 0 : 1);\n", 3, Adapter{ID: "js-debug", Language: "javascript", Command: node, Args: []string{jsAdapter, "{socket}"}, ReadPaths: []string{filepath.Dir(filepath.Dir(node)), filepath.Dir(filepath.Dir(jsAdapter))}, Transport: "unix", Loopback: true, Launch: map[string]any{"type": "pwa-node", "request": "launch", "runtimeExecutable": node, "autoAttachChildProcesses": false, "sourceMaps": false, "outputCapture": "std"}}, node},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
			defer cancel()
			root := t.TempDir()
			source := filepath.Join(root, "fault."+tc.extension)
			program := source
			if err := os.WriteFile(source, []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			build := func() {
				if tc.runner != "" {
					return
				}
				program = filepath.Join(root, "fault")
				args := []string{"-g", "-O0", source, "-o", program}
				compiler := "cc"
				if tc.name == "go" {
					compiler = "go"
					args = []string{"build", "-gcflags=all=-N -l", "-o", program, source}
				}
				if out, err := exec.CommandContext(ctx, compiler, args...).CombinedOutput(); err != nil {
					t.Fatalf("build %v %s", err, out)
				}
			}
			run := func() ([]byte, error) {
				if tc.runner != "" {
					return exec.CommandContext(ctx, tc.runner, program).CombinedOutput()
				}
				return exec.CommandContext(ctx, program).CombinedOutput()
			}
			build()
			before, err := run()
			if err == nil || strings.TrimSpace(string(before)) != "FAIL" {
				t.Fatalf("fault absent: %s %v", before, err)
			}
			m, err := New(Options{Sandbox: testSandbox(t), Adapters: []Adapter{tc.adapter}})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			snap, err := m.Start(ctx, StartOptions{SessionID: tc.name, AdapterID: tc.adapter.ID, Workspace: root, Program: program, Actor: "human"})
			if err != nil {
				t.Fatal(err)
			}
			c := Control{SessionID: tc.name, DebugID: snap.ID, Epoch: snap.Epoch, Actor: "human"}
			if _, err = m.Breakpoints(ctx, c, source, []Breakpoint{{Line: tc.line}}); err != nil {
				t.Fatal(err)
			}
			c.Action = "configure"
			if _, err = m.Control(ctx, c); err != nil {
				t.Fatal(err)
			}
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				snap, err = m.Snapshot(tc.name, snap.ID)
				if err != nil {
					t.Fatal(err)
				}
				if snap.State == "stopped" {
					break
				}
				select {
				case <-ctx.Done():
					internal, _ := m.lookup(tc.name, snap.ID)
					if internal != nil {
						t.Logf("adapter stderr: %+v", internal.process.Stderr.Snapshot())
					}
					t.Fatalf("waiting for stopped: %+v: %v", snap, ctx.Err())
				case <-ticker.C:
				}
			}
			raw, err := m.Inspect(ctx, c, "stackTrace", snap.ThreadID, 0, 4)
			if err != nil {
				t.Fatal(err)
			}
			var stack struct {
				Frames []struct{ ID, Line int } `json:"stackFrames"`
			}
			if err = json.Unmarshal(raw, &stack); err != nil || len(stack.Frames) == 0 || stack.Frames[0].Line != tc.line {
				t.Fatalf("stack %s %v", raw, err)
			}
			values := map[string]string{}
			for _, name := range []string{"input", "value"} {
				raw, err = m.Evaluate(ctx, c, stack.Frames[0].ID, name, false)
				if err != nil {
					t.Fatal(err)
				}
				var v struct{ Result string }
				if err = json.Unmarshal(raw, &v); err != nil {
					t.Fatal(err)
				}
				values[name] = v.Result
			}
			if values["input"] != "41" || values["value"] != "40" {
				t.Fatalf("root cause not grounded %v", values)
			}
			threads, err := m.Inspect(ctx, c, "threads", 0, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			c.Action = "stop"
			if _, err = m.Control(ctx, c); err != nil {
				t.Fatal(err)
			}
			if len(m.List(tc.name)) != 0 {
				t.Fatal("debug session leaked")
			}
			fixed := strings.Replace(tc.source, "input-1", "input+1", 1)
			if err = os.WriteFile(source, []byte(fixed), 0600); err != nil {
				t.Fatal(err)
			}
			build()
			after, err := run()
			if err != nil || strings.TrimSpace(string(after)) != "PASS" {
				t.Fatalf("repair %s %v", after, err)
			}
			receipt := map[string]any{"language": tc.name, "no_dap_output": strings.TrimSpace(string(before)), "no_dap_root_cause": "not observable from output-only arm", "dap_line": tc.line, "dap_values": values, "threads": json.RawMessage(threads), "repair": "input-1 -> input+1", "repaired_output": strings.TrimSpace(string(after)), "cleanup": "no managed sessions", "model_requests": 0}
			data, _ := json.Marshal(receipt)
			fmt.Println("CAPABILITY_DAP", string(data))
		})
	}
}
