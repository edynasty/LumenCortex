package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

func TestCategoryAndDecisionProvidersFromSharedConfig(t *testing.T) {
	config := cognition.Config{
		Decision: cognition.DecisionConfig{
			Policy: "first",
			Providers: []cognition.DecisionProviderSpec{
				{Type: "laya", BaseURL: "http://127.0.0.1:8000"},
				{
					Type: "generative", Name: "fast-fallback",
					Provider: "generic", Model: "router-model",
					BaseURL: "http://127.0.0.1:11434/v1",
					ReasoningEffort: "low", MaxTokens: 512,
				},
				{Type: "jev"},
			},
		},
		Categories: map[string]cognition.CategoryConfig{
			"deep": {
				Models: []cognition.ModelSpec{
					{Provider: "groq", Model: "openai/gpt-oss-120b"},
					{Provider: "deepseek", Model: "deepseek-flash"},
				},
			},
		},
	}
	chains, err := categoryProvidersFromConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains["deep"]) != 2 {
		t.Fatalf("deep chain=%#v", chains["deep"])
	}
	if chains["deep"][0].Name != "groq" || chains["deep"][0].Provider.Model() != "openai/gpt-oss-120b" {
		t.Fatalf("first binding=%#v model=%q", chains["deep"][0], chains["deep"][0].Provider.Model())
	}
	if chains["deep"][1].Name != "deepseek" || chains["deep"][1].Provider.Model() != "deepseek-flash" {
		t.Fatalf("second binding=%#v model=%q", chains["deep"][1], chains["deep"][1].Provider.Model())
	}

	decisions, err := decisionProvidersFromConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 3 {
		t.Fatalf("decision providers=%#v", decisions)
	}
	if decisions[0].Name() != "laya" ||
		decisions[1].Name() != "fast-fallback" ||
		decisions[1].Model() != "router-model" ||
		decisions[2].Name() != "jev" ||
		decisions[2].Model() != "jev-latest" {
		t.Fatalf("decision providers=%#v", decisions)
	}
}

func TestAgentOptionsAutoEnableCognitionFromWorkspaceProfile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".lumencortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{
		"decision":{
			"providers":[{"type":"laya","baseURL":"http://127.0.0.1:8000"}]
		},
		"categories":{
			"general":{
				"default":true,
				"models":[{"provider":"generic","model":"local-fast","baseURL":"http://127.0.0.1:11434/v1"}]
			}
		},
		"health":{"failureThreshold":4,"cooldownMs":12000}
	}`)
	if err := os.WriteFile(filepath.Join(dir, "cognition.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LCX_MODEL", "fallback-model")
	t.Setenv("LCX_BASE_URL", "http://127.0.0.1:11434/v1")
	t.Setenv("LCX_COGNITION", "")
	t.Setenv("LCX_COGNITION_PROFILE", "")
	t.Setenv("LCX_WORKFLOW", "")
	t.Setenv("LCX_WORK_UNITS", "")

	opts, err := agentOptionsFromEnv(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.CognitionEnabled {
		t.Fatal("workspace cognition profile should enable cognitive routing")
	}
	if len(opts.CategoryProviders["general"]) != 1 {
		t.Fatalf("general providers=%#v", opts.CategoryProviders["general"])
	}
	if opts.CategoryProviders["general"][0].Provider.Model() != "local-fast" {
		t.Fatalf("model=%q", opts.CategoryProviders["general"][0].Provider.Model())
	}
	if len(opts.DecisionProviders) != 1 || opts.DecisionProviders[0].Name() != "laya" {
		t.Fatalf("decision providers=%#v", opts.DecisionProviders)
	}
}


func TestGovernorCuratorUsesDedicatedProfileBinding(t *testing.T) {
	disabled, err := governorCuratorFromConfig(cognition.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if disabled != nil {
		t.Fatal("disabled Governor should use deterministic planning")
	}

	config := cognition.Config{
		Governor: &cognition.GovernorConfig{
			Enabled: true,
			Provider: "generic",
			Model: "governor-only-model",
			BaseURL: "http://127.0.0.1:11434/v1",
			ReasoningEffort: "max",
			MaxTokens: 7000,
		},
	}
	curator, err := governorCuratorFromConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if curator == nil {
		t.Fatal("expected configured Governor Curator")
	}
	if curator.Provider.Model() != "governor-only-model" {
		t.Fatalf("model=%q", curator.Provider.Model())
	}
	if curator.ReasoningEffort != "max" || curator.MaxTokens != 7000 {
		t.Fatalf("curator effort=%q maxTokens=%d", curator.ReasoningEffort, curator.MaxTokens)
	}
}


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

	if err := runWorktreeCommand(context.Background(), engine, []string{"remove", handle.ID, "--force"}); err != nil {
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
