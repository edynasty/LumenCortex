package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// pushMessageProvider answers a scripted run and records each request's rendered messages.
type pushMessageProvider struct {
	mu   sync.Mutex
	reqs [][]protocol.Message
	// gate, when set, holds the first request until closed: the worker is busy meanwhile.
	gate chan struct{}
}

func (*pushMessageProvider) Model() string { return "push-message" }

func (p *pushMessageProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req.Messages)
	n := len(p.reqs)
	gate := p.gate
	p.mu.Unlock()
	if n == 1 && gate != nil {
		select {
		case <-ctx.Done():
			return protocol.ProviderResponse{}, ctx.Err()
		case <-gate:
		}
	}
	if n == 1 {
		// One tool call keeps the run going, so a message sent while the run works has a later window.
		body, _ := json.Marshal(map[string]string{"path": "work.txt", "content": "step one"})
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "w1", Name: "write_file", Arguments: body},
		}}, FinishReason: "tool_calls"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
}

func (p *pushMessageProvider) requests() [][]protocol.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]protocol.Message(nil), p.reqs...)
}

func rendered(rows []protocol.Message) string {
	var b strings.Builder
	for _, row := range rows {
		b.WriteString(row.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// workerAndRoot creates a root and an execution child of it, the shape the Hub's message scope needs.
func workerAndRoot(t *testing.T, e *Engine, p protocol.Provider) (root, worker string) {
	t.Helper()
	root = newParentSession(t, e, p)
	worker = newParentSession(t, e, p)
	if err := e.store.AddRelation(context.Background(), session.Relation{ParentSessionID: root, ChildSessionID: worker, Kind: RelationExecution}); err != nil {
		t.Fatal(err)
	}
	return root, worker
}

// TestBusyWorkerReceivesMessageWithoutInbox pins HUB-A2: a message another agent sends is written into the
// recipient's next request window as a bounded aside, without the recipient calling worker_inbox.
func TestBusyWorkerReceivesMessageWithoutInbox(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &pushMessageProvider{gate: make(chan struct{})}
	root, worker := workerAndRoot(t, e, p)
	ctx := context.Background()
	if _, err := sup.Start(ctx, worker, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	// The worker is inside its first request; send while it is busy.
	deadline := time.Now().Add(5 * time.Second)
	for len(p.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(p.requests()) != 1 {
		t.Fatal("the worker never reached its first request")
	}
	if _, err := e.SendWorkerMessage(ctx, root, root, worker, "m1", "API shape changed in shared pkg"); err != nil {
		t.Fatal(err)
	}
	close(p.gate)
	waitInactive(t, sup, worker)
	requests := p.requests()
	if len(requests) < 2 {
		t.Fatalf("requests %d", len(requests))
	}
	if strings.Contains(rendered(requests[0]), "API shape changed") {
		t.Fatal("the message reached the request that was already built")
	}
	if !strings.Contains(rendered(requests[1]), "API shape changed") {
		t.Fatal("the message did not reach the running worker's next request")
	}
	// Delivery is recorded durably: delivered, with the transcript row and the run that wrote it.
	page, err := e.store.PendingWorkerMessages(ctx, root, worker, "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) == 0 {
		t.Fatal("the message is still queued after delivery")
	}
	if page[0].Delivery != "delivered" || page[0].DeliveredSeq < 0 || page[0].DeliveredRun == "" {
		t.Fatalf("delivery record %+v", page[0])
	}
	// An acknowledgement stops any redelivery.
	if _, err := e.AckWorkerMessages(ctx, root, worker, page[0].Seq); err != nil {
		t.Fatal(err)
	}
	again, err := e.store.PendingWorkerMessages(ctx, root, worker, "other-run", 8)
	if err != nil || len(again) != 0 {
		t.Fatalf("acked message is still pending: %+v %v", again, err)
	}
}

// TestUnackedWorkerMessageRedeliversOncePerRun pins the bounded redelivery: a delivered but unacked
// message is written again at the next run's first window, and stops after its attempts are spent.
func TestUnackedWorkerMessageRedeliversOncePerRun(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &pushMessageProvider{}
	root, worker := workerAndRoot(t, e, p)
	ctx := context.Background()
	if _, err := e.SendWorkerMessage(ctx, root, root, worker, "m1", "please re-check the API"); err != nil {
		t.Fatal(err)
	}
	var lastSeq int64
	for run := 1; run <= session.MaxWorkerMessageAttempts; run++ {
		if _, err := sup.Start(ctx, worker, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 1}); err != nil {
			t.Fatal(err)
		}
		waitInactive(t, sup, worker)
		state, err := e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false)
		if err != nil || len(state.Messages) != 1 {
			t.Fatalf("read after run %d: %+v %v", run, state, err)
		}
		message := state.Messages[0]
		if message.Attempts != run || message.Delivery != session.WorkerMessageDelivered {
			t.Fatalf("after run %d: %+v", run, message)
		}
		if message.DeliveredSeq <= lastSeq {
			t.Fatalf("run %d wrote no new aside: seq %d after %d", run, message.DeliveredSeq, lastSeq)
		}
		lastSeq = message.DeliveredSeq
	}
	// The first run's request carried it as an aside: the recipient never called worker_inbox.
	if !strings.Contains(rendered(p.requests()[0]), "please re-check the API") {
		t.Fatal("the first run's request does not carry the message")
	}
	// An explicit inbox read does not spend an attempt; the message stays readable.
	if page, err := e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false); err != nil || len(page.Messages) != 1 {
		t.Fatalf("inbox read %+v %v", page, err)
	}
	// The bound stops a further redelivery: no new aside and no new attempt.
	if _, err := sup.Start(ctx, worker, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, worker)
	final, err := e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false)
	if err != nil || len(final.Messages) != 1 {
		t.Fatalf("after the bounded run %+v %v", final, err)
	}
	if final.Messages[0].Attempts != session.MaxWorkerMessageAttempts || final.Messages[0].DeliveredSeq != lastSeq {
		t.Fatalf("redelivery exceeded its bounds: %+v", final.Messages[0])
	}
}

// TestWorkerMessageAckSurvivesRestart pins the durability of delivery state: a queued message and an ack
// are read from the store after a reopen.
func TestWorkerMessageAckSurvivesRestart(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	dir := t.TempDir()
	e, err := Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	p := &pushMessageProvider{}
	root, worker := workerAndRoot(t, e, p)
	ctx := context.Background()
	message, err := e.SendWorkerMessage(ctx, root, root, worker, "m1", "survive this")
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	e, err = Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	pending, err := e.store.PendingWorkerMessages(ctx, root, worker, "run-after-restart", 8)
	if err != nil || len(pending) != 1 || pending[0].Body != "survive this" {
		t.Fatalf("after restart %+v %v", pending, err)
	}
	if _, err := e.AckWorkerMessages(ctx, root, worker, message.Seq); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e, err = Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	none, err := e.store.PendingWorkerMessages(ctx, root, worker, "another-run", 8)
	if err != nil || len(none) != 0 {
		t.Fatalf("ack did not survive restart: %+v %v", none, err)
	}
}
