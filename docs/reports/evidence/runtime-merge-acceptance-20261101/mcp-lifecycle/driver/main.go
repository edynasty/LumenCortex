// Browser (Playwright MCP) lifecycle verification driver for Core wt/core-rt, branch
// rt/execution-runtime. No paid model: an in-process [OI]-compatible SSE server scripts the replies.
// Everything else is real: the Engine, the MCP registry, the playwright-mcp process and its Chromium.
//
// Every mode uses its own Engine, state directory and port, so the long idle check runs beside the
// short ones. Marks are appended to evidence/<mode>.jsonl and printed with the elapsed time.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	lcx "github.com/edynasty/LumenCortex/runtime"
)

const (
	baseRoot  = "/tmp/lcx-mcp"
	evidence  = baseRoot + "/evidence"
	secretKey = "sk-mcp-0123456789abcdefSECRETKEY"
	toolNav   = "mcp__browser__browser_navigate"
	toolWait  = "mcp__browser__browser_wait_for"
)

// ---------------------------------------------------------------- scripted model

type msg struct {
	Role       string          `json:"role"`
	Content    any             `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
}

func (m msg) text() string {
	switch c := m.Content.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		b, _ := json.Marshal(c)
		return string(b)
	}
}

type call struct {
	ID, Name string
	Args     map[string]any
}

type mockReq struct {
	N            int      `json:"n"`
	Tag          string   `json:"tag"`
	ToolNames    int      `json:"toolNames"`
	Names        []string `json:"-"`
	HasBrowser   bool     `json:"hasBrowserTool"`
	LastToolText string   `json:"lastToolResult,omitempty"`
}

type mockServer struct {
	mu   sync.Mutex
	reqs []mockReq
	url  string
	srv  *http.Server
}

func (m *mockServer) last() []mockReq {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockReq(nil), m.reqs...)
}

func decide(msgs []msg, target string) (string, []call, string) {
	last := msgs[len(msgs)-1]
	if last.Role == "tool" {
		return "final", nil, "done: " + truncate(last.text(), 400)
	}
	joined := ""
	for _, m := range msgs {
		if m.Role == "user" {
			joined += m.text() + "\n"
		}
	}
	if strings.Contains(joined, "WAIT") {
		return "wait", []call{{"b1-wait", toolWait, map[string]any{"time": 20}}}, ""
	}
	return "navigate", []call{{"b1-nav", toolNav, map[string]any{"url": target}}}, ""
}

func (m *mockServer) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Messages []msg            `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(raw, &body)
	if len(body.Messages) == 0 {
		http.Error(w, "no messages", 400)
		return
	}
	tag, calls, content := decide(body.Messages, m.url+"/hello")
	entry := mockReq{N: 0, Tag: tag, ToolNames: len(body.Tools)}
	for _, t := range body.Tools {
		if fn, ok := t["function"].(map[string]any); ok {
			name := fmt.Sprint(fn["name"])
			entry.Names = append(entry.Names, name)
			if name == toolNav {
				entry.HasBrowser = true
			}
		}
	}
	if last := body.Messages[len(body.Messages)-1]; last.Role == "tool" {
		entry.LastToolText = truncate(last.text(), 900)
	}
	m.mu.Lock()
	entry.N = len(m.reqs) + 1
	m.reqs = append(m.reqs, entry)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	base := map[string]any{"id": fmt.Sprintf("c%d", entry.N), "object": "chat.completion.chunk", "model": "fake"}
	chunk := func(choices []map[string]any, usage map[string]any) {
		c := map[string]any{}
		for k, v := range base {
			c[k] = v
		}
		c["choices"] = choices
		if usage != nil {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
		var tcs []map[string]any
		for i, c := range calls {
			args, _ := json.Marshal(c.Args)
			tcs = append(tcs, map[string]any{"index": i, "id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": string(args)}})
		}
		chunk([]map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": tcs}, "finish_reason": nil}}, nil)
	} else {
		chunk([]map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}}, nil)
	}
	chunk([]map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, nil)
	chunk([]map[string]any{}, map[string]any{"prompt_tokens": len(raw) / 4, "completion_tokens": 40,
		"total_tokens": len(raw)/4 + 40})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------- processes

type procInfo struct {
	PID, PPID, RSSKB int
	Cmd              string
}

