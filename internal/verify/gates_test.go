package verify

import (
	"strings"
	"testing"
	"time"
)

func verdict(level Level, source Source, check string, status Status) Verdict {
	return Verdict{Level: level, Source: source, Check: check, Status: status}
}

func TestModelSelfCheckNeverPassesAHostFailure(t *testing.T) {
	got := Evaluate(GateInput{
		Execution: []Verdict{verdict(LevelExecution, SourceApprovedSpec, "go-test", StatusFailed)},
		Semantic:  []Verdict{verdict(LevelSemantic, SourceModelSelf, "self", StatusPassed)},
	}, time.Now())
	if got.Passed {
		t.Fatalf("a model's self-check must not pass a failing host check: %+v", got)
	}
	if got.Status != StatusFailed || len(got.Blocking) != 1 {
		t.Fatalf("got=%+v", got)
	}
	if got.Feedback == "" || !strings.Contains(got.Feedback, "go-test") {
		t.Fatalf("feedback=%q", got.Feedback)
	}
}

func TestDeliveryPassDoesNotImplySemanticPass(t *testing.T) {
	got := Evaluate(GateInput{
		Delivery:        []Verdict{verdict(LevelDelivery, SourceHostAuthored, "delivery", StatusPassed)},
		Execution:       []Verdict{verdict(LevelExecution, SourceApprovedSpec, "build", StatusPassed)},
		Semantic:        []Verdict{verdict(LevelSemantic, SourceModelSelf, "self", StatusPassed)},
		RequireSemantic: true,
	}, time.Now())
	if got.Passed {
		t.Fatalf("delivery must not become semantic acceptance: %+v", got)
	}
	if got.Status != StatusUnknown {
		t.Fatalf("status=%s, want unknown without independent truth", got.Status)
	}
	if !strings.Contains(got.Feedback, "UNKNOWN") {
		t.Fatalf("feedback=%q", got.Feedback)
	}
}

func TestUnknownSourceNeverPasses(t *testing.T) {
	got := Evaluate(GateInput{
		Semantic:        []Verdict{{Level: LevelSemantic, Source: Source("garbage"), Check: "x", Status: StatusPassed}},
		RequireSemantic: true,
	}, time.Now())
	if got.Passed {
		t.Fatalf("an unknown source must not satisfy semantic verification: %+v", got)
	}
	if got.Status != StatusUnknown {
		t.Fatalf("status=%s", got.Status)
	}
	// The same verdict at the execution level is unsettled, never a pass.
	got = Evaluate(GateInput{Execution: []Verdict{{Level: LevelExecution, Source: Source("garbage"), Check: "x", Status: StatusPassed}}}, time.Now())
	if got.Passed || got.Status != StatusUnknown {
		t.Fatalf("got=%+v", got)
	}
}

func TestSemanticLevelRequiresIndependentSource(t *testing.T) {
	in := GateInput{
		Semantic:           []Verdict{verdict(LevelSemantic, SourceApprovedSpec, "semantic", StatusPassed)},
		RequireSemantic:    true,
		RequireIndependent: true,
	}
	got := Evaluate(in, time.Now())
	if got.Passed || got.Status != StatusUnknown {
		t.Fatalf("an approved spec check alone is not an independent review: %+v", got)
	}
	in.Semantic = append(in.Semantic, verdict(LevelSemantic, SourceIndependent, "independent_review", StatusPassed))
	if got := Evaluate(in, time.Now()); !got.Passed {
		t.Fatalf("an independent pass must satisfy the level: %+v", got)
	}
}

func TestRequiredClaimsMustBeConfirmedAtTheCurrentState(t *testing.T) {
	claim := Claim{ID: "c1", Kind: ClaimContract, Statement: "report is in USD", Source: ClaimSource{Kind: SourceKindUser},
		Status: ClaimConfirmed, Required: true, Evidence: []string{"check:units@eval:1"}, Fingerprint: "head-a+dirty-a@local"}
	got := Evaluate(GateInput{Claims: []Claim{claim}, CurrentFingerprint: "head-b+dirty-b@local"}, time.Now())
	if got.Passed {
		t.Fatalf("a stale confirmation must not pass: %+v", got)
	}
	if got.Claims.Stale != 1 || len(got.Claims.Missing) != 1 || got.Claims.Missing[0] != "c1" {
		t.Fatalf("coverage=%+v", got.Claims)
	}
	if !strings.Contains(got.Feedback, "c1") {
		t.Fatalf("feedback=%q", got.Feedback)
	}
	// Revalidated at the new state: it counts.
	claim.Status, claim.Fingerprint = ClaimRevalidated, "head-b+dirty-b@local"
	if got := Evaluate(GateInput{Claims: []Claim{claim}, CurrentFingerprint: "head-b+dirty-b@local"}, time.Now()); !got.Passed {
		t.Fatalf("a revalidated claim must count: %+v", got)
	}
}

