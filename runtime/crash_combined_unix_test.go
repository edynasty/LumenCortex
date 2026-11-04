//go:build unix

package runtime

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S7 item 2: the T1-S2 request crash points replayed on the T1-S4 fixture (a git workspace with the
// test MCP server configured), in a session whose first step already ran a side-effect tool. Restart
// recovery then meets an answered envelope, a completed execution and an admitted envelope in one
// session: Reconcile must settle only the last, and neither the request nor the tool may run again.

const combinedCounterEnv = "LCX_COMBINED_COUNTER"

// TestHelperCrashAfterATool is the child of TestKilledAroundTheRequestAfterATool: step 1 runs a shell
// call that appends to the counter; the process holds at the chosen point of the second request.
func TestHelperCrashAfterATool(t *testing.T) {
	workspace := os.Getenv(crashWorkspaceEnv)
	if workspace == "" || os.Getenv(combinedCounterEnv) == "" {
		t.Skip("helper process only")
	}
	point, counter := os.Getenv(crashPointEnv), os.Getenv(combinedCounterEnv)
	hold := func() {
		os.Stdout.WriteString("held\n")
		time.Sleep(5 * time.Minute)
		os.Exit(4) // not reached: the parent kills this process
	}
	onSecond := func() func() {
		n := 0
		return func() {
			if n++; n == 2 {
				hold()
			}
		}
	}
	switch point {
	case "after_admit":
		testHookAfterAdmit = onSecond()
	case "before_answer":
		testHookBeforeAnswer = onSecond()
	}
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	provider := &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 1 {
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				tc("c1", "bash", fmt.Sprintf(`{"command":%q}`, fmt.Sprintf("printf x >> %q", counter))),
			}}, FinishReason: "tool_calls"}, nil
		}
		if point == "in_complete" {
			hold()
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "unstored answer"}}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), os.Getenv(crashSessionEnv), provider, AgentOptions{Policy: "full", MaxSteps: 4}); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(3)
	}
	time.Sleep(5 * time.Minute)
}

func TestKilledAroundTheRequestAfterATool(t *testing.T) {
	for _, point := range crashPoints {
		t.Run(point, func(t *testing.T) {
			workspace := t.TempDir()
			gitInit(t, workspace)
			counter := filepath.Join(t.TempDir(), "counter")
			useRuntimeMCP(t, workspace, counter)
			engine := openEngineAt(t, workspace)
			handle := newTestSession(t, engine, "act, then ask")

			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrashAfterATool$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashWorkspaceEnv+"="+workspace, crashSessionEnv+"="+handle.ID, crashPointEnv+"="+point, combinedCounterEnv+"="+counter)
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
			line := make(chan string, 1)
			go func() { text, _ := bufio.NewReader(out).ReadString('\n'); line <- text }()
			select {
			case text := <-line:
				if text != "held\n" {
					t.Fatalf("child said %q", text)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("child did not reach the crash point")
			}
			killHolder(t, cmd)
			counterIs := func(when string) {
				t.Helper()
				if raw, _ := os.ReadFile(counter); string(raw) != "x" {
					t.Fatalf("%s: counter = %q, want the one shell run", when, raw)
				}
			}
			counterIs("after the kill")

			reopened := openEngineAt(t, workspace)
			envs := envelopeRows(t, reopened, handle.ID)
			if len(envs) != 2 || envs[0].State != "answered" || envs[0].Lineage != 1 ||
				envs[1].State != "unknown" || envs[1].Reason.String != session.ReasonRestartNoAnswer || envs[1].Answered.Valid {
				t.Fatalf("envelopes after restart = %+v, want answered then unknown/%s", envs, session.ReasonRestartNoAnswer)
			}
			execs, edges := readProvenance(t, reopened, handle.ID)
			if len(execs) != 1 || execs[0].Tool != "bash" || execs[0].Outcome != "completed" || execs[0].Lineage != 1 || execs[0].Request != envs[0].ID {
				t.Fatalf("executions after restart = %+v, want the shell completed under the first request", execs)
			}
			rows := transcriptRows(t, reopened, handle.ID)
			assertPaired(t, rows)
			for _, m := range rows {
				if m.Content == "unstored answer" || m.Origin == protocol.OriginToolInterrupted {
					t.Fatalf("restart recovery changed the completed step or stored the unstored answer: %+v", m)
				}
			}

			sent := 0
			resumed := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				sent++
				return protocol.ProviderResponse{Message: protocol.Message{Content: "resumed"}}, nil
			}}
			result, err := reopened.runAgentWith(context.Background(), handle.ID, resumed, AgentOptions{Input: "carry on", Policy: "full", MaxSteps: 3})
			if err != nil || result.Status != "completed" {
				t.Fatalf("resume = %+v err=%v", result, err)
			}
			final := envelopeRows(t, reopened, handle.ID)
			if sent != 1 || len(final) != 3 || final[0] != envs[0] || final[1].ID != envs[1].ID || final[1].State != "unknown" || final[2].State != "answered" {
				t.Fatalf("resume sent %d requests; envelopes %+v", sent, final)
			}
			if again, againEdges := readProvenance(t, reopened, handle.ID); len(again) != 1 || again[0] != execs[0] || len(againEdges) != len(edges) {
				t.Fatalf("resume changed the execution records: %+v", again)
			}
			counterIs("after the resume")
		})
	}
}
