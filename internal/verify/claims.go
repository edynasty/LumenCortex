package verify

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The four kinds of basis a task records (spec §23). They are explicit text, never a summary of a
// model's reasoning: a contract is what the user or host requires, a fact what was observed, an
// assumption what the agent inferred, and evidence what an independent check decided.
type ClaimKind string

const (
	ClaimContract   ClaimKind = "contract"
	ClaimFact       ClaimKind = "fact"
	ClaimAssumption ClaimKind = "assumption"
	ClaimEvidence   ClaimKind = "evidence"
)

// Valid reports whether k is a claim kind.
func (k ClaimKind) Valid() bool {
	return k == ClaimContract || k == ClaimFact || k == ClaimAssumption || k == ClaimEvidence
}

// Claim statuses. Only a trusted source confirms evidence; a model's own claim never becomes
// confirmation, and a confirmation whose fingerprint moved is invalidated, not silently reused.
type ClaimStatus string

const (
	ClaimProposed    ClaimStatus = "proposed"
	ClaimConfirmed   ClaimStatus = "confirmed"
	ClaimRejected    ClaimStatus = "rejected"
	ClaimInvalidated ClaimStatus = "invalidated"
	ClaimRevalidated ClaimStatus = "revalidated"
	ClaimUnknown     ClaimStatus = "unknown"
)

// Valid reports whether s is a claim status.
func (s ClaimStatus) Valid() bool {
	switch s {
	case ClaimProposed, ClaimConfirmed, ClaimRejected, ClaimInvalidated, ClaimRevalidated, ClaimUnknown:
		return true
	}
	return false
}

// Settled reports a status that needs no further action: confirmed, revalidated or rejected.
func (s ClaimStatus) Settled() bool {
	return s == ClaimConfirmed || s == ClaimRevalidated || s == ClaimRejected
}

// Claim source kinds. They name where a claim came from, not who may confirm it.
const (
	SourceKindUser     = "user"
	SourceKindHost     = "host"
	SourceKindModel    = "model"
	SourceKindTool     = "tool"
	SourceKindCheck    = "check"
	SourceKindEval     = "eval"
	SourceKindReview   = "review"
	SourceKindOfficial = "official"
)

// ClaimSource is a claim's provenance: a kind and a locator, never a copy of the source's content.
type ClaimSource struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
}

// Trusted reports a source that may confirm or reject a claim. A model's own statement is not
// independent, whatever it says about itself.
func (s ClaimSource) Trusted() bool {
	switch s.Kind {
	case SourceKindUser, SourceKindHost, SourceKindCheck, SourceKindEval, SourceKindReview, SourceKindOfficial:
		return true
	}
	return false
}

// Valid reports whether the source kind is known.
func (s ClaimSource) Valid() bool {
	switch s.Kind {
	case SourceKindUser, SourceKindHost, SourceKindModel, SourceKindTool, SourceKindCheck, SourceKindEval, SourceKindReview, SourceKindOfficial:
		return true
	}
	return false
}

// ClaimEvent is one status change of a claim, kept so the original attempts and their outcomes
// survive a later repair.
type ClaimEvent struct {
	Status      ClaimStatus `json:"status"`
	By          string      `json:"by"`
	At          time.Time   `json:"at"`
	Fingerprint string      `json:"fingerprint,omitempty"`
	Note        string      `json:"note,omitempty"`
}

// Claim is one durable basis record. It is stored with the session's Task State, so it outlives the
// transcript window without a second memory.
type Claim struct {
	ID        string      `json:"id"`
	Kind      ClaimKind   `json:"kind"`
	Statement string      `json:"statement"`
	Source    ClaimSource `json:"source"`
	Status    ClaimStatus `json:"status"`
	// Required marks a claim the completion gate must find confirmed. A required assumption is a
	// transform the task depends on; a required contract is a deliverable the user stated.
	Required bool `json:"required,omitempty"`
	// Evidence are locators of what confirmed the claim, e.g. "check:io@eval:12" or "review:rq-…".
	Evidence []string `json:"evidence,omitempty"`
	// Fingerprint is the workspace state the claim was confirmed at. A different current
	// fingerprint makes a confirmation stale.
	Fingerprint string       `json:"fingerprint,omitempty"`
	At          time.Time    `json:"at"`
	UpdatedAt   time.Time    `json:"updatedAt"`
	History     []ClaimEvent `json:"history,omitempty"`
}

