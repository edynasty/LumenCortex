package taskstate

import (
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/verify"
)

func TestClaimsAreDurableTaskState(t *testing.T) {
	at := time.Now().UTC()
	var state State
	model, err := state.AddModelClaim(verify.Claim{Kind: verify.ClaimAssumption, Statement: "the input is sorted", Required: true}, at)
	if err != nil {
		t.Fatal(err)
	}
	if model.ID != "c1" || model.Status != verify.ClaimProposed {
		t.Fatalf("claim=%+v", model)
	}
	host, err := state.AddHostClaim(verify.Claim{Kind: verify.ClaimContract, Statement: "the report is in metres"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if host.ID != "h2" || host.Source.Kind != verify.SourceKindHost {
		t.Fatalf("claim=%+v", host)
	}
	if _, err := state.AddModelClaim(verify.Claim{Kind: verify.ClaimEvidence, Statement: "x"}, at); err == nil {
		t.Fatal("a model must not record evidence")
	}
	if _, err := state.SettleClaim(model.ID, verify.ClaimConfirmed, verify.ClaimSource{Kind: verify.SourceKindModel}, "fp", []string{"x"}, "", at); err == nil {
		t.Fatal("a model must not settle a claim")
	}
	settled, err := state.SettleClaim(model.ID, verify.ClaimConfirmed, verify.ClaimSource{Kind: verify.SourceKindCheck, Ref: "sorted"},
		"fp", []string{"check:sorted@eval:3"}, "verified", at)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != verify.ClaimConfirmed || len(settled.Evidence) != 1 {
		t.Fatalf("claim=%+v", settled)
	}
	coverage := state.ClaimCoverage("fp")
	if coverage.Required != 1 || coverage.Confirmed != 1 || !coverage.Complete() {
		t.Fatalf("coverage=%+v", coverage)
	}

	// The claims and the verification record survive an encode/decode round trip.
	state.SetReview(verify.ReviewResult{Passed: true, Source: verify.SourceIndependent, Model: "test/b0",
		RequestID: "rq-1", Findings: []verify.ReviewFinding{{Severity: verify.SeverityMinor, Issue: "naming"}}}, at)
	state.SetVerification(verify.GateResult{Status: verify.StatusPassed, Levels: map[verify.Level]verify.Status{verify.LevelFinal: verify.StatusPassed}}, "fp", at)
	raw, err := state.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(1, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Claims) != 2 || decoded.Claims[0].ID != "c1" {
		t.Fatalf("claims=%+v", decoded.Claims)
	}
	if decoded.Verification == nil || decoded.Verification.Status != string(verify.StatusPassed) || decoded.Verification.Review == nil || !decoded.Verification.Review.Passed {
		t.Fatalf("verification=%+v", decoded.Verification)
	}

	// A moved workspace invalidates the confirmation; revalidation is explicit.
	changed := decoded.InvalidateStaleClaims("other", at)
	if len(changed) != 1 || changed[0] != "c1" {
		t.Fatalf("changed=%v", changed)
	}
	if decoded.ClaimCoverage("other").Complete() {
		t.Fatal("an invalidated claim must not count")
	}
	if _, err := decoded.SettleClaim("c1", verify.ClaimRevalidated, verify.ClaimSource{Kind: verify.SourceKindEval, Ref: "run"},
		"other", []string{"eval:cell-1"}, "rechecked", at); err != nil {
		t.Fatal(err)
	}
	if !decoded.ClaimCoverage("other").Complete() {
		t.Fatal("a revalidated claim must count at the new fingerprint")
	}
}

func TestRequiredClaims(t *testing.T) {
	var state State
	if _, err := state.AddModelClaim(verify.Claim{Kind: verify.ClaimFact, Statement: "one", Required: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := state.AddModelClaim(verify.Claim{Kind: verify.ClaimFact, Statement: "two"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := state.RequiredClaims(); len(got) != 1 || got[0].Statement != "one" {
		t.Fatalf("required=%+v", got)
	}
}
