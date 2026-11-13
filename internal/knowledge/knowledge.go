// Package knowledge is the contract of Core's persistent cognition (R1b/T5a, remediation R1): the
// source-neutral SourceRef a maintenance batch is built from, the structured candidate a maintainer may
// propose, the structured Assertion a host may bind to a check, the frozen policy caps, the claim and
// support identities, the freshness classes of an observation, and the applicability of an adopted
// candidate to a reader's scope now.
//
// It is pure. The session store keeps the state, the runtime gathers the facts (fences, source
// versions, blobs, bindings, fresh check results) and runs the maintainer and the checks; this package
// validates and decides. A candidate is untrusted model output whatever it says: nothing here turns its
// text into a fact. The only support a candidate can gain is a qualified observation of the check that
// a host-approved binding assigns to its exact assertion (kind, target, key and value), which the
// runtime makes from that check run in a closed episode. Free text, a check the maintainer names and a
// passing check bound to another proposition support nothing.
package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Policy caps (DATA_AND_FAILURES §4 R1b, EVALUATION §H/B). They are frozen into PolicyVersion: a
// result committed under one policy is not the result of another, and the support identity carries it.
const (
	// SchemaVersion 2 is the support contract of T5a-R1: a candidate is qualified only through a
	// host-approved binding of its structured assertion; version 1 rows named their checks themselves.
	SchemaVersion = 2
	// MaxBatchRefs bounds the sources one maintenance batch reads.
	MaxBatchRefs = 64
	// MaxBatchInputBytes bounds the whole serialized batch message the maintainer receives: the
	// instruction line, the JSON wrapper, the policy, every source projection with its ref and the
	// catalog of supportable assertions. A batch that would exceed it holds fewer sources.
	MaxBatchInputBytes = 32 << 10
	// MaxCandidates bounds the candidates one batch may commit.
	MaxCandidates = 8
	// MaxOutputBytes bounds the maintainer's answer; a longer one is rejected whole.
	MaxOutputBytes = 16 << 10
	// MaxQueuedBatches bounds the batches one trigger processes before it yields; the rest stays dirty.
	MaxQueuedBatches = 64
	// MaxCitedPaths bounds the paths a candidate cites.
	MaxCitedPaths = 4
	// MaxSourceRefs bounds the sources one candidate cites.
	MaxSourceRefs = 8
	// MaxLocators bounds the cognition index a Start receives.
	MaxLocators = 6
	// MaxRevalidations and RevalidationBudget bound the checks a Start runs to establish fresh support
	// for the candidates it inherits.
	MaxRevalidations   = 2
	RevalidationBudget = 15 * time.Second
	// RunReuseWindow bounds how long a run reuses a run-bound observation it made itself (FreshnessRun).
	RunReuseWindow = 5 * time.Minute
	// MaxVerifications bounds the checks one maintenance batch runs to qualify its candidates.
	MaxVerifications = 4
	// Text bounds of one candidate.
	MaxSubjectBytes   = 120
	MaxClaimBytes     = 1024
	MaxRationaleBytes = 1024
	// Bounds of a structured assertion.
	MaxAssertionKeyBytes   = 128
	MaxAssertionValueBytes = 256
	maxPathBytes           = 256
	maxRefBytes            = 512
)

// PolicyVersion names the caps above. It changes whenever one of them does.
var PolicyVersion = policyVersion()

func policyVersion() string {
	caps := fmt.Sprintf("schema=%d refs=%d in=%d cand=%d out=%d queue=%d cited=%d srcs=%d locators=%d reval=%d/%s reuse=%s verify=%d text=%d/%d/%d assertion=%d/%d",
		SchemaVersion, MaxBatchRefs, MaxBatchInputBytes, MaxCandidates, MaxOutputBytes, MaxQueuedBatches, MaxCitedPaths,
		MaxSourceRefs, MaxLocators, MaxRevalidations, RevalidationBudget, RunReuseWindow, MaxVerifications,
		MaxSubjectBytes, MaxClaimBytes, MaxRationaleBytes, MaxAssertionKeyBytes, MaxAssertionValueBytes)
	sum := sha256.Sum256([]byte(caps))
	return "t5a-2." + hex.EncodeToString(sum[:6])
}

