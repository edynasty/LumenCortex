package knowledge

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// ScopeKind describes visibility, not a string prefix or a permission supplied by a model.
type ScopeKind string

const (
	ScopeGlobal    ScopeKind = "global"
	ScopeProject   ScopeKind = "project"
	ScopeWorkspace ScopeKind = "workspace"
	ScopeSession   ScopeKind = "session"
	ScopeReader    ScopeKind = "reader"
	ScopeSource    ScopeKind = "source"
)

// Scope identifies the authority and the narrowing boundary of a record. Project and workspace IDs
// are opaque, host-derived identities, never paths supplied by a candidate. Source is a restriction on
// an owner session, not an ancestor of Project. Expiration belongs to Times, not identity.
type Scope struct {
	Kind        ScopeKind `json:"kind"`
	OwnerID     string    `json:"ownerId"`
	ProjectID   string    `json:"projectId,omitempty"`
	WorkspaceID string    `json:"workspaceId,omitempty"`
	SessionID   string    `json:"sessionId,omitempty"`
	ReaderID    string    `json:"readerId,omitempty"`
	SourceID    string    `json:"sourceId,omitempty"`
}

func (s Scope) Validate() error {
	if !plainText(s.OwnerID, 256) {
		return errors.New("scope owner is required")
	}
	for _, v := range []string{s.ProjectID, s.WorkspaceID, s.SessionID, s.ReaderID, s.SourceID} {
		if v != "" && !plainText(v, 256) {
			return errors.New("scope identity is malformed")
		}
	}
	if s.Kind == ScopeGlobal {
		if s.ProjectID != "" || s.WorkspaceID != "" || s.SessionID != "" || s.ReaderID != "" || s.SourceID != "" {
			return errors.New("global scope cannot carry a narrower identity")
		}
		return nil
	}
	if s.ProjectID == "" {
		return errors.New("project identity is required")
	}
	switch s.Kind {
	case ScopeProject:
		if s.WorkspaceID != "" || s.SessionID != "" || s.ReaderID != "" || s.SourceID != "" {
			return errors.New("project scope cannot carry a narrower identity")
		}
	case ScopeWorkspace:
		if s.WorkspaceID == "" || s.SessionID != "" || s.ReaderID != "" || s.SourceID != "" {
			return errors.New("workspace scope requires only a workspace identity")
		}
	case ScopeSession:
		if s.WorkspaceID == "" || s.SessionID == "" || s.ReaderID != "" || s.SourceID != "" {
			return errors.New("session scope requires its workspace and session")
		}
	case ScopeReader:
		if s.WorkspaceID == "" || s.SessionID == "" || s.ReaderID == "" || s.SourceID != "" {
			return errors.New("reader scope requires its workspace, session and reader")
		}
	case ScopeSource:
		if s.WorkspaceID == "" || s.SessionID == "" || s.SourceID == "" || s.ReaderID != "" {
			return errors.New("source scope requires its owner session and source")
		}
	default:
		return fmt.Errorf("unknown scope kind %q", s.Kind)
	}
	return nil
}

// Reader is constructed by the runtime from the current store, session and workspace. A model never
// chooses these fields. GlobalAccess is an explicit host grant; P0 does not create a global store.
type Reader struct {
	OwnerID      string   `json:"ownerId"`
	ProjectID    string   `json:"projectId"`
	WorkspaceID  string   `json:"workspaceId"`
	SessionID    string   `json:"sessionId,omitempty"`
	ID           string   `json:"id,omitempty"`
	SourceIDs    []string `json:"sourceIds,omitempty"`
	GlobalAccess bool     `json:"globalAccess,omitempty"`
}

func (s Scope) VisibleTo(r Reader) bool {
	if s.Validate() != nil || s.OwnerID != r.OwnerID {
		return false
	}
	if s.Kind == ScopeGlobal {
		return r.GlobalAccess
	}
	if s.ProjectID != r.ProjectID {
		return false
	}
	if s.Kind == ScopeProject {
		return true
	}
	if s.WorkspaceID != r.WorkspaceID {
		return false
	}
	if s.Kind == ScopeWorkspace {
		return true
	}
	if s.SessionID != r.SessionID {
		return false
	}
	switch s.Kind {
	case ScopeSession:
		return true
	case ScopeReader:
		return s.ReaderID == r.ID
	case ScopeSource:
		return slices.Contains(r.SourceIDs, s.SourceID)
	}
	return false
}

