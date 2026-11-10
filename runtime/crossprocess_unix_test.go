//go:build unix

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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S7 item 3: real OS processes, each with its own Engine on one state database. Two workers run
// different sessions at once while a third process only reads; a held write transaction makes the next
// write wait out busy_timeout and end the run as a storage failure (plan §3.1 rule 5); a session fence
// committed by another process is linearized with admission.

const (
	xpRoleEnv    = "LCX_XP_ROLE" // worker | poller | busy
	xpWSEnv      = "LCX_XP_WS"
	xpSessionEnv = "LCX_XP_SESSION"
	xpStepsEnv   = "LCX_XP_STEPS"
	xpTagEnv     = "LCX_XP_TAG"
	xpSigEnv     = "LCX_XP_SIG"  // busy: the file the shell creates when it starts
	xpStopEnv    = "LCX_XP_STOP" // poller: stop when this file exists
	xpPauseEnv   = "LCX_XP_PAUSE"
)

// xpResult is what a helper process reports on its last stdout line, after "XP ".
type xpResult struct {
	Status string  `json:"status"`
	Err    string  `json:"err,omitempty"`
	Calls  int     `json:"calls"`
	Reads  int     `json:"reads,omitempty"`
	Errors int     `json:"errors,omitempty"`
	First  string  `json:"first,omitempty"`
	CallAt []int64 `json:"callAt,omitempty"`
}

// xpProvider runs a fixed script: write_file, shell, read_file in turn for steps calls, then a final
// answer. Each call can pause, so another process can act while the run is in flight.
type xpProvider struct {
	mu     sync.Mutex
	tag    string
	steps  int
	pause  time.Duration
	calls  int
	callAt []int64
	script func(n int) protocol.ProviderResponse
}

func (p *xpProvider) Model() string { return "xp" }

func (p *xpProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.callAt = append(p.callAt, time.Now().UnixNano())
	if p.pause > 0 {
		time.Sleep(p.pause)
	}
	if p.script != nil {
		return p.script(p.calls), nil
	}
	n := p.calls
	if n > p.steps {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done " + p.tag}, FinishReason: "stop"}, nil
	}
	var call protocol.ToolCall
	switch n % 3 {
	case 1:
		call = protocol.ToolCall{ID: fmt.Sprintf("w%d", n), Name: "write_file", Arguments: json.RawMessage(fmt.Sprintf(`{"path":"%s-%d.txt","content":"%s %d\n"}`, p.tag, n, p.tag, n))}
	case 2:
		call = protocol.ToolCall{ID: fmt.Sprintf("s%d", n), Name: "bash", Arguments: json.RawMessage(fmt.Sprintf(`{"command":"printf x >> %s.log"}`, p.tag))}
	default:
		call = protocol.ToolCall{ID: fmt.Sprintf("r%d", n), Name: "read_file", Arguments: json.RawMessage(fmt.Sprintf(`{"path":"%s-%d.txt"}`, p.tag, n-2))}
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{call}}, FinishReason: "tool_calls"}, nil
}

func xpReport(r xpResult) {
	raw, _ := json.Marshal(r)
	os.Stdout.WriteString("XP " + string(raw) + "\n")
}

// TestHelperCrossProcess is the child process of the cross-process tests.
func TestHelperCrossProcess(t *testing.T) {
	role := os.Getenv(xpRoleEnv)
	if role == "" {
		t.Skip("helper process only")
	}
	engine, err := Open(Options{Workspace: os.Getenv(xpWSEnv)})
	if err != nil {
		xpReport(xpResult{Status: "open_failed", Err: err.Error()})
		return
	}
	defer engine.Close()
	ctx := context.Background()
	id := os.Getenv(xpSessionEnv)
	switch role {
	case "worker", "busy":
		steps, _ := strconv.Atoi(os.Getenv(xpStepsEnv))
		pause, _ := time.ParseDuration(os.Getenv(xpPauseEnv) + "ms")
		p := &xpProvider{tag: os.Getenv(xpTagEnv), steps: steps, pause: pause}
		if role == "busy" {
			sig := os.Getenv(xpSigEnv)
			p.script = func(n int) protocol.ProviderResponse {
				if n > 1 {
					return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "must not be asked"}, FinishReason: "stop"}
				}
				return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
					{ID: "s1", Name: "bash", Arguments: json.RawMessage(`{"command":"printf x >> count.txt && touch ` + sig + ` && sleep 2"}`)},
					{ID: "w1", Name: "write_file", Arguments: json.RawMessage(`{"path":"after-busy.txt","content":"must not exist\n"}`)},
				}}, FinishReason: "tool_calls"}
			}
		}
		result, err := engine.runAgentWith(ctx, id, p, AgentOptions{Policy: "full", MaxSteps: steps + 2})
		r := xpResult{Status: result.Status, Calls: p.calls, CallAt: p.callAt}
		if err != nil {
			r.Err = err.Error()
		}
		xpReport(r)
	case "poller":
		stop := os.Getenv(xpStopEnv)
		ids := strings.Split(id, ",")
		r := xpResult{Status: "done"}
		for {
			if _, err := os.Stat(stop); err == nil {
				break
			}
			for _, sid := range ids {
				var errs []error
				if _, _, err := engine.Session(ctx, sid); err != nil {
					errs = append(errs, err)
				}
				if _, err := engine.TaskState(ctx, sid); err != nil {
					errs = append(errs, err)
				}
				if err := engine.store.EachMessageFrom(ctx, sid, 0, func(session.Message) error { return nil }); err != nil {
					errs = append(errs, err)
				}
				r.Reads += 3
				for _, err := range errs {
					r.Errors++
					if r.First == "" {
						r.First = err.Error()
					}
				}
			}
			if _, err := engine.ListSessions(ctx, 50, 0); err != nil {
				r.Errors++
				if r.First == "" {
					r.First = err.Error()
				}
			}
			r.Reads++
		}
		xpReport(r)
	}
}