// Source kinds with a producer in Core. A kind is added here together with the code that records it;
// any other kind is refused, so an unknown or unauthorized source never enters a batch.
const (
	// KindSession is a top-level session whose run reached a terminal status (completed or failed).
	KindSession = "session"
)

var kinds = map[string]bool{KindSession: true}

// Sensitivity of a source's visible content.
const (
	// SensitivitySession: the session's visible messages, shown under the session's own fences.
	SensitivitySession = "session"
	// SensitivityPrivate: the source may not leave its session; it is never put in a batch.
	SensitivityPrivate = "private"
)

// SourceRef identifies one version of one source in a source-neutral way: what it is (Kind, ID), which
// version (Digest), where it was observed (Scope), which run and session produced it, how sensitive it
// is and how its provenance was established. The runtime fills it from Core's own records, never from
// model or tool input.
type SourceRef struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Digest      string `json:"digest"`
	Scope       string `json:"scope"`
	RunID       string `json:"runId,omitempty"`
	SessionID   string `json:"sessionId"`
	Sensitivity string `json:"sensitivity"`
	// Provenance is how Core knows the version: "runtime" for a record Core itself committed.
	Provenance string `json:"provenance"`
}

// ProvenanceRuntime: the record was committed by Core's own runtime path.
const ProvenanceRuntime = "runtime"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Ref is the stable reference a candidate cites: kind:id@digest.
func (r SourceRef) Ref() string { return r.Kind + ":" + r.ID + "@" + r.Digest }

// ParseRef splits a reference Ref produced into its kind, id and digest; ok is false for anything
// else. The id may contain neither ':' at its start nor '@'.
func ParseRef(ref string) (kind, id, digest string, ok bool) {
	kind, rest, found := strings.Cut(ref, ":")
	if !found || !kinds[kind] {
		return "", "", "", false
	}
	id, digest, found = strings.Cut(rest, "@")
	if !found || id == "" || !digestPattern.MatchString(digest) {
		return "", "", "", false
	}
	return kind, id, digest, true
}

// Validate refuses a ref Core could not have produced: an unknown kind, a malformed digest, a missing
// scope or session, an unknown sensitivity or provenance.
func (r SourceRef) Validate() error {
	switch {
	case !kinds[r.Kind]:
		return fmt.Errorf("source kind %q is not authorized", r.Kind)
	case r.ID == "" || len(r.ID) > maxRefBytes:
		return errors.New("source id must be 1-512 bytes")
	case !digestPattern.MatchString(r.Digest):
		return fmt.Errorf("source digest %q is not sha256:<64 hex>", r.Digest)
	case r.Scope == "" || len(r.Scope) > maxRefBytes:
		return errors.New("source scope must be 1-512 bytes")
	case r.SessionID == "":
		return errors.New("source session is required")
	case r.Sensitivity != SensitivitySession && r.Sensitivity != SensitivityPrivate:
		return fmt.Errorf("source sensitivity %q is unknown", r.Sensitivity)
	case r.Provenance != ProvenanceRuntime:
		return fmt.Errorf("source provenance %q is not runtime", r.Provenance)
	}
	return nil
}

// Digest hashes parts with a separator no part can contain, as sha256:<hex>.
func Digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DedupeKey is the identity of one processing of a source: authority, source version, reader scope and
// policy. A source seen again at the same version under the same policy is not processed again; a new
// version or a new policy is.
func DedupeKey(r SourceRef, readerScope string) string {
	return Digest("core", r.Kind, r.ID, r.Digest, readerScope, PolicyVersion)
}

// RedoKey is the identity of the next processing of a source version whose earlier processing, recorded
// under key, no longer stands: the maintenance session that did it was fenced (a source of its batch
// was revoked or deleted), which withdrew everything that session proposed. The earlier record is kept
// as written; the source version is processed once more under this key.
func RedoKey(key, maintenanceSessionID string) string {
	return Digest("redo", key, maintenanceSessionID)
}

// Claim types a maintainer may propose. They are fixed: a type is pushed to later runs, so it may not
// carry free text.
const (
	ClaimProcedure = "procedure"
	ClaimFact      = "fact"
	ClaimFailure   = "failure"
	ClaimDecision  = "decision"
)

var claimTypes = map[string]bool{ClaimProcedure: true, ClaimFact: true, ClaimFailure: true, ClaimDecision: true}