func psAll() []procInfo {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=,command=").Output()
	if err != nil {
		return nil
	}
	var procs []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		rss, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		cmd := strings.Join(f[3:], " ")
		procs = append(procs, procInfo{PID: pid, PPID: ppid, RSSKB: rss, Cmd: cmd})
	}
	return procs
}

func byPID(procs []procInfo) map[int]procInfo {
	m := make(map[int]procInfo, len(procs))
	for _, p := range procs {
		m[p.PID] = p
	}
	return m
}

// descendants returns pid and every process below it, breadth first.
func descendants(procs []procInfo, pid int) []procInfo {
	children := map[int][]procInfo{}
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p)
	}
	var out []procInfo
	queue := []int{pid}
	seen := map[int]bool{pid: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, p := range children[cur] {
			if seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			out = append(out, p)
			queue = append(queue, p.PID)
		}
	}
	return out
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func shortCmd(s string) string {
	if len(s) <= 90 {
		return s
	}
	return s[:90] + "..."
}

func isBrowserProc(cmd string) bool {
	c := strings.ToLower(cmd)
	for _, marker := range []string{"playwright", "chromium", "chrome", "remote-debugging-pipe", "mcp/cli.js"} {
		if strings.Contains(c, marker) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- env

type env struct {
	mode  string
	root  string
	ws    string
	eng   *lcx.Engine
	sid   string
	mock  *mockServer
	jl    *os.File
	base  map[int]bool
	start time.Time
	evMu  sync.Mutex
	evs   []evRec
	notes []map[string]any
}

type evRec struct {
	At   time.Time
	Type string
	Data map[string]any
}

func setup(mode string, port int) (*env, error) {
	root := filepath.Join(baseRoot, mode)
	_ = os.RemoveAll(root)
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, err
	}
	cfgDir := filepath.Join(root, "xdg", "lumencortex")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return nil, err
	}
	cfg := map[string]any{
		"model":             "local/fake",
		"trustedWorkspaces": []string{ws},
		"execution":         map[string]any{"logLevel": "info"},
		"providers": map[string]any{"local": map[string]any{
			"name": "local", "package": "openai-compatible",
			"settings": map[string]any{"baseURL": fmt.Sprintf("http://127.0.0.1:%d/v1", port), "apiKey": "{env:MCP_KEY}"},
			"models": map[string]any{"fake": map[string]any{"modelID": "fake",
				"limit": map[string]any{"context": 200000, "output": 4096}}},
		}},
	}
	cfgJSON, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(cfgDir, "lumencortex.json"), cfgJSON, 0o644); err != nil {
		return nil, err
	}
	for k, v := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(root, "xdg"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CACHE_HOME":  filepath.Join(root, "cache"),
		"MCP_KEY":         secretKey,
		"PATH":            baseRoot + "/bin:" + os.Getenv("PATH"),
	} {
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
	}

	mock := &mockServer{url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	mock.srv = &http.Server{Handler: http.HandlerFunc(mock.serve)}
	go mock.srv.Serve(ln)

	if err := os.MkdirAll(evidence, 0o755); err != nil {
		return nil, err
	}
	jl, err := os.OpenFile(filepath.Join(evidence, mode+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}

	e := &env{mode: mode, root: root, ws: ws, mock: mock, jl: jl, start: time.Now(),
		base: map[int]bool{}}
	for _, p := range psAll() {
		e.base[p.PID] = true
	}

	eng, err := lcx.Open(lcx.Options{Workspace: ws})
	if err != nil {
		return nil, err
	}
	e.eng = eng
	events, stopEvents := eng.Events(8192)
	go func() {
		defer stopEvents()
		for ev := range events {
			rec := evRec{At: ev.At, Type: ev.Type}
			if len(ev.Data) > 0 {
				_ = json.Unmarshal(ev.Data, &rec.Data)
			}
			e.evMu.Lock()
			e.evs = append(e.evs, rec)
			e.evMu.Unlock()
		}
	}()
	handle, err := eng.NewSession(context.Background(), lcx.SessionOptions{Goal: "browser lifecycle check"})
	if err != nil {
		return nil, err
	}
	e.sid = handle.ID
	return e, nil
}

func (e *env) mark(what string, detail map[string]any) {
	rec := map[string]any{
		"t":       time.Now().Format(time.RFC3339Nano),
		"elapsed": fmt.Sprintf("%.1fs", time.Since(e.start).Seconds()),
		"what":    what,
	}
	for k, v := range detail {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	fmt.Fprintln(e.jl, string(b))
	_ = e.jl.Sync()
	e.notes = append(e.notes, rec)
	keys := make([]string, 0, len(detail))
	for k := range detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, detail[k]))
	}
	fmt.Printf("[%6.1fs] %-26s %s\n", time.Since(e.start).Seconds(), what, strings.Join(parts, " "))
}

