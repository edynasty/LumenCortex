package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S7 transaction latency (plan §5 T1-S7 item 4, budget: admission or commit p99 <= 50 ms). One
// session already holding 2,000 transcript rows and a Task State runs N cycles of the T1 write path:
// AdmitRequest, AppendAnswered with one tool call, AdmitExecution, CommitToolResult (tool row, Task
// State version, post record and two file edges). A second goroutine reads the transcript the whole
// time, sharing the store's single connection as the loop and the event readers do. Each Store call is
// one write transaction (TestEachStoreWriteIsExactlyOneTransaction), so its wall time is the
// transaction's, including the wait for the connection.
//
// A measurement, not an assertion: it runs only with LCX_T1_PERF=1 and prints one JSON line.
func TestT1StoreTransactionLatency(t *testing.T) {
	if os.Getenv("LCX_T1_PERF") != "1" {
		t.Skip("measurement only: set LCX_T1_PERF=1")
	}
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	filler := strings.Repeat("x", 512)
	for i := 0; i < 2000; i++ {
		gapRow(t, store, "s", "user", protocol.Message{Content: fmt.Sprintf("row %d %s", i, filler)})
	}
	version, err := store.PutTaskState(ctx, "s", TaskStateWrite{Base: 0, Reason: "start", JSON: json.RawMessage(`{"windowStartSeq":0}`)})
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.EachMessageFrom(ctx, "s", 1900, func(Message) error { return nil })
		}
	}()

	n := 500
	timings := map[string][]float64{}
	timed := func(name string, fn func() error) {
		t.Helper()
		begin := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		timings[name] = append(timings[name], float64(time.Since(begin).Microseconds())/1000)
	}
	for i := 0; i < n; i++ {
		req, exec := fmt.Sprintf("r%d", i), fmt.Sprintf("e%d", i)
		a := mainAdmission("s", req)
		a.FirstSeq, a.LastSeq = i64(0), i64(int64(2000+3*i))
		timed("AdmitRequest", func() error { _, err := store.AdmitRequest(ctx, a); return err })
		timed("AppendAnswered", func() error {
			_, err := store.AppendAnswered(ctx, "s", req, protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "call", Name: "shell", Arguments: json.RawMessage(`{"command":"true"}`)}}})
			return err
		})
		x := execAdmission("s", exec, req)
		timed("AdmitExecution", func() error { return store.AdmitExecution(ctx, x) })
		c := toolCommit("s", exec, OutcomeComplete, CoverageComplete, fileEdge(fmt.Sprintf("a%d.go", i)), fileEdge(fmt.Sprintf("b%d.go", i)))
		c.Edges[0].SourceRef, c.Edges[1].SourceRef = req, req
		c.TaskState = &TaskStateWrite{Base: version, Reason: "tool:shell", JSON: json.RawMessage(fmt.Sprintf(`{"windowStartSeq":%d}`, i))}
		timed("CommitToolResult", func() error {
			_, v, err := store.CommitToolResult(ctx, c)
			version = v
			return err
		})
	}
	close(stop)
	wg.Wait()

	out := map[string]any{"cycles": n, "transcriptRows": 2000 + 3*n}
	for name, ms := range timings {
		sort.Float64s(ms)
		out[name] = map[string]float64{"p50": ms[len(ms)/2], "p99": ms[(len(ms)*99)/100], "max": ms[len(ms)-1]}
	}
	raw, _ := json.Marshal(out)
	t.Logf("T1TX %s", raw)
}
