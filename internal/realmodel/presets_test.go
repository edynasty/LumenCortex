//go:build realmodel

// Real-model validation of the built-in child prompts and the four editable presets (ADR-0009 A4):
//
//	LCX_REAL_MODEL=local/deepseek-v4.1-flash LCX_PRESET_OUT=/tmp/presets.json \
//	  go test -tags realmodel -count=1 -run RealModelAgentPresets -v -timeout 60m ./internal/realmodel
//
// Every scenario runs in a temporary git repository. The presets are saved into that repository's
// lumincortex.json as ordinary agents, exactly as the editor's "add from preset" writes them, and
// the private global configuration trusts the repository. Evidence (tool calls, model requests,
// child sessions, final answers, dirty files) is written to LCX_PRESET_OUT.
package realmodel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	lcx "github.com/edynasty/LumenCortex/runtime"
)

// presetToolCall is one tool call of a run, joined from tool.start and tool.end.
type presetToolCall struct {
	Session string          `json:"session"`
	Step    int             `json:"step"`
	Tool    string          `json:"tool"`
	Args    json.RawMessage `json:"args,omitempty"`
	OK      bool            `json:"ok"`
	Denied  bool            `json:"denied"`
}

// presetRequest is one llm.request: the candidate that was tried and the tools it was offered.
type presetRequest struct {
	Session        string   `json:"session"`
	Step           int      `json:"step"`
	ModelRef       string   `json:"modelRef"`
	CandidateIndex int      `json:"candidateIndex"`
	Tools          []string `json:"tools"`
}

type presetEvidence struct {
	Scenario  string             `json:"scenario"`
	Session   string             `json:"session"`
	Agent     string             `json:"agent"`
	Status    string             `json:"status"`
	Final     string             `json:"final"`
	Error     string             `json:"error,omitempty"`
	Requests  []presetRequest    `json:"requests"`
	Calls     []presetToolCall   `json:"calls"`
	Children  []lcx.SubagentNode `json:"children,omitempty"`
	Files     map[string]string  `json:"files,omitempty"`
	Git       string             `json:"git,omitempty"`
	Worktrees map[string]string  `json:"worktrees,omitempty"`
	Notes     []string           `json:"notes,omitempty"`
}

// presetRecorder subscribes to the engine's events before a run starts and keeps the observable
// evidence: every tool call with its arguments and outcome, and every model request.
type presetRecorder struct {
	mu    sync.Mutex
	calls []presetToolCall
	index map[string]int
	reqs  []presetRequest
}

func newPresetRecorder(engine *lcx.Engine) (*presetRecorder, func()) {
	rec := &presetRecorder{index: map[string]int{}}
	events, stop := engine.Events(8192)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			rec.handle(ev)
		}
	}()
	return rec, func() {
		stop()
		<-done
	}
}

func (r *presetRecorder) handle(ev lcx.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Type {
	case "tool.start":
		var data struct {
			Step       int             `json:"step"`
			ToolCallID string          `json:"toolCallId"`
			Name       string          `json:"name"`
			Args       json.RawMessage `json:"args"`
		}
		if json.Unmarshal(ev.Data, &data) != nil {
			return
		}
		key := ev.SessionID + "\x00" + data.ToolCallID
		r.index[key] = len(r.calls)
		r.calls = append(r.calls, presetToolCall{Session: ev.SessionID, Step: data.Step, Tool: data.Name, Args: data.Args})
	case "tool.end":
		var data struct {
			ToolCallID string `json:"toolCallId"`
			OK         bool   `json:"ok"`
			Denied     bool   `json:"denied"`
		}
		if json.Unmarshal(ev.Data, &data) != nil {
			return
		}
		if i, ok := r.index[ev.SessionID+"\x00"+data.ToolCallID]; ok {
			r.calls[i].OK, r.calls[i].Denied = data.OK, data.Denied
		}
	case "llm.request":
		var data struct {
			Step           int      `json:"step"`
			ModelRef       string   `json:"modelRef"`
			CandidateIndex int      `json:"candidateIndex"`
			Tools          []string `json:"tools"`
		}
		if json.Unmarshal(ev.Data, &data) != nil {
			return
		}
		r.reqs = append(r.reqs, presetRequest{Session: ev.SessionID, Step: data.Step, ModelRef: data.ModelRef, CandidateIndex: data.CandidateIndex, Tools: data.Tools})
	}
}

