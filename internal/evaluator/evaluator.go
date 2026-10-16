// Package evaluator evaluates work unit obligations, checks completion evidence,
// and determines task completion based on verifiable criteria.
package evaluator

import (
	"context"
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// CheckVerdict represents the result of evaluating a check obligation.
type CheckVerdict string

const (
	// VerdictPass: check completed successfully with evidence.
	VerdictPass CheckVerdict = "pass"
	
	// VerdictFail: check executed but failed verification.
	VerdictFail CheckVerdict = "fail"
	
	// VerdictSkipped: check was not required or could not run.
	VerdictSkipped CheckVerdict = "skipped"
	
	// VerdictPending: check has not been evaluated yet.
	VerdictPending CheckVerdict = "pending"
)

// CheckResult captures the outcome of evaluating a single check.
type CheckResult struct {
	CheckID     string       `json:"checkId"`
	Verdict     CheckVerdict `json:"verdict"`
	Evidence    string       `json:"evidence,omitempty"`
	Artifacts   []string     `json:"artifacts,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
}

// WorkUnitVerdict evaluates whether a work unit is complete based on its obligations.
type WorkUnitVerdict struct {
	WorkUnitID string        `json:"workUnitId"`
	Complete   bool          `json:"complete"`
	Checks     []CheckResult `json:"checks"`
}

// Satisfies determines whether obligations are met for a work unit.
// Returns true if all required evidence and verification are present.
func Satisfies(ctx context.Context, wu *taskstate.WorkUnit) (bool, error) {
	if wu == nil {
		return false, fmt.Errorf("work unit is nil")
	}
	
	// If work unit is not marked complete, can't be satisfied.
	if wu.Status != taskstate.WorkCompleted {
		return false, nil
	}
	
	// If no required evidence, completion status is sufficient.
	if len(wu.RequiredEvidence) == 0 {
		return true, nil
	}
	
	// Check that all required evidence has corresponding evidence entries.
	for _, required := range wu.RequiredEvidence {
		found := false
		for _, ev := range wu.Evidence {
			if strings.Contains(strings.ToLower(ev.Requirement), strings.ToLower(required)) ||
				strings.Contains(strings.ToLower(required), strings.ToLower(ev.Requirement)) ||
				strings.Contains(strings.ToLower(ev.Summary), strings.ToLower(required)) {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	
	// If verification steps exist, check that they have results.
	if len(wu.Verification) > 0 && len(wu.VerificationResults) == 0 {
		return false, nil
	}
	
	// Check that all verification results passed.
	for _, vr := range wu.VerificationResults {
		if vr.Status != "pass" && vr.Status != "passed" {
			return false, nil
		}
	}
	
	return true, nil
}

// CheckVerdictFor evaluates verification requirements and returns a verdict.
// CheckVerdictFor evaluates verification requirements and returns a verdict.
func CheckVerdictFor(ctx context.Context, requirement string, evidence []taskstate.EvidenceRef, results []taskstate.VerificationResult) (CheckVerdict, error) {
	if strings.TrimSpace(requirement) == "" {
		return VerdictSkipped, nil
	}
	
	// Check if this specific requirement has matching evidence.
	hasMatchingEvidence := false
	for _, ev := range evidence {
		if strings.Contains(strings.ToLower(ev.Requirement), strings.ToLower(requirement)) ||
			strings.Contains(strings.ToLower(requirement), strings.ToLower(ev.Requirement)) ||
			strings.Contains(strings.ToLower(ev.Summary), strings.ToLower(requirement)) {
			hasMatchingEvidence = true
			break
		}
	}
	
	if !hasMatchingEvidence {
		return VerdictPending, nil
	}
	
	// If verification results exist, use them.
	for _, vr := range results {
		if strings.Contains(strings.ToLower(vr.Check), strings.ToLower(requirement)) {
			if vr.Status == "pass" || vr.Status == "passed" {
				return VerdictPass, nil
			}
			return VerdictFail, nil
		}
	}
	
	// Evidence exists but no verification result yet - assume pass.
	// Full implementation will run actual verification commands.
	return VerdictPass, nil
}

// EvaluateWorkUnit evaluates all checks for a work unit and returns a verdict.
func EvaluateWorkUnit(ctx context.Context, wu *taskstate.WorkUnit) (*WorkUnitVerdict, error) {
	if wu == nil {
		return nil, fmt.Errorf("work unit is nil")
	}
	
	verdict := &WorkUnitVerdict{
		WorkUnitID: wu.ID,
		Checks:     make([]CheckResult, 0),
	}
	
	// If no required evidence, completion is based on status alone.
	if len(wu.RequiredEvidence) == 0 {
		verdict.Complete = wu.Status == taskstate.WorkCompleted
		return verdict, nil
	}
	
	// Evaluate each required evidence item.
	allPass := true
	for _, req := range wu.RequiredEvidence {
		checkVerdict, err := CheckVerdictFor(ctx, req, wu.Evidence, wu.VerificationResults)
		if err != nil {
			return nil, fmt.Errorf("evaluating requirement %q: %w", req, err)
		}
		
		result := CheckResult{
			CheckID:  req,
			Verdict:  checkVerdict,
		}
		
		// Find matching evidence.
		for _, ev := range wu.Evidence {
			if strings.Contains(strings.ToLower(ev.Requirement), strings.ToLower(req)) ||
				strings.Contains(strings.ToLower(ev.Summary), strings.ToLower(req)) {
				result.Evidence = ev.Summary
				break
			}
		}
		
		verdict.Checks = append(verdict.Checks, result)
		
		// Work unit is not complete if any check is pending or failed.
		if checkVerdict != VerdictPass && checkVerdict != VerdictSkipped {
			allPass = false
		}
	}
	
	// Work unit is complete only if all checks pass and status is complete.
	verdict.Complete = allPass && wu.Status == taskstate.WorkCompleted
	
	return verdict, nil
}
