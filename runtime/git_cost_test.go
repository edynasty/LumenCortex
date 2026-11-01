//go:build !windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// R1 (T1 G1 acceptance §9): the git processes one tool call costs through the real Engine, toolset and
// SQLite, counted by a logging git wrapper first on PATH (test-only; production code keeps no counter).
// Each call runs in its own Start and is compared with a Start that calls no tool, so any git process
// the call adds outside its snapshots is counted too.
//
// What T1 adds per call (its snapshots):
//
//	write_file: 1 hash-object for its pre blobs when a target exists, 1 for its post blobs
//	shell:      2 status, plus at most 1 hash-object for the paths it changed (0 when it changed none)
//	read_file:  none
//	note_write: none
//
// What shell already cost before T1 (42ee234, internal/toolset/shell.go): WorkspaceState before and
// after the command, a `rev-parse --verify -q HEAD` and a `status --porcelain=v1` each. It is pinned
// as legacy so neither a hidden T1 call nor a change to the old one passes unseen.

// gitArgvLog puts a logging git wrapper first on PATH and returns a reader of the argv lines so far.
func gitArgvLog(t *testing.T) func() []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nexec %q \"$@\"\n", log, real)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		raw, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

type gitCost struct{ status, hashObject, legacyShell, other int }

func classifyGit(lines []string) (c gitCost) {
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "status --porcelain=v2 "):
			c.status++
		case strings.HasPrefix(l, "hash-object -- "):
			c.hashObject++
		case strings.HasPrefix(l, "rev-parse --verify -q HEAD"), strings.HasPrefix(l, "status --porcelain=v1 "):
			c.legacyShell++
		default:
			c.other++
		}
	}
	return c
}

func TestToolGitProcessCost(t *testing.T) {
	workspace := t.TempDir()
	gitInit(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := gitArgvLog(t)
	engine := openEngineAt(t, workspace)

	// start runs one Start whose first step makes the given calls (none: a final answer) and returns
	// the git argv it caused.
	start := func(name string, toolCall ...protocol.ToolCall) gitCost {
		t.Helper()
		handle := newTestSession(t, engine, name)
		var steps []protocol.ProviderResponse
		if len(toolCall) > 0 {
			steps = append(steps, toolCalls(toolCall...))
		}
		before := len(calls())
		result, err := engine.runAgentWith(context.Background(), handle.ID, &stepProvider{steps: steps}, AgentOptions{Policy: "full", MaxSteps: 3})
		if err != nil || result.Status != "completed" {
			t.Fatalf("%s: run = %+v err=%v", name, result, err)
		}
		lines := calls()[before:]
		for _, l := range lines {
			t.Logf("%s: git %s", name, l)
		}
		return classifyGit(lines)
	}

	start("warm-up") // one-time per-engine git work is not a tool's cost
	baseline := start("baseline")
	if baseline.status != 0 || baseline.hashObject != 0 || baseline.legacyShell != 0 {
		t.Fatalf("a Start without a tool call ran snapshots: %+v", baseline)
	}

	for _, tc := range []struct {
		name string
		call protocol.ToolCall
		want gitCost
	}{
		{"write_file of a new file", tcall("w", "write_file", `{"path":"a.txt","content":"alpha\n"}`), gitCost{hashObject: 1}},
		{"write_file of an existing file", tcall("w", "write_file", `{"path":"seed.txt","content":"seed two\n"}`), gitCost{hashObject: 2}},
		{"shell that changes a file", tcall("s", "bash", `{"command":"printf 'x' >> log.txt"}`), gitCost{status: 2, hashObject: 1, legacyShell: 4}},
		{"shell that changes nothing", tcall("s", "bash", `{"command":"true"}`), gitCost{status: 2, legacyShell: 4}},
		{"read_file", tcall("r", "read_file", `{"path":"seed.txt"}`), gitCost{}},
		{"note_write", tcall("n", "note_write", `{"text":"the seed file holds one line"}`), gitCost{}},
	} {
		got := start(tc.name, tc.call)
		got.other -= baseline.other
		if got != tc.want {
			t.Errorf("%s: git processes beyond a tool-less Start = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "log.txt")); err != nil {
		t.Fatalf("shell did not run: %v", err)
	}
}

func tcall(id, name, args string) protocol.ToolCall { return tc(id, name, args) }
