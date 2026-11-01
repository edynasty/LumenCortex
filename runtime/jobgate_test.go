package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// A run that started a background job cannot complete while the job runs: the gate refuses the answer
// (bounded), the job the run left behind is cancelled when the run ends, and the next run of the session
// starts with a runtime row naming the session's open jobs.
func TestRunJobsGateCancelAndIndexRow(t *testing.T) {
	engine, _ := trustedProject(t, `{}`, false)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "build"})
	if err != nil {
		t.Fatal(err)
	}
	var corrections []string
	provider := &exitProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if last := req.Messages[len(req.Messages)-1]; last.Role == "user" && strings.HasPrefix(last.Content, "Completion refused") {
			corrections = append(corrections, last.Content)
		}
		if n == 1 {
			args, _ := json.Marshal(map[string]string{"command": "sleep 30", "mode": "background"})
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{{ID: "bg1", Name: "bash", Arguments: args}}}, FinishReason: "tool_calls"}, nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
	}}
	result, err := engine.runAgentWith(ctx, handle.ID, provider, AgentOptions{Input: "build it", Policy: "full", MaxSteps: 8})
	if err != nil || result.Status != "completed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(corrections) != 2 || !strings.Contains(corrections[0], "sleep 30") || provider.n != 4 {
		t.Fatalf("requests=%d corrections=%q, want 4 requests and 2 corrections naming the job", provider.n, corrections)
	}
	jobs, err := engine.Jobs(ctx, JobQuery{SessionID: handle.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != session.JobCancelled || jobs[0].Error != "run_ended" {
		t.Fatalf("jobs after the run = %+v, want one cancelled with run_ended", jobs)
	}

	host, err := engine.SubmitJob(ctx, JobRequest{SessionID: handle.ID, Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.CancelJob(ctx, host.ID)
	waitJobState(t, engine, host.ID, session.JobRunning, 20*time.Second)
	next := &recordingProvider{final: "checked"}
	if _, err := engine.runAgentWith(ctx, handle.ID, next, AgentOptions{Input: "carry on", Policy: "read-only", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	var row string
	for _, m := range next.requests[0].Messages {
		if strings.HasPrefix(m.Content, jobsIndexPrefix) {
			row = m.Content
		}
	}
	if !strings.Contains(row, host.ID+" running (sleep 30)") || strings.Contains(row, jobs[0].ID) {
		t.Fatalf("jobs row = %q, want the running host job and not the cancelled one", row)
	}
	if job, _ := engine.Job(ctx, host.ID); job.State != session.JobRunning {
		t.Fatalf("host job state after a run = %q: a run cancels only its own jobs", job.State)
	}
}
