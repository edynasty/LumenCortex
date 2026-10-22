package runtime

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/instructions"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// promptFixture is an Engine on a workspace with the project inputs ADR-0013 gates: a global
// AGENTS.md and skill, and a workspace AGENTS.md and skill.
type promptFixture struct {
	engine    *Engine
	workspace string
	globalDir string
}

func newPromptFixture(t *testing.T, trusted bool) promptFixture {
	t.Helper()
	workspace := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	keys := ""
	if trusted {
		keys = `"trustedWorkspaces": [` + quoteJSON(workspace) + `]`
	}
	useGlobalConfig(t, modelConfig(keys))
	globalDir, err := globalDirPath()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(globalDir, instructions.FileName), "Global rule: answer in English.")
	writeFile(t, filepath.Join(globalDir, "skills", "global-skill", "SKILL.md"), "---\nname: Global skill\n---\nglobal skill body")
	writeFile(t, filepath.Join(workspace, instructions.FileName), "Project rule: run the focused tests.")
	writeFile(t, filepath.Join(workspace, ".lumencortex", "skills", "project-skill", "SKILL.md"), "---\nname: Project skill\n---\nproject skill body")

	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return promptFixture{engine: engine, workspace: workspace, globalDir: globalDir}
}

func quoteJSON(value string) string { return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"` }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func globalDirPath() (string, error) {
	return config.GlobalDir()
}

func layerByKind(t *testing.T, layers []PromptLayer, kind string) PromptLayer {
	t.Helper()
	for _, layer := range layers {
		if layer.Kind == kind {
			return layer
		}
	}
	t.Fatalf("no %s layer in %#v", kind, layers)
	return PromptLayer{}
}

func includedTexts(layers []PromptLayer) []string {
	out := make([]string, 0, len(layers))
	for _, layer := range layers {
		if layer.Included {
			out = append(out, layer.Text)
		}
	}
	return out
}

