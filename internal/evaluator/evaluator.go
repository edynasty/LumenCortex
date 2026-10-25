// Package evaluator decides configured checks (design §4.7, invariant I8). A check is a test or build
// command run by the runtime in the session's workspace, or a JSON Schema validated in process. Its
// result is stored with the fingerprint of the workspace it was decided at and the approval it ran
// under (ADR-0017), and only a passed result produced by the runtime or accepted by a person, at the
// current fingerprint, for the approved definition and approval epoch, satisfies the check. A model
// never produces a result, and a check whose definition the host has not approved is never run here.
package evaluator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
)

// Result statuses, kinds and provenances.
const (
	StatusPassed = "passed"
	StatusFailed = "failed"
	StatusError  = "error"
	// StatusSpecUnapproved is the verdict of a check whose definition the host has not approved
	// (ADR-0017). Nothing ran and nothing is stored for it.
	StatusSpecUnapproved = "spec_unapproved"

	KindTest   = "test"
	KindBuild  = "build"
	KindSchema = "schema"

	ProvenanceRuntime = "runtime"
	// ProvenanceHumanPrefix starts the provenance of a person's acceptance, "human:<id>".
	ProvenanceHumanPrefix = "human:"

	ArtifactLog  = "log"
	ArtifactDiff = "diff"
)

// Spec is one configured check. Work units and the completion gate refer to checks by ID; neither can
// define a command.
type Spec struct {
	ID       string        `json:"id"`
	Kind     string        `json:"kind"`
	Command  string        `json:"command,omitempty"`
	Schema   string        `json:"schema,omitempty"`
	Files    []string      `json:"files,omitempty"`
	Timeout  time.Duration `json:"timeout"`
	Required bool          `json:"required"`
	// Source is the configuration file the check came from: global, or workspace (present only when
	// the workspace is trusted; config drops an untrusted workspace's checks).
	Source string `json:"source"`
	// ApprovalEpoch is the host approval this definition runs under (ADR-0017): the epoch of the
	// approved check set it belongs to, 0 when the set or this definition is not approved. It is set
	// by the runtime, never read from configuration, and is not part of the definition's digest.
	ApprovalEpoch int64 `json:"approvalEpoch,omitempty"`
}

// specCanon is the canonical form a definition's digest covers: every field that changes what the
// check runs or whether it is required, in a fixed order.
type specCanon struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Command   string   `json:"command"`
	Schema    string   `json:"schema"`
	Files     []string `json:"files"`
	TimeoutMS int64    `json:"timeoutMs"`
	Required  bool     `json:"required"`
	Source    string   `json:"source"`
}

func canon(s Spec) specCanon {
	files := s.Files
	if files == nil {
		files = []string{}
	}
	return specCanon{ID: s.ID, Kind: s.Kind, Command: s.Command, Schema: s.Schema, Files: files,
		TimeoutMS: s.Timeout.Milliseconds(), Required: s.Required, Source: s.Source}
}