// Stored candidate statuses. A candidate row is never rewritten: the status records the decision made
// when it was committed; what holds now is its Applicability.
const (
	// StatusQualified: when committed, the check a host-approved binding assigns to its assertion
	// passed in a closed episode.
	StatusQualified = "qualified"
	// StatusCandidateOnly: well-formed and sourced, but no host-approved binding supported it.
	StatusCandidateOnly = "candidate_only"
	// StatusRejected: malformed, unsourced, or naming a source or path Core does not know.
	StatusRejected = "rejected"
)

// Proposal is one candidate as the maintainer wrote it. Every field is untrusted. It names no check
// and no status: support is decided by Core from Assertion and the host's bindings alone, and an entry
// that carries any other field (checkIds, status, ...) does not decode.
type Proposal struct {
	ClaimType  string     `json:"claimType"`
	Subject    string     `json:"subject"`
	Claim      string     `json:"claim,omitempty"`
	Rationale  string     `json:"rationale"`
	SourceRefs []string   `json:"sourceRefs"`
	CitedPaths []string   `json:"citedPaths,omitempty"`
	Assertion  *Assertion `json:"assertion,omitempty"`
}

// Assertion kinds: the structured propositions a host may bind to a check. Each has a fixed shape, so
// a binding names exactly one value of exactly one key, and a check result can support nothing else.
const (
	// AssertConfigValue: in the configuration file Target, Key has the value Value.
	AssertConfigValue = "config_value"
	// AssertDependencyVersion: the manifest Target pins dependency Key at version Value.
	AssertDependencyVersion = "dependency_version"
	// AssertSymbolSignature: in source file Target, symbol Key has the signature Value.
	AssertSymbolSignature = "symbol_signature"
	// AssertFileExists: the workspace file Target is "present" or "absent" (Value); no Key.
	AssertFileExists = "file_exists"
	// AssertPortValue: the service named Target listens on port Value (1-65535); no Key.
	AssertPortValue = "port_value"
)

// Assertion is the structured, deterministic part of a fact candidate: what a bound check verifies.
// The predicate is always equality of Value: a binding approves one (Kind, Target, Key, Value), so a
// different value, key, target or kind has a different digest and no binding.
type Assertion struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Key    string `json:"key,omitempty"`
	Value  string `json:"value"`
}

var (
	pathKinds    = map[string]bool{AssertConfigValue: true, AssertDependencyVersion: true, AssertSymbolSignature: true, AssertFileExists: true}
	keyedKinds   = map[string]bool{AssertConfigValue: true, AssertDependencyVersion: true, AssertSymbolSignature: true}
	serviceName  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	portNumber   = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	assertionRef = "lc-claim:v1:"
)

// Validate refuses an assertion of an unknown kind or of the wrong shape for its kind.
func (a Assertion) Validate() error {
	if !pathKinds[a.Kind] && a.Kind != AssertPortValue {
		return fmt.Errorf("assertion kind %q is not one of config_value, dependency_version, symbol_signature, file_exists, port_value", a.Kind)
	}
	if pathKinds[a.Kind] && !CleanPath(a.Target) {
		return fmt.Errorf("assertion target %q is not a clean workspace-relative path", clip(a.Target))
	}
	if a.Kind == AssertPortValue && !serviceName.MatchString(a.Target) {
		return fmt.Errorf("assertion target %q is not a service name", clip(a.Target))
	}
	if keyedKinds[a.Kind] != (a.Key != "") {
		if a.Key == "" {
			return fmt.Errorf("a %s assertion names its key", a.Kind)
		}
		return fmt.Errorf("a %s assertion has no key", a.Kind)
	}
	if a.Key != "" && !plainText(a.Key, MaxAssertionKeyBytes) {
		return fmt.Errorf("assertion key must be 1-%d bytes of single-line text", MaxAssertionKeyBytes)
	}
	switch {
	case !plainText(a.Value, MaxAssertionValueBytes):
		return fmt.Errorf("assertion value must be 1-%d bytes of single-line text", MaxAssertionValueBytes)
	case a.Kind == AssertFileExists && a.Value != "present" && a.Value != "absent":
		return errors.New(`a file_exists assertion's value is "present" or "absent"`)
	case a.Kind == AssertPortValue && !validPort(a.Value):
		return errors.New("a port_value assertion's value is a port number 1-65535")
	case len(a.Ref()) > maxRefBytes:
		return fmt.Errorf("assertion reference exceeds %d bytes", maxRefBytes)
	}
	return nil
}

