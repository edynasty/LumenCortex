package agent

import (
	"context"
	"fmt"
	"strings"
)

// RunJobs is the run's view of the background jobs it started (ADR-0019 §3).
type RunJobs interface {
	// ActiveJobs lists the run's queued and running jobs.
	ActiveJobs(ctx context.Context, runID string) ([]ActiveJob, error)
}

// ActiveJob is one queued or running job of the run, as the completion gate names it to the model.
type ActiveJob struct {
	ID      string
	State   string
	Command string
}

// maxJobCorrections bounds how often the gate refuses a final answer over active jobs: a model that
// keeps answering instead of waiting or cancelling completes, and the host cancels the jobs when the
// run ends. One open job never loops a run.
const maxJobCorrections = 2

// jobsCorrection is the completion-gate correction for a final answer given while the run still has
// queued or running background jobs, or "" when there are none, the run has no job view, or the gate
// already refused maxJobCorrections answers. A failed lookup does not block completion: the jobs are
// cancelled when the run ends either way.
func (l *Loop) jobsCorrection(ctx context.Context, sessionID string, step int64, opts Options, r *run) string {
	if l.Jobs == nil || opts.RunID == "" || r.jobCorrections >= maxJobCorrections {
		return ""
	}
	active, err := l.Jobs.ActiveJobs(ctx, opts.RunID)
	if err != nil {
		l.emit("jobs.gate_error", sessionID, map[string]any{"step": step, "error": err.Error()})
		return ""
	}
	if len(active) == 0 {
		return ""
	}
	r.jobCorrections++
	ids := make([]string, 0, len(active))
	names := make([]string, 0, len(active))
	for _, job := range active {
		ids = append(ids, job.ID)
		command := job.Command
		if len(command) > 60 {
			command = command[:57] + "..."
		}
		names = append(names, fmt.Sprintf("%s %s: %s", job.ID, job.State, command))
	}
	l.emit("jobs.blocked_final", sessionID, map[string]any{"step": step, "jobs": ids, "correction": r.jobCorrections})
	return fmt.Sprintf("Completion refused: %d background job(s) this run started are still queued or running (%s). "+
		"Wait for them with job_wait and check their results, or cancel the ones you no longer need with job_cancel, "+
		"before you give the final answer. Jobs still active when the run ends are cancelled.",
		len(active), strings.Join(names, "; "))
}
