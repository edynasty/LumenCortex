package agent

// Review probe (throwaway; lives only in /tmp/lc-adr0024-r3/probe, never in the repository).
//
// R2-02 third-round verification through the REAL Loop.Run on a REAL SQLite database with the REAL
// toolset (read_file/write_file + Git snapshots) and a fake large-output bash (tierWorkspace):
//
//   20 pre-gate steps (reads, Unknown bash, Proven writes; history > window) -> gate refusal #1 ->
//   12 repair steps (reads, Proven write, mixed [read_file, write_file] group, Unknown bash) ->
//   gate refusal #2 -> 40 repair steps (reads first, then write/bash/read pattern) -> completion.
//
// For each of the five window configurations the continuous run is recorded; then, for a set of cut
// points (first R1, first R2, first R3, after gate #2, after a Proven write, after an Unknown bash,
// late repair, final answer) a CHILD PROCESS runs the same script on a fresh DB/workspace and is
// SIGKILLed inside Provider.Complete of call n (after persisting the request it was about to send).
// The parent reopens the DB cold, Reconciles, derives RepairContext three ways, Resumes the Loop and
// compares the cold request/decisions with the child's live request and the continuous run.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

type zzCfg struct {
	name     string
	context  int
	output   int
	bigBytes int
}

var zzCfgs = []zzCfg{
	{"32768_4096", 32768, 4096, 6500},
	{"64000_8192", 64000, 8192, 12800},
	{"128000_8192", 128000, 8192, 25600},
	{"200000_16384", 200000, 16384, 40000},
	{"600000_32768", 600000, 32768, 120000},
}

const (
	zzPre   = 20
	zzG1    = zzPre + 1       // 21
	zzQ1    = 12              // 22..33
	zzG2    = zzG1 + zzQ1 + 1 // 34
	zzQ2    = 40              // 35..74
	zzFinal = zzG2 + zzQ2 + 1 // 75
	zzSID   = "s-zz-r3"
	zzJob   = "job_zz_gate"
)

func zzStepKind(n int) string {
	switch {
	case n <= zzPre:
		switch {
		case n == 5 || n == 12:
			return "write"
		case n%3 == 0:
			return "bash"
		default:
			return "read"
		}
	case n == zzG1 || n == zzG2 || n == zzFinal:
		return "answer"
	case n < zzG2:
		i := n - zzG1 - 1
		switch {
		case i < 4:
			return "read"
		case i == 4:
			return "write"
		case i == 5:
			return "multi"
		default:
			return "bash"
		}
	default:
		i := n - zzG2 - 1
		switch {
		case i < 4:
			return "read"
		case i%5 == 0:
			return "write"
		case i%5 == 1 || i%5 == 2:
			return "bash"
		default:
			return "read"
		}
	}
}

func zzResponse(n int) protocol.ProviderResponse {
	id := fmt.Sprintf("c%d", n)
	switch zzStepKind(n) {
	case "read":
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "read_file", `{"path":"big.txt"}`)}}}
	case "write":
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			call(id, "write_file", fmt.Sprintf(`{"path":"main.go","content":"package main\n// step %d\n"}`, n))}}}
	case "multi":
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			call(id+"a", "read_file", `{"path":"small.txt"}`),
			call(id+"b", "write_file", fmt.Sprintf(`{"path":"main.go","content":"package main\n// multi %d\n"}`, n)),
		}}}
	case "bash":
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "bash", `{"command":"go test ./..."}`)}}}
	default:
		return answer(fmt.Sprintf("All done (answer at call %d).", n))
	}
}

type zzTools struct {
	real    *toolset.Set
	bashOut string
	specs   []protocol.ToolSpec
}

func newZZTools(ws string, bigBytes int) (zzTools, error) {
	set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: zzSID})
	if err != nil {
		return zzTools{}, err
	}
	var specs []protocol.ToolSpec
	for _, s := range set.Specs(nil) {
		if s.Name == "read_file" || s.Name == "write_file" || s.Name == "bash" {
			specs = append(specs, s)
		}
	}
	line := "ok   github.com/acme/pkg/sub 0.12s\n"
	return zzTools{real: set, bashOut: strings.Repeat(line, bigBytes/len(line)), specs: specs}, nil
}