// Ref names what the assertion is about, without its value: the constraint ref a host binds to a
// check. Two assertions with one Ref and different values contradict each other.
func (a Assertion) Ref() string {
	raw, _ := json.Marshal([]string{a.Kind, a.Target, a.Key})
	return assertionRef + string(raw)
}

// ParseAssertionRef is the inverse of Ref: the kind, target and key of a binding's constraint ref, ok
// false for a ref that is not an assertion's.
func ParseAssertionRef(ref string) (Assertion, bool) {
	rest, found := strings.CutPrefix(ref, assertionRef)
	if !found {
		return Assertion{}, false
	}
	var parts []string
	if json.Unmarshal([]byte(rest), &parts) != nil || len(parts) != 3 {
		return Assertion{}, false
	}
	return Assertion{Kind: parts[0], Target: parts[1], Key: parts[2]}, true
}

// ValueDigest is the constraint digest a host binds: the whole proposition, value included.
func (a Assertion) ValueDigest() string {
	return Digest("lc-claim-value:v1", a.Kind, a.Target, a.Key, "equals", a.Value)
}

// Subject is the rendering of what the assertion is about: kind(target[, key]).
func (a Assertion) Subject() string {
	if a.Key == "" {
		return a.Kind + "(" + a.Target + ")"
	}
	return a.Kind + "(" + a.Target + ", " + a.Key + ")"
}

// Render is the statement Core stores and serves for an assertion candidate: Subject() = Value. A
// reader of a qualified candidate gets this text, never model prose, as its claim.
func (a Assertion) Render() string { return a.Subject() + " = " + a.Value }

func validPort(v string) bool {
	if !portNumber.MatchString(v) {
		return false
	}
	n := 0
	for _, c := range v {
		n = n*10 + int(c-'0')
	}
	return n <= 65535
}

// plainText: 1-max bytes of valid UTF-8 without control characters.
func plainText(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Parsed is one entry of the maintainer's answer: the proposal when it decoded, the raw text and the
// reason when it did not.
type Parsed struct {
	Proposal Proposal
	Raw      string
	Err      error
}

// ErrOutput reports an answer that is not the required object at all; nothing of it is a candidate.
var ErrOutput = errors.New("maintainer output is not a candidates object")

// ParseOutput decodes the maintainer's final answer: one JSON object {"candidates":[...]} (a fenced
// ```json block around it is accepted), at most MaxOutputBytes, at most MaxCandidates entries. Each
// entry decodes strictly on its own: an unknown field, such as a status the model gives itself, rejects
// that entry. A well-formed entry is only well-formed: Validate decides whether it may be kept.
func ParseOutput(final string) ([]Parsed, error) {
	if len(final) > MaxOutputBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrOutput, len(final), MaxOutputBytes)
	}
	text := strings.TrimSpace(final)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	var top struct {
		Candidates []rawJSON `json:"candidates"`
	}
	if err := decodeStrict(text, &top); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOutput, err)
	}
	if top.Candidates == nil {
		return nil, fmt.Errorf("%w: candidates is missing", ErrOutput)
	}
	if len(top.Candidates) > MaxCandidates {
		return nil, fmt.Errorf("%w: %d candidates exceeds %d", ErrOutput, len(top.Candidates), MaxCandidates)
	}
	out := make([]Parsed, 0, len(top.Candidates))
	for _, raw := range top.Candidates {
		p := Parsed{Raw: string(raw)}
		p.Err = decodeStrict(string(raw), &p.Proposal)
		out = append(out, p)
	}
	return out, nil
}

// Known is what Core knows when it validates a proposal: the refs of the batch it handed out.
type Known struct {
	Sources map[string]SourceRef
}

