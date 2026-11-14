package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
)

// MaxGraphRecordBytes bounds a stored revision. Two kinds of revision must still fit a record that
// content has filled: the host's lifecycle decision (archive, restore, revoke, tier, consolidation),
// which a full record would refuse, and the settlement of an Experience use that was already applied,
// whose checked outcome would be lost. Every other revision leaves their room (Node.Room); they are
// measured against the bound alone.
const MaxGraphRecordBytes = 65536

// lifecycleRoom is what lifecycle revisions can add to a record, however many follow one another.
// They replace the reason (512 bytes, at most six each in JSON: 3092 with its name), the memory
// policy (911, the 128 bytes of a canonical identity at six each), the status (3), two times (20)
// and the version (18): 4044 bytes.
const lifecycleRoom = 4096

// Node is a native Go cognition revision, not a Node-runtime database row. Status is lifecycle,
// Evidence is an immutable observation and Applicability is computed for a reader at use time.
type Node struct {
	ID              string        `json:"id"`
	Version         int64         `json:"version"`
	SessionID       string        `json:"sessionId"`
	Kind            string        `json:"kind"`
	Title           string        `json:"title"`
	Body            string        `json:"body,omitempty"`
	Tags            []string      `json:"tags,omitempty"`
	Status          string        `json:"status"`
	Scope           Scope         `json:"scope"`
	Times           Times         `json:"times"`
	Assertion       *Assertion    `json:"assertion,omitempty"`
	Sources         []SourceRef   `json:"sources,omitempty"`
	Evidence        []Evidence    `json:"evidence,omitempty"`
	Experience      *Experience   `json:"experience,omitempty"`
	Epistemic       string        `json:"epistemic,omitempty"`
	ModelConfidence *float64      `json:"modelConfidence,omitempty"`
	Dependencies    []NodeRef     `json:"dependencies,omitempty"`
	Memory          *MemoryPolicy `json:"memory,omitempty"`
	RevisionReason  string        `json:"revisionReason,omitempty"`
}

