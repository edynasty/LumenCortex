package evaluator

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/streambuf"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Artifact and output bounds (design §4.7): an artifact keeps at most 1 MiB, head and tail; a blocked
// completion quotes the last 2 KiB of a check's output.
const (
	MaxArtifactBytes = 1 << 20
	TailBytes        = 2 << 10
)

// ErrNoFingerprint reports an acceptance outside git: there is no fingerprint to accept a result for.
var ErrNoFingerprint = errors.New("workspace has no fingerprint (not a git repository with a commit)")

// ErrUnknownCheck reports a check id the configuration does not define (or dropped as untrusted).
var ErrUnknownCheck = errors.New("check is not configured")

// ErrSpecUnapproved reports a check whose definition the host has not approved in its current form
// (ADR-0017): it is not run by the completion gate or a work unit, cannot be accepted, and satisfies
// nothing until the user approves the current check set.
var ErrSpecUnapproved = errors.New("check definition is not approved; approve the current check set (lcx-go checks status)")

// ErrStore wraps a failure to read or write results and artifacts.
var ErrStore = errors.New("evaluator store")

// storeErr marks err as a store failure unless the caller's context ended it.
func storeErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %w", ErrStore, err)
}

// Store is the part of the session store the evaluator writes and reads.
type Store interface {
	PutArtifact(ctx context.Context, sessionID string, a session.Artifact) error
	AppendEvalResult(ctx context.Context, sessionID string, row session.EvalRow) (int64, error)
	DecidingEvalResult(ctx context.Context, sessionID, checkID, fingerprint, specDigest string, approvalEpoch int64) (*session.EvalRow, error)
	CloseEpisode(ctx context.Context, sessionID, episodeID, status string, post map[int64]string) error
}

// Evaluator decides checks for one session in its workspace: the session's worktree when it has one.
// The runtime builds one per run; it remembers the episodes it closed itself, the only runtime results
// the run may reuse (Satisfies), and forgets them all once a side-effect tool completes (Mutated). An
// Evaluator built for a host read remembers nothing, so only a person's acceptance satisfies there.
type Evaluator struct {
	Store     Store
	SessionID string
	Workspace string
	// WorktreeID is "local" for the main checkout, else the worktree path.
	WorktreeID string

	mu sync.Mutex
	// generation counts the side-effect tools completed in the run; closed maps the seq of each runtime
	// result in an episode this Evaluator closed to the generation it was closed at.
	generation uint64
	closed     map[int64]uint64
}

// Mutated records that a side-effect tool completed in the run: no episode closed before it is reused.
func (e *Evaluator) Mutated() {
	e.mu.Lock()
	e.generation++
	e.mu.Unlock()
}

// inRun reports whether seq is a result of an episode this Evaluator closed, with no side-effect tool
// completed since.
func (e *Evaluator) inRun(seq int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	gen, ok := e.closed[seq]
	return ok && gen == e.generation
}

func (e *Evaluator) register(seqs []int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed == nil {
		e.closed = make(map[int64]uint64)
	}
	for _, seq := range seqs {
		e.closed[seq] = e.generation
	}
}

// Verdict is the decision of one check: the result that decided it, whether it passed, whether a stored
// result was reused instead of running, the last 2 KiB of the output of a fresh run, and the status of
// the episode the check was decided in ("" when nothing ran or was looked up: an unapproved or
// unconfigured check).
type Verdict struct {
	Result  Result `json:"result"`
	Passed  bool   `json:"passed"`
	Reused  bool   `json:"reused"`
	Tail    string `json:"tail,omitempty"`
	Episode string `json:"episode,omitempty"`
}

// ConfigUnchanged reports whether the check definitions and their approval are still those the
// episode's specs were taken from. The runtime reads them from the Engine workspace (configScope); the
// evaluator only knows the session's workspace (executionScope).
type ConfigUnchanged func(ctx context.Context) (bool, error)

