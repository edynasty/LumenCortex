package verify

import (
	"fmt"
	"strings"
	"time"
)

// Level is what a verdict decides. Delivery, execution, semantic and final are separate: a passing
// delivery never becomes a semantic pass, and the final gate is the only one that accepts a task.
type Level string

const (
	// LevelDelivery: the host's delivery contract was verified.
	LevelDelivery Level = "delivery"
	// LevelExecution: the program or command ran.
	LevelExecution Level = "execution"
	// LevelSemantic: the result's content is correct, not merely produced.
	LevelSemantic Level = "semantic"
	// LevelFinal: the task is accepted.
	LevelFinal Level = "final"
)

// Source is who decided a verdict. Only a trusted source may satisfy a semantic requirement; a
// model's self-check never upgrades to independent verification.
type Source string

const (
	// SourceHostAuthored is a check the host wrote and the agent cannot change or weaken.
	SourceHostAuthored Source = "host_authored_check"
	// SourceApprovedSpec is a check the host approved as part of the check set.
	SourceApprovedSpec Source = "approved_spec_check"
	// SourceModelSelf is the agent's own claim that the work is done.
	SourceModelSelf Source = "model_self_check"
	// SourceIndependent is an independent check or reviewer in its own context.
	SourceIndependent Source = "independent_check"
	// SourceOfficial is the official verifier of the task.
	SourceOfficial Source = "official_verifier"
)

// Valid reports a source this runtime knows. An unknown string is never trusted: a verdict that
// names one decides nothing, whatever status it claims.
func (s Source) Valid() bool {
	switch s {
	case SourceHostAuthored, SourceApprovedSpec, SourceModelSelf, SourceIndependent, SourceOfficial:
		return true
	}
	return false
}

// Trusted reports a source independent of the agent's own claim. An unknown source is not trusted.
func (s Source) Trusted() bool { return s.Valid() && s != SourceModelSelf }

// Independent reports a source that is independent by construction, not merely non-model.
func (s Source) Independent() bool { return s == SourceIndependent || s == SourceOfficial }

// Valid reports a level this runtime knows.
func (l Level) Valid() bool {
	switch l {
	case LevelDelivery, LevelExecution, LevelSemantic, LevelFinal:
		return true
	}
	return false
}

// Valid reports a status this runtime knows.
func (s Status) Valid() bool {
	switch s {
	case StatusPassed, StatusFailed, StatusUnknown, StatusError, StatusSpecUnapproved:
		return true
	}
	return false
}

// Deciding reports a verdict the gate may read: a known level, source and status. An unknown value
// is recorded as unsettled and never as a pass.
func (v Verdict) Deciding() bool { return v.Level.Valid() && v.Source.Valid() && v.Status.Valid() }

// Status is a verdict's outcome.
type Status string

const (
	StatusPassed         Status = "passed"
	StatusFailed         Status = "failed"
	StatusUnknown        Status = "unknown"
	StatusError          Status = "error"
	StatusSpecUnapproved Status = "spec_unapproved"
)

// Verdict is one verification result. Detail is a bounded diagnostic tail, never model reasoning.
type Verdict struct {
	Level    Level     `json:"level"`
	Source   Source    `json:"source"`
	Check    string    `json:"check"`
	Status   Status    `json:"status"`
	Detail   string    `json:"detail,omitempty"`
	Evidence []string  `json:"evidence,omitempty"`
	At       time.Time `json:"at,omitempty"`
}

// Failed reports a verdict that blocks completion.
func (v Verdict) Failed() bool {
	return v.Status == StatusFailed || v.Status == StatusError || v.Status == StatusSpecUnapproved
}

// Unsettled reports a verdict that decided nothing.
func (v Verdict) Unsettled() bool {
	return v.Status == StatusUnknown || v.Status == "" || (!v.Failed() && v.Status != StatusPassed)
}

// Attempt is one bounded repair attempt the gate asked for and the feedback it carried. Attempts
// are preserved so a later reader sees every failure, not only the last one.
type Attempt struct {
	Index    int       `json:"index"`
	Status   Status    `json:"status"`
	Feedback string    `json:"feedback"`
	Checks   []string  `json:"checks,omitempty"`
	At       time.Time `json:"at"`
}

