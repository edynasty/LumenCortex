package runtime

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S4 against the real Engine, toolset, SQLite and a stdio MCP server: every tier is exercised by a
// real tool call, and the stored execution rows and edges are read back by SQL.

const (
	rtMCPEnv     = "LCX_RT_MCP_HELPER"
	rtMCPCounter = "LCX_RT_MCP_COUNTER" // file the exec tool "act" appends one byte to per call
	rtMCPHang    = "LCX_RT_MCP_HANG"    // "1": act appends, prints "acting" to the counter's .acting file, and never answers
)

// TestHelperRuntimeMCPServer is a stdio MCP server: "echo" is annotated read-only, "act" is not, so the
// registry exposes it as exec.
func TestHelperRuntimeMCPServer(t *testing.T) {
	if os.Getenv(rtMCPEnv) != "1" {
		t.Skip("helper process only")
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	write := func(v any) {
		raw, _ := json.Marshal(v)
		_, _ = out.Write(append(raw, '\n'))
		_ = out.Flush()
	}
	for in.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		var id any
		_ = json.Unmarshal(msg.ID, &id)
		reply := func(result any) { write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "rt", "version": "1"}})
		case "tools/list":
			schema := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}
			reply(map[string]any{"tools": []map[string]any{
				{"name": "echo", "description": "Echo text", "inputSchema": schema, "annotations": map[string]any{"readOnlyHint": true}},
				{"name": "act", "description": "Act once", "inputSchema": schema},
			}})
		case "tools/call":
			name, _ := msg.Params["name"].(string)
			if name == "act" {
				counter := os.Getenv(rtMCPCounter)
				f, err := os.OpenFile(counter, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err == nil {
					_, _ = f.WriteString("x")
					_ = f.Close()
				}
				if os.Getenv(rtMCPHang) == "1" {
					_ = os.WriteFile(counter+".acting", []byte("acting"), 0o600)
					time.Sleep(5 * time.Minute)
				}
			}
			reply(map[string]any{"content": []map[string]any{{"type": "text", "text": name + " done"}}})
		default:
			reply(map[string]any{})
		}
	}
}

