package toolset

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/protocol"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}

// A shell call reports a workspace change only when one happened: reading commands do not, writes,
// edits to an already dirty file, untracked files, commits and failing commands that wrote do. Where
// the state cannot be read, every call counts as a change.
func TestShellReportsOnlyActualWorkspaceChanges(t *testing.T) {
	root := gitRepo(t)
	set, err := New(Options{Workspace: root, Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	run := func(command string) bool {
		t.Helper()
		result, err := set.Execute(context.Background(), "bash", map[string]any{"command": command}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result.MutatesWorkspace
	}
	for _, c := range []struct {
		command string
		changed bool
	}{
		{"git status && git log --oneline && cat a.txt", false},
		{"echo b >> a.txt", true},
		{"git diff", false},
		{"echo c >> a.txt", true}, // a.txt was already dirty
		{"mkdir -p d && echo x > d/new.txt", true},
		{"ls -R", false},
		{"git add -A && git commit -qm next", true},
		{"echo y > a.txt; exit 3", true},
		{"exit 1", false},
		{"echo ignored > out.log && printf 'out.log\\n' >> .git/info/exclude", false},
	} {
		if got := run(c.command); got != c.changed {
			t.Errorf("%q: changed=%v, want %v", c.command, got, c.changed)
		}
	}

	plain, err := New(Options{Workspace: t.TempDir(), Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := plain.Execute(context.Background(), "bash", map[string]any{"command": "true"}, nil); err != nil || !result.MutatesWorkspace {
		t.Fatalf("outside git a command must count as a change: %+v err=%v", result, err)
	}
}

type shellOutcome struct {
	ExitCode   int    `json:"exitCode"`
	Cancelled  bool   `json:"cancelled"`
	TimedOut   bool   `json:"timedOut"`
	TimeoutSec int64  `json:"timeoutSec"`
	Error      string `json:"error"`
	Stdout     struct {
		Head string `json:"head"`
	} `json:"stdout"`
}

// A foreground command's timeout is its timeout_sec, else the configured default (120 s when unset),
// clamped to the configured maximum (1800 s when unset), and the run's tool deadline bounds either:
// past it the command's group is terminated, the partial output is kept, and the result is a failed,
// interrupted call (effects unknown) rather than an error. A call with no time left before the tool
// deadline does not start.
func TestShellTimeoutSourcesAndResult(t *testing.T) {
	root := t.TempDir()
	newSet := func(def, limit time.Duration) *Set {
		set, err := New(Options{Workspace: root, Policy: PolicyFull, ShellTimeout: def, ShellMaxTimeout: limit})
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	// Timeout/output cases need the interpreter to start before the deadline. A
	// developer's login profile can exceed even one second under race instrumentation.
	// Use the same real bash without profiles; login-shell selection is covered below.
	controlled := shell.Bash()
	if controlled.Name != "cmd.exe" {
		controlled.Args = []string{"-c"}
	}
	controlledSet := func(def, limit time.Duration) *Set {
		set := newSet(def, limit)
		set.shell.Interpreter = controlled
		return set
	}
	run := func(ctx context.Context, set *Set, args map[string]any) (protocol.ToolResult, shellOutcome, time.Duration) {
		t.Helper()
		started := time.Now()
		result, err := set.Execute(ctx, "bash", args, nil)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var out shellOutcome
		if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
			t.Fatalf("%v: content %q: %v", args, result.Content, err)
		}
		return result, out, elapsed
	}
	slow := "echo begun; sleep 20"

	t.Run("echo under the defaults", func(t *testing.T) {
		result, out, _ := run(context.Background(), newSet(0, 0), map[string]any{"command": "echo hello"})
		if !result.OK || out.TimedOut || result.Interrupted || out.Cancelled || out.ExitCode != 0 {
			t.Fatalf("result=%+v out=%+v", result, out)
		}
	})
	t.Run("argument", func(t *testing.T) {
		result, out, elapsed := run(context.Background(), controlledSet(0, 0), map[string]any{"command": slow, "timeout_sec": float64(1)})
		if result.OK || !result.Interrupted || !out.TimedOut || out.TimeoutSec != 1 || out.ExitCode != -1 || out.Cancelled {
			t.Fatalf("result=%+v out=%+v", result, out)
		}
		if !strings.Contains(out.Stdout.Head, "begun") || !strings.Contains(out.Error, "1s timeout") {
			t.Fatalf("want the partial output and the reason: %+v", out)
		}
		if elapsed > 8*time.Second {
			t.Fatalf("returned after %s", elapsed)
		}
	})
	t.Run("injected short default", func(t *testing.T) {
		result, out, elapsed := run(context.Background(), controlledSet(500*time.Millisecond, 0), map[string]any{"command": slow})
		if !result.Interrupted || !out.TimedOut || out.TimeoutSec != 1 || out.ExitCode != -1 {
			t.Fatalf("the configured default must apply: result=%+v out=%+v", result, out)
		}
		if !strings.Contains(out.Stdout.Head, "begun") {
			t.Fatalf("want the partial output: %+v", out)
		}
		if elapsed > 8*time.Second {
			t.Fatalf("returned after %s", elapsed)
		}
	})
	t.Run("an argument replaces a lower default", func(t *testing.T) {
		result, out, _ := run(context.Background(), newSet(time.Second, 0), map[string]any{"command": "sleep 2; echo late", "timeout_sec": float64(10)})
		if !result.OK || out.TimedOut || result.Interrupted {
			t.Fatalf("timeout_sec must replace a lower default: result=%+v out=%+v", result, out)
		}
	})
	t.Run("a lower maximum clamps an explicit argument and the default", func(t *testing.T) {
		set := newSet(0, time.Second)
		result, out, elapsed := run(context.Background(), set, map[string]any{"command": slow, "timeout_sec": float64(5)})
		if !result.Interrupted || !out.TimedOut || out.TimeoutSec != 1 || !strings.Contains(out.Error, "1s timeout") {
			t.Fatalf("timeout_sec must clamp to the configured maximum: result=%+v out=%+v", result, out)
		}
		if elapsed > 8*time.Second {
			t.Fatalf("returned after %s", elapsed)
		}
		if timeout, _, _, err := set.shellTimeout(context.Background(), map[string]any{"command": "true"}); err != nil || timeout != time.Second {
			t.Fatalf("the default must clamp to the maximum too: timeout=%s err=%v", timeout, err)
		}
	})
	t.Run("a configured higher limit admits a longer argument", func(t *testing.T) {
		set := newSet(3*time.Second, 10*time.Second)
		result, out, _ := run(context.Background(), set, map[string]any{"command": "sleep 2; echo late", "timeout_sec": float64(10)})
		if !result.OK || out.TimedOut || result.Interrupted {
			t.Fatalf("the configured maximum must admit the argument: result=%+v out=%+v", result, out)
		}
		timeout, source, _, err := set.shellTimeout(context.Background(), map[string]any{"command": "true", "timeout_sec": float64(30)})
		if err != nil || source != "argument" || timeout != 10*time.Second {
			t.Fatalf("argument 30s with a 10s maximum: timeout=%s source=%q err=%v", timeout, source, err)
		}
	})
	t.Run("tool deadline bounds the timeout", func(t *testing.T) {
		ctx := protocol.WithToolDeadline(context.Background(), time.Now().Add(time.Second))
		_, out, elapsed := run(ctx, newSet(0, 0), map[string]any{"command": slow, "timeout_sec": float64(600)})
		if !out.TimedOut || out.TimeoutSec > 1 || !strings.Contains(out.Error, "deadline") {
			t.Fatalf("the tool deadline must bound the timeout: %+v", out)
		}
		if elapsed > 8*time.Second {
			t.Fatalf("returned after %s", elapsed)
		}
	})
	t.Run("cancellation keeps the partial output and its reason", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cancel once the command's output arrived, so the partial stdout is observable.
		seen := make(chan struct{}, 1)
		go func() {
			select {
			case <-seen:
			case <-time.After(5 * time.Second):
			}
			cancel()
		}()
		started := time.Now()
		result, err := controlledSet(0, 0).Execute(ctx, "bash", map[string]any{"command": slow}, func(output protocol.ToolOutput) {
			if strings.Contains(output.Chunk, "begun") {
				select {
				case seen <- struct{}{}:
				default:
				}
			}
		})
		elapsed := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		var out shellOutcome
		if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
			t.Fatalf("content %q: %v", result.Content, err)
		}
		if result.OK || !result.Interrupted || !out.Cancelled || out.TimedOut || out.ExitCode != -1 {
			t.Fatalf("result=%+v out=%+v", result, out)
		}
		if !strings.Contains(out.Stdout.Head, "begun") || !strings.Contains(out.Error, "cancelled") {
			t.Fatalf("want the partial output and the cancellation reason: %+v", out)
		}
		if elapsed > 8*time.Second {
			t.Fatalf("returned after %s", elapsed)
		}
	})
	t.Run("no time left", func(t *testing.T) {
		marker := filepath.Join(root, "ran")
		ctx := protocol.WithToolDeadline(context.Background(), time.Now().Add(-time.Second))
		result, out, _ := run(ctx, newSet(0, 0), map[string]any{"command": "touch " + marker})
		if result.OK || !strings.Contains(out.Error, "not run") {
			t.Fatalf("result=%+v out=%+v", result, out)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("a call past the tool deadline ran: %v", err)
		}
	})
	for _, bad := range []any{float64(0), float64(1.5), float64(86401), "10", true} {
		if _, err := newSet(0, 0).Execute(context.Background(), "bash", map[string]any{"command": "true", "timeout_sec": bad}, nil); err == nil {
			t.Errorf("timeout_sec=%v was accepted", bad)
		}
	}
}

// The bash tool runs its command in bash and says so, while the runner the host shares with checks and
// RunShell keeps running /bin/sh: an approved check keeps the meaning it was approved with (ADR-0019 §0).
func TestBashToolRunsBashAndTheSharedRunnerKeepsSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX interpreters only")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("/bin/bash not installed")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not installed")
	}
	root := t.TempDir()
	shared := shell.New(root)
	set, err := New(Options{Workspace: root, Policy: PolicyFull, Shell: shared})
	if err != nil {
		t.Fatal(err)
	}
	// The trailing builtin keeps the interpreter from exec'ing ps in its own place.
	const which = "ps -o comm= -p $$; true"
	result, err := set.Execute(context.Background(), protocol.ToolBash, map[string]any{"command": which}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Stdout struct {
			Head string `json:"head"`
		} `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("%v: %s", err, result.Content)
	}
	if got := strings.TrimSpace(out.Stdout.Head); filepath.Base(got) != "bash" {
		t.Fatalf("bash tool ran in %q, want bash", got)
	}
	for _, spec := range set.Specs(nil) {
		if spec.Name == protocol.ToolBash && !strings.Contains(spec.Description, "/bin/bash -lc") {
			t.Fatalf("description does not name the interpreter: %q", spec.Description)
		}
	}
	host, err := shared.Run(context.Background(), which, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(host.Stdout.Head)); filepath.Base(got) != "sh" {
		t.Fatalf("shared runner ran in %q, want sh", got)
	}
}
