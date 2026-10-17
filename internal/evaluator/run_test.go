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
	verdicts, err := f.ev.Decide(context.Background(), []Spec{spec}, "wu_1")
	if err != nil {
		f.t.Fatal(err)
	}
	return verdicts[0]
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

var statusOK = Spec{ID: "status", Kind: KindTest, Command: "grep -q ok status.txt", Timeout: 30 * time.Second, Required: true}

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
	slow := Spec{ID: "slow", Kind: KindTest, Command: "sleep 30 & wait", Timeout: 300 * time.Millisecond, Required: true}
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
	if _, err := f.ev.Decide(ctx, []Spec{slow}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled decision: %v", err)
	}
	if len(f.rows()) != before {
		t.Fatal("a cancelled check stored a result")
	}
}

// Schema checks validate workspace files in process; a file that does not conform fails, a schema that
// cannot be read or leaves the workspace is an error.
func TestSchemaCheck(t *testing.T) {
	f := newFixture(t, true)
	f.write("schema.json", `{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`)
	f.write("good.json", `{"name":"x"}`)
	f.write("bad.json", `{"name":3}`)
	spec := Spec{ID: "cfg", Kind: KindSchema, Schema: "schema.json", Files: []string{"good.json"}, Required: true}
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