// Decide decides specs in order as one check episode, as the completion gate and a work unit's
// completion do (ADR-0017). Before the first check it takes a snapshot of the workspace; each fresh
// result is stored open in the episode with workUnitID; after the last check it takes the workspace
// snapshot again outside any transaction, asks unchanged (nil: nothing to ask), and closes the episode
// in one short transaction: closed when both are as before, spec_unapproved when the definitions or
// their approval changed, scope_changed when the workspace did. A verdict passes only in a closed
// episode; otherwise its result is kept as history and decides nothing, and the episode is not run
// again here. A stored result is reused only as Satisfies allows. A cancelled ctx stops the decision,
// stores nothing for the check it interrupted, and closes the episode as interrupted.
func (e *Evaluator) Decide(ctx context.Context, specs []Spec, workUnitID string, unchanged ConfigUnchanged) ([]Verdict, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	pre, err := e.snapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	episode, err := newEpisodeID()
	if err != nil {
		return nil, err
	}
	out := make([]Verdict, 0, len(specs))
	fresh := map[int64]int{} // seq of a fresh result -> its index in out
	for _, spec := range specs {
		// An approved definition whose input differs here runs content nobody approved: it is decided as
		// unapproved and not run (CHECKS-002).
		changed := ""
		if Approved(spec) {
			if changed = e.changedInput(spec); changed != "" {
				spec.ApprovalEpoch = 0
			}
		}
		current := Fingerprint(pre.env, spec)
		var stored *Result
		if current != "" && Approved(spec) {
			row, err := e.Store.DecidingEvalResult(ctx, e.SessionID, spec.ID, current, SpecDigest(spec), spec.ApprovalEpoch)
			if err != nil {
				return nil, e.interrupt(ctx, episode, fresh, storeErr(ctx, err))
			}
			if row != nil {
				r := fromRow(*row)
				stored = &r
			}
		}
		var tail string
		ran := false
		result, passed, err := CheckVerdict(stored, current, spec, stored != nil && e.inRun(stored.Seq), func() (Result, error) {
			ran = true
			r, t, err := e.run(ctx, spec, current, workUnitID, pre, episode)
			tail = t
			return r, err
		})
		if err != nil {
			return nil, e.interrupt(ctx, episode, fresh, err)
		}
		if result.Status == StatusSpecUnapproved {
			result.WorkUnitID, tail = workUnitID, ErrSpecUnapproved.Error()
			if changed != "" {
				tail = inputTail(changed)
			}
		}
		if ran {
			fresh[result.Seq] = len(out)
		}
		out = append(out, Verdict{Result: result, Passed: passed, Reused: !ran && passed, Tail: tail})
	}
	post, err := e.snapshot(ctx, false)
	if err != nil {
		return nil, e.interrupt(ctx, episode, fresh, err)
	}
	status := EpisodeClosed
	if unchanged != nil {
		same, err := unchanged(ctx)
		if err != nil {
			return nil, e.interrupt(ctx, episode, fresh, err)
		}
		if !same {
			status = EpisodeSpecUnapproved
		}
	}
	// An input that differs after the checks ran (or already differed) leaves the episode
	// spec_unapproved, as a changed set does: nothing decided in it counts (CHECKS-002).
	if status == EpisodeClosed {
		for _, spec := range specs {
			if Approved(spec) && e.changedInput(spec) != "" {
				status = EpisodeSpecUnapproved
				break
			}
		}
	}
	if status == EpisodeClosed && post.env != pre.env {
		status = EpisodeScopeChanged
	}
	postFingerprints := make(map[int64]string, len(fresh))
	for seq, i := range fresh {
		postFingerprints[seq] = postFingerprint(post.env, specs[i])
	}
	if err := e.Store.CloseEpisode(ctx, e.SessionID, episode, status, postFingerprints); err != nil {
		return nil, storeErr(ctx, err)
	}
	if status == EpisodeClosed {
		seqs := make([]int64, 0, len(fresh))
		for seq := range fresh {
			seqs = append(seqs, seq)
		}
		e.register(seqs)
	}
	for seq, i := range fresh {
		out[i].Result.EpisodeStatus, out[i].Result.PostFingerprint = status, postFingerprints[seq]
	}
	for i := range out {
		if out[i].Result.Status == StatusSpecUnapproved {
			continue
		}
		out[i].Episode = status
		if status != EpisodeClosed && out[i].Passed {
			out[i].Passed = false
			out[i].Tail = episodeTail(status)
		}
	}
	return out, nil
}

