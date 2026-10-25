package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// Checks decides configured checks for the run's session (design §4.7). The runtime implements it over
// internal/evaluator in the session's workspace.
type Checks interface {
	// Configured returns the ids of the configured checks and of those that are required, sorted.
	Configured() (all, required []string)
	// Decide decides the checks in order. A fresh result is stored with workUnitID. An id that is not
	// configured yields a non-passing error verdict without running anything. The error is the caller's
	// cancellation, session.ErrFenced when the session is fenced (nothing ran), or an evaluator failure,
	// never a check's outcome.
	Decide(ctx context.Context, ids []string, workUnitID string) ([]evaluator.Verdict, error)
	// Mutated records that a side-effect tool completed: a check episode the run closed before it is
	// no longer reused (ADR-0017); the check is decided again.
	Mutated()
	// SetApproved reports whether the check set the gate enumerates is one the host approved as it is
	// now, or the project never had an approved set (ADR-0017). It is false when an approved set was
	// emptied, lost a check, had a required check made optional, or its approval was revoked: then no
	// list of required checks can be trusted, an empty one included.
	SetApproved(ctx context.Context) (bool, error)
}

// checkSetID names the check set as a whole in a verdict of the set-level gate.
const checkSetID = "(check set)"

// checkFailureExit maps an evaluator failure to the run's exit: cancellation interrupts it, a fence ends
// it as fenced, a store failure is storage, anything else (the workspace could not be inspected) is
// config.
func checkFailureExit(ctx context.Context, err error) ExitCause {
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err}
	case errors.Is(err, session.ErrFenced):
		return ExitCause{Status: StatusFailed, Kind: KindFenced, Err: err}
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

// ErrChecksUnapproved ends a non-interactive run whose completion gate needs a check the host has not
// approved (KindSpecUnapproved).
var ErrChecksUnapproved = errors.New("required checks are not approved by the host; approve the current check set (lcx-go checks status, lcx-go checks approve)")

// unapprovedChecks returns the ids of failed verdicts that only the user can clear: a definition the
// host has not approved, or one whose definition or approval changed while its episode ran.
func unapprovedChecks(failed []evaluator.Verdict) []string {
	var out []string
	for _, v := range failed {
		if verdictStatus(v) == evaluator.StatusSpecUnapproved {
			out = append(out, v.Result.Check)
		}
	}
	return out
}

// verdictStatus is the status a verdict reports: its result's, except that a result that passed in an
// episode that did not close reports the episode's status (scope_changed, spec_unapproved,
// interrupted), since it decides nothing.
func verdictStatus(v evaluator.Verdict) string {
	if !v.Passed && v.Result.Status == evaluator.StatusPassed && v.Episode != "" && v.Episode != evaluator.EpisodeClosed {
		return v.Episode
	}
	return v.Result.Status
}

// unitChecks converts verdicts to the unit's stored check results.
func unitChecks(verdicts []evaluator.Verdict) []taskstate.UnitCheck {
	out := make([]taskstate.UnitCheck, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, taskstate.UnitCheck{Check: v.Result.Check, Status: verdictStatus(v), Provenance: v.Result.Provenance, Seq: v.Result.Seq})
	}
	return out
}

// checksCorrection is the completion_blocked text for required checks that did not pass: each check
// with its status, artifact hashes and the last 2 KiB of its output.
func checksCorrection(failed []evaluator.Verdict) string {
	var b strings.Builder
	b.WriteString("Completion gate rejected final completion: required checks did not pass.\n")
	for _, v := range failed {
		fmt.Fprintf(&b, "- %s: %s", v.Result.Check, verdictStatus(v))
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
		view := verdictView{Check: v.Result.Check, Status: verdictStatus(v), Passed: v.Passed, Reused: v.Reused,
			Provenance: v.Result.Provenance, Seq: v.Result.Seq, Artifacts: v.Result.Artifacts}
		if !v.Passed {
			view.Tail = v.Tail
		}
		out = append(out, view)
	}
	return out
}
