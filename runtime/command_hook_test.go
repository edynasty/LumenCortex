package runtime

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"
)

func TestCommandHookSandboxAndProtocol(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("real sandbox-exec fixture; Linux requires host caller-isolation setup")
	}
	e := openTestEngine(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "must-not-write")
	// No inherited secret and no write outside the disposable workspace. Both
	// violations would change the actual process result, not a mock expectation.
	t.Setenv("LC_HOOK_SECRET", "must-not-inherit")
	script := `test -z "$LC_HOOK_SECRET" || exit 6; if touch "$1" 2>/dev/null; then exit 7; fi; printf '{"decision":"deny","reason":"isolated"}'`
	if err := e.RegisterCommandHook(CommandHookOptions{ID: "external", Version: 1, Events: []LifecycleEvent{LifecycleBeforeEval}, Safety: true, Command: "/bin/sh", Args: []string{"-c", script, "hook", outside}}); err != nil {
		t.Fatal(err)
	}
	out, err := e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecycleBeforeEval})
	if err != nil || !out.Denied() || len(out.Audit) != 1 || out.Audit[0].Reason != "isolated" {
		t.Fatalf("sandbox hook: %+v %v", out, err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("sandbox wrote outside workspace: %v", err)
	}
	for _, script := range []string{`printf '{"decision":"continue","permission":"full"}'`, `printf '{"decision":"continue"}{}'`, `printf '{"decision":"allow"}'`} {
		h := &commandLifecycleHook{opts: CommandHookOptions{Command: "/bin/sh", Args: []string{"-c", script}, Timeout: time.Second}}
		result, err := h.Handle(ctx, LifecycleRequest{Event: LifecycleBeforeEval})
		if err == nil {
			t.Fatalf("invalid protocol accepted: %s %+v", script, result)
		}
	}
	t.Log("COMMAND_HOOK_REAL sandbox denied external write; environment isolated; strict JSON; policy denial retained")
}

func TestCommandHookTimeoutTerminatesProgram(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("sandbox-exec fixture")
	}
	h := &commandLifecycleHook{opts: CommandHookOptions{Command: "/bin/sh", Args: []string{"-c", "sleep 30 & wait"}, Timeout: time.Second}}
	start := time.Now()
	if _, err := h.Handle(context.Background(), LifecycleRequest{Event: LifecycleBeforeEval}); err == nil {
		t.Fatal("timed-out process accepted")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout failed to bound owned process cleanup")
	}
}