// Contains permits narrowing a record, never widening its audience on revision. Moving or copying a
// workspace does not silently transfer ownership. Source and Reader boundaries cannot inherit each
// other's private content. A separate source binding still has to be current and unfenced.
func (s Scope) Contains(child Scope) bool {
	if s.Validate() != nil || child.Validate() != nil || s.OwnerID != child.OwnerID {
		return false
	}
	if s == child {
		return true
	}
	if s.Kind == ScopeGlobal {
		return false
	} // an export/import decision is not implicit inheritance
	if s.ProjectID != child.ProjectID || child.Kind == ScopeGlobal {
		return false
	}
	if s.Kind == ScopeProject {
		return true
	}
	if s.WorkspaceID != child.WorkspaceID || child.Kind == ScopeProject {
		return false
	}
	if s.Kind == ScopeWorkspace {
		return true
	}
	if s.SessionID != child.SessionID || child.Kind == ScopeWorkspace {
		return false
	}
	return s.Kind == ScopeSession && (child.Kind == ScopeReader || child.Kind == ScopeSource)
}

// ValidTime is a half-open interval [From, Until). Zero endpoints are unknown/unbounded, not an
// invented observation date. It describes the world, independently of when Core learned the claim.
type ValidTime struct {
	From  time.Time `json:"from,omitempty"`
	Until time.Time `json:"until,omitempty"`
}

func (v ValidTime) Validate() error {
	if !v.From.IsZero() && !v.Until.IsZero() && !v.Until.After(v.From) {
		return errors.New("valid time must be a nonempty half-open interval")
	}
	return nil
}

func (v ValidTime) Overlaps(other ValidTime) bool {
	return v.Validate() == nil && other.Validate() == nil &&
		(v.Until.IsZero() || other.From.IsZero() || other.From.Before(v.Until)) &&
		(other.Until.IsZero() || v.From.IsZero() || v.From.Before(other.Until))
}

// Times keeps five different events. RecordedAt and invalidation/verification timestamps are assigned
// by Core; observed/valid time may describe an external observation. Commit revision orders concurrent
// writes; wall clock is not a substitute. Historical rows remain immutable except privacy erasure.
type Times struct {
	ObservedAt    time.Time `json:"observedAt,omitempty"`
	Valid         ValidTime `json:"valid"`
	RecordedAt    time.Time `json:"recordedAt"`
	VerifiedAt    time.Time `json:"verifiedAt,omitempty"`
	InvalidatedAt time.Time `json:"invalidatedAt,omitempty"`
}

func (t Times) Validate() error {
	if t.RecordedAt.IsZero() {
		return errors.New("recorded time is required")
	}
	return t.Valid.Validate()
}

// TimeState returns a use-time limitation. Future-dated observations and verification never acquire
// freshness through a negative age. Invalidation cannot be undone by refreshing the same identity.
func (t Times) TimeState(at time.Time) string {
	if t.Validate() != nil || at.IsZero() {
		return "invalid_time"
	}
	if !t.InvalidatedAt.IsZero() {
		return "invalidated"
	}
	if t.RecordedAt.After(at) || t.ObservedAt.After(at) || t.VerifiedAt.After(at) {
		return "future_observation"
	}
	if !t.Valid.From.IsZero() && at.Before(t.Valid.From) {
		return "not_yet_valid"
	}
	if !t.Valid.Until.IsZero() && !at.Before(t.Valid.Until) {
		return "expired"
	}
	return ""
}

func FreshAt(observed, at time.Time, window time.Duration) bool {
	return !observed.IsZero() && !at.Before(observed) && window > 0 && at.Sub(observed) < window
}

// Evidence identifies a traceable observation. Type/trust say who produced it, not whether its claim
// is true. Only the runtime may set passed plus the exact approved binding and closed result identity.
// Supporting and contradicting claims are stable assertion digests, never model-generated prose.
type Evidence struct {
	ID                  string    `json:"id"`
	Source              SourceRef `json:"source"`
	Type                string    `json:"type"`
	Provenance          string    `json:"provenance"`
	Trust               string    `json:"trust"`
	Verification        string    `json:"verification"`
	Scope               Scope     `json:"scope"`
	Times               Times     `json:"times"`
	Freshness           Freshness `json:"freshness"`
	Fingerprint         string    `json:"fingerprint,omitempty"`
	BindingDigest       string    `json:"bindingDigest,omitempty"`
	SpecDigest          string    `json:"specDigest,omitempty"`
	ApprovalEpoch       int64     `json:"approvalEpoch,omitempty"`
	SessionID           string    `json:"sessionId,omitempty"`
	RunID               string    `json:"runId,omitempty"`
	ResultSeq           int64     `json:"resultSeq,omitempty"`
	SupportingClaims    []string  `json:"supportingClaims,omitempty"`
	ContradictingClaims []string  `json:"contradictingClaims,omitempty"`
	InvalidationReason  string    `json:"invalidationReason,omitempty"`
}