// GateInput is everything the final gate reads. Claims are the session's durable basis records; a
// required claim that is not confirmed at CurrentFingerprint blocks the final level.
type GateInput struct {
	Delivery  []Verdict
	Execution []Verdict
	Semantic  []Verdict
	Claims    []Claim
	// CurrentFingerprint is the workspace state the gate decides at. Empty means there is no
	// fingerprint: a confirmation tied to a fingerprint is then stale.
	CurrentFingerprint string
	// RequireSemantic makes the semantic level a condition of the final level.
	RequireSemantic bool
	// RequireIndependent makes at least one independent semantic source a condition.
	RequireIndependent bool
	// MaxRepairs bounds repair attempts after a failed or unknown gate. Zero takes
	// DefaultMaxRepairs.
	MaxRepairs int
	// Attempts are the repair attempts already made.
	Attempts []Attempt
}

// DefaultMaxRepairs bounds the feedback → repair → recheck loop when a host sets no bound.
const DefaultMaxRepairs = 2

// MaxRepairsLimit is the largest bound a host may set.
const MaxRepairsLimit = 5

// MaxGateFeedbackBytes bounds the feedback one gate result carries to the model.
const MaxGateFeedbackBytes = 4096

// GateResult is the final gate's decision.
type GateResult struct {
	Passed bool   `json:"passed"`
	Status Status `json:"status"`
	// Levels is the status of each level that was read.
	Levels map[Level]Status `json:"levels"`
	// Blocking are the verdicts that failed.
	Blocking []Verdict `json:"blocking,omitempty"`
	// Unknown are the verdicts that decided nothing.
	Unknown []Verdict `json:"unknown,omitempty"`
	// Claims is the coverage of the session's required claims.
	Claims Coverage `json:"claims"`
	// Feedback is the bounded, actionable failure text.
	Feedback string `json:"feedback,omitempty"`
	// Attempts is the bounded attempt history, oldest first.
	Attempts []Attempt `json:"attempts,omitempty"`
	// RepairAllowed reports that the gate asked for another attempt; Exhausted reports that the
	// repair budget is used up and the run must stop.
	RepairAllowed bool `json:"repairAllowed,omitempty"`
	Exhausted     bool `json:"exhausted,omitempty"`
}

// Evaluate decides the final gate. The order is fixed: delivery, execution, semantic, claims. The
// result never upgrades a model self-check into a pass, and a delivery pass alone leaves the final
// level unsettled when the semantic level is required and nothing independent decided it.
func Evaluate(in GateInput, now time.Time) GateResult {
	maxRepairs := in.MaxRepairs
	if maxRepairs <= 0 {
		maxRepairs = DefaultMaxRepairs
	}
	if maxRepairs > MaxRepairsLimit {
		maxRepairs = MaxRepairsLimit
	}
	out := GateResult{Levels: map[Level]Status{}, Attempts: append([]Attempt(nil), in.Attempts...)}
	out.Levels[LevelDelivery] = levelStatus(in.Delivery, false, false)
	out.Levels[LevelExecution] = levelStatus(in.Execution, false, false)
	out.Levels[LevelSemantic] = semanticStatus(in.Semantic, in.RequireSemantic, in.RequireIndependent)
	out.Claims = ClaimCoverage(in.Claims, in.CurrentFingerprint)

	for _, verdicts := range [][]Verdict{in.Delivery, in.Execution} {
		for _, v := range verdicts {
			if !v.Deciding() {
				out.Unknown = append(out.Unknown, v)
				continue
			}
			if v.Failed() {
				out.Blocking = append(out.Blocking, v)
			} else if v.Unsettled() {
				out.Unknown = append(out.Unknown, v)
			}
		}
	}
	// A model's own verdict is recorded but never satisfies a semantic requirement: it is neither
	// blocking nor deciding. Only trusted semantic verdicts count.
	for _, v := range in.Semantic {
		if !v.Deciding() || !v.Source.Trusted() {
			continue
		}
		if v.Failed() {
			out.Blocking = append(out.Blocking, v)
		} else if v.Unsettled() {
			out.Unknown = append(out.Unknown, v)
		}
	}

	switch {
	case len(out.Blocking) > 0:
		out.Status = StatusFailed
	case out.Levels[LevelSemantic] == StatusUnknown && in.RequireSemantic:
		out.Status = StatusUnknown
	case len(out.Unknown) > 0:
		out.Status = StatusUnknown
	case out.Claims.Required > 0 && !out.Claims.Complete():
		out.Status = StatusFailed
	default:
		out.Status = StatusPassed
		out.Passed = true
	}
	out.Levels[LevelFinal] = out.Status

	if !out.Passed {
		out.Feedback = gateFeedback(out, in)
		out.Attempts = append(out.Attempts, Attempt{
			Index: len(out.Attempts) + 1, Status: out.Status, Feedback: out.Feedback,
			Checks: blockingChecks(out.Blocking), At: now,
		})
		if len(out.Attempts) > maxRepairs+1 {
			out.Attempts = append([]Attempt(nil), out.Attempts[len(out.Attempts)-(maxRepairs+1):]...)
		}
		out.RepairAllowed = len(in.Attempts) < maxRepairs
		out.Exhausted = !out.RepairAllowed
	}
	return out
}

