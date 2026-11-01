//go:build unix

package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S4 crash matrix: a real Engine in a child process is killed with SIGKILL around one side-effect
// tool call, and a fresh Engine must settle the execution without running the tool again. The side
// effect is counted outside the run: shell and the exec MCP tool append one byte to a counter file,
// and write_file's target is overwritten by the parent after the kill, so a replay would show.

const (
	toolCrashPointEnv = "LCX_TOOL_CRASH_POINT"
	toolCrashToolEnv  = "LCX_TOOL_CRASH_TOOL"
)

// toolCrashCall is the one call the child's model issues for each tool. The shell command appends to
// the counter and, for the in_execution point, announces itself and blocks.
func toolCrashCall(tool, counter, point string) protocol.ToolCall {
	switch tool {
	case "write_file":
		return tc("c1", "write_file", `{"path":"a.txt","content":"written by the run\n"}`)
	case "bash":
		cmd := fmt.Sprintf("printf x >> %q", counter)
		if point == "in_execution" {
			cmd += fmt.Sprintf(" && printf acting > %q && sleep 300", counter+".acting")
		}
		return tc("c1", "bash", fmt.Sprintf(`{"command":%q}`, cmd))
	default:
		return tc("c1", "mcp__srv__act", `{"text":"go"}`)
	}
}

// TestHelperCrashAtToolPoint is the child: it runs one tool call up to the chosen point and waits to be
// killed. "held" on stdout marks a hook point; in_execution is marked by the tool's .acting file.
func TestHelperCrashAtToolPoint(t *testing.T) {
	workspace := os.Getenv(crashWorkspaceEnv)
	if workspace == "" || os.Getenv(toolCrashPointEnv) == "" {
		t.Skip("helper process only")
	}
	point, tool := os.Getenv(toolCrashPointEnv), os.Getenv(toolCrashToolEnv)
	hold := func() {
		os.Stdout.WriteString("held\n")
		time.Sleep(5 * time.Minute)
		os.Exit(4)
	}
	switch point {
	case "after_admit":
		testHookAfterExecAdmit = hold
	case "before_commit":
		testHookBeforeToolCommit = hold
	case "after_commit":
		testHookAfterToolCommit = hold
	}
	// TestMain gives this process the shared catalog; trust the workspace again here so the project
	// MCP server may start (ADR-0013).
	useGlobalConfig(t, mcpGlobalConfig(workspace))
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if tool == "mcp" {
		if _, err := engine.MCPStart(context.Background(), "", "srv"); err != nil {
			t.Fatal(err)
		}
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	call := toolCrashCall(tool, os.Getenv(rtMCPCounter), point)
	provider := &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 1 {
			return toolCalls(call), nil
		}
		hold() // after_commit for a tool that never reached the hook is a test bug; never answer twice
		return protocol.ProviderResponse{}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), os.Getenv(crashSessionEnv), provider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(3)
	}
	time.Sleep(5 * time.Minute)
}

func TestKilledAroundAToolIsUnknownAndNotReplayed(t *testing.T) {
	points := []string{"after_admit", "in_execution", "before_commit", "after_commit"}
	for _, tool := range []string{"write_file", "bash", "mcp"} {
		for _, point := range points {
			if tool == "write_file" && point == "in_execution" {
				// write_file is one synchronous write inside the process: there is no point between
				// "started" and "returned" that differs from before_commit.
				continue
			}
			t.Run(tool+"/"+point, func(t *testing.T) { runToolCrash(t, tool, point) })
		}
	}
}

