package agent

// Review probe (throwaway): live controlled overflow on a giant newest post-gate group vs. the cold
// Resume of the very same persisted history (plus Start's OriginResume row).

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
	"github.com/edynasty/LumenCortex/protocol"
)

type zzGiantTools struct {
	zzTools
	bashCalls *atomic.Int64
	giantAt   int64
	giant     string
}

func (z zzGiantTools) Execute(ctx context.Context, name string, args map[string]any, out func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name == "bash" {
		if z.bashCalls.Add(1) == z.giantAt {
			return protocol.ToolResult{OK: true, Content: z.giant, MutatesWorkspace: true}, nil
		}
	}
	return z.zzTools.Execute(ctx, name, args, out)
}

func TestZZR3GiantNewestResume(t *testing.T) {
	ctx := context.Background()
	cfg := zzCfgs[0] // 32768/4096
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	zzInitWorkspace(t, ws, cfg.bigBytes)
	dbDir := filepath.Join(root, "db")
	ref := zzRef(cfg)

	script := func(n int) protocol.ProviderResponse {
		id := fmt.Sprintf("g%d", n)
		switch {
		case n <= 3:
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "read_file", `{"path":"big.txt"}`)}}}
		case n == 4:
			return answer("All done.")
		default:
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "bash", `{"command":"go test -v ./..."}`)}}}
		}
	}
	runOnce := func(fresh bool, maxSteps int) (Result, error, []protocol.ProviderRequest, []json.RawMessage) {
		raw, err := session.Open(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		runID := "run-live"
		if fresh {
			_ = raw.Create(ctx, session.Session{ID: zzSID, Goal: "g", Status: "running"})
			if _, _, err := raw.BeginRun(ctx, zzSID, session.RunStart{RunID: runID, Input: &protocol.Message{Role: "user", Content: "make the suite pass"}}); err != nil {
				t.Fatal(err)
			}
		} else {
			runID = "run-resume"
			if _, err := raw.Reconcile(ctx, zzSID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := raw.BeginRun(ctx, zzSID, session.RunStart{RunID: runID}); err != nil {
				t.Fatal(err)
			}
		}
		var steps []json.RawMessage
		store := realSQLiteAgentStore{s: raw, steps: &steps}
		base, err := newZZTools(ws, cfg.bigBytes)
		if err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int64
		giantAt := int64(3)
		if !fresh {
			giantAt = -1
		}
		tools := zzGiantTools{zzTools: base, bashCalls: &calls, giantAt: giantAt, giant: strings.Repeat("FAIL github.com/acme/pkg/sub 0.12s verbose output\n", 4000)}
		repo, err := repository.New(ws)
		if err != nil {
			t.Fatal(err)
		}
		var reqs []protocol.ProviderRequest
		n := 0
		if !fresh {
			n = 100
		}
		gate := 0
		prov := &funcProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			n++
			gate = n
			reqs = append(reqs, req)
			if !fresh {
				return answer("resumed answer"), nil
			}
			return script(n), nil
		}}
		jobs := jobsFunc(func() []ActiveJob {
			if fresh && gate == 4 {
				return []ActiveJob{{ID: zzJob, State: "running", Command: "make test"}}
			}
			return nil
		})
		m := testModel(ref, prov)
		m.Context, m.Output = cfg.context, cfg.output
		l := &Loop{Models: map[config.ModelRef]Model{ref: m}, Store: store, Tools: tools, Snapshots: repo, Jobs: jobs}
		res, rerr := l.Run(ctx, zzSID, Options{Session: ref, RunID: runID, MaxSteps: maxSteps, Resumed: !fresh})
		return res, rerr, reqs, steps
	}

	res, err, reqs, _ := runOnce(true, 20)
	t.Logf("LIVE: status=%s err=%v requests=%d", res.Status, err, len(reqs))

	res2, err2, reqs2, steps2 := runOnce(false, 1)
	t.Logf("COLD RESUME: status=%s err=%v requests=%d", res2.Status, err2, len(reqs2))
	if len(reqs2) > 0 {
		state, rest := zzSplit(reqs2[0].Messages)
		var roles []string
		giantSeen := false
		for _, m := range reqs2[0].Messages {
			roles = append(roles, m.Role+"/"+m.Origin)
			if strings.Contains(m.Content, "verbose output") {
				giantSeen = true
			}
		}
		t.Logf("COLD request: %d messages (%d after State, resume rows dropped) roles=%v giantNewestGroupSent=%v", len(reqs2[0].Messages), len(rest), roles, giantSeen)
		if i := strings.Index(state, "[Context Window Notice"); i >= 0 {
			t.Logf("COLD notice: %q", state[i:])
		}
		for _, raw := range steps2 {
			st, in := zzStage(t, raw)
			if in != nil {
				t.Logf("COLD stage=%s floor_relaxed=%+v", st, *in)
			}
		}
	}
}

type jobsFunc func() []ActiveJob

func (f jobsFunc) ActiveJobs(context.Context, string) ([]ActiveJob, error) { return f(), nil }