// levelStatus reads the status of one level. It ignores sources the level does not accept.
func levelStatus(verdicts []Verdict, requireSemantic, requireIndependent bool) Status {
	if len(verdicts) == 0 {
		return StatusUnknown
	}
	status := StatusPassed
	for _, v := range verdicts {
		switch {
		case v.Failed():
			return StatusFailed
		case v.Unsettled():
			status = StatusUnknown
		}
	}
	return status
}

// semanticStatus reads the semantic level from trusted verdicts only. A required level with no
// trusted verdict, or with only a model self-check, is UNKNOWN: without independent truth there is
// nothing to accept.
func semanticStatus(verdicts []Verdict, required, requireIndependent bool) Status {
	var trusted []Verdict
	independent := false
	for _, v := range verdicts {
		if !v.Deciding() || !v.Source.Trusted() {
			continue
		}
		trusted = append(trusted, v)
		if v.Source.Independent() && !v.Failed() && !v.Unsettled() {
			independent = true
		}
	}
	if len(trusted) == 0 {
		if required {
			return StatusUnknown
		}
		return StatusUnknown
	}
	status := levelStatus(trusted, required, requireIndependent)
	if requireIndependent && status == StatusPassed && !independent {
		return StatusUnknown
	}
	return status
}

func blockingChecks(verdicts []Verdict) []string {
	var out []string
	for _, v := range verdicts {
		if v.Check != "" {
			out = append(out, v.Check)
		}
	}
	return out
}

// gateFeedback is the bounded, actionable text a blocked completion carries. It names every failing
// verdict with a short diagnostic tail and every required claim that does not count.
func gateFeedback(result GateResult, in GateInput) string {
	var b strings.Builder
	b.WriteString("Completion gate rejected completion.")
	if len(result.Blocking) > 0 {
		b.WriteString("\nFailed verification:")
		for _, v := range result.Blocking {
			fmt.Fprintf(&b, "\n- %s (%s, %s): %s", v.Check, v.Level, v.Source, v.Status)
			if tail := strings.TrimSpace(v.Detail); tail != "" {
				fmt.Fprintf(&b, "\n  %s", tail)
			}
			if b.Len() > MaxGateFeedbackBytes {
				break
			}
		}
	}
	if result.Levels[LevelSemantic] == StatusUnknown && in.RequireSemantic {
		b.WriteString("\nSemantic verification is UNKNOWN: no independent check decided the result. A model's own check is not independent evidence.")
	}
	if len(result.Unknown) > 0 {
		b.WriteString("\nUnsettled verification:")
		for _, v := range result.Unknown {
			fmt.Fprintf(&b, "\n- %s (%s, %s): %s", v.Check, v.Level, v.Source, v.Status)
			if b.Len() > MaxGateFeedbackBytes {
				break
			}
		}
	}
	if len(result.Claims.Missing) > 0 {
		b.WriteString("\nRequired claims not confirmed at the current state: " + strings.Join(result.Claims.Missing, ", "))
		b.WriteString(". Confirm each with fresh evidence (a check, evaluation, review or official verifier), or record it as not required.")
	}
	b.WriteString("\nFix the cause and propose completion again. Delivery and execution passing do not prove the result is semantically correct.")
	text := b.String()
	if len(text) > MaxGateFeedbackBytes {
		text = truncate(text, MaxGateFeedbackBytes)
	}
	return text
}
