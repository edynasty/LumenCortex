package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// Checks decides configured checks for the run's session (design §4.7). The runtime implements it over
// internal/evaluator in the session's workspace.
type Checks interface {
	// Configured returns the ids of the configured checks and of those that are required, sorted.
	Configured() (all, required []string)
	// Decide decides the checks in order. A fresh result is stored with workUnitID. An id that is not
	// configured yields a non-passing error verdict without running anything. The error is the caller's
	// cancellation or an evaluator failure, never a check's outcome.
	Decide(ctx context.Context, ids []string, workUnitID string) ([]evaluator.Verdict, error)
}

// checkFailureExit maps an evaluator failure to the run's exit: cancellation interrupts it, a store
// failure is storage, anything else (the workspace could not be inspected) is config.
func checkFailureExit(ctx context.Context, err error) ExitCause {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err}
	case errors.Is(err, evaluator.ErrStore):
		return ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err}
	default:
		return ExitCause{Status: StatusFailed, Kind: KindConfig, Err: fmt.Errorf("deciding checks: %w", err)}
	}
}

// failing returns the verdicts that did not pass.
func failing(verdicts []evaluator.Verdict) []evaluator.Verdict {
	var out []evaluator.Verdict
	for _, v := range verdicts {
		if !v.Passed {
			out = append(out, v)
		}
	}
	return out
}

// unitChecks converts verdicts to the unit's stored check results.
func unitChecks(verdicts []evaluator.Verdict) []taskstate.UnitCheck {
	out := make([]taskstate.UnitCheck, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, taskstate.UnitCheck{Check: v.Result.Check, Status: v.Result.Status, Provenance: v.Result.Provenance, Seq: v.Result.Seq})
	}
	return out
}

// checksCorrection is the completion_blocked text for required checks that did not pass: each check
// with its status, artifact hashes and the last 2 KiB of its output.
func checksCorrection(failed []evaluator.Verdict) string {
	var b strings.Builder
	b.WriteString("Completion gate rejected final completion: required checks did not pass.\n")
	for _, v := range failed {
		fmt.Fprintf(&b, "- %s: %s", v.Result.Check, v.Result.Status)
		if len(v.Result.Artifacts) > 0 {
			fmt.Fprintf(&b, " (artifacts %s)", strings.Join(v.Result.Artifacts, ", "))
		}
		b.WriteString("\n")
		if tail := strings.TrimSpace(v.Tail); tail != "" {
			fmt.Fprintf(&b, "  output tail:\n```\n%s\n```\n", tail)
		}
	}
	b.WriteString("Fix the cause and propose completion again. Only the runtime's run of a check, or the user's acceptance of it, counts.")
	return b.String()
}

// verdictView is a verdict as a tool result reports it.
type verdictView struct {
	Check      string   `json:"check"`
	Status     string   `json:"status"`
	Passed     bool     `json:"passed"`
	Reused     bool     `json:"reused,omitempty"`
	Provenance string   `json:"provenance,omitempty"`
	Seq        int64    `json:"seq,omitempty"`
	Artifacts  []string `json:"artifacts,omitempty"`
	Tail       string   `json:"tail,omitempty"`
}

func verdictViews(verdicts []evaluator.Verdict) []verdictView {
	out := make([]verdictView, 0, len(verdicts))
	for _, v := range verdicts {
		view := verdictView{Check: v.Result.Check, Status: v.Result.Status, Passed: v.Passed, Reused: v.Reused,
			Provenance: v.Result.Provenance, Seq: v.Result.Seq, Artifacts: v.Result.Artifacts}
		if !v.Passed {
			view.Tail = v.Tail
		}
		out = append(out, view)
	}
	return out
}
