package agent

// Review probe (throwaway; /tmp only, never in the repository). Self-contained so it compiles on both
// a8b4786 (prev) and 7d064d0 (head): a gated repair whose newest post-gate tool result alone exceeds
// the only (32K/4096) model. Live run vs. cold Resume of the SAME persisted history.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

const zgSID = "s-zz-giant"

type zgTools struct {
	real  *toolset.Set
	specs []protocol.ToolSpec
	bash  *atomic.Int64
	giant int64
}

func (z zgTools) Specs([]string) []protocol.ToolSpec { return z.specs }

func (z zgTools) Execute(ctx context.Context, name string, args map[string]any, out func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name == "bash" {
		if z.bash.Add(1) == z.giant {
			return protocol.ToolResult{OK: true, Content: strings.Repeat("FAIL github.com/acme/pkg/sub 0.12s verbose output\n", 4000), MutatesWorkspace: true}, nil
		}
		return protocol.ToolResult{OK: true, Content: strings.Repeat("ok   github.com/acme/pkg/sub 0.12s\n", 150), MutatesWorkspace: true}, nil
	}
	return z.real.Execute(ctx, name, args, out)
}

type zgJobs struct{ gate *atomic.Int64 }

func (j zgJobs) ActiveJobs(context.Context, string) ([]ActiveJob, error) {
	if j.gate.Load() == 4 {
		return []ActiveJob{{ID: "job_zz_gate", State: "running", Command: "make test"}}, nil
	}
	return nil, nil
}

func TestZZR3GiantNewestLiveVsResume(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepoForTest(t, ws)
	line := "// read-only context line 0123456789abcdef\n"
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(strings.Repeat(line, 6500/len(line))), 0o644); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(root, "db")
	ref := config.ModelRef("test/m-32768_4096")

	run := func(fresh bool, maxSteps int) (Result, error, []protocol.ProviderRequest, []json.RawMessage) {
		raw, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		runID := "run-live"
		if fresh {
			_ = raw.Create(ctx, session.Session{ID: zgSID, Goal: "make the suite pass", Status: "running"})
			if _, _, err := raw.BeginRun(ctx, zgSID, session.RunStart{RunID: runID, Input: &protocol.Message{Role: "user", Content: "make the suite pass"}}); err != nil {
				t.Fatal(err)
			}
		} else {
			runID = "run-resume"
			if _, err := raw.Reconcile(ctx, zgSID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, zgSID, session.RunStart{RunID: runID}); err != nil {
				t.Fatal(err)
			}
		}
		var steps []json.RawMessage
		store := realSQLiteAgentStore{s: raw, steps: &steps}
		set, err := toolset.New(toolset.Options{Workspace: ws, Policy: toolset.PolicyFull, SessionID: zgSID})
		if err != nil {
			t.Fatal(err)
		}
		var specs []protocol.ToolSpec
		for _, s := range set.Specs(nil) {
			if s.Name == "read_file" || s.Name == "write_file" || s.Name == "bash" {
				specs = append(specs, s)
			}
		}
		var bash atomic.Int64
		giant := int64(3)
		if !fresh {
			giant = -1
		}
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		var gate atomic.Int64
		var reqs []protocol.ProviderRequest
		n := 0
		prov := &funcProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			n++
			gate.Store(int64(n))
			reqs = append(reqs, req)
			if !fresh {
				return answer("resumed answer"), nil
			}
			id := fmt.Sprintf("g%d", n)
			switch {
			case n <= 3:
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "read_file", `{"path":"big.txt"}`)}}}, nil
			case n == 4:
				return answer("All done."), nil
			default:
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "bash", `{"command":"go test -v ./..."}`)}}}, nil
			}
		}}
		m := testModel(ref, prov)
		m.Context, m.Output = 32768, 4096
		l := &Loop{Models: map[config.ModelRef]Model{ref: m}, Store: store, Tools: zgTools{real: set, specs: specs, bash: &bash, giant: giant}, Snapshots: repo, Jobs: zgJobs{gate: &gate}}
		res, rerr := l.Run(ctx, zgSID, Options{Session: ref, RunID: runID, MaxSteps: maxSteps, Resumed: !fresh})
		return res, rerr, reqs, steps
	}

	res, err, reqs, _ := run(true, 20)
	t.Logf("LIVE: status=%s err=%v requests=%d", res.Status, err, len(reqs))

	res2, err2, reqs2, steps2 := run(false, 1)
	t.Logf("COLD RESUME of the same DB: status=%s err=%v requests=%d", res2.Status, err2, len(reqs2))
	for _, req := range reqs2 {
		var shape []string
		giantSent := false
		for _, m := range req.Messages {
			shape = append(shape, fmt.Sprintf("%s/%s(%dB)", m.Role, m.Origin, len(m.Content)))
			if strings.Contains(m.Content, "verbose output") {
				giantSent = true
			}
		}
		t.Logf("COLD request messages=%v giantToolResultSent=%v", shape, giantSent)
		for i, m := range req.Messages {
			if m.Role != "system" {
				c := m.Content
				if len(c) > 1800 {
					c = c[:1800] + "…"
				}
				t.Logf("COLD msg[%d] %s/%s: %q", i, m.Role, m.Origin, c)
			}
		}
	}
	for _, raw := range steps2 {
		var st struct {
			Decisions []Decision `json:"decisions"`
		}
		_ = json.Unmarshal(raw, &st)
		for _, d := range st.Decisions {
			in := string(d.Inputs)
			if len(in) > 600 {
				in = in[:600]
			}
			t.Logf("COLD decision rule=%s inputs=%s", d.Rule, in)
		}
	}
}
