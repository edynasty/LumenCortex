package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// The output bounds a job tool returns: the same head and tail the shell tool keeps, so a long build's
// log is readable without flooding the transcript.
const (
	jobHeadBytes = 8 << 10
	jobTailBytes = 24 << 10
	// jobListLimit bounds job_list: a run's recent jobs, newest first.
	jobListLimit = 20
	// maxJobWaitSeconds bounds job_wait's own timeout; the tool deadline bounds it further.
	maxJobWaitSeconds = 3600
)

// JobInfo is one background job as a tool sees it: the durable record, without the command's output.
type JobInfo struct {
	ID         string
	State      string
	Command    string
	ExitCode   *int
	Error      string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// Terminal reports whether no further transition follows the job's state.
func (j JobInfo) Terminal() bool {
	switch j.State {
	case "completed", "failed", "timed_out", "cancelled", "outcome_unknown":
		return true
	}
	return false
}

// Duration is how long the job ran, or has been running.
func (j JobInfo) Duration(now time.Time) time.Duration {
	if j.StartedAt.IsZero() {
		return 0
	}
	end := j.FinishedAt
	if end.IsZero() {
		end = now
	}
	return end.Sub(j.StartedAt)
}

// JobStream is one bounded stream of a job's output: its head, its tail, the byte total the command
// produced and whether anything between them was cut.
type JobStream struct {
	Head      string `json:"head"`
	Tail      string `json:"tail"`
	Total     int64  `json:"total"`
	Truncated bool   `json:"truncated"`
	// Omitted is how many bytes between the head and the tail were cut.
	Omitted int64 `json:"omitted"`
}

// JobOutput is a job's stdout and stderr as they stand: a running job's partial output is readable.
type JobOutput struct {
	Stdout JobStream
	Stderr JobStream
}

// JobController is the background job queue of the run's session, bound per run by the runtime. Get,
// Output and Cancel refuse a job of another session; Active is this run's queued and running jobs.
type JobController interface {
	Submit(ctx context.Context, command string, timeout time.Duration) (JobInfo, error)
	Get(ctx context.Context, id string) (JobInfo, error)
	Output(ctx context.Context, id string) (JobOutput, error)
	Cancel(ctx context.Context, id string) (JobInfo, error)
	List(ctx context.Context, limit int) ([]JobInfo, error)
	Active(ctx context.Context) ([]JobInfo, error)
	// Wait returns as soon as one of the named jobs is terminal, the timeout passes or ctx ends. An
	// empty id list waits for the run's active jobs instead.
	Wait(ctx context.Context, ids []string, timeout time.Duration) (JobInfo, bool, error)
}

// registerJobTools adds the background job tools and the shell tool's background mode. They are exec
// tools, registered exactly where shell is: a policy that withholds commands withholds these too.
func (s *Set) registerJobTools() {
	s.add(protocol.ToolSpec{
		Name: "job_status",
		Description: "Read one background job of this session: its state, exit code, duration and the head and tail " +
			"of its output (bounded; a running job's output is partial).",
		Permission: "exec",
		Parameters: obj(map[string]any{"job_id": str()}, "job_id"),
	}, s.jobStatus)
	s.add(protocol.ToolSpec{
		Name: "job_wait",
		Description: "Wait until one of the named background jobs ends, or any active job of this run when no id is " +
			"given. Bounded by timeout_sec and by the time left in the run: it never cancels the job, and a timeout " +
			"just reports that nothing ended yet.",
		Permission: "exec",
		Parameters: obj(map[string]any{
			"job_ids":     map[string]any{"type": "array", "items": str(), "maxItems": 32},
			"timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": maxJobWaitSeconds},
		}),
	}, s.jobWait)
	s.add(protocol.ToolSpec{
		Name:        "job_cancel",
		Description: "Cancel a background job of this session: a queued job never starts, a running one has its process group killed.",
		Permission:  "exec",
		Parameters:  obj(map[string]any{"job_id": str()}, "job_id"),
	}, s.jobCancel)
	s.add(protocol.ToolSpec{
		Name: "job_list",
		Description: "List this session's recent background jobs, newest first, with their states: the record of what " +
			"was started and how it ended.",
		Permission: "exec",
		Parameters: obj(map[string]any{}),
	}, s.jobList)
}

// runShellBackground submits a command as a background job and returns at once with its id. The run
// cannot deliver a final answer while its jobs are active: wait for them, read them or cancel them.
func (s *Set) runShellBackground(ctx context.Context, command string, args map[string]any) (protocol.ToolResult, error) {
	var timeout time.Duration
	if _, present := args["timeout_sec"]; present {
		seconds := intArg(args, "timeout_sec", 0)
		if seconds < 1 || seconds > 86400 {
			return protocol.ToolResult{}, errors.New("timeout_sec must be an integer between 1 and 86400")
		}
		timeout = time.Duration(seconds) * time.Second
	}
	job, err := s.jobs.Submit(ctx, command, timeout)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	content, _ := json.Marshal(map[string]any{
		"jobId": job.ID,
		"state": job.State,
		"mode":  "background",
	})
	return protocol.ToolResult{
		OK:               true,
		Content:          string(content),
		MutatesWorkspace: true,
	}, nil
}