// postFingerprint is spec's fingerprint at the post snapshot; outside git there is none, and the
// column stays empty.
func postFingerprint(env Env, spec Spec) string { return Fingerprint(env, spec) }

// episodeTail explains a verdict that ran or was reused but counts for nothing.
func episodeTail(status string) string {
	switch status {
	case EpisodeScopeChanged:
		return "the workspace changed while the checks ran (episode scope_changed); the result is kept as history and decides nothing"
	case EpisodeSpecUnapproved:
		return "the check definitions or their approval changed while the checks ran (episode spec_unapproved): " + ErrSpecUnapproved.Error()
	}
	return "the check episode did not close"
}

// interrupt closes the episode's open rows as interrupted, best effort and past the caller's
// cancellation, and returns cause. A row it cannot close stays open, which satisfies nothing either.
func (e *Evaluator) interrupt(ctx context.Context, episode string, fresh map[int64]int, cause error) error {
	if len(fresh) == 0 {
		return cause
	}
	post := make(map[int64]string, len(fresh))
	for seq := range fresh {
		post[seq] = ""
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = e.Store.CloseEpisode(closeCtx, e.SessionID, episode, EpisodeInterrupted, post)
	return cause
}

// newEpisodeID is a random episode id, "ep_" and 24 hex digits.
func newEpisodeID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ep_" + hex.EncodeToString(b[:]), nil
}

// Satisfied reports, without running anything, whether spec has a satisfying stored result at the
// workspace's current fingerprint, and that result.
func (e *Evaluator) Satisfied(ctx context.Context, spec Spec) (bool, *Result, error) {
	snap, err := e.snapshot(ctx, false)
	if err != nil {
		return false, nil, err
	}
	return e.satisfied(ctx, spec, snap.env)
}

// SatisfiedAll is Satisfied for several specs against one snapshot.
func (e *Evaluator) SatisfiedAll(ctx context.Context, specs []Spec) ([]bool, []*Result, Env, error) {
	snap, err := e.snapshot(ctx, false)
	if err != nil {
		return nil, nil, Env{}, err
	}
	ok := make([]bool, len(specs))
	results := make([]*Result, len(specs))
	for i, spec := range specs {
		if ok[i], results[i], err = e.satisfied(ctx, spec, snap.env); err != nil {
			return nil, nil, Env{}, err
		}
	}
	return ok, results, snap.env, nil
}

func (e *Evaluator) satisfied(ctx context.Context, spec Spec, env Env) (bool, *Result, error) {
	current := Fingerprint(env, spec)
	if current == "" || !Approved(spec) || e.changedInput(spec) != "" {
		return false, nil, nil
	}
	row, err := e.Store.DecidingEvalResult(ctx, e.SessionID, spec.ID, current, SpecDigest(spec), spec.ApprovalEpoch)
	if err != nil || row == nil {
		return false, nil, storeErr(ctx, err)
	}
	r := fromRow(*row)
	return Satisfies(r, current, spec, e.inRun(r.Seq)), &r, nil
}

// Environment returns the workspace's current fingerprint environment.
func (e *Evaluator) Environment(ctx context.Context) (Env, error) {
	snap, err := e.snapshot(ctx, false)
	return snap.env, err
}