// xpStart starts a helper process; wait collects its XP line.
type xpProc struct {
	cmd  *exec.Cmd
	line chan string
}

func xpStart(t *testing.T, env ...string) *xpProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrossProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	p := &xpProc{cmd: cmd, line: make(chan string, 1)}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		last := ""
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "XP ") {
				last = strings.TrimPrefix(sc.Text(), "XP ")
			}
		}
		p.line <- last
	}()
	return p
}

func (p *xpProc) wait(t *testing.T, timeout time.Duration) xpResult {
	t.Helper()
	select {
	case line := <-p.line:
		_ = p.cmd.Wait()
		var r xpResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("helper said %q (%v)", line, err)
		}
		return r
	case <-time.After(timeout):
		t.Fatalf("helper %v did not finish in %s", p.cmd.Args, timeout)
	}
	return xpResult{}
}

func xpEnv(role, workspace, sessionID, tag string, steps int) []string {
	return []string{xpRoleEnv + "=" + role, xpWSEnv + "=" + workspace, xpSessionEnv + "=" + sessionID, xpTagEnv + "=" + tag, xpStepsEnv + "=" + strconv.Itoa(steps)}
}

func xpWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	gitInit(t, workspace)
	return workspace
}

func stateDB(t *testing.T, engine *Engine) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=10000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Two workers, each its own process and Engine, run 30 provider calls on different sessions at the same
// time over one state database while a third process reads both sessions without pause. Both runs
// complete; every provider call has exactly one answered envelope; every write_file and shell call one
// completed execution with a complete lineage; the reader saw no error; the database is intact.
func TestCrossProcessWorkersAndReader(t *testing.T) {
	workspace := xpWorkspace(t)
	engine := openEngineAt(t, workspace)
	a := newTestSession(t, engine, "worker a")
	b := newTestSession(t, engine, "worker b")
	stop := filepath.Join(t.TempDir(), "stop")
	const steps = 30

	poller := xpStart(t, append(xpEnv("poller", workspace, a.ID+","+b.ID, "p", 0), xpStopEnv+"="+stop)...)
	wa := xpStart(t, xpEnv("worker", workspace, a.ID, "a", steps)...)
	wb := xpStart(t, xpEnv("worker", workspace, b.ID, "b", steps)...)
	ra, rb := wa.wait(t, 3*time.Minute), wb.wait(t, 3*time.Minute)
	if err := os.WriteFile(stop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rp := poller.wait(t, time.Minute)

	for name, r := range map[string]xpResult{"a": ra, "b": rb} {
		if r.Status != "completed" || r.Err != "" || r.Calls != steps+1 {
			t.Fatalf("worker %s = %+v", name, r)
		}
	}
	if rp.Errors != 0 || rp.Reads == 0 {
		t.Fatalf("reader = %+v", rp)
	}
	for _, h := range []*SessionHandle{a, b} {
		envs := envelopeRows(t, engine, h.ID)
		if len(envs) != steps+1 {
			t.Fatalf("session %s: %d envelopes for %d provider calls", h.ID, len(envs), steps+1)
		}
		for _, e := range envs {
			if e.State != "answered" || e.Purpose != "main" || e.Lineage != 1 {
				t.Fatalf("session %s envelope %+v", h.ID, e)
			}
		}
		execs, _ := readProvenance(t, engine, h.ID)
		want := 0
		for n := 1; n <= steps; n++ {
			if n%3 != 0 {
				want++
			}
		}
		if len(execs) != want {
			t.Fatalf("session %s: %d executions, want %d", h.ID, len(execs), want)
		}
		for _, x := range execs {
			if x.Outcome != "completed" || x.Lineage != 1 {
				t.Fatalf("session %s execution %+v", h.ID, x)
			}
		}
		assertPaired(t, transcriptRows(t, engine, h.ID))
	}
	var ic string
	if err := stateDB(t, engine).QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
		t.Fatalf("integrity_check = %q, %v", ic, err)
	}
	t.Logf("reader made %d reads during the two runs", rp.Reads)
}