// sample records the process tree below pid, its total RSS, and every orphan candidate.
func (e *env) sample(label string, pid int) map[string]any {
	procs := psAll()
	tree := descendants(procs, pid)
	rss := 0
	var parts []string
	for _, p := range tree {
		rss += p.RSSKB
		parts = append(parts, fmt.Sprintf("%d(rss=%dKB)", p.PID, p.RSSKB))
	}
	self := byPID(procs)[pid]
	rss += self.RSSKB
	var orphans []string
	for _, p := range procs {
		if e.base[p.PID] || p.PPID != 1 || !isBrowserProc(p.Cmd) {
			continue
		}
		orphans = append(orphans, fmt.Sprintf("%d:%s", p.PID, shortCmd(p.Cmd)))
	}
	sort.Strings(orphans)
	detail := map[string]any{
		"label":             label,
		"pid":               pid,
		"pidAlive":          alive(pid),
		"descendants":       len(tree),
		"tree":              parts,
		"totalRSSKB":        rss,
		"orphanCount":       len(orphans),
		"orphans":           orphans,
		"browserProcsAlive": countBrowserProcs(procs, pid, e.base),
	}
	e.mark("sample:"+label, detail)
	return detail
}

// countBrowserProcs counts the browser processes that belong to this run: the server pid, its
// descendants, and any process reparented away from it.
func countBrowserProcs(procs []procInfo, pid int, baseline map[int]bool) int {
	n := 0
	if alive(pid) {
		n++
	}
	n += len(descendants(procs, pid))
	for _, p := range procs {
		if baseline[p.PID] || p.PPID != 1 || !isBrowserProc(p.Cmd) {
			continue
		}
		n++
	}
	return n
}

// survivors reports every process this run left behind as a root reparented to init, with the subtree
// each root still owns and their total RSS.
func (e *env) survivors() map[string]any {
	procs := psAll()
	byP := byPID(procs)
	var roots []int
	for _, p := range procs {
		if e.base[p.PID] || p.PPID != 1 || !isBrowserProc(p.Cmd) {
			continue
		}
		roots = append(roots, p.PID)
	}
	sort.Ints(roots)
	var pids []string
	rss := 0
	seen := map[int]bool{}
	for _, root := range roots {
		group := append([]procInfo{byP[root]}, descendants(procs, root)...)
		for _, p := range group {
			if seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			rss += p.RSSKB
			pids = append(pids, fmt.Sprintf("%d:%s", p.PID, shortCmd(p.Cmd)))
		}
	}
	return map[string]any{"survivorRoots": roots, "survivorCount": len(pids),
		"survivorRSSKB": rss, "survivors": pids}
}

func (e *env) orphans() []string {
	procs := psAll()
	var out []string
	for _, p := range procs {
		if e.base[p.PID] || p.PPID != 1 || !isBrowserProc(p.Cmd) {
			continue
		}
		out = append(out, fmt.Sprintf("%d:%s", p.PID, shortCmd(p.Cmd)))
	}
	sort.Strings(out)
	return out
}

