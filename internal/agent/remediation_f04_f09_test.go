package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/debugger"
	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type workspaceWritingDebugger struct {
	workspace string
}

func (d workspaceWritingDebugger) Start(_ context.Context, _, _ string, _ []string, _ bool) (debugger.Snapshot, error) {
	outDir := filepath.Join(d.workspace, "build")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return debugger.Snapshot{}, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "debuggee_ignored.log"), []byte("spawned debuggee side effect\n"), 0o644); err != nil {
		return debugger.Snapshot{}, err
	}
	return debugger.Snapshot{ID: "dbg-1", Epoch: 1, State: "stopped"}, nil
}

func (d workspaceWritingDebugger) Attach(context.Context, debugger.Control) (debugger.Snapshot, error) {
	return debugger.Snapshot{ID: "dbg-1", Epoch: 1, State: "stopped"}, nil
}

func (d workspaceWritingDebugger) Control(context.Context, debugger.Control) (debugger.Snapshot, error) {
	return debugger.Snapshot{ID: "dbg-1", Epoch: 1, State: "running"}, nil
}

func (d workspaceWritingDebugger) ControlAndWait(context.Context, debugger.Control, debugger.WaitOptions) (debugger.Snapshot, debugger.StopInfo, error) {
	return debugger.Snapshot{ID: "dbg-1", Epoch: 1, State: "stopped"}, debugger.StopInfo{Reason: "step"}, nil
}

func (d workspaceWritingDebugger) Disconnect(context.Context, debugger.Control, bool) (debugger.Snapshot, string, error) {
	return debugger.Snapshot{ID: "dbg-1", Epoch: 1, State: "terminated"}, "terminated", nil
}

func (d workspaceWritingDebugger) SetExceptionBreakpoints(context.Context, debugger.Control, []string) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}

func (d workspaceWritingDebugger) Breakpoints(context.Context, debugger.Control, string, []debugger.Breakpoint) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}

func (d workspaceWritingDebugger) Inspect(context.Context, debugger.Control, string, int, int, int) (json.RawMessage, error) {
	return json.RawMessage(`[]`), nil
}