// Accept records a person's acceptance of spec at the current fingerprint under its approval: a passed
// result with provenance human:<actor>, an episode of its own closed at the fingerprint the person saw.
// It is a row, not a flag, and holds until the fingerprint or the approval changes. Outside git it
// fails with ErrNoFingerprint; an unapproved spec, or one whose input differs in this workspace, fails
// with ErrSpecUnapproved (ADR-0017: accepting a result is not approving the definition that produced it).
func (e *Evaluator) Accept(ctx context.Context, spec Spec, actor string) (Result, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 128 || strings.ContainsAny(actor, " \t\r\n") {
		return Result{}, errors.New("actor must be 1-128 bytes without whitespace")
	}
	if !Approved(spec) {
		return Result{}, ErrSpecUnapproved
	}
	if changed := e.changedInput(spec); changed != "" {
		return Result{}, fmt.Errorf("%w: %s, which the check runs, is not the approved content", ErrSpecUnapproved, changed)
	}
	env, err := e.Environment(ctx)
	if err != nil {
		return Result{}, err
	}
	current := Fingerprint(env, spec)
	if current == "" {
		return Result{}, ErrNoFingerprint
	}
	episode, err := newEpisodeID()
	if err != nil {
		return Result{}, err
	}
	r := Result{Check: spec.ID, Kind: spec.Kind, Status: StatusPassed, Provenance: ProvenanceHumanPrefix + actor,
		Fingerprint: current, SpecDigest: SpecDigest(spec), ApprovalEpoch: spec.ApprovalEpoch,
		EpisodeID: episode, EpisodeStatus: EpisodeClosed, PreFingerprint: current, PostFingerprint: current,
		Artifacts: []string{}, At: time.Now().UTC()}
	return e.store(ctx, r)
}

func (e *Evaluator) store(ctx context.Context, r Result) (Result, error) {
	seq, err := e.Store.AppendEvalResult(ctx, e.SessionID, session.EvalRow{
		WorkUnitID: r.WorkUnitID, CheckID: r.Check, Kind: r.Kind, Status: r.Status, Provenance: r.Provenance,
		Fingerprint: r.Fingerprint, SpecDigest: r.SpecDigest, ApprovalEpoch: r.ApprovalEpoch,
		EpisodeID: r.EpisodeID, EpisodeStatus: r.EpisodeStatus, PreFingerprint: r.PreFingerprint, PostFingerprint: r.PostFingerprint,
		Artifacts: r.Artifacts, At: r.At,
	})
	if err != nil {
		return Result{}, storeErr(ctx, err)
	}
	r.Seq = seq
	return r, nil
}

// FromRow converts a stored row.
func FromRow(row session.EvalRow) Result { return fromRow(row) }

func fromRow(row session.EvalRow) Result {
	artifacts := row.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}
	return Result{Seq: row.Seq, WorkUnitID: row.WorkUnitID, Check: row.CheckID, Kind: row.Kind, Status: row.Status,
		Provenance: row.Provenance, Fingerprint: row.Fingerprint, SpecDigest: row.SpecDigest, ApprovalEpoch: row.ApprovalEpoch,
		EpisodeID: row.EpisodeID, EpisodeStatus: row.EpisodeStatus, PreFingerprint: row.PreFingerprint, PostFingerprint: row.PostFingerprint,
		Artifacts: artifacts, At: row.At}
}

// run executes spec once as part of episode and stores its runtime result open in it, with its log
// and, unless it passed, the diff its fingerprint covers.
func (e *Evaluator) run(ctx context.Context, spec Spec, fingerprint, workUnitID string, snap snapshot, episode string) (Result, string, error) {
	var (
		status string
		log    []byte
	)
	switch spec.Kind {
	case KindTest, KindBuild:
		var err error
		status, log, err = e.runCommand(ctx, spec)
		if err != nil {
			return Result{}, "", err
		}
	case KindSchema:
		status, log = e.runSchema(spec)
	default:
		status, log = StatusError, []byte(fmt.Sprintf("unknown check kind %q\n", spec.Kind))
	}
	at := time.Now().UTC()
	logArtifact, err := e.putArtifact(ctx, ArtifactLog, log, at)
	if err != nil {
		return Result{}, "", err
	}
	artifacts := []string{logArtifact}
	if status != StatusPassed && len(snap.diff) > 0 {
		diffArtifact, err := e.putArtifact(ctx, ArtifactDiff, snap.diff, at)
		if err != nil {
			return Result{}, "", err
		}
		artifacts = append(artifacts, diffArtifact)
	}
	stored, err := e.store(ctx, Result{WorkUnitID: workUnitID, Check: spec.ID, Kind: spec.Kind, Status: status,
		Provenance: ProvenanceRuntime, Fingerprint: fingerprint, SpecDigest: SpecDigest(spec), ApprovalEpoch: spec.ApprovalEpoch,
		EpisodeID: episode, EpisodeStatus: EpisodeOpen, PreFingerprint: fingerprint,
		Artifacts: artifacts, At: at})
	if err != nil {
		return Result{}, "", err
	}
	return stored, lastBytes(log, TailBytes), nil
}

