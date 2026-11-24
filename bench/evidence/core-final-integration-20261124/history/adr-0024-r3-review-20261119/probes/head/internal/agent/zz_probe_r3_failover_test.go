package agent

// Review probe (throwaway; /tmp only). Long gated repair on a REAL SQLite store with two candidates
// (session S=32768/4096, failover L=64000/8192 or 128000/8192): failover to the larger candidate must
// come first; Floor Relaxation may only appear once every larger candidate overflowed strictly, and it
// must run on the largest available candidate.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestZZR3FailoverBeforeRelaxation(t *testing.T) {
	for _, large := range []zzCfg{zzCfgs[1], zzCfgs[2]} {
		large := large
		t.Run("S32K_L"+large.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			ws := filepath.Join(root, "ws")
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			small := zzCfgs[0]
			bigBytes := 6500
			zzInitWorkspace(t, ws, bigBytes)
			raw, err := session.Open(filepath.Join(root, "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			if err := raw.Create(ctx, session.Session{ID: zzSID, Goal: "make the suite pass", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, zzSID, session.RunStart{RunID: "run-fo", Input: &protocol.Message{Role: "user", Content: "make the suite pass"}}); err != nil {
				t.Fatal(err)
			}
			var steps []json.RawMessage
			store := realSQLiteAgentStore{s: raw, steps: &steps}
			tools, err := newZZTools(ws, bigBytes)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := repository.New(ws)
			if err != nil {
				t.Fatal(err)
			}
			// One shared global script; each provider records which model received the request.
			var mu sync.Mutex
			global := 0
			sentTo := map[int]config.ModelRef{}
			const gate1, gate2, final = 21, 40, 82
			script := func(n int) protocol.ProviderResponse {
				id := fmt.Sprintf("f%d", n)
				switch {
				case n == gate1 || n == gate2 || n == final:
					return answer(fmt.Sprintf("done at %d", n))
				case n%7 == 0:
					return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
						call(id, "write_file", fmt.Sprintf(`{"path":"main.go","content":"package main\n// %d\n"}`, n))}}}
				case n%3 == 0:
					return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "bash", `{"command":"go test ./..."}`)}}}
				default:
					return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "read_file", `{"path":"big.txt"}`)}}}
				}
			}
			mk := func(ref config.ModelRef) protocol.Provider {
				return &funcProvider{fn: func(_ context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
					mu.Lock()
					defer mu.Unlock()
					global++
					sentTo[global] = ref
					return script(global), nil
				}}
			}
			sRef, lRef := zzRef(small), config.ModelRef("test/L-"+large.name)
			sm := testModel(sRef, mk(sRef))
			sm.Context, sm.Output = small.context, small.output
			lm := testModel(lRef, mk(lRef))
			lm.Context, lm.Output = large.context, large.output
			jobs := jobsFunc(func() []ActiveJob {
				mu.Lock()
				defer mu.Unlock()
				if global == gate1 || global == gate2 {
					return []ActiveJob{{ID: zzJob, State: "running", Command: "make test"}}
				}
				return nil
			})
			var evMu sync.Mutex
			overflowByStep := map[int64][]string{}
			l := &Loop{Models: map[config.ModelRef]Model{sRef: sm, lRef: lm}, Store: store, Tools: tools, Snapshots: repo, Jobs: jobs,
				Emit: func(e Event) {
					if e.Type != "window.overflow" {
						return
					}
					evMu.Lock()
					defer evMu.Unlock()
					st, _ := e.Data["step"].(int64)
					overflowByStep[st] = append(overflowByStep[st], fmt.Sprint(e.Data["modelRef"]))
				}}
			res, rerr := l.Run(ctx, zzSID, Options{Session: sRef, Failover: []config.ModelRef{lRef}, RunID: "run-fo", MaxSteps: final + 5})
			t.Logf("result status=%s err=%v requests=%d", res.Status, rerr, global)

			var line []string
			violations := 0
			stageCount := map[string]int{}
			for _, raw := range steps {
				var s zzStepInfo
				if err := json.Unmarshal(raw, &s); err != nil {
					t.Fatal(err)
				}
				stage, in := zzStage(t, raw)
				stageCount[fmt.Sprintf("%s@%s", stage, strings.TrimPrefix(s.ModelRef, "test/"))]++
				over := overflowByStep[s.Step]
				fb := false
				for _, d := range s.Decisions {
					if d.Rule == RuleFailoverFloorFallback {
						fb = true
					}
				}
				tag := fmt.Sprintf("%d:%s/%s", s.Step, strings.TrimPrefix(s.ModelRef, "test/")[:1], stage)
				if len(over) > 0 {
					tag += fmt.Sprintf("{ovf:%s}", strings.Join(over, ","))
				}
				if fb {
					tag += "{fallback}"
				}
				line = append(line, tag)
				if in != nil {
					// Relaxation must run on L and only after L itself overflowed strictly in this step.
					sawL := false
					for _, o := range over {
						if o == string(lRef) {
							sawL = true
						}
					}
					if s.ModelRef != string(lRef) || !sawL || !in.LargerCandidatesExhausted {
						violations++
						t.Errorf("step %d: relaxed on %s (L overflowed strictly this step: %v, exhausted flag %v)", s.Step, s.ModelRef, sawL, in.LargerCandidatesExhausted)
					}
				} else if s.ModelRef == string(sRef) {
					for _, o := range over {
						if o == string(sRef) {
							violations++
							t.Errorf("step %d answered by S after S overflowed", s.Step)
						}
					}
				}
			}
			keys := make([]string, 0, len(stageCount))
			for k := range stageCount {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var counts []string
			for _, k := range keys {
				counts = append(counts, fmt.Sprintf("%s=%d", k, stageCount[k]))
			}
			t.Logf("stage@model counts: %s | violations=%d", strings.Join(counts, " "), violations)
			t.Logf("timeline: %s", strings.Join(line, " "))
		})
	}
}