// useRuntimeMCP points the project mcp.json at the helper server and trusts the workspace.
func useRuntimeMCP(t *testing.T, workspace, counter string) {
	t.Helper()
	t.Setenv(rtMCPEnv, "1")
	t.Setenv(rtMCPCounter, counter)
	writeEngineMCP(t, workspace, map[string]any{
		"srv": map[string]any{"command": os.Args[0], "args": []string{"-test.run=^TestHelperRuntimeMCPServer$", "-test.count=1"}},
	})
	useGlobalConfig(t, mcpGlobalConfig(workspace))
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

type execRow struct {
	ID, Tool, Request, Outcome, Args string
	Pre, Post, Coverage, RunID       sql.NullString
	PreUnavailable, Lineage          int
}

type edgeRow struct {
	Target, SourceKind, Source, Origin string
	Lineage                            int
}

func readProvenance(t *testing.T, engine *Engine, sessionID string) ([]execRow, []edgeRow) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT execution_id, tool_name, source_ref, outcome, args_digest, pre_ref, post_ref, coverage, run_id, pre_unavailable, lineage_complete
		FROM derived_origins WHERE session_id = ? AND kind = 'execution' ORDER BY id`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var execs []execRow
	for rows.Next() {
		var r execRow
		if err := rows.Scan(&r.ID, &r.Tool, &r.Request, &r.Outcome, &r.Args, &r.Pre, &r.Post, &r.Coverage, &r.RunID, &r.PreUnavailable, &r.Lineage); err != nil {
			t.Fatal(err)
		}
		execs = append(execs, r)
	}
	rows.Close()
	rows, err = db.Query(`SELECT target_ref, source_kind, source_ref, origin_class, lineage_complete
		FROM derived_origins WHERE session_id = ? AND kind = 'file' ORDER BY id`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var edges []edgeRow
	for rows.Next() {
		var r edgeRow
		if err := rows.Scan(&r.Target, &r.SourceKind, &r.Source, &r.Origin, &r.Lineage); err != nil {
			t.Fatal(err)
		}
		edges = append(edges, r)
	}
	return execs, edges
}

func gitBlob(t *testing.T, dir, path string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "hash-object", "--", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

type stepProvider struct {
	mu    sync.Mutex
	steps []protocol.ProviderResponse
}

func (p *stepProvider) Model() string { return "step-test" }
func (p *stepProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.steps) == 0 {
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
	}
	r := p.steps[0]
	p.steps = p.steps[1:]
	return r, nil
}

// Every tier through the real Engine: the file tool, shell and the exec MCP tool are admitted with a
// pre-state, the read MCP tool is admitted without one, read_file is not admitted at all. Each
// admitted call commits once, completed and complete, and the edges name the blobs git computes for
// the files the calls wrote, from the request that issued them.
func TestRealEngineRecordsEveryTier(t *testing.T) {
	workspace := t.TempDir()
	gitInit(t, workspace)
	useRuntimeMCP(t, workspace, filepath.Join(workspace, "mcp.log"))
	engine := openEngineAt(t, workspace)
	if _, err := engine.MCPStart(context.Background(), "", "srv"); err != nil {
		t.Fatal(err)
	}
	handle := newTestSession(t, engine, "write things")
	provider := &stepProvider{steps: []protocol.ProviderResponse{
		toolCalls(tc("w", "write_file", `{"path":"a.txt","content":"alpha\n"}`), tc("r", "read_file", `{"path":"a.txt"}`), tc("s", "shell", `{"command":"printf 'x' >> log.txt"}`)),
		toolCalls(tc("e", "mcp__srv__echo", `{"text":"hi"}`), tc("m", "mcp__srv__act", `{"text":"go"}`)),
	}}
	result, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 5})
	if err != nil || result.Status != "completed" {
		t.Fatalf("run = %+v err=%v", result, err)
	}
	envs := envelopeRows(t, engine, handle.ID)
	execs, edges := readProvenance(t, engine, handle.ID)
	tools := make([]string, len(execs))
	for i, x := range execs {
		tools[i] = x.Tool
	}
	if fmt.Sprint(tools) != "[write_file shell mcp__srv__echo mcp__srv__act]" {
		t.Fatalf("executions = %v, want write_file, shell and both MCP tools; read_file is not admitted", tools)
	}
	wantRequest := []string{envs[0].ID, envs[0].ID, envs[1].ID, envs[1].ID}
	for i, x := range execs {
		if x.Outcome != "completed" || x.Coverage.String != "complete" || x.Lineage != 1 || x.Request != wantRequest[i] || x.RunID.String != envs[0].RunID || !strings.HasPrefix(x.Args, "sha256:") {
			t.Errorf("execution %s = %+v", x.Tool, x)
		}
	}
	if x := execs[0]; !strings.HasPrefix(x.Pre.String, "files:1:sha256:") || !strings.HasPrefix(x.Post.String, "files:1:sha256:") || x.Pre.String == x.Post.String {
		t.Errorf("write_file pre/post = %q %q", x.Pre.String, x.Post.String)
	}
	for _, x := range []execRow{execs[1], execs[3]} {
		if !strings.HasPrefix(x.Pre.String, "ws:") || !strings.HasPrefix(x.Post.String, "ws:") || x.PreUnavailable != 0 {
			t.Errorf("%s pre/post = %q %q", x.Tool, x.Pre.String, x.Post.String)
		}
	}
	if x := execs[2]; x.Pre.Valid || x.Post.Valid || x.PreUnavailable != 0 {
		t.Errorf("read MCP = %+v, want admission only", x)
	}

	var got []string
	for _, e := range edges {
		if e.SourceKind != "request" || e.Origin != "agent_generated" || e.Lineage != 1 {
			t.Errorf("edge %+v", e)
		}
		got = append(got, e.Target+" <- "+e.Source)
	}
	want := []string{
		"a.txt@" + gitBlob(t, workspace, "a.txt") + " <- " + envs[0].ID,
		"log.txt@" + gitBlob(t, workspace, "log.txt") + " <- " + envs[0].ID,
		"mcp.log@" + gitBlob(t, workspace, "mcp.log") + " <- " + envs[1].ID,
	}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("edges:\n got %v\nwant %v", got, want)
	}
}

// A workspace that is not a git work tree still runs shell; its execution records the pre-state as
// unavailable and nothing it commits is lineage-complete.
func TestRealEngineShellOutsideGitIsUnavailable(t *testing.T) {
	workspace := t.TempDir()
	engine := openEngineAt(t, workspace)
	handle := newTestSession(t, engine, "shell")
	provider := &stepProvider{steps: []protocol.ProviderResponse{toolCalls(tc("s", "shell", `{"command":"printf 'x' > out.txt"}`))}}
	if result, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil || result.Status != "completed" {
		t.Fatalf("run = %+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); err != nil {
		t.Fatalf("shell did not run: %v", err)
	}
	execs, edges := readProvenance(t, engine, handle.ID)
	if len(execs) != 1 || execs[0].PreUnavailable != 1 || execs[0].Pre.Valid || execs[0].Coverage.String != "unavailable" || execs[0].Lineage != 0 || len(edges) != 0 {
		t.Fatalf("executions=%+v edges=%+v", execs, edges)
	}
}

// A shell call that changes more paths than one commit may hold records the first MaxEdgesPerCommit
// edges, marks the coverage partial, and leaves every edge lineage-incomplete: a cut list is not the
// whole derivation.
func TestRealEngineShellEdgeCapIsPartial(t *testing.T) {
	workspace := t.TempDir()
	gitInit(t, workspace)
	engine := openEngineAt(t, workspace)
	handle := newTestSession(t, engine, "many files")
	provider := &stepProvider{steps: []protocol.ProviderResponse{toolCalls(tc("s", "shell", `{"command":"for i in $(seq 1 70); do printf x > f$i.txt; done"}`))}}
	if result, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil || result.Status != "completed" {
		t.Fatalf("run = %+v err=%v", result, err)
	}
	execs, edges := readProvenance(t, engine, handle.ID)
	if len(execs) != 1 || execs[0].Outcome != "completed" || execs[0].Coverage.String != "partial" || execs[0].Lineage != 0 {
		t.Fatalf("execution = %+v", execs)
	}
	if len(edges) != 64 {
		t.Fatalf("edges = %d, want the cap of 64", len(edges))
	}
	for _, e := range edges {
		if e.Lineage != 0 {
			t.Fatalf("edge %+v is lineage-complete under a cut list", e)
		}
	}
}

// Snapshots run concurrently with git commands in the same work tree without ever leaving an
// index.lock behind or failing them: a writer loop of `git add` / `git status` races the snapshots.
func TestSnapshotsRaceGitWithoutIndexLock(t *testing.T) {
	workspace := t.TempDir()
	gitInit(t, workspace)
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := repository.New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var gitErrs []string
	var mu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(workspace, fmt.Sprintf("f%02d.txt", i%50)), []byte(fmt.Sprint(i)), 0o644)
			if out, err := exec.Command("git", "-C", workspace, "add", "-A").CombinedOutput(); err != nil {
				mu.Lock()
				gitErrs = append(gitErrs, string(out))
				mu.Unlock()
			}
		}
	}()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if snap := svc.SnapshotWorkspace(ctx); !snap.OK {
			t.Errorf("snapshot %d failed", i)
		}
		if _, err := svc.SnapshotFiles(ctx, []string{"f01.txt", "f02.txt"}); err != nil {
			t.Error(err)
		}
	}
	close(stop)
	wg.Wait()
	if len(gitErrs) > 0 {
		t.Fatalf("%d git add failures while snapshots ran, first: %s", len(gitErrs), gitErrs[0])
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("index.lock left behind: %v", err)
	}
}

// The shell snapshot on a 10k-file work tree (half tracked and dirty, half untracked): reports p50/p95
// of one pre+post pair and fails only past the plan's 2s stop line.
func TestShellSnapshotLatencyOn10kFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("10k-file fixture")
	}
	workspace := t.TempDir()
	gitInit(t, workspace)
	for d := 0; d < 100; d++ {
		dir := filepath.Join(workspace, fmt.Sprintf("d%03d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 100; f++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", f)), []byte(fmt.Sprintf("%d/%d\n", d, f)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Track and commit the first half (d000-d049); the shell expands the globs.
	commit := exec.Command("sh", "-c", "git add d00* d01* d02* d03* d04* && git -c user.email=t@t -c user.name=t commit -q -m half")
	commit.Dir = workspace
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit fixture: %v %s", err, out)
	}
	// Dirty 1000 tracked files so the status lists 6000 paths.
	for d := 0; d < 10; d++ {
		for f := 0; f < 100; f++ {
			if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("d%03d/f%03d.txt", d, f)), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	svc, err := repository.New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var samples []time.Duration
	for i := 0; i < 21; i++ {
		start := time.Now()
		pre := svc.SnapshotWorkspace(ctx)
		post := svc.SnapshotWorkspace(ctx)
		elapsed := time.Since(start)
		if !pre.OK || !post.OK {
			t.Fatal("snapshot failed")
		}
		if i > 0 { // the first pair warms the file system cache
			samples = append(samples, elapsed)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50, p95 := samples[len(samples)/2], samples[len(samples)*95/100]
	t.Logf("shell snapshot pre+post on 10k files (6000 status entries): p50=%s p95=%s n=%d", p50, p95, len(samples))
	if p95 > 2*time.Second {
		t.Fatalf("p95 %s exceeds the 2s stop line", p95)
	}
}