// Bounds of a claim.
const (
	MaxClaims         = 64
	MaxClaimStatement = 1024
	MaxClaimEvidence  = 8
	MaxClaimHistory   = 16
)

var (
	// ErrTooManyClaims: the session holds MaxClaims claims.
	ErrTooManyClaims = errors.New("too many claims")
	// ErrClaimNotFound: no claim has the id.
	ErrClaimNotFound = errors.New("claim not found")
	// ErrClaimNotTrusted: only a trusted source may settle a claim.
	ErrClaimNotTrusted = errors.New("only a host, user, check, evaluation, review or official source may settle a claim")
)

// NormalizeClaim validates a claim a caller is about to record and returns it with the defaults a
// model-written claim gets: status proposed, and no evidence.
func NormalizeClaim(c Claim, at time.Time) (Claim, error) {
	c.ID = strings.TrimSpace(c.ID)
	c.Statement = strings.TrimSpace(c.Statement)
	if c.Statement == "" || len(c.Statement) > MaxClaimStatement || !utf8.ValidString(c.Statement) {
		return Claim{}, fmt.Errorf("claim statement must be 1-%d bytes of text", MaxClaimStatement)
	}
	if !c.Kind.Valid() {
		return Claim{}, fmt.Errorf("unknown claim kind %q", c.Kind)
	}
	if !c.Source.Valid() {
		return Claim{}, fmt.Errorf("unknown claim source %q", c.Source.Kind)
	}
	if c.Source.Kind == SourceKindModel {
		// A model may state a contract, a fact or an assumption; it may not manufacture evidence,
		// and it may not confirm its own claim.
		if c.Kind == ClaimEvidence {
			return Claim{}, errors.New("a model cannot record evidence: evidence comes from a check, evaluation, review or official verifier")
		}
		if c.Status != "" && c.Status != ClaimProposed {
			return Claim{}, errors.New("a model may only propose a claim; a trusted source settles it")
		}
		c.Status = ClaimProposed
		c.Evidence = nil
		c.Fingerprint = ""
	}
	if c.Status == "" {
		c.Status = ClaimProposed
	}
	if !c.Status.Valid() {
		return Claim{}, fmt.Errorf("unknown claim status %q", c.Status)
	}
	// A claim recorded already settled carries the proof it was settled with: a confirmation names its
	// evidence locators and the workspace state they were read at, and only a trusted source may record
	// one. Without that proof it would count as confirmed while nothing independent decided it.
	if c.Status == ClaimConfirmed || c.Status == ClaimRevalidated {
		if !c.Source.Trusted() {
			return Claim{}, ErrClaimNotTrusted
		}
		if len(c.Evidence) == 0 {
			return Claim{}, errors.New("confirming a claim needs at least one evidence locator")
		}
		c.Fingerprint = strings.TrimSpace(c.Fingerprint)
		if c.Fingerprint == "" {
			return Claim{}, errors.New("confirming a claim needs the workspace fingerprint its evidence was read at")
		}
	}
	if len(c.Evidence) > MaxClaimEvidence {
		c.Evidence = append([]string(nil), c.Evidence[:MaxClaimEvidence]...)
	}
	if c.At.IsZero() {
		c.At = at
	}
	c.UpdatedAt = at
	c.History = append([]ClaimEvent(nil), c.History...)
	return c, nil
}

// Settle moves a claim to status by source at fingerprint, recording the change. Confirming or
// revalidating requires a trusted source and at least one evidence locator; rejecting requires a
// trusted source. Invalidating is the runtime's own transition and needs neither.
func (c *Claim) Settle(status ClaimStatus, by ClaimSource, fingerprint string, evidence []string, note string, at time.Time) error {
	if c == nil {
		return errors.New("nil claim")
	}
	if !status.Valid() {
		return fmt.Errorf("unknown claim status %q", status)
	}
	switch status {
	case ClaimConfirmed, ClaimRevalidated:
		if !by.Trusted() {
			return ErrClaimNotTrusted
		}
		if len(evidence) == 0 {
			return errors.New("confirming a claim needs at least one evidence locator")
		}
		fingerprint = strings.TrimSpace(fingerprint)
		if fingerprint == "" {
			// An unknown workspace state cannot tie the evidence to anything: recording the
			// confirmation would make it count forever, so it is refused instead.
			return errors.New("confirming a claim needs the workspace fingerprint its evidence was read at")
		}
	case ClaimRejected:
		if !by.Trusted() {
			return ErrClaimNotTrusted
		}
	case ClaimInvalidated:
		// The runtime invalidates a stale confirmation; no external source is needed.
	case ClaimProposed:
		if c.Source.Kind != SourceKindModel {
			return errors.New("only a model-proposed claim returns to proposed")
		}
	}
	c.Status = status
	if len(evidence) > 0 {
		c.Evidence = append([]string(nil), evidence[:min(len(evidence), MaxClaimEvidence)]...)
	}
	if status == ClaimConfirmed || status == ClaimRevalidated {
		c.Fingerprint = fingerprint
	}
	c.UpdatedAt = at
	event := ClaimEvent{Status: status, By: by.Kind, At: at, Fingerprint: fingerprint, Note: truncate(note, MaxReasonBytes)}
	if by.Ref != "" {
		event.By = by.Kind + ":" + by.Ref
	}
	c.History = append(c.History, event)
	if len(c.History) > MaxClaimHistory {
		c.History = append([]ClaimEvent(nil), c.History[len(c.History)-MaxClaimHistory:]...)
	}
	return nil
}