func (z zzTools) Specs([]string) []protocol.ToolSpec { return z.specs }

func (z zzTools) Execute(ctx context.Context, name string, args map[string]any, out func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name == "bash" {
		return protocol.ToolResult{OK: true, Content: z.bashOut, MutatesWorkspace: true}, nil
	}
	return z.real.Execute(ctx, name, args, out)
}

type zzProvider struct {
	mu      sync.Mutex
	base    int
	n       int
	reqs    map[int]json.RawMessage
	killAt  int
	killOut string
}

func (p *zzProvider) Model() string { return "fake" }

func (p *zzProvider) global() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.base + p.n
}

func (p *zzProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.n++
	g := p.base + p.n
	raw, err := json.Marshal(req)
	if err != nil {
		p.mu.Unlock()
		return protocol.ProviderResponse{}, err
	}
	if p.reqs != nil {
		p.reqs[g] = raw
	}
	p.mu.Unlock()
	if p.killAt > 0 && g == p.killAt {
		f, err := os.OpenFile(p.killOut, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err == nil {
			_, _ = f.Write(raw)
			_ = f.Sync()
			_ = f.Close()
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	return zzResponse(g), nil
}

type zzJobs struct{ p *zzProvider }

func (j zzJobs) ActiveJobs(context.Context, string) ([]ActiveJob, error) {
	g := j.p.global()
	if g == zzG1 || g == zzG2 {
		return []ActiveJob{{ID: zzJob, State: "running", Command: "make test"}}, nil
	}
	return nil, nil
}

func zzInitWorkspace(t testing.TB, ws string, bigBytes int) {
	initGitRepoForTest(t.(*testing.T), ws)
	line := "// read-only context line 0123456789abcdef\n"
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(strings.Repeat(line, bigBytes/len(line))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "small.txt"), []byte("small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zzRef(cfg zzCfg) config.ModelRef { return config.ModelRef("test/m-" + cfg.name) }

type zzRunOut struct {
	res   Result
	err   error
	reqs  map[int]json.RawMessage
	steps []json.RawMessage
}

// zzRun runs the scripted Loop on dbDir/ws. fresh creates the session; otherwise the run is a cold
// Resume (Reconcile + BeginRun without input + Options.Resumed).
func zzRun(t testing.TB, dbDir, ws string, cfg zzCfg, fresh bool, base, maxSteps, killAt int, killOut string) zzRunOut {
	ctx := context.Background()
	raw, err := session.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	runID := "run-live"
	if fresh {
		if err := raw.Create(ctx, session.Session{ID: zzSID, Goal: "make the suite pass", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, zzSID, session.RunStart{RunID: runID, Input: &protocol.Message{Role: "user", Content: "make the suite pass"}}); err != nil {
			t.Fatal(err)
		}
	} else {
		runID = fmt.Sprintf("run-resume-%d", base)
		if _, err := raw.Reconcile(ctx, zzSID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := raw.BeginRun(ctx, zzSID, session.RunStart{RunID: runID}); err != nil {
			t.Fatal(err)
		}
	}
	var steps []json.RawMessage
	store := realSQLiteAgentStore{s: raw, steps: &steps}
	tools, err := newZZTools(ws, cfg.bigBytes)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := repository.New(ws)
	if err != nil {
		t.Fatal(err)
	}
	prov := &zzProvider{base: base, reqs: map[int]json.RawMessage{}, killAt: killAt, killOut: killOut}
	ref := zzRef(cfg)
	m := testModel(ref, prov)
	m.Context, m.Output = cfg.context, cfg.output
	l := &Loop{Models: map[config.ModelRef]Model{ref: m}, Store: store, Tools: tools, Snapshots: repo, Jobs: zzJobs{p: prov}}
	res, rerr := l.Run(ctx, zzSID, Options{Session: ref, RunID: runID, MaxSteps: maxSteps, Resumed: !fresh})
	return zzRunOut{res: res, err: rerr, reqs: prov.reqs, steps: steps}
}

// TestZZR3Child is the crash child: it runs the script from scratch and SIGKILLs itself inside call n.
func TestZZR3Child(t *testing.T) {
	if os.Getenv("ZZ_R3_CHILD") != "1" {
		t.Skip("child only")
	}
	var cfg zzCfg
	for _, c := range zzCfgs {
		if c.name == os.Getenv("ZZ_R3_CFG") {
			cfg = c
		}
	}
	kill, _ := strconv.Atoi(os.Getenv("ZZ_R3_KILL"))
	out := zzRun(t, os.Getenv("ZZ_R3_DB"), os.Getenv("ZZ_R3_WS"), cfg, true, 0, zzFinal+5, kill, os.Getenv("ZZ_R3_OUT"))
	t.Fatalf("child was not killed: status=%s err=%v", out.res.Status, out.err)
}

type zzStepInfo struct {
	Step      int64      `json:"step"`
	ModelRef  string     `json:"modelRef"`
	Decisions []Decision `json:"decisions"`
	Window    *struct {
		FirstSeq int64 `json:"firstSeq"`
		Dropped  int   `json:"dropped"`
		Tokens   int   `json:"tokens"`
	} `json:"window"`
}

func zzStage(t testing.TB, raw json.RawMessage) (string, *windowFloorRelaxedInputs) {
	var s zzStepInfo
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for _, d := range s.Decisions {
		if d.Rule != RuleWindowFloorRelaxed {
			continue
		}
		var in windowFloorRelaxedInputs
		if err := json.Unmarshal(d.Inputs, &in); err != nil {
			t.Fatal(err)
		}
		switch {
		case !in.GatePinnedInState:
			return "R1", &in
		case in.PostBlockCausalFloorSeq > in.BlockSeq && in.RelaxedFloorSeq == in.PostBlockCausalFloorSeq:
			return "R2", &in
		default:
			return "R3", &in
		}
	}
	return "strict", nil
}

func zzMsgs(t testing.TB, raw json.RawMessage) []protocol.Message {
	var req protocol.ProviderRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	return req.Messages
}

// zzSplit returns the State text and the transcript messages after it, with resume rows removed.
func zzSplit(msgs []protocol.Message) (string, []protocol.Message) {
	i := 0
	for i < len(msgs) && msgs[i].Role == "system" {
		i++
	}
	if i >= len(msgs) {
		return "", nil
	}
	state := msgs[i].Content
	var rest []protocol.Message
	for _, m := range msgs[i+1:] {
		if m.Origin == protocol.OriginResume {
			continue
		}
		rest = append(rest, m)
	}
	return state, rest
}

func zzPairing(msgs []protocol.Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			if len(pending) > 0 {
				return fmt.Errorf("msg %d: assistant while calls %v unanswered", i, pending)
			}
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		case "tool":
			if !pending[m.ToolCallID] {
				return fmt.Errorf("msg %d: orphan tool result %s", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		default:
			if len(pending) > 0 {
				return fmt.Errorf("msg %d: %s row while calls %v unanswered", i, m.Role, pending)
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("unanswered at end: %v", pending)
	}
	return nil
}

var (
	zzExecRe = regexp.MustCompile(`ex-[0-9a-f]{32}`)
	zzRefRe  = regexp.MustCompile(`seq=(\d+)(?: tool=([a-z_]+))?(?: exec=(ex-[0-9a-f]+))? evidence=([a-z_]+)`)
	zzPinRe  = regexp.MustCompile(`\[Pinned Completion-Gate State — seq (\d+) \(refusal #(\d+), stage: ([a-z_]+)\)\]`)
	zzLedRe  = regexp.MustCompile(`Causal repair ledger: originalAnchorSeq=(-?\d+), strictFloorSeq=(-?\d+), postGateProven=(\d+), postGateUnknown=(\d+)`)
)

func zzCanon(msgs []protocol.Message, normExec bool) string {
	b, _ := json.Marshal(msgs)
	s := string(b)
	if normExec {
		s = zzExecRe.ReplaceAllString(s, "ex-*")
	}
	return s
}

func zzFirstDiff(a, b []protocol.Message) string {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		ja, _ := json.Marshal(a[i])
		jb, _ := json.Marshal(b[i])
		if string(ja) != string(jb) {
			return fmt.Sprintf("first diff at msg %d of (%d vs %d): %.160s || %.160s", i, len(a), len(b), ja, jb)
		}
	}
	return fmt.Sprintf("length differs %d vs %d", len(a), len(b))
}

func zzDecisionRules(t testing.TB, raw json.RawMessage) []string {
	var s zzStepInfo
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range s.Decisions {
		out = append(out, d.Rule)
	}
	return out
}

func TestZZR3ResumeParity(t *testing.T) {
	only := os.Getenv("ZZ_R3_ONLY")
	for _, cfg := range zzCfgs {
		cfg := cfg
		if only != "" && only != cfg.name {
			continue
		}
		t.Run(cfg.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()
			ws := filepath.Join(root, "ws-cont")
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			zzInitWorkspace(t, ws, cfg.bigBytes)
			cont := zzRun(t, filepath.Join(root, "db-cont"), ws, cfg, true, 0, zzFinal+5, 0, "")
			if cont.err != nil || cont.res.Status != StatusCompleted || len(cont.reqs) != zzFinal {
				t.Fatalf("continuous run: status=%s err=%v requests=%d (want %d)", cont.res.Status, cont.err, len(cont.reqs), zzFinal)
			}
			if len(cont.steps) != zzFinal {
				t.Fatalf("continuous step records=%d want %d", len(cont.steps), zzFinal)
			}
			for i, raw := range cont.steps {
				if err := VerifyStep(raw); err != nil {
					t.Errorf("continuous VerifyStep[%d]: %v", i, err)
				}
			}
			// Per-request invariants on the continuous run.
			stages := map[int]string{}
			counts := map[string]int{}
			var stageLine []string
			for n := 1; n <= zzFinal; n++ {
				st, _ := zzStage(t, cont.steps[n-1])
				stages[n] = st
				counts[st]++
				stageLine = append(stageLine, fmt.Sprintf("%d:%s", n, st))
				msgs := zzMsgs(t, cont.reqs[n])
				state, rest := zzSplit(msgs)
				if err := zzPairing(rest); err != nil {
					t.Errorf("call %d (%s) pairing: %v", n, st, err)
				}
				if n > zzG1 {
					inState, inTx := countGateOccurrencesInRequest(protocol.ProviderRequest{Messages: msgs}, zzJob)
					pinned := strings.Contains(state, "[Pinned Completion-Gate State")
					if n <= zzFinal && inState+inTx < 1 {
						t.Errorf("call %d (%s): gate feedback LOST (inState=%d inTx=%d)", n, st, inState, inTx)
					}
					if pinned && inTx != 0 {
						t.Errorf("call %d (%s): gate DUPLICATED pinned+transcript (inTx=%d)", n, st, inTx)
					}
					if !pinned && inState != 0 {
						t.Errorf("call %d (%s): gate text in State without pin", n, st)
					}
				}
			}
			t.Logf("[%s] stage counts=%v", cfg.name, counts)
			t.Logf("[%s] stages: %s", cfg.name, strings.Join(stageLine, " "))

			// Cut points.
			cutSet := map[int]string{}
			firstOf := func(want string, from int) int {
				for n := from; n <= zzFinal; n++ {
					if stages[n] == want {
						return n
					}
				}
				return -1
			}
			for _, st := range []string{"R1", "R2", "R3"} {
				if n := firstOf(st, 1); n > 0 {
					cutSet[n] += "first-" + st + " "
				}
			}
			cutSet[zzG2+1] += "after-gate2 "
			for n := zzG2 + 2; n <= zzFinal; n++ {
				if zzStepKind(n-1) == "write" && (stages[n] == "R3" || stages[n] == "R2") {
					cutSet[n] += "after-proven "
					break
				}
			}
			for n := zzG2 + 2; n <= zzFinal; n++ {
				if zzStepKind(n-1) == "bash" && stages[n] == "R3" {
					cutSet[n] += "after-unknown "
					break
				}
			}
			cutSet[zzFinal-1] += "late "
			cutSet[zzFinal] += "final-answer "
			var cuts []int
			for n := range cutSet {
				cuts = append(cuts, n)
			}
			sort.Ints(cuts)

			for _, n := range cuts {
				label := strings.TrimSpace(cutSet[n])
				dir := filepath.Join(root, fmt.Sprintf("cut-%d", n))
				cws := filepath.Join(dir, "ws")
				if err := os.MkdirAll(cws, 0o755); err != nil {
					t.Fatal(err)
				}
				zzInitWorkspace(t, cws, cfg.bigBytes)
				db := filepath.Join(dir, "db")
				reqOut := filepath.Join(dir, "live-req.json")
				cmd := exec.Command(os.Args[0], "-test.run=^TestZZR3Child$", "-test.count=1")
				cmd.Env = append(os.Environ(), "ZZ_R3_CHILD=1", "ZZ_R3_CFG="+cfg.name, "ZZ_R3_DB="+db, "ZZ_R3_WS="+cws,
					fmt.Sprintf("ZZ_R3_KILL=%d", n), "ZZ_R3_OUT="+reqOut)
				outb, cerr := cmd.CombinedOutput()
				wstat, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !wstat.Signaled() || wstat.Signal() != syscall.SIGKILL {
					t.Fatalf("cut %d: child not SIGKILLed: err=%v out=%s", n, cerr, outb)
				}
				liveRaw, err := os.ReadFile(reqOut)
				if err != nil {
					t.Fatalf("cut %d: no live request: %v", n, err)
				}

				// Cold derivations on the crashed DB, before and after BeginRun's resume row.
				coldRC := func(phase string) RepairContext {
					raw, err := session.Open(db)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = raw.Close() }()
					if _, err := raw.Reconcile(ctx, zzSID); err != nil {
						t.Fatal(err)
					}
					st := realSQLiteAgentStore{s: raw}
					r := &run{}
					if err := r.load(ctx, st, zzSID, nil, cfg.context); err != nil {
						t.Fatal(err)
					}
					rc1, err := r.deriveRepairContext(ctx, st, zzSID)
					if err != nil {
						t.Fatal(err)
					}
					rc2, err := StreamRepairContextFromStore(ctx, st, zzSID, 0)
					if err != nil {
						t.Fatal(err)
					}
					if rc1 != rc2 {
						t.Errorf("cut %d %s: load-derived %+v != streamed %+v", n, phase, rc1, rc2)
					}
					var entries []window.Entry
					if err := st.Transcript(ctx, zzSID, 0, func(seq int64, m protocol.Message) error {
						entries = append(entries, window.Entry{Seq: seq, Msg: m})
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					sums, err := st.ListAnsweredExecutions(ctx, zzSID, 0)
					if err != nil {
						t.Fatal(err)
					}
					full := &run{}
					full.populateMutations(entries, sums)
					meta, ok := HistoryGroupsFromEntries(entries, full.mutations)
					if !ok {
						t.Fatalf("cut %d: HistoryGroupsFromEntries failed", n)
					}
					rc3 := DeriveRepairContextFromHistory(meta)
					if rc3.Active != rc1.Active || rc3.BlockSeq != rc1.BlockSeq || rc3.MinRequiredSeq != rc1.MinRequiredSeq ||
						rc3.OriginalMinRequiredSeq != rc1.OriginalMinRequiredSeq || rc3.PostBlockCausalFloorSeq != rc1.PostBlockCausalFloorSeq ||
						rc3.Stage != rc1.Stage || rc3.BlockCount != rc1.BlockCount || rc3.GateFeedback != rc1.GateFeedback ||
						rc3.PostBlockProvenCount != rc1.PostBlockProvenCount || rc3.PostBlockUnknownCount != rc1.PostBlockUnknownCount {
						t.Errorf("cut %d %s: full-entries derivation differs:\n full=%+v\n load=%+v", n, phase, rc3, rc1)
					}
					// Provenance of every ref.
					bySeq := map[int64][]session.AnsweredExecutionSummary{}
					for _, s := range sums {
						bySeq[s.AnsweredSeq] = append(bySeq[s.AnsweredSeq], s)
					}
					refs := append([]CausalEvidenceRef{}, rc1.CausalProgress[:rc1.CausalProgressLen]...)
					if rc1.HasOriginalEvidence {
						refs = append(refs, rc1.OriginalEvidence)
					}
					for _, ref := range refs {
						var hit *session.AnsweredExecutionSummary
						for i := range bySeq[ref.AnsweredSeq] {
							if bySeq[ref.AnsweredSeq][i].ExecutionID == ref.ExecutionID {
								hit = &bySeq[ref.AnsweredSeq][i]
							}
						}
						switch {
						case hit == nil:
							t.Logf("cut %d PROVENANCE: ref %+v -> no persisted execution with that id at that answered_seq", n, ref)
						case hit.ToolName != ref.ToolName:
							t.Logf("cut %d PROVENANCE MISMATCH: ref seq=%d tool=%s toolCallId=%s exec=%s evidence=%s, but exec %s is tool %s",
								n, ref.AnsweredSeq, ref.ToolName, ref.ToolCallID, ref.ExecutionID, ref.MutationEvidence, hit.ExecutionID, hit.ToolName)
						}
					}
					return rc1
				}
				rcBefore := coldRC("pre-BeginRun")

				resumed := zzRun(t, db, cws, cfg, false, n-1, 1, 0, "")
				if n == cuts[0] || os.Getenv("ZZ_R3_TAIL") == "1" {
					raw2, err := session.Open(db)
					if err != nil {
						t.Fatal(err)
					}
					var tail []string
					_ = raw2.EachMessageFrom(ctx, zzSID, 0, func(sm session.Message) error {
						var m protocol.Message
						_ = json.Unmarshal(sm.JSON, &m)
						tail = append(tail, fmt.Sprintf("%d:%s/%s/%dB", sm.Seq, m.Role, m.Origin, len(m.Content)))
						return nil
					})
					_ = raw2.Close()
					t.Logf("    cut %d persisted tail after resume: %v", n, tail[max(0, len(tail)-6):])
				}
				if len(resumed.reqs) != 1 || resumed.reqs[n] == nil {
					t.Fatalf("cut %d (%s): resumed run sent %d requests (status=%s err=%v)", n, label, len(resumed.reqs), resumed.res.Status, resumed.err)
				}
				for i, raw := range resumed.steps {
					if err := VerifyStep(raw); err != nil {
						t.Errorf("cut %d resumed VerifyStep[%d]: %v", n, i, err)
					}
				}
				coldMsgs := zzMsgs(t, resumed.reqs[n])
				liveMsgs := zzMsgs(t, liveRaw)
				contMsgs := zzMsgs(t, cont.reqs[n])
				coldState, coldRest := zzSplit(coldMsgs)
				liveState, liveRest := zzSplit(liveMsgs)
				contState, contRest := zzSplit(contMsgs)
				if err := zzPairing(coldRest); err != nil {
					t.Errorf("cut %d cold pairing: %v", n, err)
				}

				stateLive := coldState == liveState
				msgsLive := zzCanon(coldRest, false) == zzCanon(liveRest, false)
				stateCont := zzExecRe.ReplaceAllString(coldState, "ex-*") == zzExecRe.ReplaceAllString(contState, "ex-*")
				msgsCont := zzCanon(coldRest, true) == zzCanon(contRest, true)
				liveVsCont := zzCanon(liveRest, true) == zzCanon(contRest, true) && zzExecRe.ReplaceAllString(liveState, "ex-*") == zzExecRe.ReplaceAllString(contState, "ex-*")

				coldStage := "?"
				var coldIn *windowFloorRelaxedInputs
				if len(resumed.steps) > 0 {
					coldStage, coldIn = zzStage(t, resumed.steps[0])
				}
				contStage, contIn := zzStage(t, cont.steps[n-1])
				t.Logf("[%s] cut n=%d (%s): cont=%s cold=%s | RC(pre-begin) active=%v stage=%s block=%d count=%d min=%d orig=%d postFloor=%d proven=%d unknown=%d refs=%d",
					cfg.name, n, label, contStage, coldStage, rcBefore.Active, rcBefore.Stage, rcBefore.BlockSeq, rcBefore.BlockCount,
					rcBefore.MinRequiredSeq, rcBefore.OriginalMinRequiredSeq, rcBefore.PostBlockCausalFloorSeq,
					rcBefore.PostBlockProvenCount, rcBefore.PostBlockUnknownCount, rcBefore.CausalProgressLen)
				t.Logf("[%s] cut n=%d parity: live==cold state:%v msgs:%v | cont==cold state:%v msgs:%v | live==cont:%v",
					cfg.name, n, stateLive, msgsLive, stateCont, msgsCont, liveVsCont)
				if !msgsLive {
					t.Logf("    live vs cold msgs: %s", zzFirstDiff(liveRest, coldRest))
				}
				if !stateLive {
					t.Logf("    live state tail: %q", liveState[max(0, len(liveState)-700):])
					t.Logf("    cold state tail: %q", coldState[max(0, len(coldState)-700):])
				}
				if contIn != nil && coldIn != nil {
					ci, ki := *contIn, *coldIn
					if !reflect.DeepEqual(ci, ki) {
						t.Logf("    floor_relaxed inputs differ:\n      cont=%+v\n      cold=%+v", ci, ki)
					}
				} else if (contIn == nil) != (coldIn == nil) {
					t.Logf("    floor_relaxed presence differs: cont=%v cold=%v", contIn != nil, coldIn != nil)
				}
				cr := zzDecisionRules(t, cont.steps[n-1])
				kr := zzDecisionRules(t, resumed.steps[0])
				if strings.Join(cr, ",") != strings.Join(kr, ",") {
					t.Logf("    decision rules differ:\n      cont=%v\n      cold=%v", cr, kr)
				}
				// Live notice fields vs cold RepairContext.
				if m := zzPinRe.FindStringSubmatch(liveState); m != nil {
					bs, _ := strconv.ParseInt(m[1], 10, 64)
					bc, _ := strconv.Atoi(m[2])
					if bs != rcBefore.BlockSeq || bc != rcBefore.BlockCount || m[3] != string(rcBefore.Stage) {
						t.Errorf("cut %d: live pinned header %v != cold RC block=%d count=%d stage=%s", n, m[1:], rcBefore.BlockSeq, rcBefore.BlockCount, rcBefore.Stage)
					}
					if l := zzLedRe.FindStringSubmatch(liveState); l != nil {
						p, _ := strconv.Atoi(l[3])
						u, _ := strconv.Atoi(l[4])
						if p != rcBefore.PostBlockProvenCount || u != rcBefore.PostBlockUnknownCount {
							t.Errorf("cut %d: live ledger counts %v != cold %d/%d", n, l[1:], rcBefore.PostBlockProvenCount, rcBefore.PostBlockUnknownCount)
						}
					}
					liveRefs := zzRefRe.FindAllStringSubmatch(liveState, -1)
					t.Logf("    live ledger refs=%d (cold CausalProgressLen=%d + original=%v)", len(liveRefs), rcBefore.CausalProgressLen, rcBefore.HasOriginalEvidence)
				}
			}
		})
	}
}
