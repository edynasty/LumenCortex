package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// scriptedJobs reports active jobs from a list of answers, one per lookup; past the end it repeats the
// last one.
type scriptedJobs struct {
	answers [][]ActiveJob
	err     error
	calls   int
	runIDs  []string
}

func (j *scriptedJobs) ActiveJobs(_ context.Context, runID string) ([]ActiveJob, error) {
	j.runIDs = append(j.runIDs, runID)
	defer func() { j.calls++ }()
	if j.err != nil {
		return nil, j.err
	}
	if j.calls < len(j.answers) {
		return j.answers[j.calls], nil
	}
	return j.answers[len(j.answers)-1], nil
}

// A final answer while the run's job is running is refused with a correction naming the job; once the
// job has ended the next answer completes the run.
func TestJobGateRefusesFinalWhileARunJobIsActive(t *testing.T) {
	store := newState("jg")
	jobs := &scriptedJobs{answers: [][]ActiveJob{
		{{ID: "job_1", State: "running", Command: "make test"}},
		nil,
	}}
	provider := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return answer("done"), nil
	}}
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: okTools(), Jobs: jobs}).Run(context.Background(), "jg",
		Options{Session: testRef, MaxSteps: 5, RunID: "run-j"})
	if err != nil {
		t.Fatal(err)
	}
	assertExit(t, store, result, StatusCompleted, "")
	blocked := blockedRows(store)
	if len(blocked) != 1 || !strings.Contains(blocked[0], "job_1 running: make test") || !strings.Contains(blocked[0], "job_wait") {
		t.Fatalf("corrections = %q, want one naming job_1 and job_wait", blocked)
	}
	if provider.n != 2 || jobs.runIDs[0] != "run-j" {
		t.Fatalf("requests=%d runIDs=%v, want 2 requests asked for run-j", provider.n, jobs.runIDs)
	}
}

// A repeated final answer cannot abandon live resources; the explicit host step limit ends the run.
func TestJobGateObeysExplicitStepLimit(t *testing.T) {
	store := newState("jb")
	jobs := &scriptedJobs{answers: [][]ActiveJob{{{ID: "job_1", State: "queued", Command: "sleep 999"}}}}
	provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return answer("done"), nil
	}}
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: okTools(), Jobs: jobs}).Run(context.Background(), "jb",
		Options{Session: testRef, MaxSteps: 10, RunID: "run-b"})
	if err == nil {
		t.Fatal("expected the explicit step limit")
	}
	assertExit(t, store, result, StatusInterrupted, KindMaxSteps)
	if provider.n != 10 {
		t.Fatalf("requests=%d, want 10", provider.n)
	}
}

// The job view failing is not a reason to keep the run open: the jobs are cancelled at the run's end
// anyway, so the answer completes.
func TestJobGateLookupFailureDoesNotBlock(t *testing.T) {
	store := newState("je")
	jobs := &scriptedJobs{err: errors.New("database is locked")}
	provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return answer("done"), nil
	}}
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: okTools(), Jobs: jobs}).Run(context.Background(), "je",
		Options{Session: testRef, MaxSteps: 3, RunID: "run-e"})
	if err != nil {
		t.Fatal(err)
	}
	assertExit(t, store, result, StatusCompleted, "")
	if provider.n != 1 || len(blockedRows(store)) != 0 {
		t.Fatalf("requests=%d corrections=%d, want 1 and 0", provider.n, len(blockedRows(store)))
	}
}
