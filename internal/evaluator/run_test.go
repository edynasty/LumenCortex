package evaluator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

type fixture struct {
	t     *testing.T
	store *session.Store
	dir   string
	ev    *Evaluator
}

// newFixture opens a session store with session s1 and a workspace; withGit makes it a repository with
// one commit holding status.txt = "bad".
func newFixture(t *testing.T, withGit bool) *fixture {
	t.Helper()
	store, err := session.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Create(context.Background(), session.Session{ID: "s1", Goal: "g", Status: "idle", Metadata: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := &fixture{t: t, store: store, dir: dir, ev: &Evaluator{Store: store, SessionID: "s1", Workspace: dir, WorktreeID: "local"}}
	f.write("status.txt", "bad\n")
	if withGit {
		f.git("init", "-q")
		f.git("add", "status.txt")
		f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "init")
	}
	return f
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) git(args ...string) {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (f *fixture) decide(spec Spec) Verdict {
	f.t.Helper()
	verdicts, err := f.ev.Decide(context.Background(), []Spec{spec}, "wu_1", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return verdicts[0]
}

// nextRun replaces the fixture's Evaluator with a new one over the same session and workspace, as the
// runtime builds one per run.
func (f *fixture) nextRun() {
	f.ev = &Evaluator{Store: f.store, SessionID: "s1", Workspace: f.dir, WorktreeID: "local"}
}

func (f *fixture) rows() []session.EvalRow {
	f.t.Helper()
	rows, err := f.store.EvalResults(context.Background(), "s1", 0, 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func (f *fixture) artifact(hash string) session.Artifact {
	f.t.Helper()
	a, err := f.store.Artifact(context.Background(), "s1", hash)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

// The fixtures' specs run under approval epoch 1, as the runtime sets it for an approved set (ADR-0017).
var statusOK = Spec{ID: "status", Kind: KindTest, Command: "grep -q ok status.txt", Timeout: 30 * time.Second, Required: true, ApprovalEpoch: 1}

// A check runs in the session's workspace; its runtime result is stored with its log, and a non-passing
// result in a dirty tree also stores the diff its fingerprint covers. A pass at the same fingerprint is
// reused; any change to the tree runs the check again.
func TestDecideStoresEvidenceAndReusesOnlyAtTheSameFingerprint(t *testing.T) {
	f := newFixture(t, true)

	v := f.decide(statusOK)
	if v.Passed || v.Result.Status != StatusFailed || v.Result.Provenance != ProvenanceRuntime || v.Result.WorkUnitID != "wu_1" {
		t.Fatalf("clean failing tree: %+v", v)
	}
	if len(v.Result.Artifacts) != 1 || f.artifact(v.Result.Artifacts[0]).Kind != ArtifactLog {
		t.Fatalf("a failure in a clean tree stores exactly its log: %v", v.Result.Artifacts)
	}
	if log := string(f.artifact(v.Result.Artifacts[0]).Data); !strings.Contains(log, "$ grep -q ok status.txt") || !strings.Contains(log, "exit 1") {
		t.Fatalf("log: %q", log)
	}
	if !strings.Contains(v.Tail, "exit 1") {
		t.Fatalf("tail %q does not quote the output", v.Tail)
	}

	f.write("notes.md", "draft\n") // untracked, still failing: the diff artifact lists it
	v = f.decide(statusOK)
	if v.Passed || len(v.Result.Artifacts) != 2 {
		t.Fatalf("dirty failing tree: %+v", v)
	}
	diff := f.artifact(v.Result.Artifacts[1])
	if diff.Kind != ArtifactDiff || !strings.Contains(string(diff.Data), "untracked notes.md sha256:") {
		t.Fatalf("diff artifact %s: %q", diff.Kind, diff.Data)
	}

	f.write("status.txt", "ok\n")
	v = f.decide(statusOK)
	if !v.Passed || v.Reused || len(v.Result.Artifacts) != 1 {
		t.Fatalf("fresh pass: %+v", v)
	}
	before := len(f.rows())
	again := f.decide(statusOK)
	if !again.Passed || !again.Reused || again.Result.Seq != v.Result.Seq || len(f.rows()) != before {
		t.Fatalf("same fingerprint must reuse the pass without running: %+v rows %d -> %d", again, before, len(f.rows()))
	}

	f.write("status.txt", "ok\nmore\n")
	if v := f.decide(statusOK); v.Reused || len(f.rows()) != before+1 {
		t.Fatalf("an edit must run the check again: %+v", v)
	}
	edited := statusOK
	edited.Command = "grep -q ok status.txt && true"
	if v := f.decide(edited); v.Reused {
		t.Fatal("editing the check must invalidate its results")
	}
}

// A person's acceptance is a passed human:<id> row at the current fingerprint. It decides the check
// without running it until the fingerprint changes; then the check runs and its failure blocks again.
func TestHumanAcceptanceHoldsUntilTheFingerprintChanges(t *testing.T) {
	f := newFixture(t, true)
	if v := f.decide(statusOK); v.Passed {
		t.Fatal("precondition: the check fails")
	}
	accepted, err := f.ev.Accept(context.Background(), statusOK, "desktop-user")
	if err != nil || accepted.Provenance != "human:desktop-user" || accepted.Status != StatusPassed || accepted.Fingerprint == "" {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	if _, err := f.ev.Accept(context.Background(), statusOK, "two words"); err == nil {
		t.Fatal("an actor with whitespace was accepted")
	}
	before := len(f.rows())
	v := f.decide(statusOK)
	if !v.Passed || !v.Reused || v.Result.Seq != accepted.Seq || len(f.rows()) != before {
		t.Fatalf("acceptance not reused: %+v", v)
	}
	if ok, r, err := f.ev.Satisfied(context.Background(), statusOK); !ok || r == nil || r.Seq != accepted.Seq || err != nil {
		t.Fatalf("Satisfied: %v %+v %v", ok, r, err)
	}

	f.write("other.txt", "x\n")
	v = f.decide(statusOK)
	if v.Passed || v.Reused || v.Result.Provenance != ProvenanceRuntime {
		t.Fatalf("acceptance outlived the fingerprint: %+v", v)
	}
	if ok, _, _ := f.ev.Satisfied(context.Background(), statusOK); ok {
		t.Fatal("Satisfied after the fingerprint changed")
	}
}

// ADR-0017: an unapproved definition is not run and stores nothing, cannot be accepted, and is never
// satisfied; a pass or an acceptance under one approval epoch counts for nothing under the next.
func TestApprovalGatesRunsAcceptanceAndReuse(t *testing.T) {
	f := newFixture(t, true)
	f.write("status.txt", "ok\n")
	unapproved := statusOK
	unapproved.ApprovalEpoch = 0
	v := f.decide(unapproved)
	if v.Passed || v.Result.Status != StatusSpecUnapproved || len(f.rows()) != 0 || !strings.Contains(v.Tail, "not approved") {
		t.Fatalf("unapproved spec: %+v rows=%d", v, len(f.rows()))
	}
	if _, err := f.ev.Accept(context.Background(), unapproved, "desktop-user"); !errors.Is(err, ErrSpecUnapproved) {
		t.Fatalf("accept unapproved: %v", err)
	}
	pass := f.decide(statusOK)
	if !pass.Passed || pass.Result.ApprovalEpoch != 1 || pass.Result.SpecDigest != SpecDigest(statusOK) {
		t.Fatalf("approved pass: %+v", pass)
	}
	if ok, _, _ := f.ev.Satisfied(context.Background(), unapproved); ok {
		t.Fatal("an unapproved spec is satisfied by the pass of its approved twin")
	}
	next := statusOK
	next.ApprovalEpoch = 2
	if ok, _, _ := f.ev.Satisfied(context.Background(), next); ok {
		t.Fatal("a pass of epoch 1 satisfies epoch 2")
	}
	before := len(f.rows())
	if v := f.decide(next); !v.Passed || v.Reused || len(f.rows()) != before+1 || v.Result.ApprovalEpoch != 2 {
		t.Fatalf("epoch 2 must run again: %+v", v)
	}
	accepted, err := f.ev.Accept(context.Background(), next, "desktop-user")
	if err != nil || accepted.ApprovalEpoch != 2 {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	third := statusOK
	third.ApprovalEpoch = 3
	if ok, _, _ := f.ev.Satisfied(context.Background(), third); ok {
		t.Fatal("an acceptance of epoch 2 satisfies epoch 3")
	}
}

// Outside git there is no fingerprint: nothing is reused, every decision runs, and nothing can be
// accepted.
func TestOutsideGitEveryDecisionRuns(t *testing.T) {
	f := newFixture(t, false)
	f.write("status.txt", "ok\n")
	first, second := f.decide(statusOK), f.decide(statusOK)
	if !first.Passed || !second.Passed || second.Reused || first.Result.Fingerprint != "" || len(f.rows()) != 2 {
		t.Fatalf("outside git: %+v %+v rows=%d", first, second, len(f.rows()))
	}
	if _, err := f.ev.Accept(context.Background(), statusOK, "desktop-user"); !errors.Is(err, ErrNoFingerprint) {
		t.Fatalf("accept outside git: %v", err)
	}
	if ok, _, err := f.ev.Satisfied(context.Background(), statusOK); ok || err != nil {
		t.Fatalf("Satisfied outside git: %v %v", ok, err)
	}
}

// A check that outlives its timeout is an error result; its process group is killed and the decision
// returns promptly. The caller's cancellation is not a result: nothing is stored.
func TestTimeoutIsAnErrorAndCancellationStoresNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX")
	}
	f := newFixture(t, true)
	slow := Spec{ID: "slow", Kind: KindTest, Command: "sleep 30 & wait", Timeout: 300 * time.Millisecond, Required: true, ApprovalEpoch: 1}
	started := time.Now()
	v := f.decide(slow)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if v.Passed || v.Result.Status != StatusError || !strings.Contains(string(f.artifact(v.Result.Artifacts[0]).Data), "timed out") {
		t.Fatalf("timeout: %+v", v)
	}

	before := len(f.rows())
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	slow.Timeout = time.Minute
	if _, err := f.ev.Decide(ctx, []Spec{slow}, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled decision: %v", err)
	}
	if len(f.rows()) != before {
		t.Fatal("a cancelled check stored a result")
	}
}

// rowBySeq returns the stored row with seq.
func (f *fixture) rowBySeq(seq int64) session.EvalRow {
	f.t.Helper()
	for _, r := range f.rows() {
		if r.Seq == seq {
			return r
		}
	}
	f.t.Fatalf("no row %d", seq)
	return session.EvalRow{}
}

// ADR-0017 episodes: a fresh pass is stored in an episode closed at the same fingerprint it began with.
// A runtime pass is reused only by the run that closed its episode, and not after a side-effect tool;
// a later run decides the check again. A person's acceptance holds across runs.
func TestEpisodeReuseIsInRunOnly(t *testing.T) {
	f := newFixture(t, true)
	f.write("status.txt", "ok\n")
	pass := f.decide(statusOK)
	row := f.rowBySeq(pass.Result.Seq)
	if !pass.Passed || pass.Episode != EpisodeClosed || row.EpisodeID == "" || row.EpisodeStatus != EpisodeClosed ||
		row.PreFingerprint != row.Fingerprint || row.PostFingerprint != row.Fingerprint {
		t.Fatalf("closed episode: %+v row %+v", pass, row)
	}
	if again := f.decide(statusOK); !again.Passed || !again.Reused || again.Result.Seq != pass.Result.Seq {
		t.Fatalf("in-run reuse: %+v", again)
	}

	// A side-effect tool completed: the run decides again, then reuses the new episode.
	f.ev.Mutated()
	before := len(f.rows())
	rerun := f.decide(statusOK)
	if !rerun.Passed || rerun.Reused || len(f.rows()) != before+1 {
		t.Fatalf("after a side-effect tool: %+v rows %d -> %d", rerun, before, len(f.rows()))
	}
	if again := f.decide(statusOK); !again.Reused || again.Result.Seq != rerun.Result.Seq {
		t.Fatalf("reuse after the rerun: %+v", again)
	}

	// The next run: the same fingerprint, the same approval, still decided again.
	f.nextRun()
	if ok, _, err := f.ev.Satisfied(context.Background(), statusOK); ok || err != nil {
		t.Fatalf("another run's pass satisfies: %v %v", ok, err)
	}
	before = len(f.rows())
	if v := f.decide(statusOK); !v.Passed || v.Reused || len(f.rows()) != before+1 {
		t.Fatalf("cross-run: %+v rows %d -> %d", v, before, len(f.rows()))
	}

	// A person's acceptance is its own closed episode and holds in the next run.
	accepted, err := f.ev.Accept(context.Background(), statusOK, "desktop-user")
	if err != nil || accepted.EpisodeStatus != EpisodeClosed || accepted.PreFingerprint != accepted.Fingerprint {
		t.Fatalf("accept: %+v %v", accepted, err)
	}
	f.nextRun()
	if v := f.decide(statusOK); !v.Passed || !v.Reused || v.Result.Seq != accepted.Seq {
		t.Fatalf("acceptance in the next run: %+v", v)
	}
}

// A check that changes a tracked file while it runs ends its episode scope_changed: its pass is kept as
// history and decides nothing. A check that only writes ignored build output keeps it closed.
func TestEpisodeScopeChangedAndIgnoredOutput(t *testing.T) {
	f := newFixture(t, true)
	f.write(".gitignore", "build/\n")
	f.write("status.txt", "ok\n")
	f.git("add", ".gitignore", "status.txt")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "ok")

	builds := Spec{ID: "build", Kind: KindBuild, Command: "mkdir -p build && echo out > build/out && grep -q ok status.txt", Timeout: 30 * time.Second, Required: true, ApprovalEpoch: 1}
	v := f.decide(builds)
	if !v.Passed || v.Episode != EpisodeClosed || f.rowBySeq(v.Result.Seq).EpisodeStatus != EpisodeClosed {
		t.Fatalf("ignored output: %+v", v)
	}

	edits := Spec{ID: "edits", Kind: KindTest, Command: "grep -q ok status.txt && echo more >> status.txt", Timeout: 30 * time.Second, Required: true, ApprovalEpoch: 1}
	v = f.decide(edits)
	row := f.rowBySeq(v.Result.Seq)
	if v.Passed || v.Result.Status != StatusPassed || v.Episode != EpisodeScopeChanged || !strings.Contains(v.Tail, "scope_changed") {
		t.Fatalf("tracked edit: %+v", v)
	}
	if row.EpisodeStatus != EpisodeScopeChanged || row.PostFingerprint == row.PreFingerprint {
		t.Fatalf("scope_changed row: %+v", row)
	}
	if ok, _, _ := f.ev.Satisfied(context.Background(), edits); ok {
		t.Fatal("a scope_changed pass satisfies")
	}
}

// When the definitions or their approval change while the episode runs, it ends spec_unapproved and
// nothing in it passes, a reused result included; it is not reused afterwards.
func TestEpisodeSpecChangedDuringTheRun(t *testing.T) {
	f := newFixture(t, true)
	f.write("status.txt", "ok\n")
	reused := f.decide(statusOK)
	other := Spec{ID: "other", Kind: KindTest, Command: "true", Timeout: 30 * time.Second, Required: true, ApprovalEpoch: 1}
	changed := func(context.Context) (bool, error) { return false, nil }
	verdicts, err := f.ev.Decide(context.Background(), []Spec{statusOK, other}, "", changed)
	if err != nil {
		t.Fatal(err)
	}
	if verdicts[0].Result.Seq != reused.Result.Seq || verdicts[0].Passed || verdicts[0].Episode != EpisodeSpecUnapproved {
		t.Fatalf("reused verdict in a spec_unapproved episode: %+v", verdicts[0])
	}
	if verdicts[1].Passed || verdicts[1].Episode != EpisodeSpecUnapproved || f.rowBySeq(verdicts[1].Result.Seq).EpisodeStatus != EpisodeSpecUnapproved {
		t.Fatalf("fresh verdict: %+v", verdicts[1])
	}
	before := len(f.rows())
	if v := f.decide(other); v.Reused || len(f.rows()) != before+1 {
		t.Fatalf("a spec_unapproved pass was reused: %+v", v)
	}
}

// A cancelled episode closes its stored rows interrupted; a row left open by a crash, and a row from
// before episodes, never satisfy and are never reused.
func TestEpisodeInterruptedOpenAndLegacyRows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX")
	}
	f := newFixture(t, true)
	f.write("status.txt", "ok\n")
	slow := Spec{ID: "slow", Kind: KindTest, Command: "sleep 30 & wait", Timeout: time.Minute, Required: true, ApprovalEpoch: 1}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	if _, err := f.ev.Decide(ctx, []Spec{statusOK, slow}, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled episode: %v", err)
	}
	rows := f.rows()
	if len(rows) != 1 || rows[0].CheckID != "status" || rows[0].EpisodeStatus != EpisodeInterrupted {
		t.Fatalf("interrupted episode rows: %+v", rows)
	}

	env, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	current := Fingerprint(env, statusOK)
	base := session.EvalRow{CheckID: "status", Kind: KindTest, Status: StatusPassed, Provenance: "human:desktop-user",
		Fingerprint: current, SpecDigest: SpecDigest(statusOK), ApprovalEpoch: 1}
	open := base
	open.EpisodeID, open.EpisodeStatus, open.PreFingerprint = "ep_crashed", EpisodeOpen, current
	for _, r := range []session.EvalRow{base, open} {
		if _, err := f.store.AppendEvalResult(context.Background(), "s1", r); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _, err := f.ev.Satisfied(context.Background(), statusOK); ok || err != nil {
		t.Fatalf("an open or legacy row satisfies: %v %v", ok, err)
	}
	before := len(f.rows())
	if v := f.decide(statusOK); !v.Passed || v.Reused || len(f.rows()) != before+1 {
		t.Fatalf("an open or legacy row was reused: %+v", v)
	}
}

// A newer row that cannot decide (an episode a crash left open, or a row from before episodes) does not
// shadow an older closed one: a person's acceptance still holds. Closing an episode twice changes
// nothing, and an unknown status is refused.
func TestUndecidingRowsDoNotShadowAClosedOne(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	accepted, err := f.ev.Accept(ctx, statusOK, "desktop-user")
	if err != nil {
		t.Fatal(err)
	}
	base := session.EvalRow{CheckID: "status", Kind: KindTest, Status: StatusPassed, Provenance: ProvenanceRuntime,
		Fingerprint: accepted.Fingerprint, SpecDigest: accepted.SpecDigest, ApprovalEpoch: 1}
	open := base
	open.EpisodeID, open.EpisodeStatus, open.PreFingerprint = "ep_crashed", EpisodeOpen, accepted.Fingerprint
	var openSeq int64
	for _, r := range []session.EvalRow{open, base} {
		seq, err := f.store.AppendEvalResult(ctx, "s1", r)
		if err != nil {
			t.Fatal(err)
		}
		if r.EpisodeID != "" {
			openSeq = seq
		}
	}
	if ok, r, err := f.ev.Satisfied(ctx, statusOK); !ok || err != nil || r.Seq != accepted.Seq {
		t.Fatalf("acceptance shadowed: ok=%v r=%+v err=%v", ok, r, err)
	}
	if err := f.store.CloseEpisode(ctx, "s1", "ep_crashed", EpisodeInterrupted, map[int64]string{openSeq: ""}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CloseEpisode(ctx, "s1", "ep_crashed", EpisodeClosed, map[int64]string{openSeq: accepted.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if row := f.rowBySeq(openSeq); row.EpisodeStatus != EpisodeInterrupted {
		t.Fatalf("a closed episode was closed again: %+v", row)
	}
	if err := f.store.CloseEpisode(ctx, "s1", "ep_crashed", "done", map[int64]string{openSeq: ""}); !errors.Is(err, session.ErrEpisodeInvalid) {
		t.Fatalf("unknown status: %v", err)
	}
}

// Schema checks validate workspace files in process; a file that does not conform fails, a schema that
// cannot be read or leaves the workspace is an error.
func TestSchemaCheck(t *testing.T) {
	f := newFixture(t, true)
	f.write("schema.json", `{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`)
	f.write("good.json", `{"name":"x"}`)
	f.write("bad.json", `{"name":3}`)
	spec := Spec{ID: "cfg", Kind: KindSchema, Schema: "schema.json", Files: []string{"good.json"}, Required: true, ApprovalEpoch: 1}
	if v := f.decide(spec); !v.Passed {
		t.Fatalf("valid file: %+v %s", v, v.Tail)
	}
	spec.Files = []string{"good.json", "bad.json"}
	if v := f.decide(spec); v.Passed || v.Result.Status != StatusFailed || !strings.Contains(v.Tail, "bad.json") {
		t.Fatalf("invalid file: %+v %s", v, v.Tail)
	}
	spec.Files = []string{"missing.json"}
	if v := f.decide(spec); v.Result.Status != StatusFailed {
		t.Fatalf("missing file: %+v", v)
	}
	spec.Schema, spec.Files = "../outside.json", []string{"good.json"}
	if v := f.decide(spec); v.Result.Status != StatusError {
		t.Fatalf("schema outside the workspace: %+v", v)
	}
}

// The artifact bound keeps head and tail of an oversized output under the 1 MiB cap.
func TestArtifactsAreBounded(t *testing.T) {
	data := []byte(strings.Repeat("a", MaxArtifactBytes) + strings.Repeat("z", MaxArtifactBytes))
	out, truncated := bound(data, MaxArtifactBytes)
	if !truncated || len(out) > MaxArtifactBytes || out[0] != 'a' || out[len(out)-1] != 'z' || !strings.Contains(string(out), "bytes omitted") {
		t.Fatalf("bound: truncated=%v len=%d", truncated, len(out))
	}
	if same, truncated := bound([]byte("small"), MaxArtifactBytes); truncated || string(same) != "small" {
		t.Fatal("small data changed")
	}
}