// Stale reports a claim whose confirmation cannot be matched to the current workspace state. A
// confirmation carries the fingerprint its evidence was read at, so an unknown current fingerprint
// establishes no match and the confirmation is stale rather than reused. A claim with no fingerprint
// is not stale: it was never tied to workspace content, and NormalizeClaim/Settle no longer record a
// confirmation that way.
func (c Claim) Stale(current string) bool {
	if c.Status != ClaimConfirmed && c.Status != ClaimRevalidated {
		return false
	}
	if c.Fingerprint == "" {
		return false
	}
	return c.Fingerprint != current
}

// Invalidated reports a claim the runtime marked invalid; it needs revalidation before it counts.
func (c Claim) Invalidated() bool { return c.Status == ClaimInvalidated }

// Coverage is what a session's claims cover at one fingerprint.
type Coverage struct {
	// Required is how many claims are required.
	Required int `json:"required"`
	// Confirmed is how many required claims are confirmed or revalidated at the current fingerprint.
	Confirmed int `json:"confirmed"`
	// Stale is how many required confirmations moved with the workspace.
	Stale int `json:"stale"`
	// Unsettled is how many required claims are proposed or unknown.
	Unsettled int `json:"unsettled"`
	// Rejected is how many required claims a trusted source rejected.
	Rejected int `json:"rejected"`
	// Missing names the required claims that do not count, in claim order.
	Missing []string `json:"missing,omitempty"`
}

// Complete reports that every required claim counts.
func (c Coverage) Complete() bool { return c.Required == 0 || len(c.Missing) == 0 }

// ClaimCoverage computes the coverage of claims at the current fingerprint.
func ClaimCoverage(claims []Claim, current string) Coverage {
	var out Coverage
	for _, c := range claims {
		if !c.Required {
			continue
		}
		out.Required++
		switch {
		case c.Status == ClaimRejected:
			out.Rejected++
			out.Missing = append(out.Missing, c.ID)
		case c.Stale(current):
			out.Stale++
			out.Missing = append(out.Missing, c.ID)
		case c.Status == ClaimConfirmed || c.Status == ClaimRevalidated:
			out.Confirmed++
		default:
			out.Unsettled++
			out.Missing = append(out.Missing, c.ID)
		}
	}
	return out
}

// InvalidateStale marks every confirmation made at another fingerprint invalid. It reports the ids
// it changed. Revalidation is a separate, explicit step with fresh evidence. An unknown current
// fingerprint cannot prove the workspace moved, so nothing is rewritten: coverage still reports those
// confirmations stale, and the durable evidence survives an unreadable fingerprint.
func InvalidateStale(claims []Claim, current string, at time.Time) []string {
	if strings.TrimSpace(current) == "" {
		return nil
	}
	var changed []string
	for i := range claims {
		if claims[i].Stale(current) {
			claims[i].Status = ClaimInvalidated
			claims[i].UpdatedAt = at
			claims[i].History = append(claims[i].History, ClaimEvent{Status: ClaimInvalidated, By: "runtime", At: at, Fingerprint: current})
			if len(claims[i].History) > MaxClaimHistory {
				claims[i].History = append([]ClaimEvent(nil), claims[i].History[len(claims[i].History)-MaxClaimHistory:]...)
			}
			changed = append(changed, claims[i].ID)
		}
	}
	return changed
}

// FindClaim returns the claim with id.
func FindClaim(claims []Claim, id string) (int, bool) {
	for i := range claims {
		if claims[i].ID == id {
			return i, true
		}
	}
	return -1, false
}