// In an untrusted workspace the project inputs are not in the prompt, and the report says so; in a
// trusted one they are (ADR-0013).
func TestEffectivePromptGatesProjectInputsOnTrust(t *testing.T) {
	untrusted := newPromptFixture(t, false)
	report, err := untrusted.engine.EffectivePrompt(context.Background(), EffectivePromptRequest{Agent: "ask", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Trusted {
		t.Fatal("the workspace reports as trusted")
	}
	all := strings.Join(includedTexts(report.Layers), "\n")
	if strings.Contains(all, "Project rule") || strings.Contains(all, "project skill body") {
		t.Fatalf("untrusted prompt carries project inputs: %s", all)
	}
	if !strings.Contains(all, "Global rule") || !strings.Contains(all, "global skill body") {
		t.Fatalf("untrusted prompt dropped the user's own inputs: %s", all)
	}
	project := layerByKind(t, report.Layers, LayerProjectInstructions)
	if project.Included || project.Reason != instructions.ReasonUntrusted || project.Source != filepath.Join(untrusted.workspace, instructions.FileName) {
		t.Fatalf("project layer=%+v", project)
	}
	skills := layerByKind(t, report.Layers, LayerSkills)
	if !strings.Contains(skills.Warning, "project-skill") {
		t.Fatalf("skills layer does not name the withheld skill: %+v", skills)
	}
	if !slices.Contains(report.Warnings, project.Warning) {
		t.Fatalf("warnings=%v", report.Warnings)
	}

	trusted := newPromptFixture(t, true)
	report, err = trusted.engine.EffectivePrompt(context.Background(), EffectivePromptRequest{Agent: "ask", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Trusted {
		t.Fatal("the workspace reports as untrusted")
	}
	all = strings.Join(includedTexts(report.Layers), "\n")
	if !strings.Contains(all, "Project rule") || !strings.Contains(all, "project skill body") {
		t.Fatalf("trusted prompt lacks project inputs: %s", all)
	}
	project = layerByKind(t, report.Layers, LayerProjectInstructions)
	if !project.Included || project.Source != filepath.Join(trusted.workspace, instructions.FileName) {
		t.Fatalf("project layer=%+v", project)
	}
}

// The layers are ordered as the run sends them, each labelled with its source, and the agent's
// instructions come last, unlabelled.
func TestEffectivePromptLayerOrderAndLabels(t *testing.T) {
	f := newPromptFixture(t, true)
	report, err := f.engine.EffectivePrompt(context.Background(), EffectivePromptRequest{Agent: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(report.Layers))
	for _, layer := range report.Layers {
		kinds = append(kinds, layer.Kind)
	}
	want := []string{LayerBuiltin, LayerGlobalInstructions, LayerProjectInstructions, LayerSkills, LayerAgentInstructions}
	if !slices.Equal(kinds, want) {
		t.Fatalf("layers=%v", kinds)
	}
	global := layerByKind(t, report.Layers, LayerGlobalInstructions)
	if !strings.HasPrefix(global.Text, "## Global instructions (from "+filepath.Join(f.globalDir, instructions.FileName)+")") {
		t.Fatalf("global layer=%q", global.Text)
	}
	project := layerByKind(t, report.Layers, LayerProjectInstructions)
	if !strings.HasPrefix(project.Text, "## Project instructions (from ") {
		t.Fatalf("project layer=%q", project.Text)
	}
	skills := layerByKind(t, report.Layers, LayerSkills)
	if !strings.HasPrefix(skills.Text, "## Skills (from ") {
		t.Fatalf("skills layer=%q", skills.Text)
	}
	agent := layerByKind(t, report.Layers, LayerAgentInstructions)
	if agent.Included || agent.Reason != "none" || agent.Source != "agent:ask (builtin)" {
		t.Fatalf("agent layer=%+v", agent)
	}
	if report.Policy != "read-only" || report.Delegation != "" {
		t.Fatalf("grant=%s/%q", report.Policy, report.Delegation)
	}
}

// The contract: the prompt a real run sends is the prompt EffectivePrompt reports for the same
// inputs, layer by layer and tool by tool.
func TestEffectivePromptMatchesARealRun(t *testing.T) {
	f := newPromptFixture(t, true)
	provider := &recordingProvider{final: "answered"}
	bindRef(f.engine, "a", provider)
	supervisor, err := NewRunSupervisor(f.engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	handle := newTestSession(t, f.engine, "explain the transaction path")
	opts := AgentOptions{Agent: "ask", Policy: "read-only", Interactive: true, MaxSteps: 2}
	if _, err := supervisor.Start(context.Background(), handle.ID, opts); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)
	if len(provider.requests) != 1 {
		t.Fatalf("requests=%d", len(provider.requests))
	}
	sent := provider.requests[0]

	report, err := f.engine.EffectivePrompt(context.Background(), EffectivePromptRequest{
		SessionID: handle.ID, Agent: "ask", Policy: "read-only", Interactive: true, Subagents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every included layer but the Task State block is a system message, in order.
	var want []string
	for _, layer := range report.Layers {
		if layer.Kind == LayerTaskState || !layer.Included {
			continue
		}
		want = append(want, layer.Text)
	}
	if got := systemContents(sent.Messages); !slices.Equal(got, want) {
		t.Fatalf("system messages=%#v\nlayers=%#v", got, want)
	}
	// The Task State block is the first user message.
	state := layerByKind(t, report.Layers, LayerTaskState)
	if state.Text == "" || sent.Messages[len(want)].Role != "user" || sent.Messages[len(want)].Content != state.Text {
		t.Fatalf("task state=%q message=%+v", state.Text, sent.Messages[len(want)])
	}
	if !strings.HasPrefix(state.Text, taskstate.BlockHeader) {
		t.Fatalf("task state=%q", state.Text)
	}
	// The included tools are the specs the run sent, with the same permissions.
	included := map[string]PromptTool{}
	for _, tool := range report.Tools {
		if tool.Included {
			included[tool.Name] = tool
		}
	}
	if len(included) != len(sent.Tools) {
		t.Fatalf("report has %d tools, the run sent %d", len(included), len(sent.Tools))
	}
	for _, spec := range sent.Tools {
		tool, ok := included[spec.Name]
		if !ok || tool.Permission != spec.Permission {
			t.Fatalf("tool %s: report=%+v spec=%+v", spec.Name, tool, spec)
		}
	}
}

// A tool the run withholds is still reported, with the filter that withheld it.
func TestEffectivePromptReportsWithheldTools(t *testing.T) {
	f := newPromptFixture(t, true)
	report, err := f.engine.EffectivePrompt(context.Background(), EffectivePromptRequest{
		Agent: "ask", Policy: "read-only", ToolDenylist: []string{"git_diff"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]PromptTool{}
	for _, tool := range report.Tools {
		tools[tool.Name] = tool
	}
	if tool := tools["shell"]; tool.Included || tool.Reason != "policy" {
		t.Fatalf("shell=%+v", tool)
	}
	if tool := tools["write_file"]; tool.Included || tool.Reason != "policy" {
		t.Fatalf("write_file=%+v", tool)
	}
	if tool := tools["git_diff"]; tool.Included || tool.Reason != "denylist" {
		t.Fatalf("git_diff=%+v", tool)
	}
	if tool := tools["start_task"]; tool.Included || tool.Reason != "delegation" {
		t.Fatalf("start_task=%+v", tool)
	}
	if tool := tools["read_file"]; !tool.Included || tool.Source != "builtin" {
		t.Fatalf("read_file=%+v", tool)
	}
	if tool := tools["note_write"]; !tool.Included || tool.Source != "task" {
		t.Fatalf("note_write=%+v", tool)
	}
}

// EffectivePrompt describes a run, it does not start one: no session row, no transcript row, no
// Task State version and no file is written, and it works with no session at all.
func TestEffectivePromptWritesNothing(t *testing.T) {
	f := newPromptFixture(t, true)
	handle := newTestSession(t, f.engine, "read only a prompt")
	before := rowCounts(t, f.engine)
	agentFile := filepath.Join(f.workspace, instructions.FileName)
	info, err := os.Stat(agentFile)
	if err != nil {
		t.Fatal(err)
	}

	for _, req := range []EffectivePromptRequest{
		{},
		{Agent: "ask"},
		{SessionID: handle.ID, Agent: "ask", Interactive: true},
		{SessionID: handle.ID, Policy: "workspace", Delegation: "propose", Subagents: true},
	} {
		if _, err := f.engine.EffectivePrompt(context.Background(), req); err != nil {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	if after := rowCounts(t, f.engine); !slices.Equal(before, after) {
		t.Fatalf("rows changed: %v -> %v", before, after)
	}
	if now, err := os.Stat(agentFile); err != nil || !now.ModTime().Equal(info.ModTime()) {
		t.Fatalf("an input file changed: %v", err)
	}
	if _, info, err := f.engine.Session(context.Background(), handle.ID); err != nil || info.Status == "running" {
		t.Fatalf("session=%+v err=%v", info, err)
	}
}

// rowCounts is the number of rows in the tables a run writes, in a fixed order.
func rowCounts(t *testing.T, engine *Engine) []int {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out []int
	for _, table := range []string{"sessions", "session_messages", "agent_steps", "task_states", "journal"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		out = append(out, count)
	}
	return out
}

// The project instruction file is read from the Engine workspace, never from the session's worktree:
// an agent cannot edit the instructions that govern it and have them apply to the same run.
func TestEffectivePromptReadsTheEngineWorkspaceNotTheWorktree(t *testing.T) {
	engine, root := worktreeEngine(t)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	useGlobalConfig(t, modelConfig(`"trustedWorkspaces": [`+quoteJSON(root)+`]`))
	writeFile(t, filepath.Join(root, instructions.FileName), "Engine workspace rule.")
	handle, identity := attachedSession(t, engine, "worktree instructions")
	writeFile(t, filepath.Join(identity.Path, instructions.FileName), "Worktree rule.")

	report, err := engine.EffectivePrompt(context.Background(), EffectivePromptRequest{SessionID: handle.ID, Agent: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	project := layerByKind(t, report.Layers, LayerProjectInstructions)
	if !project.Included || project.Source != filepath.Join(engine.workspace, instructions.FileName) || !strings.Contains(project.Text, "Engine workspace rule.") {
		t.Fatalf("project layer=%+v", project)
	}
	if strings.Contains(project.Text, "Worktree rule.") {
		t.Fatal("the worktree's instruction file was read")
	}
}

// A child session's report carries the authority its relation fixes, not the host's request.
func TestEffectivePromptChildAuthority(t *testing.T) {
	f := newPromptFixture(t, true)
	parent := newTestSession(t, f.engine, "parent")
	child := newTestSession(t, f.engine, "child")
	if err := f.engine.store.AddRelation(context.Background(), session.Relation{
		ParentSessionID: parent.ID, ChildSessionID: child.ID, Kind: RelationExecution,
	}); err != nil {
		t.Fatal(err)
	}
	report, err := f.engine.EffectivePrompt(context.Background(), EffectivePromptRequest{
		SessionID: child.ID, Policy: "full", Delegation: "isolated", Subagents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Policy != "workspace" || report.Delegation != "" || report.Agent != "worker" {
		t.Fatalf("child grant=%s/%q agent=%s", report.Policy, report.Delegation, report.Agent)
	}
	for _, tool := range report.Tools {
		if tool.Name == "shell" && tool.Included {
			t.Fatalf("a child report offers shell: %+v", tool)
		}
		if tool.Name == "start_task" && tool.Included {
			t.Fatalf("a child report offers start_task: %+v", tool)
		}
	}
}

// A real run in an untrusted workspace sends the user's own inputs and none of the project's, and
// records what it withheld; the child path assembles the same layers (ADR-0013).
func TestRunGatesProjectInputsOnTrust(t *testing.T) {
	f := newPromptFixture(t, false)
	events, unsubscribe := f.engine.Events(4096)
	defer unsubscribe()
	provider := &recordingProvider{final: "answered"}
	bindRef(f.engine, "a", provider)
	supervisor, err := NewRunSupervisor(f.engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	handle := newTestSession(t, f.engine, "explain the workspace")
	if _, err := supervisor.Start(context.Background(), handle.ID, AgentOptions{Agent: "ask", Interactive: true, MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)

	system := strings.Join(systemContents(provider.requests[0].Messages), "\n")
	if !strings.Contains(system, "Global rule") || !strings.Contains(system, "global skill body") {
		t.Fatalf("the run dropped the user's own inputs: %s", system)
	}
	if strings.Contains(system, "Project rule") || strings.Contains(system, "project skill body") {
		t.Fatalf("the run sent project inputs from an untrusted workspace: %s", system)
	}
	raw := drainEvents(events)
	if !strings.Contains(raw, `"skills.loaded"`) || !strings.Contains(raw, "project-skill") || !strings.Contains(raw, "untrusted workspace") {
		t.Fatalf("skills.loaded does not record the withheld project skill: %s", raw)
	}

	// A child run assembles the same layers: the base prompt is the child's, the rest is the same.
	childProvider := &recordingProvider{final: "explored"}
	childRef := bindRef(f.engine, "b", childProvider)
	parent := newTestSession(t, f.engine, "parent")
	child := newTestSession(t, f.engine, "child")
	if err := f.engine.store.AddRelation(context.Background(), session.Relation{
		ParentSessionID: parent.ID, ChildSessionID: child.ID, Kind: RelationSubagent,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.RunAgent(context.Background(), child.ID, AgentOptions{ModelRef: childRef, MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	system = strings.Join(systemContents(childProvider.requests[0].Messages), "\n")
	if !strings.Contains(system, "Global rule") || !strings.Contains(system, "global skill body") {
		t.Fatalf("the child run dropped the user's own inputs: %s", system)
	}
	if strings.Contains(system, "Project rule") || strings.Contains(system, "project skill body") {
		t.Fatalf("the child run sent project inputs from an untrusted workspace: %s", system)
	}
}

// A child session's report describes the child's own prompt, not a thread's.
func TestEffectivePromptChildBasePrompt(t *testing.T) {
	f := newPromptFixture(t, true)
	parent := newTestSession(t, f.engine, "parent")
	child := newTestSession(t, f.engine, "child")
	if err := f.engine.store.AddRelation(context.Background(), session.Relation{
		ParentSessionID: parent.ID, ChildSessionID: child.ID, Kind: RelationSubagent,
	}); err != nil {
		t.Fatal(err)
	}
	report, err := f.engine.EffectivePrompt(context.Background(), EffectivePromptRequest{SessionID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	base := layerByKind(t, report.Layers, LayerBuiltin)
	if base.Text != explorePrompt {
		t.Fatalf("child base prompt=%q", base.Text)
	}
}
