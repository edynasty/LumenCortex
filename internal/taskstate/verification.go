package taskstate

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/edynasty/LumenCortex/internal/verify"
)

// Verification and basis records live in Task State, beside the notes and work units, so they
// outlive the transcript window without a second memory (spec §23, §24). They hold explicit text
// and runtime verdicts only: no model reasoning is ever written here.

// Claim ID prefixes: a model proposes "c<seq>", the host writes "h<seq>". They never collide, so a
// host claim cannot be mistaken for a model proposal.
const (
	modelClaimPrefix = "c"
	hostClaimPrefix  = "h"
)

// AddModelClaim records a claim a model proposed. It may state a contract, a fact or an assumption,
// never evidence, and it always lands proposed: only a trusted source settles it. It returns the
// stored claim.
func (s *State) AddModelClaim(c verify.Claim, at time.Time) (verify.Claim, error) {
	return s.addClaim(c, verify.SourceKindModel, modelClaimPrefix, at)
}

// AddHostClaim records a claim a host supplied: a contract or a fact with the host's authority. A
// host may also record evidence and settle it.
func (s *State) AddHostClaim(c verify.Claim, at time.Time) (verify.Claim, error) {
	return s.addClaim(c, verify.SourceKindHost, hostClaimPrefix, at)
}

func (s *State) addClaim(c verify.Claim, sourceKind, prefix string, at time.Time) (verify.Claim, error) {
	if len(s.Claims) >= verify.MaxClaims {
		return verify.Claim{}, fmt.Errorf("%w (%d)", verify.ErrTooManyClaims, verify.MaxClaims)
	}
	if c.Source.Kind == "" {
		c.Source.Kind = sourceKind
	}
	normalized, err := verify.NormalizeClaim(c, at)
	if err != nil {
		return verify.Claim{}, err
	}
	if normalized.ID == "" {
		s.ClaimSeq++
		normalized.ID = prefix + strconv.FormatInt(s.ClaimSeq, 10)
	}
	if _, exists := verify.FindClaim(s.Claims, normalized.ID); exists {
		return verify.Claim{}, fmt.Errorf("claim %s already exists", normalized.ID)
	}
	s.Claims = append(s.Claims, normalized)
	return normalized, nil
}

// SettleClaim moves one claim to status by source at the fingerprint, with evidence. It is the only
// way a claim becomes confirmed, rejected, invalidated or revalidated.
func (s *State) SettleClaim(id string, status verify.ClaimStatus, by verify.ClaimSource, fingerprint string, evidence []string, note string, at time.Time) (verify.Claim, error) {
	i, ok := verify.FindClaim(s.Claims, id)
	if !ok {
		return verify.Claim{}, verify.ErrClaimNotFound
	}
	if err := s.Claims[i].Settle(status, by, fingerprint, evidence, note, at); err != nil {
		return verify.Claim{}, err
	}
	return s.Claims[i], nil
}

// InvalidateStaleClaims marks confirmations made at another fingerprint invalid and reports their
// ids. Revalidation is explicit: the caller decides with fresh evidence.
func (s *State) InvalidateStaleClaims(current string, at time.Time) []string {
	return verify.InvalidateStale(s.Claims, current, at)
}

// ClaimCoverage is the coverage of the required claims at the current fingerprint.
func (s State) ClaimCoverage(current string) verify.Coverage {
	return verify.ClaimCoverage(s.Claims, current)
}

// RequiredClaims returns the claims the completion gate must find confirmed.
func (s State) RequiredClaims() []verify.Claim {
	var out []verify.Claim
	for _, c := range s.Claims {
		if c.CountsAsRequired() {
			out = append(out, c)
		}
	}
	return out
}

// VerificationRecord is the durable outcome of the completion gate: the last gate result, every
// bounded repair attempt, and the review when one ran. It is evidence, not a session status: the
// run status stays what the loop wrote.
type VerificationRecord struct {
	Status string `json:"status"`
	// Levels is the status of each verification level the gate read.
	Levels map[string]string `json:"levels,omitempty"`
	// Attempts are the bounded repair attempts, oldest first.
	Attempts []verify.Attempt `json:"attempts,omitempty"`
	// Feedback is the last bounded failure feedback.
	Feedback string `json:"feedback,omitempty"`
	// Review is the independent review, when one ran. One run reviews at most once.
	Review *ReviewRecord `json:"review,omitempty"`
	// Fingerprint is the workspace state the gate decided at.
	Fingerprint string    `json:"fingerprint,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// ReviewRecord is one independent review as the run recorded it.
type ReviewRecord struct {
	Passed    bool                   `json:"passed"`
	Findings  []verify.ReviewFinding `json:"findings,omitempty"`
	Source    string                 `json:"source,omitempty"`
	Model     string                 `json:"model,omitempty"`
	RequestID string                 `json:"requestId,omitempty"`
	At        time.Time              `json:"at"`
}

// SetVerification records the gate result and its attempts.
func (s *State) SetVerification(result verify.GateResult, fingerprint string, at time.Time) {
	levels := map[string]string{}
	for level, status := range result.Levels {
		levels[string(level)] = string(status)
	}
	var review *ReviewRecord
	if s.Verification != nil {
		review = s.Verification.Review
	}
	s.Verification = &VerificationRecord{
		Status: string(result.Status), Levels: levels, Attempts: append([]verify.Attempt(nil), result.Attempts...),
		Feedback: result.Feedback, Fingerprint: fingerprint, UpdatedAt: at, Review: review,
	}
}

// SetReview records the independent review on the verification record, creating it when the gate
// did not write one (a review of a run that failed before completion).
func (s *State) SetReview(review verify.ReviewResult, at time.Time) {
	if s.Verification == nil {
		s.Verification = &VerificationRecord{UpdatedAt: at}
	}
	s.Verification.Review = &ReviewRecord{
		Passed: review.Passed, Findings: append([]verify.ReviewFinding(nil), review.Findings...),
		Source: string(review.Source), Model: review.Model, RequestID: review.RequestID, At: at,
	}
	s.Verification.UpdatedAt = at
}

// ErrNoVerification reports a session whose gate never decided.
var ErrNoVerification = errors.New("the session has no verification record")