// Validate keeps a proposal only when every field is within bounds and every reference is one Core
// issued: each source ref is from this batch, each cited path is a clean workspace-relative path
// (whether it exists is the runtime's to check, with FileBlobs), and an assertion, which only a fact
// may carry, has the shape of its kind. A free-text claim is required without an assertion; with one,
// Core renders the claim itself. It returns the first reason to reject, "" to keep.
func Validate(p Proposal, known Known) string {
	switch {
	case !claimTypes[p.ClaimType]:
		return fmt.Sprintf("claim type %q is not one of procedure, fact, failure, decision", p.ClaimType)
	case !boundedText(p.Subject, MaxSubjectBytes):
		return fmt.Sprintf("subject must be 1-%d bytes of text", MaxSubjectBytes)
	case p.Assertion == nil && !boundedText(p.Claim, MaxClaimBytes):
		return fmt.Sprintf("claim must be 1-%d bytes of text", MaxClaimBytes)
	case len(p.Claim) > MaxClaimBytes || !utf8.ValidString(p.Claim):
		return fmt.Sprintf("claim must be at most %d bytes of text", MaxClaimBytes)
	case len(p.Rationale) > MaxRationaleBytes || !utf8.ValidString(p.Rationale):
		return fmt.Sprintf("rationale must be at most %d bytes of text", MaxRationaleBytes)
	case len(p.SourceRefs) == 0:
		return "a candidate must cite at least one source of this batch"
	case len(p.SourceRefs) > MaxSourceRefs:
		return fmt.Sprintf("a candidate cites at most %d sources", MaxSourceRefs)
	case len(p.CitedPaths) > MaxCitedPaths:
		return fmt.Sprintf("a candidate cites at most %d paths", MaxCitedPaths)
	case p.Assertion != nil && p.ClaimType != ClaimFact:
		return "only a fact carries an assertion"
	}
	if p.Assertion != nil {
		if err := p.Assertion.Validate(); err != nil {
			return err.Error()
		}
	}
	seen := map[string]bool{}
	for _, ref := range p.SourceRefs {
		r, ok := known.Sources[ref]
		if !ok {
			return fmt.Sprintf("source %q is not a source of this batch", clip(ref))
		}
		if r.Sensitivity != SensitivitySession {
			return fmt.Sprintf("source %q may not leave its session", clip(ref))
		}
		if seen[ref] {
			return fmt.Sprintf("source %q is cited twice", clip(ref))
		}
		seen[ref] = true
	}
	for _, p := range p.CitedPaths {
		if !CleanPath(p) {
			return fmt.Sprintf("cited path %q is not a clean workspace-relative path", clip(p))
		}
	}
	return ""
}

// CleanPath: a slash-separated, workspace-relative path without . or .. elements.
func CleanPath(p string) bool {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00") ||
		strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	return true
}

// ClaimDigest identifies a proposition independent of who proposed it (the claim identity): an
// assertion is its value digest, a free-text claim its normalized subject and text.
func ClaimDigest(p Proposal) string {
	if p.Assertion != nil {
		return p.Assertion.ValueDigest()
	}
	return Digest(strings.ToLower(strings.TrimSpace(p.Subject)), strings.TrimSpace(p.Claim))
}

// SupportDigest is the support identity of one candidate row: the claim, the exact source versions
// it was derived from, the source scope, the policy and the binding that qualified it. A proposal
// restating a claim is a duplicate only with the same support identity; a new independent source, a
// new version of a source, another policy or another binding is new support and is recorded.
func SupportDigest(claimDigest, sourceDigest, scope, policy, bindingDigest string) string {
	return Digest("support:v1", claimDigest, sourceDigest, scope, policy, bindingDigest)
}

// SubjectKey is what two free-text candidates must share to conflict.
func SubjectKey(subject string) string {
	return strings.Join(strings.Fields(strings.ToLower(subject)), " ")
}

// CandidateSubjectKey is what two candidates must share to conflict: the assertion's ref (kind, target
// and key, whatever the maintainer called it), or the normalized subject of a free-text claim.
func CandidateSubjectKey(subject string, a *Assertion) string {
	if a != nil {
		return a.Ref()
	}
	return SubjectKey(subject)
}

// Freshness is the validity class of an observation: what besides its check fingerprint (workspace
// state, check definition, its inputs, approval epoch) it may depend on, and so who may reuse it.
type Freshness string

const (
	// FreshnessVersion: the check is evaluated by Core in-process from workspace files the fingerprint
	// covers (a schema check). An observation at the reader's current fingerprint, definition and
	// approval is fresh for any run.
	FreshnessVersion Freshness = "version"
	// FreshnessRun: the check runs a process, which may read the environment, the network, the clock or
	// anything else the fingerprint does not cover (unknown dependencies). Its observation is fresh only
	// for the run that made it, while nothing mutated since, at the same fingerprint, definition and
	// approval, and for at most RunReuseWindow. Every other run must establish its own.
	FreshnessRun Freshness = "run"
)