// Another process holds a write transaction past busy_timeout while a worker's shell call runs. The
// worker's next write (the shell's commit) waits 5 s and fails: the run ends without a further provider
// request and without running the queued write_file. After the holder lets go, a reopened Engine settles
// the shell execution outcome_unknown, and a resumed run does not run the shell again.
func TestCrossProcessBusyIsAStorageFailure(t *testing.T) {
	workspace := xpWorkspace(t)
	engine := openEngineAt(t, workspace)
	h := newTestSession(t, engine, "busy")
	sig := filepath.Join(t.TempDir(), "shell-started")
	worker := xpStart(t, append(xpEnv("busy", workspace, h.ID, "busy", 1), xpSigEnv+"="+sig)...)

	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(sig); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker's shell never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	db := stateDB(t, engine)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	// Hold until the worker has ended: a fixed hold can be outlasted, since a best-effort write may spend
	// the first busy_timeout and the commit then starts its own.
	held := time.Now()
	var r xpResult
	select {
	case line := <-worker.line:
		_ = worker.cmd.Wait()
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("worker said %q (%v)", line, err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the worker did not end while the write lock was held")
	}
	if _, err := conn.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	released := time.Now()
	conn.Close()
	if r.Calls != 1 || r.Status == "completed" {
		var at []string
		for _, c := range r.CallAt {
			at = append(at, time.Unix(0, c).Sub(held).String())
		}
		execs, _ := readProvenance(t, engine, h.ID)
		t.Fatalf("worker = %+v, want one provider call and a run that did not complete\ncalls at %v after the hold began (released at %s)\nexecutions %+v\nenvelopes %+v",
			r, at, released.Sub(held), execs, envelopeRows(t, engine, h.ID))
	}
	t.Logf("busy worker ended %+v", r)
	if _, err := os.Stat(filepath.Join(workspace, "after-busy.txt")); !os.IsNotExist(err) {
		t.Fatalf("the write_file queued after the busy commit ran (%v)", err)
	}

	reopened := openEngineAt(t, workspace)
	execs, _ := readProvenance(t, reopened, h.ID)
	if len(execs) != 1 || execs[0].Tool != "bash" || execs[0].Outcome != "outcome_unknown" || execs[0].Lineage != 0 {
		t.Fatalf("executions after reopen = %+v, want the shell outcome_unknown", execs)
	}
	for _, e := range envelopeRows(t, reopened, h.ID) {
		if e.State == "admitted" {
			t.Fatalf("envelope still admitted after reopen: %+v", e)
		}
	}
	info, cause := sessionState(t, reopened, h.ID)
	if info.Status == "running" {
		t.Fatalf("session still running after reopen: %+v", info)
	}
	t.Logf("busy run ended: worker %+v, session %s cause %+v", r, info.Status, cause)

	result, err := reopened.runAgentWith(context.Background(), h.ID, completingProvider("resumed"), AgentOptions{Policy: "full", MaxSteps: 3})
	if err != nil || result.Status != "completed" {
		t.Fatalf("resume = %+v err=%v", result, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(workspace, "count.txt")); string(raw) != "x" {
		t.Fatalf("count.txt = %q: the shell ran again", raw)
	}
	assertPaired(t, transcriptRows(t, reopened, h.ID))
}

// A session fence committed by another process while a worker runs, over several trials in which the
// fence lands at different points of the loop. In every trial: every answered envelope and every
// admitted execution was created before the fence committed; at most one envelope did not end
// answered, and it carries the fence: failed/"fenced" when request admission saw it,
// unknown/"fenced_before_answer" when the answer did; with neither, the fence met a tool call, which
// then has no result. Every provider request had an envelope; at most one began after the fence, the
// one admitted before it whose answer met it. The run ends fenced. A worker on another session in the
// same database, running throughout, is not affected.
func TestCrossProcessFenceIsLinearizedWithAdmission(t *testing.T) {
	workspace := xpWorkspace(t)
	engine := openEngineAt(t, workspace)
	db := stateDB(t, engine)
	g := newTestSession(t, engine, "unaffected")
	other := xpStart(t, xpEnv("worker", workspace, g.ID, "g", 300)...)
	seen := map[string]int{}
	for trial := 0; trial < 6; trial++ {
		f := newTestSession(t, engine, fmt.Sprintf("to be fenced %d", trial))
		// Even trials pause in the provider (the fence tends to meet the answer), odd ones do not (it
		// tends to meet an admission).
		pause := "0"
		if trial%2 == 0 {
			pause = "10"
		}
		fenced := xpStart(t, append(xpEnv("worker", workspace, f.ID, fmt.Sprintf("f%d", trial), 300), xpPauseEnv+"="+pause)...)
		deadline := time.Now().Add(time.Minute)
		for {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM request_envelopes WHERE session_id = ?`, f.ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= 4+trial {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the worker made no progress")
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := db.Exec(`INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at)
			VALUES('session', ?, 1, 'revoked', 'active', 't', 't')`, f.ID); err != nil {
			t.Fatal(err)
		}
		committed := time.Now().UTC()
		rf := fenced.wait(t, 2*time.Minute)
		if rf.Status == "completed" || rf.Calls >= 301 {
			t.Fatalf("trial %d: the fenced worker = %+v", trial, rf)
		}
		if _, cause := sessionState(t, engine, f.ID); cause == nil || cause.Kind != "fenced" {
			t.Fatalf("trial %d: fenced session cause = %+v", trial, cause)
		}
		var states []string
		var fenceSeen string
		sent := 0 // envelopes whose request was sent: answered, or unknown because the answer met the fence
		rows, err := db.Query(`SELECT state, COALESCE(reason, ''), created_at FROM request_envelopes WHERE session_id = ?`, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var state, reason, created string
			if err := rows.Scan(&state, &reason, &created); err != nil {
				t.Fatal(err)
			}
			at, _ := time.Parse(time.RFC3339Nano, created)
			states = append(states, state+"/"+reason+"/"+at.Sub(committed).String())
			switch {
			case state == "answered" && at.Before(committed):
				sent++
			case state == "unknown" && reason == "fenced_before_answer" && at.Before(committed),
				state == "failed" && reason == "fenced":
				if fenceSeen != "" {
					t.Fatalf("trial %d: two envelopes carry the fence: %v", trial, states)
				}
				fenceSeen = state + "/" + reason
				if state == "unknown" {
					sent++
				}
			default:
				t.Fatalf("trial %d: envelope %s/%s created %s after the fence: %v", trial, state, reason, at.Sub(committed), states)
			}
		}
		rows.Close()
		// Every provider request had been admitted. One admitted before the fence may still have been sent
		// after it (admission commits, then the request is sent); its answer then met the fence.
		if rf.Calls != sent {
			t.Fatalf("trial %d: %d provider requests, %d sent envelopes: %v", trial, rf.Calls, sent, states)
		}
		late := 0
		for _, at := range rf.CallAt {
			if time.Unix(0, at).After(committed) {
				late++
			}
		}
		if late > 1 || (late == 1 && fenceSeen != "unknown/fenced_before_answer") {
			t.Fatalf("trial %d: %d provider requests began after the fence committed, fence seen as %q: %v", trial, late, fenceSeen, states)
		}
		if fenceSeen == "" {
			// Every request was answered. Either the fence met a tool call of the last answer (its
			// admission or its commit), and that call has no result of its own in the transcript; or it
			// committed after the last result and the run saw it at the next step boundary, before the
			// next request's admission. Both are linearized: the checks above and below prove that no
			// request and no execution began after the fence.
			calls, results := 0, 0
			for _, m := range transcriptRows(t, engine, f.ID) {
				calls += len(m.ToolCalls)
				if m.Role == "tool" && m.Origin != protocol.OriginToolInterrupted {
					results++
				}
			}
			fenceSeen = "step boundary"
			if results < calls {
				fenceSeen = "tool call"
			}
		}
		seen[fenceSeen]++
		rows, err = db.Query(`SELECT created_at FROM derived_origins WHERE session_id = ? AND kind = 'execution'`, f.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var created string
			if err := rows.Scan(&created); err != nil {
				t.Fatal(err)
			}
			if at, _ := time.Parse(time.RFC3339Nano, created); at.After(committed) {
				t.Fatalf("trial %d: an execution was admitted at %s, after the fence committed", trial, created)
			}
		}
		rows.Close()
	}
	rg := other.wait(t, 5*time.Minute)
	if rg.Status != "completed" || rg.Calls != 301 {
		t.Fatalf("the unaffected worker = %+v", rg)
	}
	for _, e := range envelopeRows(t, engine, g.ID) {
		if e.State != "answered" {
			t.Fatalf("unaffected session envelope %+v", e)
		}
	}
	t.Logf("where the fence was seen over 6 trials: %v", seen)
}