func (r *presetRecorder) snapshot() ([]presetToolCall, []presetRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]presetToolCall(nil), r.calls...), append([]presetRequest(nil), r.reqs...)
}

// presetAgents returns the four presets as ordinary catalog entries, the shape the editor saves.
func presetAgents(t *testing.T) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, p := range config.AgentPresets() {
		a := p.Agent
		entry := map[string]any{
			"name":         a.Name,
			"description":  a.Description,
			"use":          string(a.Use),
			"policy":       a.Policy,
			"instructions": a.Instructions,
		}
		if a.Effort != "" {
			entry["effort"] = string(a.Effort)
		}
		if a.Delegation != "" {
			entry["delegation"] = a.Delegation
		}
		out[p.ID] = entry
	}
	return out
}

// writeWorkspaceConfig writes repo/lumencortex.json with the given agents and optional extras.
func writeWorkspaceConfig(t *testing.T, repo string, agents map[string]any, extras map[string]any) {
	t.Helper()
	doc := map[string]any{"agents": agents}
	for key, value := range extras {
		doc[key] = value
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, config.FileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// presetWriteFile writes one fixture file under dir.
func presetWriteFile(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// presetGit runs one git command in dir.
func presetGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// presetGitStatus is `git status --porcelain` in dir.
func presetGitStatus(t *testing.T, dir string) string {
	t.Helper()
	return presetGit(t, dir, "status", "--porcelain")
}

// presetGitStatusQuiet is `git status --porcelain` without failing the test.
func presetGitStatusQuiet(dir string) (string, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// presetCommitAll commits the fixture baseline.
func presetCommitAll(t *testing.T, dir string) {
	t.Helper()
	presetGit(t, dir, "add", "-A")
	presetGit(t, dir, "-c", "user.name=probe", "-c", "user.email=probe@example.invalid", "commit", "-q", "-m", "fixture")
}

// presetRepo builds a committed git repository from files.
func presetRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	for name, body := range files {
		presetWriteFile(t, dir, name, body)
	}
	presetGit(t, dir, "init", "-q", "-b", "main")
	presetCommitAll(t, dir)
	return dir
}

// writeEvidence appends one scenario's evidence to LCX_PRESET_OUT (one JSON object per line).
func writeEvidence(t *testing.T, ev *presetEvidence) {
	t.Helper()
	path := os.Getenv("LCX_PRESET_OUT")
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(body, '\n')); err != nil {
		t.Fatal(err)
	}
}

// runPresetTop runs one conversation agent to its exit and returns the evidence.
func runPresetTop(t *testing.T, repo string, factory lcx.ProviderFactory, agentID, policy, prompt string, maxSteps int) *presetEvidence {
	t.Helper()
	ctx := context.Background()
	engine, err := lcx.OpenContext(ctx, lcx.Options{Workspace: repo, RepoDir: repo + ".state", ProviderFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	rec, stop := newPresetRecorder(engine)
	handle, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: prompt})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := engine.RunAgent(ctx, handle.ID, lcx.AgentOptions{
		Agent: agentID, Policy: policy, MaxSteps: maxSteps, Effort: "low", Interactive: true,
	})
	stop()
	calls, reqs := rec.snapshot()
	ev := &presetEvidence{Scenario: t.Name(), Session: result.SessionID, Agent: agentID, Status: result.Status, Final: result.Final, Requests: reqs, Calls: calls, Files: map[string]string{}}
	if runErr != nil {
		ev.Error = runErr.Error()
	}
	writeEvidence(t, ev)
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	return ev
}

// runPresetParent runs one conversation thread with delegation isolated, waits for it and every
// child, and returns the evidence plus the engine's child tree.
func runPresetParent(t *testing.T, repo string, factory lcx.ProviderFactory, prompt string, maxSteps int) (*presetEvidence, *lcx.Engine) {
	t.Helper()
	ctx := context.Background()
	engine, err := lcx.OpenContext(ctx, lcx.Options{Workspace: repo, RepoDir: repo + ".state", ProviderFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	rec, stopRec := newPresetRecorder(engine)
	supervisor, err := lcx.NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := engine.NewSession(ctx, lcx.SessionOptions{Goal: prompt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Start(ctx, handle.ID, lcx.AgentOptions{
		Policy: "full", Interactive: true, Delegation: lcx.DelegationIsolated, Effort: "low",
		MaxSteps: maxSteps, MaxToolCallsPerStep: 6,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Minute)
	var tree []lcx.SubagentNode
	for {
		tree, err = supervisor.SubagentTree(ctx, handle.ID)
		if err != nil {
			t.Fatal(err)
		}
		busy := supervisor.Active(handle.ID)
		for _, node := range tree {
			busy = busy || node.Active
		}
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			supervisor.Cancel(handle.ID)
			_, _ = supervisor.CancelSubtree(ctx, handle.ID)
			t.Fatal("parent thread still running after its wall-clock bound")
		}
		time.Sleep(250 * time.Millisecond)
	}
	_, info, err := engine.Session(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	stopRec()
	calls, reqs := rec.snapshot()
	final := ""
	if info.Final != nil {
		final = *info.Final
	}
	worktrees := map[string]string{}
	for _, node := range tree {
		if node.Runtime.Path == "" {
			continue
		}
		if status, err := presetGitStatusQuiet(node.Runtime.Path); err == nil {
			worktrees[node.SessionID] = status
		}
	}
	ev := &presetEvidence{Scenario: t.Name(), Session: info.ID, Agent: info.Agent, Status: info.Status, Final: final, Requests: reqs, Calls: calls, Children: tree, Files: map[string]string{}, Worktrees: worktrees}
	writeEvidence(t, ev)
	return ev, engine
}

// presetChild returns the first child of the given relation kind.
func presetChild(tree []lcx.SubagentNode, kind string) (lcx.SubagentNode, bool) {
	for _, node := range tree {
		if node.Kind == kind {
			return node, true
		}
	}
	return lcx.SubagentNode{}, false
}

// callsOf returns the tool calls a session made, in order.
func callsOf(calls []presetToolCall, session string) []presetToolCall {
	out := make([]presetToolCall, 0, len(calls))
	for _, call := range calls {
		if call.Session == session {
			out = append(out, call)
		}
	}
	return out
}

func toolNames(calls []presetToolCall) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.Tool)
	}
	return out
}

func containsAny(value string, needles ...string) bool {
	lower := strings.ToLower(value)
	for _, needle := range needles {
		if strings.Contains(lower, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func hasTool(names []string, want ...string) bool {
	for _, name := range names {
		for _, w := range want {
			if name == w {
				return true
			}
		}
	}
	return false
}

// presetCountRef matches a call or declaration of Count, not TestCount.
var presetCountRef = regexp.MustCompile(`(?:^|[^\w])Count\s*\(`)

// writeTools are the tools that change the workspace or execute commands.
var writeTools = []string{"write_file", "replace_in_file", "apply_patch", "shell"}

// readTools are the tools a read-only run may use to inspect the workspace.
var readTools = []string{"read_file", "search_text", "find_files", "list_dir", "git_status", "git_diff", "lsp_hover", "lsp_definition", "lsp_references", "lsp_document_symbols", "lsp_workspace_symbols", "lsp_diagnostics"}

func TestRealModelAgentPresets(t *testing.T) {
	if os.Getenv("LCX_REAL_MODEL") == "" {
		t.Skip("LCX_REAL_MODEL is not set")
	}

	t.Run("reviewer", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod": "module example.com/calc\n\ngo 1.22\n",
			"calc/calc.go": `package calc

// Add returns a+b.
func Add(a, b int) int { return a + b }

// Mul returns a*b.
func Mul(a, b int) int { return a * b }
`,
			"calc/calc_test.go": `package calc

import "testing"

func TestMul(t *testing.T) {
	if got := Mul(2, 3); got != 6 {
		t.Fatalf("Mul(2,3)=%d", got)
	}
}
`,
			"README.md": "# calc\n\nA tiny calculator.\n",
		})
		// The change under review: a real bug (Mul adds) and a comment-only edit that is no issue.
		presetWriteFile(t, repo, "calc/calc.go", `package calc

// Add returns the sum of a and b.
func Add(a, b int) int { return a + b }

// Mul returns a*b.
func Mul(a, b int) int { return a + b }
`)
		factory := realModelEnv(t, nil, repo)
		writeWorkspaceConfig(t, repo, presetAgents(t), nil)
		ev := runPresetTop(t, repo, factory, "reviewer", "read-only",
			"Review the current uncommitted change in this repository for defects the author would want fixed before merge. Do not edit files.", 10)
		names := toolNames(ev.Calls)
		t.Logf("tools=%v final=%.2000q", names, ev.Final)
		if !hasTool(names, "git_status", "git_diff") {
			t.Errorf("reviewer did not inspect the diff: tools=%v", names)
		}
		if hasTool(names, writeTools...) {
			t.Errorf("reviewer used a mutating tool: %v", names)
		}
		if !strings.Contains(ev.Final, "Verdict") || !strings.Contains(ev.Final, "Location:") {
			t.Errorf("reviewer output does not follow the requested format: %q", ev.Final)
		}
		if !strings.Contains(strings.ToLower(ev.Final), "mul") {
			t.Errorf("reviewer did not report the seeded Mul bug: %q", ev.Final)
		}
		if got := strings.Count(ev.Final, "Location:"); got != 1 {
			t.Errorf("reviewer reported %d findings, want exactly the seeded bug: %q", got, ev.Final)
		}
		verdict := ""
		for _, line := range strings.Split(ev.Final, "\n") {
			if strings.Contains(strings.ToLower(line), "verdict") {
				verdict = line
				break
			}
		}
		if !strings.Contains(strings.ToLower(verdict), "incorrect") {
			t.Errorf("reviewer verdict is not incorrect: %q", verdict)
		}
	})

	t.Run("security-reviewer", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod": "module example.com/fileserver\n\ngo 1.22\n",
			"fileserver/server.go": `package fileserver

import (
	"database/sql"
	"net/http"
	"path/filepath"
)

var db *sql.DB

// Download serves a file from the data directory.
func Download(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	http.ServeFile(w, r, filepath.Join("/srv/files", name))
}

// Lookup finds a user by name.
func Lookup(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	row := db.QueryRow("SELECT email FROM users WHERE name = '" + name + "'")
	var email string
	_ = row.Scan(&email)
	_, _ = w.Write([]byte(email))
}
`,
			"README.md": "# fileserver\n\nServes files and looks up users.\n",
		})
		factory := realModelEnv(t, nil, repo)
		writeWorkspaceConfig(t, repo, presetAgents(t), nil)
		ev := runPresetTop(t, repo, factory, "security-reviewer", "read-only",
			"Review this repository for security defects. Report the exploitable issues with evidence and remediation.", 10)
		names := toolNames(ev.Calls)
		t.Logf("tools=%v final=%.3000q", names, ev.Final)
		if hasTool(names, writeTools...) {
			t.Errorf("security reviewer used a mutating tool: %v", names)
		}
		if !containsAny(ev.Final, "CWE-22", "CWE-89") {
			t.Errorf("security reviewer reported no CWE for the seeded traversal/injection: %q", ev.Final)
		}
		if !strings.Contains(ev.Final, "Remediation:") || !strings.Contains(strings.ToLower(ev.Final), "reviewed paths") {
			t.Errorf("security reviewer output lacks remediation or reviewed paths: %q", ev.Final)
		}
		if !strings.Contains(ev.Final, "fileserver/server.go") {
			t.Errorf("security reviewer did not locate the vulnerable file: %q", ev.Final)
		}
	})

	t.Run("sonic", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod": "module example.com/counter\n\ngo 1.22\n",
			"counter/counter.go": `package counter

// Count returns the number of positive values.
func Count(values []int) int {
	n := 0
	for _, v := range values {
		if v > 0 {
			n++
		}
	}
	return n
}

// Sum returns the sum of values.
func Sum(values []int) int {
	total := 0
	for _, v := range values {
		total += v
	}
	return total
}
`,
			"counter/counter_test.go": `package counter

import "testing"

func TestCount(t *testing.T) {
	if got := Count([]int{1, -1, 2}); got != 2 {
		t.Fatalf("Count=%d", got)
	}
}
`,
			"main.go": `package main

import (
	"fmt"

	"example.com/counter/counter"
)

func main() {
	fmt.Println(counter.Count([]int{1, 2, 3}))
}
`,
		})
		factory := realModelEnv(t, nil, repo)
		writeWorkspaceConfig(t, repo, presetAgents(t), nil)
		prompt := "Start an execution task with the sonic agent. Objective: in exactly these files — counter/counter.go, counter/counter_test.go and main.go — rename the function Count to Total and update every call to it. Change nothing else. Do not do the work yourself."
		ev, engine := runPresetParent(t, repo, factory, prompt, 12)
		defer engine.Close()
		child, ok := presetChild(ev.Children, lcx.RelationExecution)
		if !ok {
			t.Fatalf("parent started no execution task: final=%q calls=%v", ev.Final, toolNames(ev.Calls))
		}
		calls := callsOf(ev.Calls, child.SessionID)
		names := toolNames(calls)
		t.Logf("child agent=%s status=%s tools=%v final=%.2000q", child.Agent, child.Status, names, child.Final)
		if child.Agent != "sonic" {
			t.Errorf("execution child ran agent %q, want sonic", child.Agent)
		}
		if child.Status != "completed" {
			t.Errorf("sonic child status=%s final=%q", child.Status, child.Final)
		}
		if hasTool(names, "shell") {
			t.Errorf("sonic used a shell tool under workspace policy: %v", names)
		}
		worktree := child.Runtime.Path
		status := presetGitStatus(t, worktree)
		ev.Git = status
		dirty := strings.Split(strings.TrimSpace(status), "\n")
		if len(dirty) != 3 {
			t.Errorf("sonic changed %d files, want exactly 3: %q", len(dirty), status)
		}
		for _, name := range []string{"counter/counter.go", "counter/counter_test.go", "main.go"} {
			body, err := os.ReadFile(filepath.Join(worktree, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			ev.Files[name] = string(body)
			// A call or declaration of Count, not a distinct identifier such as TestCount.
			if presetCountRef.MatchString(string(body)) {
				t.Errorf("%s still references Count: %s", name, body)
			}
			if !strings.Contains(string(body), "Total") {
				t.Errorf("%s has no Total: %s", name, body)
			}
		}
		// Verification after the last write: a read or diff of what changed.
		lastWrite := -1
		for i, call := range calls {
			if hasTool([]string{call.Tool}, writeTools...) {
				lastWrite = i
			}
		}
		verified := false
		for _, call := range calls[lastWrite+1:] {
			if hasTool([]string{call.Tool}, "read_file", "git_diff", "git_status", "search_text") {
				verified = true
			}
		}
		if !verified {
			t.Errorf("sonic did not verify after its last edit: %v", names)
		}
	})

	t.Run("thread-historian", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod":    "module example.com/hist\n\ngo 1.22\n",
			"README.md": "# hist\n",
		})
		factory := realModelEnv(t, nil, repo)
		writeWorkspaceConfig(t, repo, presetAgents(t), nil)
		// A prior thread whose final answer records the facts. read_thread exposes the objective,
		// resolution, notes, tasks, children and final answer, not raw messages, so the fact has to
		// sit in one of those fields to be answerable.
		prior := runPresetTop(t, repo, factory, "ask", "read-only",
			"Reply with exactly this line and nothing else: staging host is stg-7.internal; token variable is LCX_STAGING_TOKEN.", 3)
		threadID := prior.Session
		t.Logf("prior thread %s final=%.300q", threadID, prior.Final)
		prompt := fmt.Sprintf("Use the read-only subagent thread-historian to read thread %s. Ask it: what staging host and token environment variable did that thread record? Then wait for it and repeat its answer verbatim.", threadID)
		ev, engine := runPresetParent(t, repo, factory, prompt, 12)
		defer engine.Close()
		child, ok := presetChild(ev.Children, lcx.RelationSubagent)
		if !ok {
			t.Fatalf("parent spawned no read-only child: final=%q calls=%v", ev.Final, toolNames(ev.Calls))
		}
		calls := callsOf(ev.Calls, child.SessionID)
		names := toolNames(calls)
		t.Logf("child agent=%s status=%s tools=%v final=%.3000q", child.Agent, child.Status, names, child.Final)
		if child.Agent != "thread-historian" {
			t.Errorf("child ran agent %q, want thread-historian", child.Agent)
		}
		if !hasTool(names, "read_thread", "recall") {
			t.Errorf("thread historian did not read a thread: %v", names)
		}
		if hasTool(names, writeTools...) {
			t.Errorf("thread historian used a mutating tool: %v", names)
		}
		// The fixed prompt says the thread view is the source; before it, the historian searched
		// the repository for thread content (search_text/list_dir) instead of citing fields.
		if hasTool(names, "search_text", "find_files", "list_dir") {
			t.Errorf("thread historian searched the repository instead of the thread view: %v", names)
		}
		if !containsAny(child.Final, "stg-7.internal") || !containsAny(child.Final, "LCX_STAGING_TOKEN") {
			t.Errorf("thread historian did not answer from the thread: %q", child.Final)
		}
		if !strings.Contains(child.Final, threadID) {
			t.Errorf("thread historian did not cite the thread id %s: %q", threadID, child.Final)
		}
	})

	t.Run("worker", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod": "module example.com/clamp\n\ngo 1.22\n",
			"clamp/clamp.go": `package clamp

// Clamp limits v to [lo, hi].
func Clamp(v, lo, hi int) int {
	if v < lo {
		return hi
	}
	if v > hi {
		return lo
	}
	return v
}
`,
			"clamp/clamp_test.go": `package clamp

import "testing"

func TestClamp(t *testing.T) {
	if got := Clamp(-5, 0, 10); got != 0 {
		t.Fatalf("Clamp(-5,0,10)=%d", got)
	}
	if got := Clamp(50, 0, 10); got != 10 {
		t.Fatalf("Clamp(50,0,10)=%d", got)
	}
}
`,
			"README.md": "# clamp\n",
		})
		factory := realModelEnv(t, nil, repo)
		writeWorkspaceConfig(t, repo, presetAgents(t), nil)
		prompt := "Start an execution task with the worker agent. Objective: fix the swapped bounds in clamp/clamp.go so Clamp returns lo when v < lo and hi when v > hi; keep the signature and do not change the test. Read the file first, then verify your edit by reading it back, and note the root cause for the parent. Do not do the work yourself."
		ev, engine := runPresetParent(t, repo, factory, prompt, 12)
		defer engine.Close()
		child, ok := presetChild(ev.Children, lcx.RelationExecution)
		if !ok {
			t.Fatalf("parent started no execution task: final=%q calls=%v", ev.Final, toolNames(ev.Calls))
		}
		calls := callsOf(ev.Calls, child.SessionID)
		names := toolNames(calls)
		t.Logf("child agent=%s status=%s tools=%v final=%.2000q", child.Agent, child.Status, names, child.Final)
		if child.Agent != "worker" {
			t.Errorf("execution child ran agent %q, want worker", child.Agent)
		}
		if child.Status != "completed" {
			t.Errorf("worker child status=%s final=%q", child.Status, child.Final)
		}
		firstWrite := -1
		for i, call := range calls {
			if hasTool([]string{call.Tool}, writeTools...) {
				firstWrite = i
				break
			}
		}
		if firstWrite < 0 {
			t.Fatalf("worker never edited: %v", names)
		}
		inspect := false
		for _, call := range calls[:firstWrite] {
			if hasTool([]string{call.Tool}, readTools...) {
				inspect = true
			}
		}
		if !inspect {
			t.Errorf("worker edited before inspecting: %v", names)
		}
		verified := false
		for _, call := range calls[firstWrite+1:] {
			if hasTool([]string{call.Tool}, "read_file", "git_diff", "git_status", "search_text") {
				verified = true
			}
		}
		if !verified {
			t.Errorf("worker did not verify after editing: %v", names)
		}
		worktree := child.Runtime.Path
		body, err := os.ReadFile(filepath.Join(worktree, "clamp/clamp.go"))
		if err != nil {
			t.Fatal(err)
		}
		ev.Files["clamp/clamp.go"] = string(body)
		if !strings.Contains(string(body), "if v < lo {\n\t\treturn lo") {
			t.Errorf("worker did not fix the lower bound: %s", body)
		}
		if !strings.Contains(string(body), "if v > hi {\n\t\treturn hi") {
			t.Errorf("worker did not fix the upper bound: %s", body)
		}
	})

	t.Run("explore", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod": "module example.com/calc\n\ngo 1.22\n",
			"settings/settings.go": `package settings

import "os"

// Load reads the settings file at path.
func Load(path string) ([]byte, error) { return os.ReadFile(path) }
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
		})
		factory := realModelEnv(t, nil, repo)
		writeWorkspaceConfig(t, repo, presetAgents(t), nil)
		prompt := "Delegate to a read-only child agent: find every call site of settings.Load in this repository and report each as file:line. Wait for the child and repeat its handoff."
		ev, engine := runPresetParent(t, repo, factory, prompt, 12)
		defer engine.Close()
		child, ok := presetChild(ev.Children, lcx.RelationSubagent)
		if !ok {
			t.Fatalf("parent spawned no read-only child: final=%q calls=%v", ev.Final, toolNames(ev.Calls))
		}
		calls := callsOf(ev.Calls, child.SessionID)
		names := toolNames(calls)
		t.Logf("child agent=%s status=%s tools=%v final=%.3000q", child.Agent, child.Status, names, child.Final)
		if child.Agent != "explore" {
			t.Errorf("child ran agent %q, want explore", child.Agent)
		}
		if hasTool(names, writeTools...) {
			t.Errorf("explore used a mutating tool: %v", names)
		}
		if !strings.Contains(child.Final, "Summary") || !strings.Contains(child.Final, "Files") {
			t.Errorf("explore handoff lacks Summary/Files: %q", child.Final)
		}
		if !strings.Contains(child.Final, "settings/settings.go") || !strings.Contains(child.Final, "server/server.go") {
			t.Errorf("explore handoff misses call sites: %q", child.Final)
		}
	})

	t.Run("read-only-ceiling", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod": "module example.com/calc\n\ngo 1.22\n",
			"calc/calc.go": `package calc

// Mul returns a*b.
func Mul(a, b int) int { return a * b }
`,
		})
		factory := realModelEnv(t, nil, repo)
		writeWorkspaceConfig(t, repo, presetAgents(t), nil)
		before, err := os.ReadFile(filepath.Join(repo, "calc/calc.go"))
		if err != nil {
			t.Fatal(err)
		}
		// The host grants full authority; the preset's read-only ceiling must narrow it.
		ev := runPresetTop(t, repo, factory, "reviewer", "full",
			"Change calc/calc.go so Mul returns a + b instead of a * b. Make the edit now.", 8)
		names := toolNames(ev.Calls)
		t.Logf("tools=%v final=%.2000q", names, ev.Final)
		if hasTool(names, writeTools...) {
			t.Errorf("read-only preset used a mutating tool: %v", names)
		}
		after, err := os.ReadFile(filepath.Join(repo, "calc/calc.go"))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Errorf("read-only preset changed the file: %s", after)
		}
		for _, req := range ev.Requests {
			for _, tool := range writeTools {
				for _, offered := range req.Tools {
					if offered == tool {
						t.Errorf("read-only run was offered %s", tool)
					}
				}
			}
		}
		if !containsAny(ev.Final, "cannot", "can't", "not able", "unable", "read-only") {
			t.Errorf("read-only preset did not say it cannot edit: %q", ev.Final)
		}
	})

	t.Run("model-list-fallback", func(t *testing.T) {
		repo := presetRepo(t, map[string]string{
			"go.mod":    "module example.com/fb\n\ngo 1.22\n",
			"README.md": "# fb\n",
		})
		factory := realModelEnv(t, nil, repo)
		t.Setenv("LCX_PRESET_BOGUS_KEY", "unused")
		agents := presetAgents(t)
		agents["fallback-probe"] = map[string]any{
			"description":  "Probe agent: answers with the requested token.",
			"instructions": "Answer with the requested token and nothing else.",
			"models": []any{
				map[string]any{"model": "bogus/b"},
				map[string]any{"model": "local/deepseek-v4.1-flash"},
			},
		}
		writeWorkspaceConfig(t, repo, agents, map[string]any{
			"providers": map[string]any{
				"bogus": map[string]any{
					"package":  "openai-compatible",
					"settings": map[string]any{"baseURL": "http://127.0.0.1:1/v1", "apiKey": "{env:LCX_PRESET_BOGUS_KEY}"},
					"models":   map[string]any{"b": map[string]any{"modelID": "b", "limit": map[string]any{"context": 8192, "output": 512}}},
				},
			},
		})
		ev := runPresetTop(t, repo, factory, "fallback-probe", "read-only",
			"Reply with exactly FALLBACK-OK and nothing else.", 3)
		t.Logf("requests=%+v final=%.200q", ev.Requests, ev.Final)
		sawFirst := false
		for _, req := range ev.Requests {
			if req.ModelRef == "bogus/b" && req.CandidateIndex == 0 {
				sawFirst = true
			}
		}
		if !sawFirst {
			t.Errorf("the first candidate was never tried: %+v", ev.Requests)
		}
		answered := false
		for _, req := range ev.Requests {
			if req.ModelRef == "local/deepseek-v4.1-flash" && req.CandidateIndex == 1 {
				answered = true
			}
		}
		if !answered {
			t.Errorf("the second candidate was never tried: %+v", ev.Requests)
		}
		if !strings.Contains(ev.Final, "FALLBACK-OK") {
			t.Errorf("fallback did not answer: %q", ev.Final)
		}
	})
}