func digest(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Strings, string slices, integers and booleans always marshal.
		panic(err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SpecDigest is the digest of one check definition (ADR-0017): "sha256:<hex>" of the canonical JSON
// of its id, kind, command, schema, files, timeout, required flag and source.
func SpecDigest(s Spec) string { return digest(canon(s)) }

// SetDigest is the digest of a whole effective check set: the canonical definitions sorted by id. The
// empty set has a digest too, so emptying a set is a change like any other.
func SetDigest(specs []Spec) string {
	out := make([]specCanon, 0, len(specs))
	for _, s := range specs {
		out = append(out, canon(s))
	}
	slices.SortFunc(out, func(a, b specCanon) int { return strings.Compare(a.ID, b.ID) })
	return digest(out)
}

// Specs converts the effective configuration's checks, sorted by ID.
func Specs(checks map[string]config.Check) []Spec {
	out := make([]Spec, 0, len(checks))
	for id, c := range checks {
		timeout := c.TimeoutSeconds
		if timeout <= 0 {
			timeout = config.DefaultCheckTimeout
		}
		out = append(out, Spec{
			ID: id, Kind: c.Kind, Command: c.Command, Schema: c.Schema, Files: slices.Clone(c.Files),
			Timeout: time.Duration(timeout) * time.Second, Required: c.IsRequired(), Source: c.Source,
		})
	}
	slices.SortFunc(out, func(a, b Spec) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Env is the workspace state a result is valid for. Head is "" outside git, and then there is no
// fingerprint. DirtyHash is "" for a clean tree. WorktreeID is "local" or the session's worktree path.
type Env struct {
	Head       string `json:"head"`
	DirtyHash  string `json:"dirtyHash"`
	WorktreeID string `json:"worktreeId"`
}

func (e Env) String() string { return e.Head + "+" + e.DirtyHash + "@" + e.WorktreeID }

// Fingerprint is what a result of spec is valid for: the environment plus the check's definition, so
// editing a check invalidates its old results. Outside git it is "", and no stored result is reused.
func Fingerprint(env Env, spec Spec) string {
	if env.Head == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(append([]string{spec.Kind, spec.Command, spec.Schema}, spec.Files...), "\x00")))
	return env.String() + "#" + hex.EncodeToString(sum[:8])
}

// Result is one decided or accepted check. SpecDigest and ApprovalEpoch are the approved definition it
// was decided or accepted under; a result stored before approvals existed has neither.
type Result struct {
	Seq           int64     `json:"seq,omitempty"`
	WorkUnitID    string    `json:"workUnitId,omitempty"`
	Check         string    `json:"check"`
	Kind          string    `json:"kind"`
	Status        string    `json:"status"`
	Provenance    string    `json:"provenance"`
	Fingerprint   string    `json:"fingerprint"`
	SpecDigest    string    `json:"specDigest,omitempty"`
	ApprovalEpoch int64     `json:"approvalEpoch,omitempty"`
	Artifacts     []string  `json:"artifacts"`
	At            time.Time `json:"at"`
}

// Approved reports whether spec runs under a host approval.
func Approved(spec Spec) bool { return spec.ApprovalEpoch > 0 }

// Satisfies is the I8 rule for a stored result of spec (ADR-0017): only a passed runtime or human
// result for the current, non-empty fingerprint, decided under spec's current approval (the same
// definition digest and approval epoch), counts. An unapproved spec is never satisfied, and any other
// provenance, an llm:* one included, never satisfies.
func Satisfies(r Result, current string, spec Spec) bool {
	if current == "" || r.Status != StatusPassed || r.Fingerprint != current || !Approved(spec) {
		return false
	}
	if r.ApprovalEpoch != spec.ApprovalEpoch || r.SpecDigest != SpecDigest(spec) {
		return false
	}
	return r.Provenance == ProvenanceRuntime || IsHuman(r.Provenance)
}

// IsHuman reports a person's provenance, "human:<id>" with a non-empty id.
func IsHuman(provenance string) bool {
	return strings.HasPrefix(provenance, ProvenanceHumanPrefix) && len(provenance) > len(ProvenanceHumanPrefix)
}

// CheckVerdict decides one check. stored is the newest result for the check with provenance runtime or
// human:* at fingerprint current under spec's approval (nil when none; always nil outside git). A
// satisfying stored result is reused, so a human acceptance holds until the fingerprint or the approval
// changes. Otherwise run executes the check now and its fresh runtime result decides, with or without a
// fingerprint. An unapproved spec is not run: its verdict is spec_unapproved. An error from run (the
// caller's cancellation) decides nothing.
func CheckVerdict(stored *Result, current string, spec Spec, run func() (Result, error)) (Result, bool, error) {
	if !Approved(spec) {
		return Result{Check: spec.ID, Kind: spec.Kind, Status: StatusSpecUnapproved, Provenance: ProvenanceRuntime,
			Fingerprint: current, Artifacts: []string{}}, false, nil
	}
	if stored != nil && Satisfies(*stored, current, spec) {
		return *stored, true, nil
	}
	fresh, err := run()
	if err != nil {
		return Result{}, false, err
	}
	return fresh, fresh.Status == StatusPassed && fresh.Provenance == ProvenanceRuntime, nil
}
