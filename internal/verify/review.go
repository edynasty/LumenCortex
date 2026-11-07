package verify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// Verifier decides semantic verdicts for a run in its own context. The runtime implements it over
// the host's approved checks; a test supplies a fake. It must not call a model unless the host
// explicitly enabled a reviewer, and it never reads hidden reasoning.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) ([]Verdict, error)
}

// VerifyRequest is what a semantic verifier reads: the task's explicit inputs and what already ran,
// never the transcript or the model's reasoning.
type VerifyRequest struct {
	SessionID string
	RunID     string
	Goal      string
	Final     string
	// Attempt is the completion attempt number, starting at 1.
	Attempt int
	Claims  []Claim
	// Execution are the execution-level verdicts the run already decided.
	Execution []Verdict
}

// Reviewer performs the single, opt-in, read-only independent review of a completed run (spec §22).
// It runs in an independent context and is metered like any other model request.
type Reviewer interface {
	Review(ctx context.Context, req ReviewRequest) (ReviewResult, error)
}

// ReviewRequest is the read-only input of one review. It carries the task contract and the
// verification evidence, never the agent's transcript or reasoning.
type ReviewRequest struct {
	SessionID string
	RunID     string
	Goal      string
	Final     string
	Claims    []Claim
	Verdicts  []Verdict
	// Evidence are locators the reviewer may cite, e.g. "check:io@eval:12".
	Evidence []string
}

// Review severities.
const (
	SeverityBlocker = "blocker"
	SeverityMajor   = "major"
	SeverityMinor   = "minor"
	SeverityNote    = "note"
)

// ReviewFinding is one issue the reviewer found, with a suggestion another agent can act on.
type ReviewFinding struct {
	Severity   string   `json:"severity"`
	Issue      string   `json:"issue"`
	Suggestion string   `json:"suggestion,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
}

// ReviewResult is the reviewer's verdict and its metering. Usage is included in the run's spend;
// Source is independent by construction.
type ReviewResult struct {
	Passed    bool            `json:"passed"`
	Findings  []ReviewFinding `json:"findings,omitempty"`
	Source    Source          `json:"source"`
	RequestID string          `json:"requestId,omitempty"`
	Model     string          `json:"model,omitempty"`
	Usage     protocol.Usage  `json:"usage"`
	// CostUSD is the review's list-price cost; CostKnown is false when the model has no price, so an
	// unknown cost is never reported as zero.
	CostUSD   float64   `json:"costUsd,omitempty"`
	CostKnown bool      `json:"costKnown,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

// Blocking reports whether a finding must be fixed before the task can be accepted.
func (f ReviewFinding) Blocking() bool {
	return f.Severity == SeverityBlocker || f.Severity == SeverityMajor
}

// Verdict converts the review to a semantic verdict. A review that did not pass blocks; a review
// that passed satisfies the semantic level only as an independent source, never as an official one.
func (r ReviewResult) Verdict() Verdict {
	source := r.Source
	if source == "" {
		source = SourceIndependent
	}
	status := StatusPassed
	detail := ""
	if !r.Passed || len(BlockingFindings(r.Findings)) > 0 {
		status = StatusFailed
		detail = FindingsFeedback(r.Findings, MaxGateFeedbackBytes)
	}
	return Verdict{Level: LevelSemantic, Source: source, Check: "independent_review", Status: status, Detail: detail, At: r.At}
}

// BlockingFindings returns the findings a repair must address.
func BlockingFindings(findings []ReviewFinding) []ReviewFinding {
	var out []ReviewFinding
	for _, f := range findings {
		if f.Blocking() {
			out = append(out, f)
		}
	}
	return out
}

// FindingsFeedback renders bounded, actionable feedback from review findings.
func FindingsFeedback(findings []ReviewFinding, limit int) string {
	var b strings.Builder
	for _, f := range findings {
		if !f.Blocking() {
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s", f.Severity, strings.TrimSpace(f.Issue))
		if s := strings.TrimSpace(f.Suggestion); s != "" {
			fmt.Fprintf(&b, " Suggestion: %s", s)
		}
		b.WriteString("\n")
		if b.Len() > limit {
			break
		}
	}
	return truncate(strings.TrimSpace(b.String()), limit)
}

// NormalizeReview validates a reviewer's answer: a passed review may not carry a blocking finding,
// and every severity is one of the four. It bounds the findings to MaxReviewFindings.
func NormalizeReview(r ReviewResult, at time.Time) (ReviewResult, error) {
	if len(r.Findings) > MaxReviewFindings {
		r.Findings = append([]ReviewFinding(nil), r.Findings[:MaxReviewFindings]...)
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		f.Issue = strings.TrimSpace(f.Issue)
		f.Suggestion = strings.TrimSpace(f.Suggestion)
		switch f.Severity {
		case SeverityBlocker, SeverityMajor, SeverityMinor, SeverityNote:
		default:
			return ReviewResult{}, fmt.Errorf("unknown review severity %q", f.Severity)
		}
		if f.Issue == "" {
			return ReviewResult{}, fmt.Errorf("review finding %d has no issue", i+1)
		}
	}
	if r.Passed && len(BlockingFindings(r.Findings)) > 0 {
		return ReviewResult{}, fmt.Errorf("a passed review cannot carry a %s or %s finding", SeverityBlocker, SeverityMajor)
	}
	if r.Source == "" {
		r.Source = SourceIndependent
	}
	if r.Source != SourceIndependent && r.Source != SourceOfficial {
		return ReviewResult{}, fmt.Errorf("a review's source must be independent or official")
	}
	if r.At.IsZero() {
		r.At = at
	}
	return r, nil
}

// MaxReviewFindings bounds one review's findings.
const MaxReviewFindings = 16