// events returns the recorded events of one type, newest last.
func (e *env) events(typ string) []evRec {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	var out []evRec
	for _, ev := range e.evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// waitEvent waits until an event of typ whose data matches want arrives, or the timeout passes.
func (e *env) waitEvent(typ string, want func(map[string]any) bool, timeout time.Duration) (evRec, bool) {
	deadline := time.Now().Add(timeout)
	seen := 0
	for time.Now().Before(deadline) {
		evs := e.events(typ)
		for ; seen < len(evs); seen++ {
			if want == nil || want(evs[seen].Data) {
				return evs[seen], true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return evRec{}, false
}

func (e *env) runOnce(input string, ctx context.Context) lcx.AgentResult {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	res, err := e.eng.RunAgent(ctx, e.sid, lcx.AgentOptions{Input: input, Policy: "full", MaxSteps: 8})
	detail := map[string]any{
		"status":  res.Status,
		"final":   truncate(res.Final, 300),
		"seconds": fmt.Sprintf("%.1f", time.Since(start).Seconds()),
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	reqs := e.mock.last()
	if len(reqs) > 0 {
		detail["lastTag"] = reqs[len(reqs)-1].Tag
		detail["lastToolResult"] = truncate(reqs[len(reqs)-1].LastToolText, 200)
		var mcpTools []string
		for _, name := range reqs[len(reqs)-1].Names {
			if strings.HasPrefix(name, "mcp__") {
				mcpTools = append(mcpTools, name)
			}
		}
		detail["mcpToolsOffered"] = mcpTools
		detail["toolsOffered"] = reqs[len(reqs)-1].ToolNames
	}
	e.mark("run.end", detail)
	return res
}

func (e *env) statuses() []lcx.MCPStatus {
	out, err := e.eng.MCPStatuses(context.Background(), e.sid)
	if err != nil {
		e.mark("status.error", map[string]any{"error": err.Error()})
		return nil
	}
	return out
}

func (e *env) pid() int {
	for _, st := range e.statuses() {
		if st.ID == "browser" {
			return st.PID
		}
	}
	return 0
}

func (e *env) closeEngine() {
	e.mark("engine.close.begin", nil)
	if err := e.eng.Close(); err != nil {
		e.mark("engine.close.error", map[string]any{"error": err.Error()})
	}
	e.mark("engine.close.done", nil)
	_ = e.mock.srv.Close()
}

// applyAndEnable applies the shipped template and turns the server on, exactly as a host does.
func (e *env) applyAndEnable() {
	ctx := context.Background()
	cfg, err := e.eng.ApplyMCPTemplate(ctx, "global", "browser")
	if err != nil {
		e.mark("template.error", map[string]any{"error": err.Error()})
		panic(err)
	}
	raw, _ := json.Marshal(cfg)
	e.mark("template.applied", map[string]any{"config": string(raw)})
	if err := e.eng.SetMCPServerEnabled(ctx, "global", "browser", true); err != nil {
		e.mark("enable.error", map[string]any{"error": err.Error()})
		panic(err)
	}
	for _, c := range e.eng.MCPConfigs() {
		if c.ID == "browser" {
			raw, _ := json.Marshal(c)
			e.mark("config.effective", map[string]any{"config": string(raw)})
		}
	}
}

// ---------------------------------------------------------------- modes

func modeIdle() {
	e, err := setup("idle", 18951)
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	defer e.jl.Close()
	e.applyAndEnable()
	e.runOnce("NAVIGATE the local page", nil)
	pid := e.pid()
	e.mark("server.pid", map[string]any{"pid": pid})
	e.sample("after-run", pid)

	lastUse := time.Now()
	stopSampling := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-t.C:
				e.sample(fmt.Sprintf("t+%.0fs", time.Since(lastUse).Seconds()), pid)
			}
		}
	}()

	ev, ok := e.waitEvent("mcp.stopped", func(d map[string]any) bool {
		return fmt.Sprint(d["serverId"]) == "browser"
	}, 700*time.Second)
	close(stopSampling)
	elapsed := time.Since(lastUse).Seconds()
	if !ok {
		e.mark("idle.timeout", map[string]any{"waitedSeconds": fmt.Sprintf("%.1f", elapsed)})
	} else {
		e.mark("mcp.stopped", map[string]any{"reason": fmt.Sprint(ev.Data["reason"]),
			"afterLastUseSeconds": fmt.Sprintf("%.1f", elapsed), "at": ev.At.Format(time.RFC3339Nano)})
	}
	time.Sleep(2 * time.Second)
	e.sample("after-idle-stop", pid)
	e.mark("orphans.after-idle", map[string]any{"orphans": e.orphans(), "pidAlive": alive(pid)})
	e.closeEngine()
	time.Sleep(2 * time.Second)
	e.mark("orphans.after-close", map[string]any{"orphans": e.orphans(), "pidAlive": alive(pid)})
}

func modeRestart() {
	e, err := setup("restart", 18952)
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	defer e.jl.Close()
	e.applyAndEnable()
	ctx := context.Background()

	// Start through the public API, then drive a real call so Chromium exists too.
	st, err := e.eng.MCPStart(ctx, e.sid, "browser")
	if err != nil {
		e.mark("mcpstart.error", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	pid1 := st.PID
	e.mark("mcp.start#1", map[string]any{"pid": pid1, "tools": st.Tools})
	e.runOnce("NAVIGATE the local page", nil)
	first := e.sample("running#1", pid1)
	chromeBefore := first["descendants"]

	// Disable: the running server must be stopped and its Chromium closed.
	if err := e.eng.SetMCPServerEnabled(ctx, "global", "browser", false); err != nil {
		e.mark("disable.error", map[string]any{"error": err.Error()})
	}
	time.Sleep(2 * time.Second)
	e.mark("after-disable", map[string]any{"oldPidAlive": alive(pid1), "orphans": e.orphans(),
		"treeBefore": chromeBefore, "statuses": len(e.statuses())})

	if err := e.eng.SetMCPServerEnabled(ctx, "global", "browser", true); err != nil {
		e.mark("enable.error", map[string]any{"error": err.Error()})
	}
	st2, err := e.eng.MCPStart(ctx, e.sid, "browser")
	if err != nil {
		e.mark("mcpstart#2.error", map[string]any{"error": err.Error()})
	} else {
		e.mark("mcp.start#2", map[string]any{"pid": st2.PID, "oldPid": pid1,
			"newPid": st2.PID != pid1, "tools": st2.Tools})
		e.runOnce("NAVIGATE the local page", nil)
		e.sample("running#2", st2.PID)
	}
	e.closeEngine()
	time.Sleep(2 * time.Second)
	e.mark("orphans.after-close", map[string]any{"orphans": e.orphans(),
		"pid1Alive": alive(pid1), "pid2Alive": alive(st2.PID)})
}

func modeCrash() {
	e, err := setup("crash", 18953)
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	defer e.jl.Close()
	e.applyAndEnable()
	e.runOnce("NAVIGATE the local page", nil)
	pid := e.pid()
	pre := e.sample("before-kill", pid)

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		e.mark("kill.error", map[string]any{"error": err.Error()})
	}
	e.mark("kill", map[string]any{"pid": pid, "signal": "SIGKILL", "treeBefore": pre["tree"]})
	time.Sleep(5 * time.Second)
	e.mark("orphans@5s", map[string]any{"orphans": e.orphans(), "pidAlive": alive(pid)})
	e.mark("survivors@5s", e.survivors())
	e.sample("t+5s", pid)
	time.Sleep(25 * time.Second)
	e.mark("orphans@30s", map[string]any{"orphans": e.orphans(), "pidAlive": alive(pid)})
	e.mark("survivors@30s", e.survivors())
	e.sample("t+30s", pid)

	// The next call must be a tool-result error, and the call after it must restart the server.
	e.runOnce("NAVIGATE the local page", nil)
	e.mark("after-failed-call", map[string]any{"pidAlive": alive(pid), "statuses": len(e.statuses())})
	e.runOnce("NAVIGATE the local page", nil)
	pid2 := e.pid()
	e.mark("restarted", map[string]any{"oldPid": pid, "newPid": pid2, "isNew": pid2 != pid && pid2 != 0})
	e.mark("survivors@restart", e.survivors())
	e.sample("after-restart", pid2)
	e.closeEngine()
	time.Sleep(2 * time.Second)
	e.mark("orphans.after-close", map[string]any{"orphans": e.orphans(), "pid2Alive": alive(pid2)})
}

func modeCancel() {
	e, err := setup("cancel", 18954)
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	defer e.jl.Close()
	e.applyAndEnable()
	e.runOnce("NAVIGATE the local page", nil)
	pid := e.pid()
	e.sample("before-cancel", pid)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan lcx.AgentResult, 1)
	go func() { done <- e.runOnce("WAIT in the browser for a while", ctx) }()

	// Cancel once a browser call is really in flight.
	inFlight := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, st := range e.statuses() {
			if st.ID == "browser" && st.CallsInFlight > 0 {
				inFlight = true
			}
		}
		if inFlight {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.mark("cancel.inflight", map[string]any{"callsInFlight": inFlight})
	cancel()
	res := <-done
	e.mark("run.cancelled", map[string]any{"status": res.Status, "final": truncate(res.Final, 200)})
	time.Sleep(3 * time.Second)
	e.mark("after-cancel", map[string]any{"pidAlive": alive(pid), "orphans": e.orphans()})
	e.sample("after-cancel", pid)

	e.closeEngine()
	time.Sleep(2 * time.Second)
	e.mark("after-close", map[string]any{"pidAlive": alive(pid), "orphans": e.orphans()})
	e.sample("after-close", pid)
}

func modeClose() {
	e, err := setup("close", 18955)
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	defer e.jl.Close()
	e.applyAndEnable()
	e.runOnce("NAVIGATE the local page", nil)
	pid := e.pid()
	e.sample("before-close", pid)
	e.closeEngine()
	time.Sleep(2 * time.Second)
	e.mark("after-close", map[string]any{"pidAlive": alive(pid), "orphans": e.orphans()})
	e.sample("after-close", pid)
}

// modePolicy checks the policy gate and the Computer Use registry view. No CodeBridge is contacted.
func modePolicy() {
	e, err := setup("policy", 18956)
	if err != nil {
		fmt.Println("setup:", err)
		os.Exit(1)
	}
	defer e.jl.Close()
	e.applyAndEnable()
	e.runOnce("NAVIGATE the local page", nil)
	res, runErr := e.eng.RunAgent(context.Background(), e.sid, lcx.AgentOptions{
		Input: "NAVIGATE the local page", Policy: "read-only", MaxSteps: 4})
	detail := map[string]any{"status": res.Status, "final": truncate(res.Final, 160)}
	if runErr != nil {
		detail["error"] = runErr.Error()
	}
	reqs := e.mock.last()
	if len(reqs) > 0 {
		var mcpTools []string
		for _, name := range reqs[len(reqs)-1].Names {
			if strings.HasPrefix(name, "mcp__") {
				mcpTools = append(mcpTools, name)
			}
		}
		detail["mcpToolsOffered"] = mcpTools
		detail["toolsOffered"] = reqs[len(reqs)-1].ToolNames
		detail["lastToolResult"] = truncate(reqs[len(reqs)-1].LastToolText, 300)
	}
	e.mark("readonly.run.end", detail)

	cfg, err := e.eng.ApplyMCPTemplate(context.Background(), "global", "computer-use")
	if err != nil {
		e.mark("computer-use.template.error", map[string]any{"error": err.Error()})
	} else {
		raw, _ := json.Marshal(cfg)
		e.mark("computer-use.template.applied", map[string]any{"config": string(raw)})
	}
	e.dumpComputerUse("disabled")
	if err := e.eng.SetMCPServerEnabled(context.Background(), "global", "computer-use", true); err != nil {
		e.mark("computer-use.enable.error", map[string]any{"error": err.Error()})
	}
	e.dumpComputerUse("enabled")
	tools, err := e.eng.MCPTools(context.Background(), e.sid)
	if err != nil {
		e.mark("computer-use.mcptools.error", map[string]any{"error": err.Error()})
	}
	e.mark("computer-use.mcptools", map[string]any{"count": len(tools)})
	e.closeEngine()
	time.Sleep(2 * time.Second)
	e.mark("orphans.after-close", map[string]any{"orphans": e.orphans()})
}

func (e *env) dumpComputerUse(label string) {
	caps, err := e.eng.Capabilities(context.Background(), lcx.CapabilityQuery{
		SessionID: e.sid, Capability: "computer.control"})
	if err != nil {
		e.mark("capabilities.error", map[string]any{"error": err.Error()})
		return
	}
	var rows []string
	for _, c := range caps {
		rows = append(rows, fmt.Sprintf("%s permission=%s enabled=%v reason=%s state=%s missingEndpoint=%q",
			c.ToolID, c.Permission, c.Enabled, c.DisabledReason, c.Availability.State, c.ConfigRequirements.MissingEndpoint))
	}
	e.mark("capabilities:"+label, map[string]any{"count": len(rows), "rows": rows})
}

func main() {
	mode := flag.String("mode", "idle", "idle|restart|crash|cancel|close")
	flag.Parse()
	switch *mode {
	case "idle":
		modeIdle()
	case "restart":
		modeRestart()
	case "crash":
		modeCrash()
	case "cancel":
		modeCancel()
	case "close":
		modeClose()
	case "policy":
		modePolicy()
	default:
		fmt.Println("unknown mode", *mode)
		os.Exit(2)
	}
}