func TestRejectedRequiredClaimBlocks(t *testing.T) {
	// Only a host or user claim can be required: a model's own claim is never a completion condition.
	claim := Claim{ID: "c1", Kind: ClaimAssumption, Statement: "input is sorted", Source: ClaimSource{Kind: SourceKindHost},
		Status: ClaimRejected, Required: true}
	got := Evaluate(GateInput{Claims: []Claim{claim}}, time.Now())
	if got.Passed || got.Claims.Rejected != 1 {
		t.Fatalf("got=%+v", got)
	}
	// The same claim from the model is a proposal: it neither blocks nor is required.
	model := claim
	model.Source = ClaimSource{Kind: SourceKindModel}
	if got := Evaluate(GateInput{Claims: []Claim{model}}, time.Now()); !got.Passed || got.Claims.Required != 0 {
		t.Fatalf("a model's own rejected claim gated completion: %+v", got)
	}
}

func TestRepairBudgetIsFiniteAndAttemptsPreserved(t *testing.T) {
	in := GateInput{
		Execution:  []Verdict{verdict(LevelExecution, SourceApprovedSpec, "test", StatusFailed)},
		MaxRepairs: 1,
	}
	first := Evaluate(in, time.Now())
	if !first.RepairAllowed || first.Exhausted {
		t.Fatalf("first failure must allow a repair: %+v", first)
	}
	if len(first.Attempts) != 1 || first.Attempts[0].Status != StatusFailed {
		t.Fatalf("attempts=%+v", first.Attempts)
	}
	in.Attempts = first.Attempts
	second := Evaluate(in, time.Now())
	if second.RepairAllowed || !second.Exhausted {
		t.Fatalf("the repair budget must run out: %+v", second)
	}
	if len(second.Attempts) != 2 || second.Attempts[0].Feedback != first.Attempts[0].Feedback {
		t.Fatalf("the original attempt must be preserved: %+v", second.Attempts)
	}
}

func TestGateFeedbackIsBounded(t *testing.T) {
	var blocking []Verdict
	for i := range 200 {
		blocking = append(blocking, Verdict{Level: LevelExecution, Source: SourceApprovedSpec,
			Check: strings.Repeat("c", 20), Status: StatusFailed, Detail: strings.Repeat("d", 200)})
		_ = i
	}
	got := Evaluate(GateInput{Execution: blocking}, time.Now())
	if len(got.Feedback) > MaxGateFeedbackBytes {
		t.Fatalf("feedback=%d bytes", len(got.Feedback))
	}
}

func TestClaimRules(t *testing.T) {
	at := time.Now()
	if _, err := NormalizeClaim(Claim{Kind: ClaimEvidence, Statement: "x", Source: ClaimSource{Kind: SourceKindModel}}, at); err == nil {
		t.Fatal("a model must not record evidence")
	}
	if _, err := NormalizeClaim(Claim{Kind: ClaimFact, Statement: "x", Source: ClaimSource{Kind: SourceKindModel}, Status: ClaimConfirmed}, at); err == nil {
		t.Fatal("a model must not confirm its own claim")
	}
	c, err := NormalizeClaim(Claim{Kind: ClaimAssumption, Statement: "unit is metres", Source: ClaimSource{Kind: SourceKindModel}}, at)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != ClaimProposed || len(c.Evidence) != 0 {
		t.Fatalf("claim=%+v", c)
	}
	if err := c.Settle(ClaimConfirmed, ClaimSource{Kind: SourceKindModel}, "fp", []string{"x"}, "", at); err == nil {
		t.Fatal("a model must not settle a claim")
	}
	if err := c.Settle(ClaimConfirmed, ClaimSource{Kind: SourceKindCheck}, "fp", nil, "", at); err == nil {
		t.Fatal("confirmation without evidence must be refused")
	}
	if err := c.Settle(ClaimConfirmed, ClaimSource{Kind: SourceKindCheck, Ref: "units"}, "fp", []string{"check:units@eval:1"}, "measured", at); err != nil {
		t.Fatal(err)
	}
	if c.Status != ClaimConfirmed || c.Fingerprint != "fp" || len(c.History) != 1 {
		t.Fatalf("claim=%+v", c)
	}
	if !c.Stale("other") || c.Stale("fp") {
		t.Fatal("staleness must follow the fingerprint")
	}
	if err := c.Settle(ClaimRejected, ClaimSource{Kind: SourceKindReview}, "fp", nil, "wrong", at); err != nil {
		t.Fatal(err)
	}
	if err := c.Settle(ClaimRevalidated, ClaimSource{Kind: SourceKindCheck}, "fp2", []string{"check:units@eval:2"}, "", at); err != nil {
		t.Fatal(err)
	}
	coverage := ClaimCoverage([]Claim{{ID: "a", Required: true, Status: ClaimConfirmed, Fingerprint: "fp"}}, "other")
	if coverage.Stale != 1 || coverage.Complete() {
		t.Fatalf("coverage=%+v", coverage)
	}
}

func TestInvalidateStaleClaims(t *testing.T) {
	claims := []Claim{
		{ID: "a", Required: true, Status: ClaimConfirmed, Fingerprint: "old"},
		{ID: "b", Status: ClaimConfirmed, Fingerprint: "old"},
		{ID: "c", Status: ClaimConfirmed, Fingerprint: "new"},
	}
	changed := InvalidateStale(claims, "new", time.Now())
	if len(changed) != 2 || claims[0].Status != ClaimInvalidated || claims[2].Status != ClaimConfirmed {
		t.Fatalf("changed=%v claims=%+v", changed, claims)
	}
	if len(claims[0].History) != 1 || claims[0].History[0].By != "runtime" {
		t.Fatalf("history=%+v", claims[0].History)
	}
}

