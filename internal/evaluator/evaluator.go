// Package evaluator decides configured checks (design §4.7, invariant I8). A check is a test or build
// command run by the runtime in the session's workspace, or a JSON Schema validated in process. Its
// result is stored with the fingerprint of the workspace it was decided at, and only a passed result
// produced by the runtime or accepted by a person, at the current fingerprint, satisfies the check. A
// model never produces a result.
package evaluator

import (
	"crypto/sha256"
	"encoding/hex"
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

// Result is one decided or accepted check.
type Result struct {
	Seq         int64     `json:"seq,omitempty"`
	WorkUnitID  string    `json:"workUnitId,omitempty"`
	Check       string    `json:"check"`
	Kind        string    `json:"kind"`
	Status      string    `json:"status"`
	Provenance  string    `json:"provenance"`
	Fingerprint string    `json:"fingerprint"`
	Artifacts   []string  `json:"artifacts"`
	At          time.Time `json:"at"`
}

// Satisfies is the I8 rule for a stored result: only a passed runtime or human result for the current,
// non-empty fingerprint counts. Any other provenance, an llm:* one included, never satisfies.
func Satisfies(r Result, current string) bool {
	if current == "" || r.Status != StatusPassed || r.Fingerprint != current {
		return false
	}
	return r.Provenance == ProvenanceRuntime || IsHuman(r.Provenance)
}

// IsHuman reports a person's provenance, "human:<id>" with a non-empty id.
func IsHuman(provenance string) bool {
	return strings.HasPrefix(provenance, ProvenanceHumanPrefix) && len(provenance) > len(ProvenanceHumanPrefix)
}

// CheckVerdict decides one check. stored is the newest result for the check with provenance runtime or
// human:* at fingerprint current (nil when none; always nil outside git). A satisfying stored result is
// reused, so a human acceptance holds until the fingerprint changes. Otherwise run executes the check
// now and its fresh runtime result decides, with or without a fingerprint. An error from run (the
// caller's cancellation) decides nothing.
func CheckVerdict(stored *Result, current string, run func() (Result, error)) (Result, bool, error) {
	if stored != nil && Satisfies(*stored, current) {
		return *stored, true, nil
	}
	fresh, err := run()
	if err != nil {
		return Result{}, false, err
	}
	return fresh, fresh.Status == StatusPassed && fresh.Provenance == ProvenanceRuntime, nil
}
