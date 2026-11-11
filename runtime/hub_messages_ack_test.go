package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// inboxReadingProvider answers the first request with a worker_inbox call and stops on every later one.
type inboxReadingProvider struct {
	mu   sync.Mutex
	reqs [][]protocol.Message
}

func (*inboxReadingProvider) Model() string { return "inbox-reading" }

func (p *inboxReadingProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req.Messages)
	n := len(p.reqs)
	p.mu.Unlock()
	if n == 1 {
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "inbox1", Name: "worker_inbox", Arguments: json.RawMessage(`{}`)},
		}}, FinishReason: "tool_calls"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
}

func (p *inboxReadingProvider) requests() [][]protocol.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]protocol.Message(nil), p.reqs...)
}

// TestPushedMessageReadWithInboxIsAckedAndNotRedelivered pins the acknowledgement of an explicit read: a
// message pushed into the request window that the recipient then reads with worker_inbox is acked, its
// read is recorded, and the next run does not write it again.
func TestPushedMessageReadWithInboxIsAckedAndNotRedelivered(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &inboxReadingProvider{}
	root, worker := workerAndRoot(t, e, p)
	ctx := context.Background()
	if _, err := e.SendWorkerMessage(ctx, root, root, worker, "m1", "PUSHED-AND-READ"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx, worker, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, worker)
	reqs := p.requests()
	if len(reqs) < 2 || !strings.Contains(rendered(reqs[0]), "PUSHED-AND-READ") {
		t.Fatalf("the message was not pushed into the first request (%d requests)", len(reqs))
	}
	read := false
	for _, row := range reqs[1] {
		if row.Role == "tool" && row.ToolCallID == "inbox1" && strings.Contains(row.Content, "PUSHED-AND-READ") {
			read = true
		}
	}
	if !read {
		t.Fatal("worker_inbox did not return the message")
	}
	page, err := e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("read %+v %v", page, err)
	}
	if m := page.Messages[0]; m.Delivery != session.WorkerMessageAcked || m.AckedAt == nil || m.ReceivedAt == nil {
		t.Errorf("after push and an inbox read: delivery=%s ackedAt=%v receivedAt=%v", m.Delivery, m.AckedAt, m.ReceivedAt)
	}
	if _, err := sup.Start(ctx, worker, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, worker)
	reqs = p.requests()
	if len(reqs) < 3 {
		t.Fatalf("the second run made no request (%d requests)", len(reqs))
	}
	// The second run's request carries the session's transcript, which holds the first run's aside and
	// inbox result; a redelivery would add another copy and count another attempt.
	if before, after := strings.Count(rendered(reqs[1]), "PUSHED-AND-READ"), strings.Count(rendered(reqs[2]), "PUSHED-AND-READ"); after != before {
		t.Errorf("the next run's request carries the message %d times, the transcript before it %d", after, before)
	}
	page, err = e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("read %+v %v", page, err)
	}
	if m := page.Messages[0]; m.Attempts != 1 || m.Delivery != session.WorkerMessageAcked {
		t.Errorf("a message the recipient read with worker_inbox was delivered again: attempts=%d delivery=%s", m.Attempts, m.Delivery)
	}
}

// TestPushedMessageIsNotRecordedAsRead pins the meaning of receivedAt: a push into the request window is
// a delivery, not the recipient's explicit read.
func TestPushedMessageIsNotRecordedAsRead(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	root, worker := workerAndRoot(t, e, &finalProvider{})
	ctx := context.Background()
	if _, err := e.SendWorkerMessage(ctx, root, root, worker, "m1", "PUSHED-ONLY"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx, worker, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, worker)
	page, err := e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("read %+v %v", page, err)
	}
	if m := page.Messages[0]; m.Delivery != session.WorkerMessageDelivered || m.ReceivedAt != nil || m.AckedAt != nil {
		t.Errorf("after a push only: delivery=%s receivedAt=%v ackedAt=%v", m.Delivery, m.ReceivedAt, m.AckedAt)
	}
}
