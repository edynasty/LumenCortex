package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/diag"
)

const (
	ExperienceCandidate     = "candidate"
	ExperienceValidated     = "validated"
	ExperienceNeedsEvidence = "needs_evidence"
	ExperienceRejected      = "rejected"
	ExperienceNeedsRevision = "needs_revision"
	ExperienceMethod        = "approved_closed_episode"
	ExperienceUncertainty   = "Conditional mechanism evidence only; strategy causality and real-task transfer benefit are unmeasured. Recheck conditions and outcome for each use."
	MaxExperienceConditions = 3
	MaxExperienceUses       = 8
	MaxExperienceSteps      = 16
	MaxExperienceChecks     = 8
)

// ExperienceProposal is the entire untrusted extractor output. It cannot name a source, scope,
// check, result, grade or state. No chat transcript or hidden reasoning is an input to this API.
type ExperienceProposal struct {
	ProblemPattern  string      `json:"problemPattern"`
	ProblemType     string      `json:"problemType"`
	Context         string      `json:"context"`
	ProposedCause   string      `json:"proposedCause,omitempty"`
	Preconditions   []Assertion `json:"preconditions"`
	Strategy        []string    `json:"strategy"`
	ExpectedOutcome Assertion   `json:"expectedOutcome"`
	ExpectedResult  string      `json:"expectedResult"`
	CitedPaths      []string    `json:"citedPaths,omitempty"`
}

var experiencePrivatePath = regexp.MustCompile(`(?:^|[\s"'(=:])(?:/|~/|[A-Za-z]:[\\/])|(?:^|[/\\])\.(?:ssh|aws|gnupg)(?:[/\\]|$)`)

// SafeExperienceText rejects rather than masks: a masked secret is still not reusable knowledge.
// This is format validation for an explicit host proposal, not automatic declassification of prose.
func SafeExperienceText(text string, max int) bool {
	return plainText(text, max) && diag.MaskSecrets(text) == text && !experiencePrivatePath.MatchString(text)
}

func (p ExperienceProposal) Validate() error {
	if !SafeExperienceText(p.ProblemPattern, 120) || !SafeExperienceText(p.ProblemType, 80) ||
		!SafeExperienceText(p.Context, 1024) || (p.ProposedCause != "" && !SafeExperienceText(p.ProposedCause, 512)) {
		return errors.New("unsafe or unbounded experience description")
	}
	if len(p.Preconditions) < 1 || len(p.Preconditions) > MaxExperienceConditions || len(p.Strategy) < 1 || len(p.Strategy) > 8 || len(p.CitedPaths) > MaxCitedPaths {
		return errors.New("experience needs bounded conditions, strategy and citations")
	}
	seen := make(map[string]bool, len(p.Preconditions))
	for _, a := range p.Preconditions {
		if err := safeExperienceAssertion(a); err != nil {
			return err
		}
		if seen[a.Ref()] {
			return errors.New("duplicate or conflicting experience conditions")
		}
		seen[a.Ref()] = true
	}
	if err := safeExperienceAssertion(p.ExpectedOutcome); err != nil {
		return err
	}
	if p.ExpectedResult != "passed" && p.ExpectedResult != "failed" {
		return errors.New("expected result must be passed or failed")
	}
	for _, step := range p.Strategy {
		if !SafeExperienceText(step, 512) {
			return errors.New("unsafe or unbounded experience strategy")
		}
	}
	seenPaths := make(map[string]bool, len(p.CitedPaths))
	for _, path := range p.CitedPaths {
		if !CleanPath(path) || !SafeExperienceText(path, maxPathBytes) || seenPaths[path] {
			return errors.New("unsafe or duplicate experience citation")
		}
		seenPaths[path] = true
	}
	return nil
}

func safeExperienceAssertion(a Assertion) error {
	if a.Validate() != nil || !SafeExperienceText(a.Target, maxPathBytes) || !SafeExperienceText(a.Value, MaxAssertionValueBytes) ||
		(a.Key != "" && (!SafeExperienceText(a.Key, MaxAssertionKeyBytes) || diag.IsSecretKey(a.Key))) {
		return errors.New("unsafe experience assertion")
	}
	return nil
}

func ParseExperienceProposal(raw string) (ExperienceProposal, error) {
	var p ExperienceProposal
	if len(raw) > MaxOutputBytes {
		return p, errors.New("experience proposal exceeds output bound")
	}
	if err := decodeStrict(raw, &p); err != nil {
		return ExperienceProposal{}, errors.New("invalid experience proposal fields")
	}
	if err := p.Validate(); err != nil {
		return ExperienceProposal{}, err
	}
	return p, nil
}

