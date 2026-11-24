package agent

// Review probe (throwaway; /tmp only, never in the repository): same shape as
// TestZZR3GiantNewestLiveVsResume but with NO completion attempt, so no repair cycle is active when
// the oversize newest group overflows. Separates ordinary-window behavior from the R2-02 repair path.

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

func TestZZR3GiantNewestNoGate(t *testing.T) {
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
		var gate atomic.Int64 // stays 0: no gate refusal, no repair cycle
		var reqs []protocol.ProviderRequest
		n := 0
		prov := &funcProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			n++
			reqs = append(reqs, req)
			if !fresh {
				return answer("resumed answer"), nil
			}
			id := fmt.Sprintf("g%d", n)
			if n <= 3 {
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "read_file", `{"path":"big.txt"}`)}}}, nil
			}
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call(id, "bash", `{"command":"go test -v ./..."}`)}}}, nil
		}}
		m := testModel(ref, prov)
		m.Context, m.Output = 32768, 4096
		l := &Loop{Models: map[config.ModelRef]Model{ref: m}, Store: store, Tools: zgTools{real: set, specs: specs, bash: &bash, giant: giant}, Snapshots: repo, Jobs: zgJobs{gate: &gate}}
		res, rerr := l.Run(ctx, zgSID, Options{Session: ref, RunID: runID, MaxSteps: maxSteps, Resumed: !fresh})
		return res, rerr, reqs, steps
	}

	res, err, reqs, _ := run(true, 20)
	t.Logf("NOGATE LIVE: status=%s err=%v requests=%d", res.Status, err, len(reqs))
	res2, err2, reqs2, steps2 := run(false, 1)
	t.Logf("NOGATE COLD RESUME of the same DB: status=%s err=%v requests=%d", res2.Status, err2, len(reqs2))
	for _, req := range reqs2 {
		var shape []string
		giantSent := false
		for _, m := range req.Messages {
			shape = append(shape, fmt.Sprintf("%s/%s(%dB)", m.Role, m.Origin, len(m.Content)))
			if strings.Contains(m.Content, "verbose output") {
				giantSent = true
			}
		}
		t.Logf("NOGATE COLD request messages=%v giantToolResultSent=%v", shape, giantSent)
	}
	for _, raw := range steps2 {
		var st struct {
			Decisions []Decision `json:"decisions"`
		}
		_ = json.Unmarshal(raw, &st)
		for _, d := range st.Decisions {
			if d.Rule == "window.dropped" || d.Rule == "window.floor_relaxed" {
				t.Logf("NOGATE COLD decision rule=%s inputs=%s", d.Rule, string(d.Inputs))
			}
		}
	}
}