func (d workspaceWritingDebugger) Evaluate(_ context.Context, _ debugger.Control, _ int, expr string, _ bool) (json.RawMessage, error) {
	target := filepath.Join(d.workspace, expr)
	if err := os.WriteFile(target, []byte("written by debug_evaluate\n"), 0o644); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"result":"ok"}`), nil
}

func initGitRepoForTest(t *testing.T, dir string) {
	t.Helper()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".gitignore", "main.go")
	runGit("commit", "-m", "init")
}

// TestF04DebugToolsClassifiedAsMutationUnknown verifies that all debug_* tools are classified as
// tierWorkspace -> MutationUnknown (never MutationNone or MutationProven), including real workspace
// writes via debug_start (Git-ignored file) and debug_evaluate (tracked/untracked workspace file).
func TestF04DebugToolsClassifiedAsMutationUnknown(t *testing.T) {
	debugTools := []string{
		"debug_start",
		"debug_attach",
		"debug_set_breakpoint",
		"debug_continue",
		"debug_step",
		"debug_pause",
		"debug_stack",
		"debug_variables",
		"debug_evaluate",
		"debug_stop",
	}

	t.Run("tier_and_fallback_classification", func(t *testing.T) {
		for _, name := range debugTools {
			if got := executionTierOf(name, nil); got != tierWorkspace {
				t.Fatalf("executionTierOf(%q) = %v, want tierWorkspace (%v)", name, got, tierWorkspace)
			}
			assistant := protocol.Message{
				Role:      "assistant",
				ToolCalls: []protocol.ToolCall{{ID: "tc-1", Name: name, Arguments: json.RawMessage(`{}`)}},
			}
			toolRows := []protocol.Message{{Role: "tool", ToolCallID: "tc-1", Content: `{"ok":true}`}}
			if ev := AggregateGroupMutation(assistant, toolRows, nil); ev != MutationUnknown {
				t.Fatalf("AggregateGroupMutation(%q, nil execs) = %q, want %q", name, ev, MutationUnknown)
			}
		}
	})

	t.Run("real_workspace_writes_via_debug_tools", func(t *testing.T) {
		ws := t.TempDir()
		initGitRepoForTest(t, ws)
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: "s-f04"})
		if err != nil {
			t.Fatal(err)
		}
		if err := set.BindDebugger(workspaceWritingDebugger{workspace: ws}); err != nil {
			t.Fatal(err)
		}

		const sid = "s-f04"
		store := envStore(sid)

		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			// Step 1: debug_start writes Git-ignored build/debuggee_ignored.log
			toolStep(call("c1", "debug_start", `{"adapter_id":"go","program":"./main.go"}`)),
			// Step 2: pure read_file (MutationNone)
			toolStep(call("c2", "read_file", `{"path":"main.go"}`)),
			// Step 3: debug_evaluate writes visible workspace file debug_written.txt
			toolStep(call("c3", "debug_evaluate", `{"debug_id":"dbg-1","epoch":1,"frame_id":0,"expression":"debug_written.txt","side_effects":true}`)),
			answerOf("done"),
		}}

		loop := Loop{
			Models:    testModels(prov),
			Store:     store,
			Tools:     set,
			Snapshots: repo,
		}
		res, err := loop.Run(context.Background(), sid, Options{Session: testRef, RunID: "r-f04", MaxSteps: 8})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("Loop.Run status=%q err=%v", res.Status, err)
		}

		if _, err := os.Stat(filepath.Join(ws, "build", "debuggee_ignored.log")); err != nil {
			t.Fatalf("expected debug_start to write build/debuggee_ignored.log: %v", err)
		}
		if _, err := os.Stat(filepath.Join(ws, "debug_written.txt")); err != nil {
			t.Fatalf("expected debug_evaluate to write debug_written.txt: %v", err)
		}

		summaries, err := store.ListAnsweredExecutions(context.Background(), sid, 0)
		if err != nil {
			t.Fatal(err)
		}
		foundStart, foundEval := false, false
		for _, s := range summaries {
			switch s.ToolName {
			case "debug_start":
				foundStart = true
				if ev := classifyExecutionSummary(s); ev != MutationUnknown {
					t.Fatalf("debug_start summary classified as %q, want %q (summary=%+v)", ev, MutationUnknown, s)
				}
			case "debug_evaluate":
				foundEval = true
				if ev := classifyExecutionSummary(s); ev != MutationUnknown {
					t.Fatalf("debug_evaluate summary classified as %q, want %q (summary=%+v)", ev, MutationUnknown, s)
				}
			}
		}
		if !foundStart || !foundEval {
			t.Fatalf("expected derived_origins execution summaries for both debug_start (%v) and debug_evaluate (%v); got %+v",
				foundStart, foundEval, summaries)
		}
	})
}

// TestF09SealFilesMutationCanonicalSlashKeyMatching verifies that sealFilesMutation matches
// pre/post snapshots and CommitReceipt entries by canonical filepath.ToSlash path keys without
// assuming identical slice index order and without case folding.
func TestF09SealFilesMutationCanonicalSlashKeyMatching(t *testing.T) {
	t.Run("windows_sort_order_divergence_a_slash_b_vs_a0_slash_x", func(t *testing.T) {
		// On Windows: "a0\\x.go" ('0'=48) sorts BEFORE "a\\b.go" ('\\'=92) in Engine.Commit's prep key sort,
		// whereas SnapshotFiles sorts ToSlash paths: "a/b.go" ('/'=47) BEFORE "a0/x.go" ('0'=48)!
		x := &execution{
			preRef:  "files:2:sha256:1111111111111111111111111111111111111111111111111111111111111111",
			targets: []string{"a/b.go", "a0/x.go"},
			files: repository.FilesSnapshot{
				Complete: true,
				Files: []repository.FileState{
					{Path: "a/b.go", Blob: "blob-a-old"},
					{Path: "a0/x.go", Blob: "blob-a0-old"},
				},
			},
		}
		after := repository.FilesSnapshot{
			Complete: true,
			Files: []repository.FileState{
				{Path: "a/b.go", Blob: "blob-a-new"},
				{Path: "a0/x.go", Blob: "blob-a0-new"},
			},
		}
		receipt := editing.CommitReceipt{
			Committed: true,
			Files: []editing.FileCommitReceipt{
				{Path: "a0/x.go", Wrote: true, OldSHA256: "sha256:old2", NewSHA256: "sha256:new2"},
				{Path: "a/b.go", Wrote: true, OldSHA256: "sha256:old1", NewSHA256: "sha256:new1"},
			},
		}
		changed := []repository.FileState{
			{Path: "a/b.go", Blob: "blob-a-new"},
			{Path: "a0/x.go", Blob: "blob-a0-new"},
		}
		got := sealFilesMutation(x, "completed", "complete", after, changed, receipt, true, true)
		if got != string(MutationProven) {
			t.Fatalf("sealFilesMutation with Windows prep sort order = %q, want %q", got, MutationProven)
		}
	})

	t.Run("case_distinct_paths_never_case_folded", func(t *testing.T) {
		x := &execution{
			preRef:  "files:1:sha256:1111111111111111111111111111111111111111111111111111111111111111",
			targets: []string{"pkg/Foo.go"},
			files: repository.FilesSnapshot{
				Complete: true,
				Files:    []repository.FileState{{Path: "pkg/Foo.go", Blob: "blob-old"}},
			},
		}
		after := repository.FilesSnapshot{
			Complete: true,
			Files:    []repository.FileState{{Path: "pkg/Foo.go", Blob: "blob-new"}},
		}
		receipt := editing.CommitReceipt{
			Committed: true,
			Files: []editing.FileCommitReceipt{
				{Path: "pkg/foo.go", Wrote: true, OldSHA256: "sha256:old", NewSHA256: "sha256:new"},
			},
		}
		got := sealFilesMutation(x, "completed", "complete", after, after.Files, receipt, true, true)
		if got != string(MutationUnknown) {
			t.Fatalf("case-mismatched path Foo.go vs foo.go must NOT match: got %q, want %q", got, MutationUnknown)
		}
	})

	t.Run("duplicate_canonical_paths_rejected_as_unknown", func(t *testing.T) {
		x := &execution{
			preRef:  "files:2:sha256:1111111111111111111111111111111111111111111111111111111111111111",
			targets: []string{"a/b.go", "a/b.go"},
			files: repository.FilesSnapshot{
				Complete: true,
				Files: []repository.FileState{
					{Path: "a/b.go", Blob: "blob-1"},
					{Path: "a/b.go", Blob: "blob-1"},
				},
			},
		}
		after := repository.FilesSnapshot{
			Complete: true,
			Files: []repository.FileState{
				{Path: "a/b.go", Blob: "blob-2"},
				{Path: "a/b.go", Blob: "blob-2"},
			},
		}
		receipt := editing.CommitReceipt{
			Committed: true,
			Files: []editing.FileCommitReceipt{
				{Path: "a/b.go", Wrote: true, OldSHA256: "sha256:o", NewSHA256: "sha256:n"},
				{Path: "a/b.go", Wrote: true, OldSHA256: "sha256:o", NewSHA256: "sha256:n"},
			},
		}
		got := sealFilesMutation(x, "completed", "complete", after, after.Files, receipt, true, true)
		if got != string(MutationUnknown) {
			t.Fatalf("duplicate canonical paths must return MutationUnknown, got %q", got)
		}
	})

	t.Run("real_apply_patch_symlink_dir_sort_divergence", func(t *testing.T) {
		ws := t.TempDir()
		initGitRepoForTest(t, ws)
		if err := os.MkdirAll(filepath.Join(ws, "real_z"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(ws, "m_dir"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "real_z", "b.go"), []byte("package z // v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "m_dir", "x.go"), []byte("package m // v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real_z", filepath.Join(ws, "a_link")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: "s-f09"})
		if err != nil {
			t.Fatal(err)
		}
		const sid = "s-f09"
		store := envStore(sid)

		argsBytes, _ := json.Marshal(map[string]any{
			"patches": []map[string]any{
				{
					"path":      "a_link/b.go",
					"operation": "update",
					"edits":     []map[string]any{{"old_text": "// v1", "new_text": "// v2"}},
				},
				{
					"path":      "m_dir/x.go",
					"operation": "update",
					"edits":     []map[string]any{{"old_text": "// v1", "new_text": "// v2"}},
				},
			},
		})
		prov := &scriptedProvider{responses: []protocol.ProviderResponse{
			toolStep(call("p1", "apply_patch", string(argsBytes))),
			answerOf("patched"),
		}}

		loop := Loop{
			Models:    testModels(prov),
			Store:     store,
			Tools:     set,
			Snapshots: repo,
		}
		res, err := loop.Run(context.Background(), sid, Options{Session: testRef, RunID: "r-f09", MaxSteps: 4})
		if err != nil || res.Status != StatusCompleted {
			t.Fatalf("Loop.Run status=%q err=%v", res.Status, err)
		}
		summaries, err := store.ListAnsweredExecutions(context.Background(), sid, 0)
		if err != nil || len(summaries) != 1 {
			t.Fatalf("expected 1 execution summary, got %+v (err=%v)", summaries, err)
		}
		if ev := classifyExecutionSummary(summaries[0]); ev != MutationProven {
			t.Fatalf("expected MutationProven for multi-file apply_patch with divergent key/rel sort, got %q (postRef=%q)",
				ev, summaries[0].PostRef)
		}
	})
}