func (p ExperienceProposal) Digest() string {
	raw, _ := json.Marshal(p)
	return Digest("experience-definition:v1", string(raw))
}

// ExperienceTrace contains only host-issued locators and digests. Commands are bound by the actual
// execution's argument digest, not by storing executable command text or its output.
type ExperienceTrace struct {
	Steps      []ExperienceStep  `json:"steps"`
	Files      []ExperienceFile  `json:"files"`
	Checks     []ExperienceCheck `json:"checks"`
	Truncated  bool              `json:"truncated"`
	Incomplete bool              `json:"incomplete"`
}

// Failed reports whether an observed action of the origin failed. Such a procedure cannot carry a
// positive expected result: the framework's own record says it did not succeed, so an expected
// outcome that holds after it (an artifact that was already there) is not its result. An application
// stops at its first failed action, so a consumer could only repeat the failure. The same trace still
// supports a warning whose expected result is the failing check.
func (t ExperienceTrace) Failed() bool {
	return slices.ContainsFunc(t.Steps, func(s ExperienceStep) bool { return s.Outcome == "failed" })
}

type ExperienceStep struct {
	ExecutionID     string `json:"executionId"`
	RequestID       string `json:"requestId"`
	Tool            string `json:"tool"`
	ArgsDigest      string `json:"argsDigest"`
	Outcome         string `json:"outcome"`
	Coverage        string `json:"coverage"`
	LineageComplete bool   `json:"lineageComplete"`
}
type ExperienceFile struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
}
type ExperienceCheck struct {
	CheckID       string `json:"checkId"`
	ResultSeq     int64  `json:"resultSeq"`
	Outcome       string `json:"outcome"`
	SpecDigest    string `json:"specDigest,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	EpisodeID     string `json:"episodeId,omitempty"`
	ApprovalEpoch int64  `json:"approvalEpoch"`
	Qualified     bool   `json:"qualified"`
}
type ExperienceOrigin struct {
	Source         SourceRef       `json:"source"`
	TaskVersion    int64           `json:"taskVersion"`
	TaskRef        string          `json:"taskRef"`
	WorkflowDigest string          `json:"workflowDigest,omitempty"`
	Outcome        string          `json:"outcome"`
	CompletedAt    time.Time       `json:"completedAt"`
	Trace          ExperienceTrace `json:"trace"`
}

// ExperienceObservation couples an exact proposition to the existing qualified Evidence contract.
type ExperienceObservation struct {
	Assertion Assertion `json:"assertion"`
	Evidence  Evidence  `json:"evidence"`
}

func (o ExperienceObservation) Validate() error {
	if err := safeExperienceAssertion(o.Assertion); err != nil {
		return err
	}
	if err := o.Evidence.Validate(); err != nil {
		return err
	}
	claims := o.Evidence.SupportingClaims
	if o.Evidence.Verification == "failed" {
		claims = o.Evidence.ContradictingClaims
	}
	if (o.Evidence.Verification != "passed" && o.Evidence.Verification != "failed") || len(claims) != 1 || claims[0] != o.Assertion.ValueDigest() {
		return errors.New("experience observation does not prove its exact assertion")
	}
	return nil
}

type ExperienceValidation struct {
	Method     string                  `json:"method,omitempty"`
	Reason     string                  `json:"reason,omitempty"`
	Conditions []ExperienceObservation `json:"conditions,omitempty"`
	Outcome    *ExperienceObservation  `json:"outcome,omitempty"`
}
type ExperienceFeedback struct {
	Source         SourceRef              `json:"source"`
	Outcome        string                 `json:"outcome"`
	Used           bool                   `json:"used"`
	ConditionsHeld bool                   `json:"conditionsHeld"`
	RevisionNeeded bool                   `json:"revisionNeeded"`
	Reason         string                 `json:"reason"`
	ApplicationID  string                 `json:"applicationId,omitempty"`
	Observation    *ExperienceObservation `json:"observation,omitempty"`
	At             time.Time              `json:"at"`
}
type ExperienceUse struct {
	// RunID is set only for an adoption made inside that active run.
	RunID             string                  `json:"runId,omitempty"`
	ID                string                  `json:"id"`
	ConsumerID        string                  `json:"consumerId"`
	BaselineDigest    string                  `json:"baselineDigest"`
	PreviousRunID     string                  `json:"previousRunId,omitempty"`
	DefinitionVersion int64                   `json:"definitionVersion"`
	DefinitionDigest  string                  `json:"definitionDigest"`
	ExpectedOutcome   Assertion               `json:"expectedOutcome"`
	ExpectedResult    string                  `json:"expectedResult"`
	Conditions        []ExperienceObservation `json:"conditions"`
	AdoptedAt         time.Time               `json:"adoptedAt"`
	Feedback          *ExperienceFeedback     `json:"feedback,omitempty"`
}

// Experience is an Experimental payload of one native graph revision. DefinitionVersion changes
// only on an explicit revision; feedback never rewrites the origin or earlier observations.
type Experience struct {
	Origin            ExperienceOrigin     `json:"origin"`
	Definition        ExperienceProposal   `json:"definition"`
	DefinitionVersion int64                `json:"definitionVersion"`
	SupersedesVersion int64                `json:"supersedesVersion,omitempty"`
	State             string               `json:"state"`
	Confidence        string               `json:"confidence"`
	Uncertainty       string               `json:"uncertainty"`
	CreatedAt         time.Time            `json:"createdAt"`
	LastVerifiedAt    time.Time            `json:"lastVerifiedAt,omitempty"`
	Validation        ExperienceValidation `json:"validation"`
	Uses              []ExperienceUse      `json:"uses,omitempty"`
}
type ExperienceStatistics struct {
	Adoptions   int `json:"adoptions"`
	Successes   int `json:"successes"`
	Failures    int `json:"failures"`
	Unknown     int `json:"unknown"`
	Unconfirmed int `json:"unconfirmed"`
}

func (x Experience) Statistics() ExperienceStatistics {
	s := ExperienceStatistics{Adoptions: len(x.Uses)}
	for _, use := range x.Uses {
		if use.Feedback == nil {
			s.Unconfirmed++
			continue
		}
		switch use.Feedback.Outcome {
		case "success":
			s.Successes++
		case "failure":
			s.Failures++
		default:
			s.Unknown++
		}
	}
	return s
}
func (x Experience) ConditionalConfidence() string {
	if x.State == ExperienceNeedsRevision {
		return "contested"
	}
	if x.State == ExperienceValidated {
		return "conditional"
	}
	return "unverified"
}

// One record holds every use of an Experience and, once a use is settled, its feedback with the
// observation of the outcome. A use is applied before it is settled: a use whose feedback no longer
// fits its record has run its procedure and can never record the checked result, a counterexample
// included. The room of that feedback is therefore kept from the adoption on (Node.Room), and a
// record takes a further use only while it can keep the room of that one as well
// (Node.AdmitsExperienceUse).
//
// The sizes are read off observations the record already holds. One assertion observed for one
// scope encodes the same fields every time; two such observations by sessions Core created differ by
// at most experienceObservationSlack: the run their source names (43 bytes), the two content hashes
// of the workspace fingerprint (128), the width of three times (30) and of three numbers (54), two
// longer names (7), and 256 bytes for the path of a worktree where the other was made in the local
// checkout. A longer path, or a session a host imported under a longer identity, can make the store
// refuse an adoption that was admitted here; nothing has been applied then.
const (
	experienceObservationSlack = 576
	// experienceFeedbackSlack is what settling a use adds besides the source and the observation it
	// is measured by: the outcome, three flags, the reason, the application and the time (236 bytes
	// at their longest), the names around them (38), the run of the source (43) and a longer state of
	// the record (5); then the slack of the observation, which a consumer makes in a worktree after
	// a host adopted for it in the local checkout. What a still longer path adds is taken from the
	// room of a lifecycle revision.
	experienceFeedbackSlack = 384 + experienceObservationSlack
	// experienceUseSlack is what a use holds besides its conditions, which the stand-in of
	// AdmitsExperienceUse leaves empty: two runs, the use, the consumer, two digests, the version and
	// the width of the adoption time (320 bytes with the identities Core issues).
	experienceUseSlack = 384
)

// feedbackBytes bounds what settling u adds to its record. The consumer that observed the conditions
// of u observes the outcome, for the same scope and through the same kind of check, and is the source
// of the feedback: the largest condition observation of u stands in for both.
func (u ExperienceUse) feedbackBytes() int {
	evidence, source := 0, 0
	for _, o := range u.Conditions {
		if n := encodedLen(o.Evidence); n > evidence {
			evidence, source = n, encodedLen(o.Evidence.Source)
		}
	}
	return evidence + source + encodedLen(u.ExpectedOutcome) + experienceFeedbackSlack
}

func encodedLen(v any) int {
	raw, _ := json.Marshal(v)
	return len(raw)
}

func (x Experience) Validate() error {
	if err := x.Definition.Validate(); err != nil {
		return err
	}
	if err := x.Origin.Source.Validate(); err != nil {
		return err
	}
	if x.Origin.Source.RunID == "" || x.Origin.TaskVersion < 0 || x.Origin.TaskRef != fmt.Sprintf("task_state:%s@%d", x.Origin.Source.SessionID, x.Origin.TaskVersion) ||
		(x.Origin.Outcome != "completed" && x.Origin.Outcome != "failed") || x.Origin.CompletedAt.IsZero() {
		return errors.New("experience has no authoritative terminal origin")
	}
	if x.Origin.WorkflowDigest != "" && !digestPattern.MatchString(x.Origin.WorkflowDigest) {
		return errors.New("invalid workflow digest")
	}
	if x.DefinitionVersion < 1 || x.SupersedesVersion != x.DefinitionVersion-1 || x.CreatedAt.IsZero() || x.Uncertainty != ExperienceUncertainty || x.Confidence != x.ConditionalConfidence() {
		return errors.New("invalid experience version or confidence")
	}
	if !slices.Contains([]string{ExperienceCandidate, ExperienceValidated, ExperienceNeedsEvidence, ExperienceRejected, ExperienceNeedsRevision}, x.State) {
		return errors.New("invalid experience state")
	}
	if err := x.validateTrace(); err != nil {
		return err
	}
	if len(x.Validation.Conditions) > len(x.Definition.Preconditions) || len(x.Uses) > MaxExperienceUses {
		return errors.New("experience observation bound exceeded")
	}
	if x.Validation.Method != "" && x.Validation.Method != ExperienceMethod {
		return errors.New("invalid experience validation method")
	}
	if x.Validation.Reason != "" && !SafeExperienceText(x.Validation.Reason, 256) {
		return errors.New("unsafe validation reason")
	}
	for i, o := range x.Validation.Conditions {
		if o.Assertion != x.Definition.Preconditions[i] {
			return errors.New("validation condition differs from definition")
		}
		if err := o.Validate(); err != nil {
			return err
		}
	}
	if o := x.Validation.Outcome; o != nil {
		if o.Assertion != x.Definition.ExpectedOutcome {
			return errors.New("validation outcome differs from definition")
		}
		if err := o.Validate(); err != nil {
			return err
		}
	}
	if x.State == ExperienceValidated {
		if x.LastVerifiedAt.IsZero() || x.Validation.Method != ExperienceMethod || len(x.Validation.Conditions) != len(x.Definition.Preconditions) || x.Validation.Outcome == nil ||
			x.Validation.Outcome.Evidence.Verification != x.Definition.ExpectedResult || (x.Definition.ExpectedResult == "passed" && (x.Origin.Outcome == "failed" || x.Origin.Trace.Failed())) ||
			x.Origin.Trace.Truncated || x.Origin.Trace.Incomplete {
			return errors.New("experience promotion lacks qualified evidence")
		}
		for _, o := range x.Validation.Conditions {
			if o.Evidence.Verification != "passed" {
				return errors.New("experience condition did not pass")
			}
		}
	}
	seen := make(map[string]bool, len(x.Uses))
	for _, use := range x.Uses {
		if seen[use.ID] {
			return errors.New("duplicate experience adoption")
		}
		seen[use.ID] = true
		if err := use.Validate(x.Origin.Source.SessionID, x.DefinitionVersion); err != nil {
			return err
		}
		if use.AdoptedAt.Before(x.CreatedAt) {
			return errors.New("adoption predates experience")
		}
		if use.DefinitionVersion == x.DefinitionVersion {
			if use.DefinitionDigest != x.Definition.Digest() || use.ExpectedOutcome != x.Definition.ExpectedOutcome || use.ExpectedResult != x.Definition.ExpectedResult || len(use.Conditions) != len(x.Definition.Preconditions) {
				return errors.New("adoption differs from current definition")
			}
			for i, o := range use.Conditions {
				if o.Assertion != x.Definition.Preconditions[i] {
					return errors.New("adoption checked the wrong condition")
				}
			}
		}
	}
	return nil
}

func (x Experience) validateTrace() error {
	t := x.Origin.Trace
	if len(t.Steps) > MaxExperienceSteps || len(t.Files) > MaxCitedPaths || len(t.Checks) > MaxExperienceChecks {
		return errors.New("experience trace exceeds bounds")
	}
	for _, step := range t.Steps {
		if !plainText(step.ExecutionID, 128) || !plainText(step.RequestID, 128) || !plainText(step.Tool, 128) || !digestPattern.MatchString(step.ArgsDigest) ||
			!slices.Contains([]string{"completed", "failed", "outcome_unknown"}, step.Outcome) {
			return errors.New("invalid experience execution reference")
		}
	}
	for _, file := range t.Files {
		if !CleanPath(file.Path) || !SafeExperienceText(file.Path, maxPathBytes) || (len(file.Blob) != 40 && len(file.Blob) != 64) || strings.Trim(file.Blob, "0123456789abcdef") != "" {
			return errors.New("invalid experience file reference")
		}
	}
	for _, c := range t.Checks {
		if !plainText(c.CheckID, 128) || c.ResultSeq < 1 || !plainText(c.Outcome, 64) {
			return errors.New("invalid experience check reference")
		}
		if c.Qualified && (c.ApprovalEpoch < 1 || !digestPattern.MatchString(c.SpecDigest) || c.Fingerprint == "" || c.EpisodeID == "" || (c.Outcome != "passed" && c.Outcome != "failed")) {
			return errors.New("unqualified experience check reference")
		}
	}
	return nil
}

func (u ExperienceUse) Validate(origin string, definitionVersion int64) error {
	if !plainText(u.ID, 128) || !plainText(u.ConsumerID, 128) || u.ConsumerID == origin || !digestPattern.MatchString(u.BaselineDigest) || !digestPattern.MatchString(u.DefinitionDigest) ||
		u.DefinitionVersion < 1 || u.DefinitionVersion > definitionVersion || u.AdoptedAt.IsZero() || len(u.Conditions) < 1 || len(u.Conditions) > MaxExperienceConditions {
		return errors.New("invalid independent experience adoption")
	}
	if err := safeExperienceAssertion(u.ExpectedOutcome); err != nil {
		return err
	}
	if u.ExpectedResult != "passed" && u.ExpectedResult != "failed" {
		return errors.New("invalid adopted outcome")
	}
	for _, o := range u.Conditions {
		if err := o.Validate(); err != nil {
			return err
		}
		if o.Evidence.SessionID != u.ConsumerID || o.Evidence.Verification != "passed" {
			return errors.New("adoption conditions lack consumer verification")
		}
	}
	if f := u.Feedback; f != nil {
		if err := f.Source.Validate(); err != nil {
			return err
		}
		if !SafeExperienceText(f.Reason, 256) {
			return errors.New("unsafe feedback reason")
		}
		wrongRun := f.Source.RunID == u.PreviousRunID
		if u.RunID != "" {
			// A tool adoption belongs to its run: only that run can have applied it. When that run
			// ended without settling the use, a later run of the consumer closes it as never applied.
			wrongRun = f.Source.RunID != u.RunID && f.Used
		}
		if f.Source.SessionID != u.ConsumerID || f.Source.RunID == "" || wrongRun || f.Source.Digest == u.BaselineDigest ||
			f.At.Before(u.AdoptedAt) || !slices.Contains([]string{"success", "failure", "unknown"}, f.Outcome) {
			return errors.New("feedback is not a completed independent use")
		}
		if !f.Used && (f.Outcome != "unknown" || f.Observation != nil) {
			return errors.New("feedback of a run that executed nothing has no outcome")
		}
		if f.Observation != nil {
			if err := f.Observation.Validate(); err != nil {
				return err
			}
			if f.Observation.Assertion != u.ExpectedOutcome || f.Observation.Evidence.SessionID != u.ConsumerID || !sameRun(f.Observation.Evidence.Source, f.Source) ||
				(f.Observation.Evidence.RunID != "" && f.Observation.Evidence.RunID != f.Source.RunID) {
				return errors.New("feedback checks the wrong outcome or source run")
			}
		}
		if f.Outcome != "unknown" && f.Observation == nil {
			return errors.New("feedback has no independent evidence")
		}
		if f.Outcome == "success" && (f.RevisionNeeded || f.Observation.Evidence.Verification != u.ExpectedResult) {
			return errors.New("feedback success contradicts evidence")
		}
		if f.Outcome == "failure" && (!f.RevisionNeeded || f.Observation.Evidence.Verification == u.ExpectedResult) {
			return errors.New("feedback failure contradicts evidence")
		}
	}
	return nil
}

// sameRun reports whether an outcome was observed in the run a feedback settles: the same source in
// the same run. Their versions differ, because the outcome is observed when the application ends and
// the feedback is recorded when the run does.
func sameRun(observed, settled SourceRef) bool {
	observed.Digest = settled.Digest
	return observed == settled
}
