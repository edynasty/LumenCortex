package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

func TestGoCLIWorktreeCommandsExposeSessionIsolation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	initCLIGitRepo(t, root)

	engine, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), lcx.SessionOptions{Goal: "cli worktree"})
	if err != nil {
		t.Fatal(err)
	}

	if err := runWorktreeCommand(context.Background(), engine, []string{"attach", handle.ID}); err != nil {
		t.Fatal(err)
	}
	runtime, err := engine.SessionRuntime(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Kind != lcx.RuntimeWorktree || runtime.Path == root || runtime.Branch == "" {
		t.Fatalf("runtime=%#v", runtime)
	}

	if err := runWorktreeCommand(context.Background(), engine, []string{"apply", handle.ID}); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected apply --yes guard, got %v", err)
	}

	// --force is gone: removal of unsaved work needs a discard of the inspected state.
	if err := runWorktreeCommand(context.Background(), engine, []string{"remove", handle.ID, "--force"}); err == nil {
		t.Fatal("the removed --force flag was accepted")
	}
	if err := os.WriteFile(filepath.Join(runtime.Path, "draft.txt"), []byte("unsaved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runWorktreeCommand(context.Background(), engine, []string{"remove", handle.ID}); !errors.Is(err, lcx.ErrWorktreeDirty) {
		t.Fatalf("remove of a dirty worktree: err=%v, want ErrWorktreeDirty", err)
	}
	inspected, err := engine.InspectSessionWorktree(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runWorktreeCommand(context.Background(), engine, []string{"remove", handle.ID, "--discard", inspected.Head, inspected.StatusDigest}); err != nil {
		t.Fatal(err)
	}
	after, err := engine.SessionRuntime(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Kind != lcx.RuntimeLocal || after.Path != root {
		t.Fatalf("after=%#v", after)
	}
}

func TestGoCLISkillsCommandsExposeRegistryLifecycle(t *testing.T) {
	root := t.TempDir()
	engine, err := lcx.Open(lcx.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	skillFile := filepath.Join(root, "example-skill.md")
	if err := os.WriteFile(skillFile, []byte("# Example Skill\n\nUse exact tests before edits.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runSkillsCommand(engine, []string{"save", "project", "example", skillFile}); err != nil {
		t.Fatal(err)
	}
	items, err := engine.Skills("effective")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "example" || !items[0].Enabled {
		t.Fatalf("items=%#v", items)
	}

	if err := runSkillsCommand(engine, []string{"disable", "project", "example"}); err != nil {
		t.Fatal(err)
	}
	items, err = engine.Skills("effective")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Enabled {
		t.Fatalf("disabled items=%#v", items)
	}

	if err := runSkillsCommand(engine, []string{"enable", "project", "example"}); err != nil {
		t.Fatal(err)
	}
	content, err := engine.SkillContent("project", "example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "exact tests") {
		t.Fatalf("content=%q", content)
	}

	if err := runSkillsCommand(engine, []string{"delete", "project", "example"}); err != nil {
		t.Fatal(err)
	}
	items, err = engine.Skills("effective")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("items after delete=%#v", items)
	}
}

func initCLIGitRepo(t *testing.T, root string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
}


func TestParseAgentCommandArgsWorktreeFlagsAndEnv(t *testing.T) {
	t.Run("default local session", func(t *testing.T) {
		t.Setenv("LCX_AGENT_WORKTREE", "")
		t.Setenv("LCX_WORKTREE_BASE", "")
		parsed, err := parseAgentCommandArgs([]string{"fix", "the", "bug"})
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Goal != "fix the bug" || parsed.Worktree || parsed.Base != "HEAD" {
			t.Fatalf("parsed=%#v", parsed)
		}
	})

	t.Run("explicit worktree and base", func(t *testing.T) {
		t.Setenv("LCX_AGENT_WORKTREE", "")
		t.Setenv("LCX_WORKTREE_BASE", "")
		parsed, err := parseAgentCommandArgs([]string{
			"--worktree", "--base", "origin/main", "fix", "isolated", "bug",
		})
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Goal != "fix isolated bug" || !parsed.Worktree || parsed.Base != "origin/main" {
			t.Fatalf("parsed=%#v", parsed)
		}
	})

	t.Run("base implies worktree", func(t *testing.T) {
		t.Setenv("LCX_AGENT_WORKTREE", "")
		t.Setenv("LCX_WORKTREE_BASE", "")
		parsed, err := parseAgentCommandArgs([]string{"--base", "HEAD~2", "inspect"})
		if err != nil {
			t.Fatal(err)
		}
		if !parsed.Worktree || parsed.Base != "HEAD~2" {
			t.Fatalf("parsed=%#v", parsed)
		}
	})

	t.Run("environment defaults and explicit opt out", func(t *testing.T) {
		t.Setenv("LCX_AGENT_WORKTREE", "true")
		t.Setenv("LCX_WORKTREE_BASE", "main")
		parsed, err := parseAgentCommandArgs([]string{"--no-worktree", "local", "task"})
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Worktree || parsed.Base != "main" || parsed.Goal != "local task" {
			t.Fatalf("parsed=%#v", parsed)
		}
	})

	t.Run("last isolation flag wins", func(t *testing.T) {
		t.Setenv("LCX_AGENT_WORKTREE", "")
		t.Setenv("LCX_WORKTREE_BASE", "")
		parsed, err := parseAgentCommandArgs([]string{
			"--base", "feature", "--no-worktree", "manual",
		})
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Worktree || parsed.Base != "feature" || parsed.Goal != "manual" {
			t.Fatalf("parsed=%#v", parsed)
		}
	})

	t.Run("invalid options fail closed", func(t *testing.T) {
		t.Setenv("LCX_AGENT_WORKTREE", "")
		t.Setenv("LCX_WORKTREE_BASE", "")
		for _, args := range [][]string{
			{},
			{"--base"},
			{"--unknown", "task"},
		} {
			if _, err := parseAgentCommandArgs(args); err == nil {
				t.Fatalf("expected error for %#v", args)
			}
		}
	})
}