func (n Node) Validate() error {
	if err := n.ValidateLifecycle(); err != nil {
		return err
	}
	if !plainText(n.ID, 128) || !plainText(n.SessionID, 128) || n.Version < 1 {
		return errors.New("invalid node identity/version")
	}
	if !slices.Contains([]string{"entity", "evidence", "belief", "abstraction", "task", "negative", "experience"}, n.Kind) {
		return errors.New("invalid node kind")
	}
	if !plainText(n.Title, 256) || len(n.Body) > 8192 || len(n.Tags) > 32 {
		return errors.New("invalid node content bounds")
	}
	for _, tag := range n.Tags {
		if !plainText(tag, 80) {
			return errors.New("invalid tag")
		}
	}
	if !slices.Contains([]string{"active", "dormant", "stale", "archived", "invalid"}, n.Status) {
		return errors.New("invalid node status")
	}
	if err := n.Scope.Validate(); err != nil {
		return err
	}
	if err := n.Times.Validate(); err != nil {
		return err
	}
	scopeOwner := n.SessionID
	if n.Kind == "experience" {
		if n.Experience == nil || n.Assertion != nil || len(n.Evidence) != 0 {
			return errors.New("experience requires its typed payload and exact observations")
		}
		if err := n.Experience.Validate(); err != nil {
			return err
		}
		scopeOwner = n.Experience.Origin.Source.SessionID
		if !slices.Contains(n.Sources, n.Experience.Origin.Source) {
			return errors.New("experience must retain its original source")
		}
		if err := n.Experience.ValidateScope(n.Scope); err != nil {
			return err
		}
	} else if n.Experience != nil {
		return errors.New("only an experience node can hold an experience payload")
	}
	if n.Scope.SessionID != "" && n.Scope.SessionID != scopeOwner {
		return errors.New("scope session differs from source owner")
	}
	if n.Assertion != nil {
		if err := n.Assertion.Validate(); err != nil {
			return err
		}
	}
	if len(n.Sources) > MaxSourceRefs || len(n.Evidence) > 32 {
		return errors.New("too many source/evidence refs")
	}
	for _, src := range n.Sources {
		if err := src.Validate(); err != nil {
			return err
		}
		if src.Sensitivity == SensitivityPrivate && n.Scope.SessionID != src.SessionID {
			return errors.New("private source cannot leave its session")
		}
	}
	for _, e := range n.Evidence {
		if err := e.Validate(); err != nil {
			return err
		}
		if !e.Scope.Contains(n.Scope) || !slices.Contains(n.Sources, e.Source) {
			return errors.New("evidence scope/source does not cover node")
		}
		if n.Assertion == nil || (!slices.Contains(e.SupportingClaims, n.Assertion.ValueDigest()) && !slices.Contains(e.ContradictingClaims, n.Assertion.ValueDigest())) {
			return errors.New("evidence does not bind node assertion")
		}
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if len(raw) > MaxGraphRecordBytes {
		return errors.New("node exceeds storage bound")
	}
	return nil
}

// Room is what a revision of n leaves of MaxGraphRecordBytes besides the room of a lifecycle revision
// and of the feedback of every open use of an Experience. A content revision with a negative room is
// refused.
func (n Node) Room() int {
	room := MaxGraphRecordBytes - lifecycleRoom - encodedLen(n)
	if n.Experience != nil {
		for _, u := range n.Experience.Uses {
			if u.Feedback == nil {
				room -= u.feedbackBytes()
			}
		}
	}
	return room
}

// AdmitsExperienceUse reports whether an Experience record can take one more use: it holds fewer
// than MaxExperienceUses and its room covers the use and the feedback that use will record. The
// origin's validation observed the same conditions and the same outcome for the same scope, so its
// observations stand in for the consumer's, each with its slack. The answer is read off the record
// alone: recall and adoption give the same one. A record that is short of bytes, not of uses, turns
// to yes again when its open uses settle below the room they kept.
func (n Node) AdmitsExperienceUse() bool {
	x := n.Experience
	if x == nil || len(x.Uses) >= MaxExperienceUses {
		return false
	}
	next := ExperienceUse{ExpectedOutcome: x.Definition.ExpectedOutcome, ExpectedResult: x.Definition.ExpectedResult, Conditions: x.Validation.Conditions}
	return n.Room() >= encodedLen(next)+experienceUseSlack+(len(next.Conditions)+1)*experienceObservationSlack+next.feedbackBytes()
}

// Edge is an immutable diagnostic relation; it confers no verification or access authority.
type Edge struct {
	ID        string  `json:"id"`
	SessionID string  `json:"sessionId"`
	From      string  `json:"from"`
	To        string  `json:"to"`
	Type      string  `json:"type"`
	Weight    float64 `json:"weight"`
}

func (e Edge) Validate() error {
	for _, id := range []string{e.ID, e.SessionID, e.From, e.To} {
		if !plainText(id, 128) {
			return errors.New("invalid edge identity")
		}
	}
	if !slices.Contains([]string{"depends_on", "derived_from", "relates_to", "supersedes", "canonicalizes", "abstracts", "invalidates", "contradicts", "verifies", "causes", "calls", "affects"}, e.Type) {
		return fmt.Errorf("invalid relation %q", e.Type)
	}
	if math.IsNaN(e.Weight) || math.IsInf(e.Weight, 0) || e.Weight < 0 || e.Weight > 1 {
		return errors.New("invalid edge weight")
	}
	return nil
}

// DecodeNode rejects unknown fields instead of silently losing their privacy or authority meaning.
func DecodeNode(raw string) (Node, error) {
	var n Node
	if len(raw) > MaxGraphRecordBytes {
		return n, errors.New("oversized node")
	}
	if err := decodeStrict(raw, &n); err != nil {
		return n, err
	}
	return n, n.Validate()
}

func DecodeEdge(raw string) (Edge, error) {
	var e Edge
	if err := decodeStrict(raw, &e); err != nil {
		return e, err
	}
	return e, e.Validate()
}
