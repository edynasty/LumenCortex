//go:build cgo

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S7 performance baseline (plan §5 T1-S7 item 4). The same file runs unchanged on Core 42ee234 and on
// the T1 head: it uses only Open, NewSession and the runAgentWith test helper that both versions have.
// It measures, over the same scripted fixture (write_file, shell, read_file, note_write, a shell that
// prints 20k lines, final answer):
//   - committed write transactions on the state database per run and per provider request, counted by
//     commitcount_test.go (test-only; production code has no counter; copy both files to run on 42ee234);
//   - the engine time of each step: from a provider response returning to the next request arriving,
//     so provider time is excluded (the provider answers at once) and no checks run.
//
// It is a measurement, not an assertion: it runs only with LCX_T1_PERF=1 and prints one JSON line.

// perfProvider plays the fixture script and records when each request arrives and each response leaves.
type perfProvider struct {
	mu       sync.Mutex
	step     int
	arrive   []time.Time
	leave    []time.Time
	count    func() int64 // committed write transactions on the engine's state database
	commitAt []int64      // count() when each request arrived
	requests int
}

func (p *perfProvider) Model() string { return "perf" }

func (p *perfProvider) Complete(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.arrive = append(p.arrive, time.Now())
	p.commitAt = append(p.commitAt, p.count())
	defer func() { p.leave = append(p.leave, time.Now()) }()
	p.requests++
	p.step++
	call := func(id, name, args string) protocol.ProviderResponse {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(args)}}}, FinishReason: "tool_calls"}
	}
	switch p.step {
	case 1:
		return call("w", "write_file", fmt.Sprintf(`{"path":"a.txt","content":"alpha %d\n"}`, time.Now().UnixNano())), nil
	case 2:
		return call("s", "bash", `{"command":"printf x >> log.txt"}`), nil
	case 3:
		return call("r", "read_file", `{"path":"a.txt"}`), nil
	case 4:
		return call("n", "note_write", `{"text":"a.txt holds the alpha line.","cites":["a.txt"]}`), nil
	case 5:
		// Streams many tool.output chunks: before T1-S6 the journal kept each chunk.
		return call("o", "bash", `{"command":"seq 1 20000"}`), nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted)-1) + 0.5)
	return sorted[i]
}

func TestT1PerfBaseline(t *testing.T) {
	if os.Getenv("LCX_T1_PERF") != "1" {
		t.Skip("measurement only: set LCX_T1_PERF=1")
	}
	installCommitCounter(t)
	workspace := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	count := func() int64 { return commitsOn(engine.store.Path()) }

	runs := 30
	if v := os.Getenv("LCX_T1_PERF_RUNS"); v != "" {
		fmt.Sscan(v, &runs)
	}
	var stepMs, startMs []float64
	var commits, requests int64
	// stepCommits[k]: commits between request k and request k+1 summed over runs (k = 0 is before the
	// first request: the Start; the last entry is from the final request to the run's end).
	stepNames := []string{"start", "after write_file", "after shell", "after read_file", "after note_write", "after shell 20k lines", "final"}
	stepCommits := make([]int64, len(stepNames))
	stepKindMs := make([][]float64, len(stepNames))
	for i := 0; i < runs+1; i++ {
		handle, err := engine.NewSession(ctx, SessionOptions{Goal: fmt.Sprintf("perf run %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		p := &perfProvider{count: count}
		before := count()
		begin := time.Now()
		result, err := engine.runAgentWith(ctx, handle.ID, p, AgentOptions{Policy: "full", MaxSteps: 8})
		if err != nil || result.Status != "completed" {
			t.Fatalf("run %d = %+v err=%v", i, result, err)
		}
		if i == 0 {
			continue // warm-up: first git, first statements
		}
		end := count()
		commits += end - before
		requests += int64(p.requests)
		if len(p.commitAt) != len(stepNames)-1 {
			t.Fatalf("run %d made %d requests, want %d", i, len(p.commitAt), len(stepNames)-1)
		}
		stepCommits[0] += p.commitAt[0] - before
		for j := 1; j < len(p.commitAt); j++ {
			stepCommits[j] += p.commitAt[j] - p.commitAt[j-1]
		}
		stepCommits[len(stepNames)-1] += end - p.commitAt[len(p.commitAt)-1]
		startMs = append(startMs, float64(p.arrive[0].Sub(begin).Microseconds())/1000)
		for j := 1; j < len(p.arrive); j++ {
			ms := float64(p.arrive[j].Sub(p.leave[j-1]).Microseconds()) / 1000
			stepMs = append(stepMs, ms)
			stepKindMs[j] = append(stepKindMs[j], ms)
		}
	}
	sort.Float64s(stepMs)
	sort.Float64s(startMs)
	perStep := map[string]float64{}
	perStepMs := map[string]float64{}
	for k, name := range stepNames {
		perStep[name] = float64(stepCommits[k]) / float64(runs)
		if len(stepKindMs[k]) > 0 {
			sort.Float64s(stepKindMs[k])
			perStepMs[name] = percentile(stepKindMs[k], 0.5)
		}
	}
	out, _ := json.Marshal(map[string]any{
		"runs":               runs,
		"requestsPerRun":     float64(requests) / float64(runs),
		"commitsPerRun":      float64(commits) / float64(runs),
		"commitsPerRequest":  float64(commits) / float64(requests),
		"commitsPerStep":     perStep,
		"stepMedianMsByStep": perStepMs,
		"stepMedianMs":       percentile(stepMs, 0.5),
		"stepP95Ms":          percentile(stepMs, 0.95),
		"startToFirstMedian": percentile(startMs, 0.5),
	})
	t.Logf("T1PERF %s", out)
}
