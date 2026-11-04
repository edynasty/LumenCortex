package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/jobs"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// testJobs is the job controller one run's tools see, over a real queue and a real database. It is the
// same binding the runtime gives a run: the run's session, run id and workspace, with a job of another
// session refused.
type testJobs struct {
	manager   *jobs.Manager
	sessionID string
	runID     string
	workspace string
}

func (j testJobs) Submit(ctx context.Context, command string, timeout time.Duration) (JobInfo, error) {
	job, err := j.manager.Submit(ctx, jobs.Spec{
		Command: command, Workspace: j.workspace, SessionID: j.sessionID, RunID: j.runID,
		ToolCallID: protocol.ToolCallID(ctx), Source: session.JobSourceAgent, Timeout: timeout,
	})
	if err != nil {
		return JobInfo{}, err
	}
	return jobInfo(job), nil
}

func (j testJobs) Get(ctx context.Context, id string) (JobInfo, error) {
	job, err := j.own(ctx, id)
	if err != nil {
		return JobInfo{}, err
	}
	return jobInfo(job), nil
}

func (j testJobs) Output(ctx context.Context, id string) (JobOutput, error) {
	if _, err := j.own(ctx, id); err != nil {
		return JobOutput{}, err
	}
	out, err := j.manager.Output(ctx, id, 32<<10)
	if err != nil {
		return JobOutput{}, err
	}
	return JobOutput{
		Stdout: JobStream{Head: out.Stdout.Head, Tail: out.Stdout.Tail, Total: out.Stdout.Total, Truncated: out.Stdout.Truncated, Omitted: out.Stdout.Omitted},
		Stderr: JobStream{Head: out.Stderr.Head, Tail: out.Stderr.Tail, Total: out.Stderr.Total, Truncated: out.Stderr.Truncated, Omitted: out.Stderr.Omitted},
	}, nil
}

func (j testJobs) Cancel(ctx context.Context, id string) (JobInfo, error) {
	if _, err := j.own(ctx, id); err != nil {
		return JobInfo{}, err
	}
	job, err := j.manager.Cancel(ctx, id)
	if err != nil {
		return JobInfo{}, err
	}
	return jobInfo(job), nil
}

func (j testJobs) List(ctx context.Context, limit int) ([]JobInfo, error) {
	found, err := j.manager.List(ctx, session.JobFilter{SessionID: j.sessionID, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]JobInfo, 0, len(found))
	for _, job := range found {
		out = append(out, jobInfo(job))
	}
	return out, nil
}

func (j testJobs) Active(ctx context.Context) ([]JobInfo, error) {
	found, err := j.manager.ActiveForRun(ctx, j.runID)
	if err != nil {
		return nil, err
	}
	out := make([]JobInfo, 0, len(found))
	for _, job := range found {
		out = append(out, jobInfo(job))
	}
	return out, nil
}

func (j testJobs) Wait(ctx context.Context, ids []string, timeout time.Duration) (JobInfo, bool, error) {
	for _, id := range ids {
		if _, err := j.own(ctx, id); err != nil {
			return JobInfo{}, false, err
		}
	}
	job, ok, err := j.manager.Wait(ctx, ids, timeout)
	if err != nil {
		return JobInfo{}, false, err
	}
	return jobInfo(job), ok, nil
}

func (j testJobs) own(ctx context.Context, id string) (session.Job, error) {
	job, err := j.manager.Get(ctx, id)
	if err != nil {
		return session.Job{}, err
	}
	if job.SessionID != j.sessionID {
		return session.Job{}, fmt.Errorf("job %s belongs to another session", id)
	}
	return job, nil
}

func jobInfo(job session.Job) JobInfo {
	return JobInfo{
		ID: job.ID, State: job.State, Command: job.Command, ExitCode: job.ExitCode, Error: job.Error,
		CreatedAt: job.CreatedAt, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
	}
}