// runCommand runs a test or build command through the shell runner in the session's workspace. Its
// process group is killed on timeout or cancellation and reaped after it exits. Exit 0 is passed,
// another exit failed, a failure to start or a timeout error. The caller's cancellation is an error,
// not a result.
func (e *Evaluator) runCommand(ctx context.Context, spec Spec) (string, []byte, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return StatusError, []byte("check has no command\n"), nil
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	runner := &shell.Runner{Workspace: e.Workspace, HeadBytes: MaxArtifactBytes / 4, TailBytes: MaxArtifactBytes / 4, Reap: true}
	result, err := runner.Run(runCtx, spec.Command, nil)
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "$ %s\n", spec.Command)
	status := StatusPassed
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		status = StatusError
		fmt.Fprintf(&b, "timed out after %s; process group killed\n", spec.Timeout)
	case err != nil:
		status = StatusError
		fmt.Fprintf(&b, "could not run: %v\n", err)
	case result.ExitCode != 0:
		status = StatusFailed
		fmt.Fprintf(&b, "exit %d after %s\n", result.ExitCode, result.Duration.Round(time.Millisecond))
	default:
		fmt.Fprintf(&b, "exit 0 after %s\n", result.Duration.Round(time.Millisecond))
	}
	writeStream(&b, "stdout", result.Stdout)
	writeStream(&b, "stderr", result.Stderr)
	return status, b.Bytes(), nil
}

// runSchema validates each of spec.Files against spec.Schema, both relative to the workspace. A schema
// that cannot be read or compiled is an error; a file that is missing, not JSON or invalid fails.
func (e *Evaluator) runSchema(spec Spec) (string, []byte) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "schema %s\n", spec.Schema)
	schemaPath, err := e.inside(spec.Schema)
	if err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
		return StatusError, b.Bytes()
	}
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
		return StatusError, b.Bytes()
	}
	compiler := jsonschema.NewCompiler()
	url := "file:///" + filepath.ToSlash(strings.TrimPrefix(schemaPath, "/"))
	if err := compiler.AddResource(url, bytes.NewReader(raw)); err != nil {
		fmt.Fprintf(&b, "error: schema is not valid JSON: %v\n", err)
		return StatusError, b.Bytes()
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		fmt.Fprintf(&b, "error: schema does not compile: %v\n", err)
		return StatusError, b.Bytes()
	}
	status := StatusPassed
	if len(spec.Files) == 0 {
		fmt.Fprintf(&b, "error: no files to validate\n")
		return StatusError, b.Bytes()
	}
	for _, file := range spec.Files {
		path, err := e.inside(file)
		if err != nil {
			status = StatusFailed
			fmt.Fprintf(&b, "%s: %v\n", file, err)
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			status = StatusFailed
			fmt.Fprintf(&b, "%s: %v\n", file, err)
			continue
		}
		doc, err := decodeJSON(raw)
		if err != nil {
			status = StatusFailed
			fmt.Fprintf(&b, "%s: not JSON: %v\n", file, err)
			continue
		}
		if err := schema.Validate(doc); err != nil {
			status = StatusFailed
			fmt.Fprintf(&b, "%s: %v\n", file, err)
			continue
		}
		fmt.Fprintf(&b, "%s: valid\n", file)
	}
	return status, b.Bytes()
}

// inside resolves a workspace-relative path and refuses one that leaves the workspace.
func (e *Evaluator) inside(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative to the workspace", rel)
	}
	root, err := filepath.EvalSymlinks(e.Workspace)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, rel)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(root, resolved); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the workspace", rel)
	}
	return resolved, nil
}

func (e *Evaluator) putArtifact(ctx context.Context, kind string, data []byte, at time.Time) (string, error) {
	data, truncated := bound(data, MaxArtifactBytes)
	sum := sha256.Sum256(data)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	err := e.Store.PutArtifact(ctx, e.SessionID, session.Artifact{
		Hash: hash, Kind: kind, Size: int64(len(data)), Truncated: truncated, CreatedAt: at, Data: data,
	})
	return hash, storeErr(ctx, err)
}