func (e Evidence) Validate() error {
	if !plainText(e.ID, 128) {
		return errors.New("evidence identity is required")
	}
	if err := e.Source.Validate(); err != nil {
		return err
	}
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	if err := e.Times.Validate(); err != nil {
		return err
	}
	if e.Source.Sensitivity == SensitivityPrivate && (e.Scope.SessionID != e.Source.SessionID || e.Scope.Kind == ScopeProject || e.Scope.Kind == ScopeWorkspace || e.Scope.Kind == ScopeGlobal) {
		return errors.New("private evidence cannot leave its source session")
	}
	if e.Provenance != ProvenanceRuntime && e.Provenance != "model" && e.Provenance != "source" {
		return errors.New("unknown evidence provenance")
	}
	if e.Type == "model" && (e.Provenance != "model" || e.Trust != "model" || e.Verification != "unverified") {
		return errors.New("model evidence cannot assign itself authority")
	}
	switch e.Type {
	case "model", "source", "check":
	default:
		return errors.New("unknown evidence type")
	}
	switch e.Trust {
	case "model", "source", "framework":
	default:
		return errors.New("unknown evidence trust")
	}
	switch e.Verification {
	case "unverified", "passed", "failed", "error":
	default:
		return errors.New("unknown verification status")
	}
	if e.Freshness != FreshnessVersion && e.Freshness != FreshnessRun {
		return errors.New("unknown evidence freshness")
	}
	for _, ref := range e.SupportingClaims {
		if slices.Contains(e.ContradictingClaims, ref) {
			return errors.New("one observation cannot support and refute the same claim")
		}
	}
	for _, refs := range [][]string{e.SupportingClaims, e.ContradictingClaims} {
		if len(refs) > MaxSourceRefs {
			return errors.New("too many evidence claims")
		}
		for _, ref := range refs {
			if !digestPattern.MatchString(ref) {
				return errors.New("evidence claim is not a digest")
			}
		}
	}
	if e.Verification == "passed" || e.Verification == "failed" {
		if e.Type != "check" || e.Provenance != ProvenanceRuntime || e.Trust != "framework" ||
			e.SessionID == "" || e.ResultSeq <= 0 || e.ApprovalEpoch <= 0 || e.Times.VerifiedAt.IsZero() ||
			!digestPattern.MatchString(e.BindingDigest) || !digestPattern.MatchString(e.SpecDigest) || e.Fingerprint == "" {
			return errors.New("verified evidence requires an exact runtime check binding and result")
		}
	}
	return nil
}

// EvidenceContext contains current facts gathered by Core; passing this pure predicate is necessary,
// not sufficient for admission: the store rechecks authority and fences in the commit transaction.
type EvidenceContext struct {
	Reader          Reader
	At              time.Time
	SourceVersion   string
	SourceWithdrawn bool
	SourceMissing   bool
	BindingActive   bool
	Fingerprint     string
	RunID           string
	InRun           bool
}

// ClaimsConflict compares exact propositions only in the same condition domain and overlapping
// valid time. Retrieval rank and observation recency do not choose a winner.
func ClaimsConflict(a, b Assertion, aScope, bScope Scope, aTime, bTime ValidTime) bool {
	return a.Validate() == nil && b.Validate() == nil && aScope == bScope && aTime.Overlaps(bTime) && a.Ref() == b.Ref() && a.ValueDigest() != b.ValueDigest()
}

func (e Evidence) Applicability(c EvidenceContext) (Applicability, string) {
	if c.SourceWithdrawn {
		return Withdrawn, "source_withdrawn"
	}
	if e.Validate() != nil {
		return Unknown, "invalid_evidence"
	}
	if !e.Scope.VisibleTo(c.Reader) {
		return Withdrawn, "scope_denied"
	}
	if c.SourceMissing {
		return SourceMissing, "source_missing"
	}
	if c.SourceVersion != e.Source.Digest {
		return Stale, "source_version_changed"
	}
	if reason := e.Times.TimeState(c.At); reason != "" {
		if reason == "not_yet_valid" || reason == "future_observation" {
			return Unknown, reason
		}
		return Stale, reason
	}
	if e.Verification == "unverified" || !c.BindingActive {
		return CandidateOnly, "not_verified"
	}
	if e.Verification == "error" {
		return Unknown, "verification_error"
	}
	if c.Fingerprint == "" || e.Fingerprint != c.Fingerprint {
		return Unknown, "state_changed"
	}
	if e.Freshness == FreshnessRun && (e.RunID == "" || e.RunID != c.RunID || e.SessionID != c.Reader.SessionID || !c.InRun || !FreshAt(e.Times.VerifiedAt, c.At, RunReuseWindow)) {
		return Unknown, "reader_revalidation_required"
	}
	if e.Verification == "failed" {
		return Stale, "verification_failed"
	}
	return Current, "bound_check_passed"
}