// jobSet is a full-policy toolset bound to one run's job queue.
func jobSet(t *testing.T, workspace string, controller func(*jobs.Manager) JobController) (*Set, *jobs.Manager) {
	t.Helper()
	stateDir := t.TempDir()
	store, err := session.Open(filepath.Join(stateDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// A job's session_id is a foreign key: the sessions these tools act for exist.
	for _, id := range []string{"session_a", "session_b"} {
		if err := store.Create(context.Background(), session.Session{ID: id, Goal: "jobs test"}); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := jobs.New(jobs.Options{
		Store:    store,
		StateDir: stateDir,
		Limits: func() config.Execution {
			return config.Execution{MaxConcurrentJobs: 2, MaxQueuedJobs: 32, JobOutputBytes: 64 << 10, JobRetentionDays: 7}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	set, err := New(Options{Workspace: workspace, Policy: PolicyFull, Jobs: controller(manager)})
	if err != nil {
		t.Fatal(err)
	}
	return set, manager
}

func decode(t *testing.T, result protocol.ToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatalf("content %q: %v", result.Content, err)
	}
	return out
}

func TestShellBackgroundModeSubmitsAndReports(t *testing.T) {
	workspace := t.TempDir()
	set, _ := jobSet(t, workspace, func(manager *jobs.Manager) JobController {
		return testJobs{manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace}
	})
	result, err := set.Execute(context.Background(), "bash", map[string]any{
		"command": "echo background-output", "mode": "background",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.MutatesWorkspace {
		t.Fatalf("result=%+v", result)
	}
	out := decode(t, result)
	if out["mode"] != "background" || out["state"] != session.JobQueued || out["jobId"] == "" {
		t.Fatalf("background submit=%v", out)
	}
	id, _ := out["jobId"].(string)
	// The job runs on the queue; job_status reports it once it is done.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, err := set.Execute(context.Background(), "job_status", map[string]any{"job_id": id}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if status.MutatesWorkspace {
			body := decode(t, status)
			if body["state"] != session.JobCompleted {
				t.Fatalf("terminal status=%v", body)
			}
			stdout, _ := body["stdout"].(map[string]any)
			if !strings.Contains(fmt.Sprint(stdout["head"]), "background-output") {
				t.Fatalf("stdout=%v", stdout)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
}

func TestJobWaitReturnsTheFinishedJob(t *testing.T) {
	workspace := t.TempDir()
	set, _ := jobSet(t, workspace, func(manager *jobs.Manager) JobController {
		return testJobs{manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace}
	})
	submitted, err := set.Execute(context.Background(), "bash", map[string]any{
		"command": "sleep 0.2; exit 3", "mode": "background",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := decode(t, submitted)["jobId"].(string)
	result, err := set.Execute(context.Background(), "job_wait", map[string]any{
		"job_ids": []any{id}, "timeout_sec": float64(20),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, result)
	if out["timedOut"] != false || out["finished"] != id || out["state"] != session.JobFailed {
		t.Fatalf("wait=%v", out)
	}
	if code, ok := out["exitCode"].(float64); !ok || int(code) != 3 {
		t.Fatalf("exit code=%v", out["exitCode"])
	}
	if !result.MutatesWorkspace {
		t.Fatal("a wait that reports a terminal job must count as a possible workspace change")
	}
}

func TestJobWaitTimesOutWithoutCancellingTheJob(t *testing.T) {
	workspace := t.TempDir()
	set, manager := jobSet(t, workspace, func(manager *jobs.Manager) JobController {
		return testJobs{manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace}
	})
	submitted, err := set.Execute(context.Background(), "bash", map[string]any{
		"command": "sleep 30", "mode": "background",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := decode(t, submitted)["jobId"].(string)
	started := time.Now()
	result, err := set.Execute(context.Background(), "job_wait", map[string]any{
		"job_ids": []any{id}, "timeout_sec": float64(1),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("job_wait took %s, want about its 1s timeout", elapsed)
	}
	out := decode(t, result)
	if out["timedOut"] != true || result.MutatesWorkspace {
		t.Fatalf("wait=%v mutates=%v", out, result.MutatesWorkspace)
	}
	job, err := manager.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != session.JobRunning {
		t.Fatalf("job state=%q after a timed-out wait, want running", job.State)
	}
	if _, err := set.Execute(context.Background(), "job_cancel", map[string]any{"job_id": id}, nil); err != nil {
		t.Fatal(err)
	}
}

// job_wait never waits past the run's tool deadline: the reserve belongs to the answer.
func TestJobWaitIsBoundedByTheToolDeadline(t *testing.T) {
	workspace := t.TempDir()
	set, manager := jobSet(t, workspace, func(manager *jobs.Manager) JobController {
		return testJobs{manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace}
	})
	submitted, err := set.Execute(context.Background(), "bash", map[string]any{
		"command": "sleep 30", "mode": "background",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := decode(t, submitted)["jobId"].(string)
	ctx := protocol.WithToolDeadline(context.Background(), time.Now().Add(300*time.Millisecond))
	started := time.Now()
	result, err := set.Execute(ctx, "job_wait", map[string]any{
		"job_ids": []any{id}, "timeout_sec": float64(600),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("job_wait ran %s past a 300ms tool deadline", elapsed)
	}
	if out := decode(t, result); out["timedOut"] != true {
		t.Fatalf("wait=%v", out)
	}
	if _, err := set.Execute(context.Background(), "job_cancel", map[string]any{"job_id": id}, nil); err != nil {
		t.Fatal(err)
	}
	_ = manager
}

func TestJobWaitWithoutIDsWaitsForTheRunsActiveJobs(t *testing.T) {
	workspace := t.TempDir()
	set, _ := jobSet(t, workspace, func(manager *jobs.Manager) JobController {
		return testJobs{manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace}
	})
	if result, err := set.Execute(context.Background(), "job_wait", map[string]any{}, nil); err != nil || !result.OK {
		t.Fatalf("idle wait: %+v err=%v", result, err)
	}
	submitted, err := set.Execute(context.Background(), "bash", map[string]any{
		"command": "sleep 0.2", "mode": "background",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := decode(t, submitted)["jobId"].(string)
	result, err := set.Execute(context.Background(), "job_wait", map[string]any{"timeout_sec": float64(20)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := decode(t, result)
	if out["finished"] != id || out["state"] != session.JobCompleted {
		t.Fatalf("wait=%v", out)
	}
}

func TestJobListAndCancel(t *testing.T) {
	workspace := t.TempDir()
	set, _ := jobSet(t, workspace, func(manager *jobs.Manager) JobController {
		return testJobs{manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace}
	})
	submitted, err := set.Execute(context.Background(), "bash", map[string]any{
		"command": "sleep 30", "mode": "background",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := decode(t, submitted)["jobId"].(string)
	list, err := set.Execute(context.Background(), "job_list", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := decode(t, list)
	entries, _ := body["jobs"].([]any)
	if len(entries) != 1 {
		t.Fatalf("job_list=%v", body)
	}
	entry, _ := entries[0].(map[string]any)
	if entry["jobId"] != id || entry["command"] != "sleep 30" {
		t.Fatalf("job_list entry=%v", entry)
	}
	cancelled, err := set.Execute(context.Background(), "job_cancel", map[string]any{"job_id": id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := decode(t, cancelled); out["state"] != session.JobCancelled || out["cancelled"] != true {
		t.Fatalf("cancel=%v", out)
	}
}

func TestJobToolsRefuseAnotherSessionsJob(t *testing.T) {
	workspace := t.TempDir()
	managerState := t.TempDir()
	store, err := session.Open(filepath.Join(managerState, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"session_a", "session_b"} {
		if err := store.Create(context.Background(), session.Session{ID: id, Goal: "jobs test"}); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := jobs.New(jobs.Options{
		Store:    store,
		StateDir: managerState,
		Limits: func() config.Execution {
			return config.Execution{MaxConcurrentJobs: 1, MaxQueuedJobs: 8, JobOutputBytes: 64 << 10, JobRetentionDays: 7}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	other, err := manager.Submit(context.Background(), jobs.Spec{
		Command: "sleep 30", Workspace: workspace, SessionID: "session_b", Source: session.JobSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Cancel(context.Background(), other.ID)
	set, err := New(Options{Workspace: workspace, Policy: PolicyFull, Jobs: testJobs{
		manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"job_status", "job_cancel"} {
		_, err := set.Execute(context.Background(), tool, map[string]any{"job_id": other.ID}, nil)
		if err == nil || !strings.Contains(err.Error(), "another session") {
			t.Fatalf("%s: err=%v, want a refusal naming the other session", tool, err)
		}
	}
	if _, err := set.Execute(context.Background(), "job_wait", map[string]any{"job_ids": []any{other.ID}}, nil); err == nil {
		t.Fatal("job_wait accepted another session's job")
	}
	// The other session's own tools see it.
	own, err := New(Options{Workspace: workspace, Policy: PolicyFull, Jobs: testJobs{
		manager: manager, sessionID: "session_b", runID: "run_b", workspace: workspace,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := own.Execute(context.Background(), "job_status", map[string]any{"job_id": other.ID}, nil); err != nil || !result.OK {
		t.Fatalf("owner status: %+v err=%v", result, err)
	}
}

// Without a job controller the shell tool has no background mode and no job tools are offered.
func TestNoJobToolsWithoutAController(t *testing.T) {
	set, err := New(Options{Workspace: t.TempDir(), Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range set.Specs(nil) {
		switch spec.Name {
		case "bash":
			properties, _ := spec.Parameters["properties"].(map[string]any)
			if _, ok := properties["mode"]; ok {
				t.Fatal("shell offers a background mode without a job queue")
			}
		case "job_status", "job_wait", "job_cancel", "job_list":
			t.Fatalf("%s registered without a job queue", spec.Name)
		}
	}
}

// A background job is an exec tool: a policy that withholds commands withholds the job tools too.
func TestJobToolsAreExecPermission(t *testing.T) {
	workspace := t.TempDir()
	set, _ := jobSet(t, workspace, func(manager *jobs.Manager) JobController {
		return testJobs{manager: manager, sessionID: "session_a", runID: "run_a", workspace: workspace}
	})
	seen := map[string]bool{}
	for _, spec := range set.Specs(nil) {
		if spec.Name == "job_status" || spec.Name == "job_wait" || spec.Name == "job_cancel" || spec.Name == "job_list" {
			seen[spec.Name] = true
			if spec.Permission != "exec" {
				t.Fatalf("%s permission=%q, want exec", spec.Name, spec.Permission)
			}
		}
	}
	if len(seen) != 4 {
		t.Fatalf("registered job tools=%v", seen)
	}
	readOnly, err := New(Options{Workspace: workspace, Policy: PolicyWorkspace, Jobs: testJobs{
		manager: nil, sessionID: "session_a", runID: "run_a", workspace: workspace,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range readOnly.Specs(nil) {
		if strings.HasPrefix(spec.Name, "job_") {
			t.Fatalf("%s offered under the workspace policy", spec.Name)
		}
	}
}
