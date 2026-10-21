//go:build realmodel

// Real-model probes. They run only with the realmodel build tag and LCX_REAL_MODEL set to the one
// model the project owner authorised for them, so `go test ./...` never calls a provider:
//
//	LCX_REAL_MODEL=local/deepseek-v4.1-flash go test -tags realmodel -count=1 -run RealModel -v ./internal/realmodel
//
// The probe reads the user's global configuration only for that model's provider entry (its apiKey
// must be a {secret:NAME} or {env:NAME} reference; the value is never read here) and resolves the
// reference through the user's credential store, as the CLI does. Everything else runs in temporary
// directories: a fixture git repository and a private global configuration that trusts it.
package realmodel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

const authorisedRealModel = "local/deepseek-v4.1-flash"

// realModelEnv prepares a private global configuration naming the authorised model and the given
// agents, trusting the workspaces. It returns the provider factory that resolves the model's key.
func realModelEnv(t *testing.T, agents map[string]any, workspaces ...string) lcx.ProviderFactory {
	t.Helper()
	ref := os.Getenv("LCX_REAL_MODEL")
	if ref == "" {
		t.Skip("LCX_REAL_MODEL is not set")
	}
	if ref != authorisedRealModel {
		t.Fatalf("LCX_REAL_MODEL=%q: only %s is authorised for real-model probes", ref, authorisedRealModel)
	}
	globalDir, err := config.GlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	credPath, err := config.DefaultCredentialStorePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(globalDir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var global struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(raw, &global); err != nil {
		t.Fatal(err)
	}
	providerID := strings.SplitN(ref, "/", 2)[0]
	entry, ok := global.Providers[providerID]
	if !ok {
		t.Fatalf("provider %q is not configured", providerID)
	}
	settings, _ := entry["settings"].(map[string]any)
	key, _ := settings["apiKey"].(string)
	if key != "" && !strings.HasPrefix(key, "{secret:") && !strings.HasPrefix(key, "{env:") {
		t.Fatalf("provider %q apiKey is not a reference; the probe copies only references", providerID)
	}
	private := filepath.Join(t.TempDir(), "xdg")
	if err := os.MkdirAll(filepath.Join(private, "lumencortex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if workspaces == nil {
		workspaces = []string{}
	}
	doc := map[string]any{
		"model":             ref,
		"providers":         map[string]any{providerID: entry},
		"trustedWorkspaces": workspaces,
	}
	if len(agents) > 0 {
		doc["agents"] = agents
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "lumencortex", config.FileName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", private)
	store := config.NewFileCredentialStore(credPath)
	return func(m config.ResolvedModel) (protocol.Provider, error) {
		return provider.FromModel(m, provider.Options{Credentials: store})
	}
}

// fixtureRepo writes a small Go module with one failing test, one widely used function and a README,
// and commits it.
func fixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=probe", "-c", "user.email=probe@example.invalid", "commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

var calcFixture = map[string]string{
	"go.mod": "module example.com/calc\n\ngo 1.22\n",
	"calc/calc.go": `package calc

// Add returns a+b.
func Add(a, b int) int { return a + b }

// Mul returns a*b.
func Mul(a, b int) int { return a + b }
`,
	"calc/calc_test.go": `package calc

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2,3)=%d", got)
	}
}

func TestMul(t *testing.T) {
	if got := Mul(2, 3); got != 6 {
		t.Fatalf("Mul(2,3)=%d", got)
	}
}
`,
	"settings/settings.go": `package settings

import "os"

// Load reads the settings file at path.
func Load(path string) ([]byte, error) { return os.ReadFile(path) }
`,
	"cmd/app/main.go": `package main

import "example.com/calc/settings"

func main() { _, _ = settings.Load("app.json") }
`,
	"server/server.go": `package server

import "example.com/calc/settings"

// Boot loads the server settings.
func Boot() error { _, err := settings.Load("server.json"); return err }
`,
	"worker/worker.go": `package worker

import "example.com/calc/settings"

// Init loads the worker settings twice: defaults, then overrides.
func Init() {
	_, _ = settings.Load("defaults.json")
	_, _ = settings.Load("worker.json")
}
`,
	"README.md": "# calc\n\nA tiny calculator.\n",
}

// probeAgents are three specialists whose descriptions are the only thing that tells them apart.
var probeAgents = map[string]any{
	"usage-tracer": map[string]any{
		"use": "delegation", "policy": "read-only",
		"description": "Traces where a function, type or setting is defined and used across the repository and reports every call site as file:line. Use for 'who calls X' or 'where is X used' questions.",
	},
	"go-test-fixer": map[string]any{
		"use": "delegation", "policy": "workspace",
		"description": "Fixes failing Go unit tests in an isolated worktree: runs go test, corrects the code under test, re-runs until green. Use for broken tests.",
	},
	"docs-editor": map[string]any{
		"use": "delegation", "policy": "workspace",
		"description": "Edits Markdown documentation (README.md, docs/) only and never changes code. Use for documentation updates.",
	},
}

// delegationCall is one spawn_subagent or start_task call the parent made.
type delegationCall struct {
	Tool  string
	Agent string
}

func TestRealModelAgentSelection(t *testing.T) {
	scenarios := []struct {
		name, prompt string
		// want is the acceptable (tool, agent) pairs; "" means the tool's default (agent omitted).
		want []delegationCall
	}{
		{
			name:   "tracer",
			prompt: "Delegate this to a child agent: find every call site of settings.Load in this repository and report them as file:line. Wait for the child and give me its list.",
			want:   []delegationCall{{"spawn_subagent", "usage-tracer"}},
		},
		{
			name:   "fixer",
			prompt: "The calc tests are failing. Start an execution task that fixes them in its own worktree; do not edit files yourself.",
			want:   []delegationCall{{"start_task", "go-test-fixer"}},
		},
		{
			name:   "docs",
			prompt: "Start an execution task that adds a Usage section to README.md describing Add and Mul. Do not edit files yourself.",
			want:   []delegationCall{{"start_task", "docs-editor"}},
		},
		{
			name:   "generic",
			prompt: "Delegate to a read-only child agent: summarise what the go.mod file declares. Wait for its answer and repeat it.",
			// No specialist matches: the default explore (omitted or named) is the expected choice.
			want: []delegationCall{{"spawn_subagent", ""}, {"spawn_subagent", config.AgentExplore}},
		},
	}
	repeats := 2
	if v := os.Getenv("LCX_PROBE_REPEATS"); v != "" {
		fmt.Sscan(v, &repeats)
	}
	type outcome struct {
		scenario string
		calls    []delegationCall
		children []lcx.SubagentNode
		status   string
		ok       bool
	}
	var outcomes []outcome
	for _, sc := range scenarios {
		for i := 0; i < repeats; i++ {
			t.Run(fmt.Sprintf("%s/%d", sc.name, i), func(t *testing.T) {
				repo := fixtureRepo(t, calcFixture)
				factory := realModelEnv(t, probeAgents, repo)
				calls, children, status := runProbeThread(t, repo, factory, sc.prompt)
				ok := false
				for _, c := range calls {
					for _, w := range sc.want {
						if c == w {
							ok = true
						}
					}
				}
				outcomes = append(outcomes, outcome{sc.name, calls, children, status, ok})
				t.Logf("status=%s calls=%+v", status, calls)
				for _, child := range children {
					t.Logf("child %s kind=%s agent=%s status=%s final=%.200q", child.SessionID, child.Kind, child.Agent, child.Status, child.Final)
				}
				if !ok {
					t.Errorf("no delegation call in %+v matched %+v", calls, sc.want)
				}
				// Every child that ran pinned the agent its call named (or the launch default).
				for _, child := range children {
					if child.Agent == "" && child.Status != "created" {
						t.Errorf("child %s ran without a pinned agent", child.SessionID)
					}
				}
			})
		}
	}
	matched := 0
	for _, o := range outcomes {
		if o.ok {
			matched++
		}
	}
	t.Logf("selection matched %d/%d", matched, len(outcomes))
}

// runProbeThread starts one interactive thread with delegation isolated, waits for it and every
// child to finish, and returns the delegation calls, the child tree and the parent status.
func runProbeThread(t *testing.T, repo string, factory lcx.ProviderFactory, prompt string) ([]delegationCall, []lcx.SubagentNode, string) {
	t.Helper()
	ctx := context.Background()
	engine, err := lcx.OpenContext(ctx, lcx.Options{Workspace: repo, ProviderFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	events, stop := engine.Events(1024)
	var mu sync.Mutex
	var calls []delegationCall
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if ev.Type != "tool.start" {
				continue
			}
			var data struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			}
			if json.Unmarshal(ev.Data, &data) != nil || (data.Name != "spawn_subagent" && data.Name != "start_task") {
				continue
			}
			var args struct {
				Agent string `json:"agent"`
			}
			_ = json.Unmarshal(data.Args, &args)
			mu.Lock()
			calls = append(calls, delegationCall{data.Name, args.Agent})
			mu.Unlock()
		}
	}()
	supervisor, err := lcx.NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	handle, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Start(ctx, handle.ID, lcx.AgentOptions{
		Interactive: true, Delegation: lcx.DelegationIsolated, Effort: "low", MaxSteps: 12,
	}); err != nil {
		t.Fatal(err)
	}
	// Wall-clock bound against a run that never ends; normal runs finish in well under it.
	deadline := time.Now().Add(8 * time.Minute)
	for {
		tree, err := supervisor.SubagentTree(ctx, handle.ID)
		if err != nil {
			t.Fatal(err)
		}
		busy := supervisor.Active(handle.ID)
		for _, n := range tree {
			busy = busy || n.Active
		}
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			supervisor.Cancel(handle.ID)
			_, _ = supervisor.CancelSubtree(ctx, handle.ID)
			t.Fatalf("probe thread still running after its wall-clock bound")
		}
		time.Sleep(200 * time.Millisecond)
	}
	tree, err := supervisor.SubagentTree(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, info, err := engine.Session(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return append([]delegationCall(nil), calls...), tree, info.Status
}