func TestBlankCurrentFingerprintNeverReusesAConfirmation(t *testing.T) {
	at := time.Now()
	claim := Claim{ID: "c1", Kind: ClaimContract, Statement: "report is in USD", Source: ClaimSource{Kind: SourceKindUser},
		Status: ClaimConfirmed, Required: true, Evidence: []string{"check:units@eval:1"}, Fingerprint: "head-a@local"}
	if !claim.Stale("") {
		t.Fatal("an unknown current fingerprint must not reuse a confirmation")
	}
	got := Evaluate(GateInput{Claims: []Claim{claim}, CurrentFingerprint: ""}, at)
	if got.Passed || got.Claims.Stale != 1 || len(got.Claims.Missing) != 1 || got.Claims.Missing[0] != "c1" {
		t.Fatalf("a blank fingerprint accepted a confirmation: %+v", got)
	}
	// An unreadable fingerprint must not rewrite the durable confirmation: coverage blocks it and the
	// record survives for a later gate that can read the workspace again.
	claims := []Claim{claim}
	if changed := InvalidateStale(claims, "", at); len(changed) != 0 || claims[0].Status != ClaimConfirmed {
		t.Fatalf("a blank fingerprint rewrote a confirmation: changed=%v claims=%+v", changed, claims)
	}
	if claim.Stale("head-a@local") || !claim.Stale("head-b@local") {
		t.Fatal("staleness must follow a readable fingerprint")
	}
}

func TestConfirmationWithoutProofIsRefused(t *testing.T) {
	at := time.Now()
	if _, err := NormalizeClaim(Claim{Kind: ClaimContract, Statement: "x", Source: ClaimSource{Kind: SourceKindHost},
		Status: ClaimConfirmed, Fingerprint: "fp"}, at); err == nil {
		t.Fatal("a confirmation without evidence must be refused")
	}
	if _, err := NormalizeClaim(Claim{Kind: ClaimContract, Statement: "x", Source: ClaimSource{Kind: SourceKindHost},
		Status: ClaimConfirmed, Evidence: []string{"check:x@eval:1"}}, at); err == nil {
		t.Fatal("a confirmation without a fingerprint must be refused")
	}
	if _, err := NormalizeClaim(Claim{Kind: ClaimContract, Statement: "x", Source: ClaimSource{Kind: SourceKindModel},
		Status: ClaimConfirmed, Evidence: []string{"x"}, Fingerprint: "fp"}, at); err == nil {
		t.Fatal("a model confirmation must be refused")
	}
	confirmed, err := NormalizeClaim(Claim{Kind: ClaimContract, Statement: "x", Source: ClaimSource{Kind: SourceKindHost},
		Status: ClaimConfirmed, Evidence: []string{"check:x@eval:1"}, Fingerprint: " fp "}, at)
	if err != nil || confirmed.Status != ClaimConfirmed || confirmed.Fingerprint != "fp" {
		t.Fatalf("claim=%+v err=%v", confirmed, err)
	}
	proposed := Claim{Kind: ClaimFact, Statement: "x", Source: ClaimSource{Kind: SourceKindCheck}}
	if err := proposed.Settle(ClaimConfirmed, ClaimSource{Kind: SourceKindCheck}, "", []string{"check:x@eval:1"}, "", at); err == nil {
		t.Fatal("settling at an unknown fingerprint must be refused")
	}
	if err := proposed.Settle(ClaimConfirmed, ClaimSource{Kind: SourceKindCheck}, "fp", []string{"check:x@eval:1"}, "", at); err != nil {
		t.Fatal(err)
	}
}

func TestReviewNormalization(t *testing.T) {
	at := time.Now()
	if _, err := NormalizeReview(ReviewResult{Passed: true, Findings: []ReviewFinding{{Severity: SeverityMajor, Issue: "x"}}}, at); err == nil {
		t.Fatal("a passed review must not carry a blocking finding")
	}
	if _, err := NormalizeReview(ReviewResult{Passed: true, Source: SourceHostAuthored}, at); err == nil {
		t.Fatal("a review's source must be independent or official")
	}
	review, err := NormalizeReview(ReviewResult{Passed: false, Source: SourceIndependent,
		Findings: []ReviewFinding{{Severity: SeverityMajor, Issue: "units wrong", Suggestion: "convert to metres"}}}, at)
	if err != nil {
		t.Fatal(err)
	}
	v := review.Verdict()
	if v.Level != LevelSemantic || v.Status != StatusFailed || !strings.Contains(v.Detail, "units wrong") {
		t.Fatalf("verdict=%+v", v)
	}
	ok := ReviewResult{Passed: true, Source: SourceIndependent}.Verdict()
	if ok.Status != StatusPassed {
		t.Fatalf("verdict=%+v", ok)
	}
}
