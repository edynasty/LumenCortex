package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

type executionRetryProvider struct {
	mu        sync.Mutex
	responses []protocol.ProviderResponse
	requests  int
	fail      bool
}

func (*executionRetryProvider) Model() string { return "execution-retry-fixture" }
func (p *executionRetryProvider) Complete(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests++
	if p.fail {
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, Err: errors.New("offline unavailable")}
	}
	if len(p.responses) == 0 {
		return finalResponse("finished"), nil
	}
	r := p.responses[0]
	p.responses = p.responses[1:]
	return r, nil
}

func TestReadOnlyChildInheritsUnlimitedOrExplicitSteps(t *testing.T) {
	for _, limit := range []int{10, 0, -1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			engine, _ := notebookEngine(t)
			p := &executionRetryProvider{}
			for i := range 140 {
				p.responses = append(p.responses, toolCallResponse(fmt.Sprint(i), "list_dir", `{"path":"."}`))
			}
			p.responses = append(p.responses, finalResponse("inspection delivered"))
			s, err := NewRunSupervisor(engine)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			parent := newParentSession(t, engine, p)
			c := &subagentController{supervisor: s, parentID: parent, parentOpts: AgentOptions{MaxSteps: limit}}
			value, err := c.Spawn(context.Background(), "inspect workspace", "")
			if err != nil {
				t.Fatal(err)
			}
			id := value.(SubagentNode).SessionID
			waitInactive(t, s, id)
			row, err := engine.store.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			p.mu.Lock()
			requests := p.requests
			p.mu.Unlock()
			if limit == 10 {
				if row.Status != "interrupted" || requests != 10 || !strings.Contains(string(row.Error), "max_steps") {
					t.Fatalf("limited child: status=%s requests=%d cause=%s", row.Status, requests, row.Error)
				}
			} else if row.Status != "completed" || row.Final == nil || *row.Final != "inspection delivered" || requests != 141 {
				t.Fatalf("long child: %+v requests=%d", row, requests)
			}
		})
	}
}

func TestImmediateChildrenInheritDisabledRetry(t *testing.T) {
	for _, kind := range []string{"explore", "worker"} {
		t.Run(kind, func(t *testing.T) {
			engine, id := notebookEngine(t)
			parent := AgentOptions{Retry: &config.Retry{Disabled: true}}
			var opts AgentOptions
			if kind == "explore" {
				opts = (&subagentController{parentOpts: parent}).childOptions("", "explore")
			} else {
				opts = executionOptions(parent, "", nil, "", "worker")
			}
			// Mutating the parent's live options cannot re-enable a child already prepared from them.
			parent.Retry.Disabled = false
			p := &executionRetryProvider{fail: true}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := engine.runAgentWith(ctx, id, p, opts)
			if err == nil || result.Status != "interrupted" || p.requests != 1 || result.Spend.ModelRequests != 1 {
				t.Fatalf("child retry policy: result=%+v requests=%d err=%v", result, p.requests, err)
			}
		})
	}
}

func TestDeliveryVerifiesAllRequestedFilesAfterExecution(t *testing.T) {
	engine, id := notebookEngine(t)
	p := &executionRetryProvider{}
	d := &DeliveryContract{}
	for i := range 4 {
		name := fmt.Sprintf("result-%d.txt", i)
		d.Files = append(d.Files, DeliveryFile{Path: name})
		p.responses = append(p.responses, toolCallResponse(fmt.Sprint(i), "write_file", fmt.Sprintf(`{"path":%q,"content":"verified artifact"}`, name)))
	}
	p.responses = append(p.responses, finalResponse("all delivered"))
	result, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Policy: "workspace", Delivery: d, MaxSteps: 16})
	if err != nil || result.Status != "completed" || result.Final != "all delivered" || result.Delivery.Status != DeliveryVerified {
		t.Fatalf("progressive delivery: %+v %v", result, err)
	}
	workspace, err := engine.agentWorkspace(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range d.Files {
		raw, err := os.ReadFile(filepath.Join(workspace, f.Path))
		if err != nil || string(raw) != "verified artifact" {
			t.Fatalf("artifact %s: %q %v", f.Path, raw, err)
		}
	}
}

func TestDeliveryIgnoresUnrelatedSuccessfulActivity(t *testing.T) {
	engine, id := notebookEngine(t)
	p := &executionRetryProvider{}
	for i := range 3 {
		p.responses = append(p.responses, toolCallResponse(fmt.Sprint(i), "write_file", fmt.Sprintf(`{"path":"unrelated.txt","content":%q}`, fmt.Sprint(i))), finalResponse("done"))
	}
	result, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Policy: "workspace", Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "required.txt"}}}, MaxSteps: 10})
	row, getErr := engine.store.Get(context.Background(), id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if err != nil || result.Status != "completed" || p.requests != 2 || result.Delivery.Status != DeliveryFailed || len(row.Error) != 0 {
		t.Fatalf("unrelated activity: %+v requests=%d cause=%s err=%v", result, p.requests, row.Error, err)
	}
}