func (s *Set) jobStatus(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	id := stringArg(args, "job_id")
	if id == "" {
		return protocol.ToolResult{}, errors.New("job_id is required")
	}
	job, err := s.jobs.Get(ctx, id)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	content, err := s.jobContent(ctx, job)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{
		OK:               true,
		Content:          content,
		MutatesWorkspace: job.Terminal(),
	}, nil
}

func (s *Set) jobWait(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	var ids []string
	if raw, ok := args["job_ids"]; ok && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return protocol.ToolResult{}, errors.New("job_ids must be an array of job ids")
		}
		for _, item := range items {
			id, _ := item.(string)
			if id == "" {
				return protocol.ToolResult{}, errors.New("job_ids must not contain an empty id")
			}
			ids = append(ids, id)
		}
	}
	timeout := 60 * time.Second
	if _, present := args["timeout_sec"]; present {
		seconds := intArg(args, "timeout_sec", 0)
		if seconds < 1 || seconds > maxJobWaitSeconds {
			return protocol.ToolResult{}, fmt.Errorf("timeout_sec must be an integer between 1 and %d", maxJobWaitSeconds)
		}
		timeout = time.Duration(seconds) * time.Second
	}
	// The run's deadline wins: job_wait must leave the run its reserve to deliver.
	if deadline, ok := protocol.ToolDeadline(ctx); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return protocol.ToolResult{
				OK:      true,
				Content: `{"timedOut":true,"reason":"the run has no time left for waiting"}`,
			}, nil
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	if len(ids) == 0 {
		active, err := s.jobs.Active(ctx)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		if len(active) == 0 {
			content, _ := json.Marshal(map[string]any{
				"timedOut": false,
				"active":   false,
				"message":  "this run has no queued or running jobs",
			})
			return protocol.ToolResult{OK: true, Content: string(content)}, nil
		}
		for _, job := range active {
			ids = append(ids, job.ID)
		}
	}
	finished, ok, err := s.jobs.Wait(ctx, ids, timeout)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	states := make([]map[string]any, 0, len(ids))
	terminal := false
	for _, id := range ids {
		job, err := s.jobs.Get(ctx, id)
		if err != nil {
			return protocol.ToolResult{}, err
		}
		entry := map[string]any{"jobId": job.ID, "state": job.State}
		if job.ExitCode != nil {
			entry["exitCode"] = *job.ExitCode
		}
		states = append(states, entry)
		if job.Terminal() {
			terminal = true
		}
	}
	sort.SliceStable(states, func(i, j int) bool { return fmt.Sprint(states[i]["jobId"]) < fmt.Sprint(states[j]["jobId"]) })
	content := map[string]any{
		"timedOut": !ok,
		"jobs":     states,
	}
	if ok {
		content["finished"] = finished.ID
		content["state"] = finished.State
		if finished.ExitCode != nil {
			content["exitCode"] = *finished.ExitCode
		}
		if finished.Error != "" {
			content["error"] = finished.Error
		}
	}
	raw, _ := json.Marshal(content)
	return protocol.ToolResult{
		OK:               true,
		Content:          string(raw),
		MutatesWorkspace: terminal,
	}, nil
}

func (s *Set) jobCancel(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	id := stringArg(args, "job_id")
	if id == "" {
		return protocol.ToolResult{}, errors.New("job_id is required")
	}
	job, err := s.jobs.Cancel(ctx, id)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	content, _ := json.Marshal(map[string]any{
		"jobId":     job.ID,
		"state":     job.State,
		"cancelled": job.State == "cancelled",
	})
	return protocol.ToolResult{OK: true, Content: string(content), MutatesWorkspace: true}, nil
}

func (s *Set) jobList(ctx context.Context, _ map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	jobs, err := s.jobs.List(ctx, jobListLimit)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	now := time.Now().UTC()
	out := make([]map[string]any, 0, len(jobs))
	for _, job := range jobs {
		entry := map[string]any{
			"jobId":      job.ID,
			"state":      job.State,
			"command":    job.Command,
			"durationMs": job.Duration(now).Milliseconds(),
		}
		if job.ExitCode != nil {
			entry["exitCode"] = *job.ExitCode
		}
		if job.Error != "" {
			entry["error"] = job.Error
		}
		out = append(out, entry)
	}
	content, _ := json.Marshal(map[string]any{"jobs": out, "count": len(out)})
	return protocol.ToolResult{OK: true, Content: string(content)}, nil
}

// jobContent is one job as JSON: the record plus the bounded head and tail of its output.
func (s *Set) jobContent(ctx context.Context, job JobInfo) (string, error) {
	out, err := s.jobs.Output(ctx, job.ID)
	if err != nil {
		return "", err
	}
	content := map[string]any{
		"jobId":      job.ID,
		"state":      job.State,
		"command":    job.Command,
		"durationMs": job.Duration(time.Now().UTC()).Milliseconds(),
		"stdout":     jobStream(out.Stdout),
		"stderr":     jobStream(out.Stderr),
	}
	if job.ExitCode != nil {
		content["exitCode"] = *job.ExitCode
	}
	if job.Error != "" {
		content["error"] = job.Error
	}
	raw, _ := json.Marshal(content)
	return string(raw), nil
}

func jobStream(stream JobStream) map[string]any {
	return map[string]any{
		"head":      stream.Head,
		"tail":      stream.Tail,
		"total":     stream.Total,
		"truncated": stream.Truncated,
		"omitted":   stream.Omitted,
	}
}