// snapshot is the workspace at the start of a decision: its environment and, when wanted, the diff the
// fingerprint covers, bounded for an artifact.
type snapshot struct {
	env  Env
	diff []byte
}

// snapshot reads HEAD, `git diff --binary HEAD` and the untracked, non-ignored files of the workspace.
// Outside git, or in a repository without a commit, the environment has no Head and no fingerprint.
func (e *Evaluator) snapshot(ctx context.Context, wantDiff bool) (snapshot, error) {
	head, err := git(ctx, e.Workspace, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return snapshot{}, ctx.Err()
		}
		return snapshot{env: Env{WorktreeID: e.WorktreeID}}, nil
	}
	// Everything that makes the tree dirty goes through h; written counts it, so a clean tree (no diff,
	// no untracked file) has DirtyHash "".
	h := sha256.New()
	written := &countWriter{w: h}
	diff := streambuf.New(MaxArtifactBytes/2, MaxArtifactBytes/2)
	var sink io.Writer = written
	if wantDiff {
		sink = io.MultiWriter(written, diff)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", e.Workspace, "diff", "--binary", "HEAD")
	cmd.Stdout = sink
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return snapshot{}, ctx.Err()
		}
		return snapshot{}, fmt.Errorf("git diff: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	untracked, err := git(ctx, e.Workspace, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		if ctx.Err() != nil {
			return snapshot{}, ctx.Err()
		}
		return snapshot{}, fmt.Errorf("git ls-files: %w", err)
	}
	var listing bytes.Buffer
	for _, rel := range strings.Split(untracked, "\x00") {
		if rel == "" {
			continue
		}
		sum, err := fileHash(filepath.Join(e.Workspace, rel))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return snapshot{}, err
		}
		fmt.Fprintf(written, "\x00untracked\x00%s\x00%s", rel, sum)
		fmt.Fprintf(&listing, "untracked %s %s\n", rel, sum)
	}
	snap := snapshot{env: Env{Head: head, WorktreeID: e.WorktreeID}}
	if written.n > 0 {
		snap.env.DirtyHash = hex.EncodeToString(h.Sum(nil))
		if wantDiff {
			joined, _ := join(diff.Snapshot())
			snap.diff = append(joined, listing.Bytes()...)
		}
	}
	return snap, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func fileHash(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		io.WriteString(h, "symlink\x00"+target)
	} else {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

func writeStream(b *bytes.Buffer, name string, s streambuf.Snapshot) {
	if s.Total == 0 {
		return
	}
	fmt.Fprintf(b, "--- %s (%d bytes) ---\n", name, s.Total)
	data, _ := join(s)
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
}

// join reassembles a head/tail snapshot: the whole output when it was retained (head and tail may
// overlap), else head, an omission marker and tail.
func join(s streambuf.Snapshot) ([]byte, bool) {
	if s.Total <= int64(len(s.Head)) {
		return s.Head, false
	}
	if !s.Truncated {
		overlap := int64(len(s.Head)+len(s.Tail)) - s.Total
		return append(append([]byte(nil), s.Head...), s.Tail[overlap:]...), false
	}
	omitted := s.Total - int64(len(s.Head)+len(s.Tail))
	out := append([]byte(nil), s.Head...)
	out = append(out, fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted)...)
	return append(out, s.Tail...), true
}

// bound keeps at most max bytes of data, head and tail, with an omission marker.
func bound(data []byte, max int) ([]byte, bool) {
	if len(data) <= max {
		return data, false
	}
	keep := (max - 64) / 2
	marker := []byte(fmt.Sprintf("\n[... %d bytes omitted ...]\n", len(data)-2*keep))
	out := append(append(append([]byte(nil), data[:keep]...), marker...), data[len(data)-keep:]...)
	return out, true
}

func lastBytes(data []byte, n int) string {
	if len(data) > n {
		data = data[len(data)-n:]
	}
	return string(data)
}

// decodeJSON decodes one JSON document, numbers kept exact, and rejects trailing data.
func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the top-level value")
	}
	return v, nil
}