// Applicability of an adopted candidate to a reader's scope now. It is not a truth value.
type Applicability string

const (
	// Current: the check a host-approved binding assigns to the candidate's assertion passed freshly for
	// this reader (see Freshness), every cited path still has its recorded blob and every source is at
	// the version the candidate was derived from.
	Current Applicability = "current"
	// Unknown: no fresh result of the bound check for this reader (another run made it, the scope
	// changed since, or the revalidation did not run or complete).
	Unknown Applicability = "unknown"
	// Stale: a cited path or a source changed, or the bound check failed freshly for this reader.
	Stale Applicability = "stale"
	// SourceMissing: a source session or a cited path is gone.
	SourceMissing Applicability = "source_missing"
	// Withdrawn: a source or the maintenance session is fenced (revocation or privacy).
	Withdrawn Applicability = "withdrawn"
	// Conflict: another candidate with at least this one's host evidence (EvidenceLevel) states
	// something else about the same subject. Among equals Core does not pick.
	Conflict Applicability = "conflict"
	// CandidateOnly: not supported by a host-approved binding (never, or no longer); readable, not a
	// fact.
	CandidateOnly Applicability = "candidate_only"
)

// Facts are what the runtime observed about one candidate for one reader.
type Facts struct {
	Status          string
	SourceWithdrawn bool
	SourceMissing   bool
	// SourceChanged: a source session is no longer at the version (digest) the candidate cites.
	SourceChanged bool
	CitedMissing  bool
	CitedChanged  bool
	Conflict      bool
	// Unsupported: no active host-approved binding of the candidate's assertion to a check with the
	// definition it was bound under (revoked, superseded, or the check definition changed).
	Unsupported bool
	// AtCurrent is the outcome of the newest observation of the bound check that is fresh for this
	// reader: "passed", "failed", or "" when there is none.
	AtCurrent string
}

// EvidenceLevel orders what the host knows of a candidate that still applies to a reader: 3 when its
// bound check passed freshly for this reader (Current), 2 when a host-approved binding supports it and
// no result is fresh (Unknown), 1 when nothing the host approved supports it (CandidateOnly), 0 when it
// no longer applies (stale, missing, withdrawn). A candidate is contested only by a claim of at least
// its own level: model output without host support never changes what a supported fact is to a reader,
// and a fresh pass is not contested by a result that is not fresh.
func EvidenceLevel(a Applicability) int {
	switch a {
	case Current:
		return 3
	case Unknown:
		return 2
	case CandidateOnly:
		return 1
	}
	return 0
}

// Decide is the applicability of a non-rejected candidate. The order is fixed: withdrawal, then a
// missing source, then a conflict, then a changed citation or source, then support.
func Decide(f Facts) (Applicability, string) {
	switch {
	case f.SourceWithdrawn:
		return Withdrawn, "a source of the candidate is fenced"
	case f.SourceMissing:
		return SourceMissing, "a source session of the candidate no longer exists"
	case f.CitedMissing:
		return SourceMissing, "a cited path no longer exists in this scope"
	case f.Conflict:
		return Conflict, "another live candidate states something else about the same subject"
	case f.CitedChanged:
		return Stale, "a cited path changed since the candidate was adopted"
	case f.SourceChanged:
		return Stale, "a source changed since the candidate was derived from it; its new version is maintained again"
	case f.Status != StatusQualified:
		return CandidateOnly, "no host-approved binding supported the candidate"
	case f.Unsupported:
		return CandidateOnly, "no host-approved binding supports the candidate's assertion now"
	case f.AtCurrent == "passed":
		return Current, "its bound check passed freshly for this reader"
	case f.AtCurrent == "failed":
		return Stale, "its bound check failed freshly for this reader"
	}
	return Unknown, "no fresh result of its bound check for this reader"
}

type rawJSON = json.RawMessage

// decodeStrict decodes exactly one JSON value into into, refusing unknown fields and trailing data.
func decodeStrict(text string, into any) error {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

func boundedText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= max && utf8.ValidString(s)
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