func runToolCrash(t *testing.T, tool, point string) {
	workspace := t.TempDir()
	gitInit(t, workspace)
	counter := filepath.Join(t.TempDir(), "counter")
	useRuntimeMCP(t, workspace, counter)
	if tool == "mcp" && point == "in_execution" {
		t.Setenv(rtMCPHang, "1")
	}
	engine := openEngineAt(t, workspace)
	handle := newTestSession(t, engine, "act once")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashAtToolPoint$", "-test.count=1")
	cmd.Env = append(os.Environ(), crashWorkspaceEnv+"="+workspace, crashSessionEnv+"="+handle.ID, toolCrashPointEnv+"="+point, toolCrashToolEnv+"="+tool)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	// stdout carries "held" at a hook point, or the child's failure; in_execution is marked by the
	// tool's .acting file instead.
	reached := make(chan string, 2)
	go func() {
		r := bufio.NewReader(out)
		text, _ := r.ReadString('\n')
		if text != "held\n" {
			rest, _ := io.ReadAll(r)
			text += string(rest)
		}
		reached <- text
	}()
	if point == "in_execution" {
		go func() {
			for i := 0; i < 600; i++ {
				if _, err := os.Stat(counter + ".acting"); err == nil {
					reached <- "acting"
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	select {
	case text := <-reached:
		if want := map[bool]string{true: "acting", false: "held\n"}[point == "in_execution"]; text != want {
			t.Fatalf("child said %q", text)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("child did not reach the crash point")
	}
	// Kill the whole group: the shell or MCP child must not survive to act after the kill.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	ran := point != "after_admit"
	sideEffects := func() string {
		if tool == "write_file" {
			raw, _ := os.ReadFile(filepath.Join(workspace, "a.txt"))
			return string(raw)
		}
		raw, _ := os.ReadFile(counter)
		return string(raw)
	}
	switch {
	case tool == "write_file" && ran:
		if got := sideEffects(); got != "written by the run\n" {
			t.Fatalf("a.txt = %q, want the run's write", got)
		}
		// Overwrite it: a replayed write_file would put the run's content back.
		if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("changed after the kill\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	case tool == "write_file":
		if got := sideEffects(); got != "" {
			t.Fatalf("a.txt = %q before the tool ran", got)
		}
	default:
		want := map[bool]string{true: "x", false: ""}[ran]
		if got := sideEffects(); got != want {
			t.Fatalf("side effects before restart = %q, want %q", got, want)
		}
	}
	afterKill := sideEffects()

	// A fresh Open reconciles the running session: an execution without a post becomes
	// outcome_unknown and incomplete; a committed one is left as it was. Twice changes nothing.
	reopened := openEngineAt(t, workspace)
	execs, edges := readProvenance(t, reopened, handle.ID)
	if len(execs) != 1 {
		t.Fatalf("executions after restart = %+v, want one", execs)
	}
	x := execs[0]
	if point == "after_commit" {
		if x.Outcome != "completed" || !x.Post.Valid || x.Coverage.String != "complete" {
			t.Fatalf("committed execution after restart = %+v", x)
		}
	} else {
		if x.Outcome != "outcome_unknown" || x.Lineage != 0 || x.Post.Valid || len(edges) != 0 {
			t.Fatalf("execution after restart = %+v edges=%+v, want outcome_unknown, incomplete, no post, no edges", x, edges)
		}
	}
	again := openEngineAt(t, workspace)
	if execs2, _ := readProvenance(t, again, handle.ID); fmt.Sprint(execs2) != fmt.Sprint(execs) {
		t.Fatalf("second reconcile changed the execution: %+v -> %+v", execs, execs2)
	}
	rows := transcript(t, handle)
	assertPaired(t, rows)

	// A resumed run answers without calling a tool: nothing may run the old call again.
	supervisor, err := NewRunSupervisor(again)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	resumed := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{Content: "resumed"}, FinishReason: "stop"}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), handle.ID, resumed, AgentOptions{Input: "carry on", Policy: "full", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
	if got := sideEffects(); got != afterKill {
		t.Fatalf("side effects after resume = %q, want %q: the call ran again", got, afterKill)
	}
	if final, _ := readProvenance(t, again, handle.ID); len(final) != 1 || final[0].Outcome != x.Outcome {
		t.Fatalf("executions after resume = %+v", final)
	}
}
