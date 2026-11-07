package evaluator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/verify"
)

// These tests are the permanent regressions of the evaluator defects an independent review found:
// a claim fingerprint that ignored gitignored and non-git content (EVAL-01) and a check whose oracle
// and data files were not part of its approval, so a run could rewrite the expected answer and pass
// (EVAL-02).

// oracleSpec decides by comparing a deliverable with the host's committed expected answer.
func oracleSpec() Spec {
	return Spec{ID: "answer", Kind: KindSemantic, Command: "diff -q answer.txt tests/expected.txt",
		Timeout: 30 * time.Second, ApprovalEpoch: 1, Files: []string{"tests/expected.txt"}, FilesDeclared: true}
}

// EVAL-02: the data file a check compares against is part of its definition. Rewriting the oracle in
// the workspace is not a new result, it is a definition change, and the check does not run.
func TestCheckBindsTheCommittedOracleItComparesAgainst(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("tests/expected.txt", "42\n")
	f.git("add", "tests/expected.txt")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "host oracle")

	spec := ResolveInputs(f.dir, []Spec{oracleSpec()})[0]
	var bound []string
	for _, in := range spec.Inputs {
		bound = append(bound, in.Path)
		if in.Path == "tests/expected.txt" && !strings.HasPrefix(in.Digest, "sha256:") {
			t.Fatalf("the oracle's content is not in the definition: %+v", in)
		}
	}
	if !containsString(bound, "tests/expected.txt") {
		t.Fatalf("the committed oracle is not bound as an input: %v", bound)
	}
	if containsString(bound, "answer.txt") {
		t.Fatalf("a deliverable that does not exist yet was bound as an input: %v", bound)
	}

	// The wrong answer, and the oracle rewritten to match it: the reviewed evaluator passed this.
	f.write("answer.txt", "41\n")
	if changed := f.ev.changedInput(spec); changed != "" {
		t.Fatalf("producing the deliverable is a definition change: %q", changed)
	}
	f.write("tests/expected.txt", "41\n")
	if changed := f.ev.changedInput(spec); changed != "tests/expected.txt" {
		t.Fatalf("a rewritten oracle was accepted: changed=%q", changed)
	}
	verdict := f.decide(spec)
	if verdict.Passed || verdict.Result.Status != StatusSpecUnapproved {
		t.Fatalf("a tampered oracle decided the check: %+v", verdict)
	}
	if !strings.Contains(verdict.Tail, "not the approved content") {
		t.Fatalf("tail=%q", verdict.Tail)
	}
}

// EVAL-02: a file the repository does not track is a deliverable an earlier attempt left behind, not
// an approved dependency: producing or changing it does not change the definition.
func TestUntrackedDeliverableIsNotAnInput(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("tests/expected.txt", "42\n")
	f.git("add", "tests/expected.txt")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "host oracle")
	f.write("answer.txt", "41\n") // left behind by an earlier attempt, untracked

	spec := ResolveInputs(f.dir, []Spec{oracleSpec()})[0]
	for _, in := range spec.Inputs {
		if in.Path == "answer.txt" {
			t.Fatalf("an untracked deliverable was bound as an approved input: %+v", spec.Inputs)
		}
	}
	f.write("answer.txt", "42\n")
	if changed := f.ev.changedInput(spec); changed != "" {
		t.Fatalf("changing the deliverable is a definition change: %q", changed)
	}
}

// EVAL-01: the workspace digest covers files git ignores. The reviewed fingerprint hashed only the
// diff and the untracked non-ignored files, so a gitignored deliverable could change under a
// confirmation that stayed valid.
func TestEnvironmentDigestCoversIgnoredFiles(t *testing.T) {
	f := newFixture(t, true)
	f.write(".gitignore", "out/\n")
	f.git("add", ".gitignore")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "ignore")
	if err := os.MkdirAll(filepath.Join(f.dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("out/report.csv", "usd,100\n")

	before, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.IgnoredHash == "" {
		t.Fatal("a workspace holding a gitignored file has an empty ignored digest")
	}
	if before.DirtyHash != "" {
		t.Fatalf("the ignored file leaked into the git-visible digest: %q", before.DirtyHash)
	}
	f.write("out/report.csv", "eur,93\n") // same length, different bytes
	after, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.String() == after.String() {
		t.Fatalf("the ignored file's content change did not move the fingerprint: %q", after.String())
	}
	// The same bytes again restore the same fingerprint: the digest is content, not a clock.
	f.write("out/report.csv", "usd,100\n")
	again, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.String() != before.String() {
		t.Fatalf("the same content produced a different fingerprint: %q vs %q", before.String(), again.String())
	}
}

// EVAL-01: outside git the environment used to be the constant "+@local", so a confirmation was
// accepted forever. It is now the content digest of the tree.
func TestEnvironmentDigestOutsideGitIsContentDerived(t *testing.T) {
	f := newFixture(t, false)
	f.write("answer.txt", "41\n")
	before, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.IgnoredHash == "" || before.String() == "+@local" || before.String() == "++@local" {
		t.Fatalf("a non-git workspace has a constant environment: %q", before.String())
	}
	if Fingerprint(before, statusOK) != "" {
		t.Fatal("a non-git workspace must not produce a reusable fingerprint")
	}
	f.write("answer.txt", "42\n")
	after, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.String() == after.String() {
		t.Fatalf("the workspace content changed but the environment did not: %q", after.String())
	}
	// The .git directory of a repository is never hashed; a plain directory tree still is.
	if err := os.MkdirAll(filepath.Join(f.dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(".git/HEAD", "ref: refs/heads/main\n")
	withGitDir, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if withGitDir.String() != after.String() {
		t.Fatalf("the .git directory changed a non-git environment: %q vs %q", withGitDir.String(), after.String())
	}
}

// EVAL-01: a check's fingerprint covers the content of its inputs, so changing the script or the
// oracle it runs invalidates a stored result even when the command text is the same.
func TestFingerprintCoversInputContent(t *testing.T) {
	env := Env{Head: "abc", DirtyHash: "clean", WorktreeID: "local"}
	original := oracleSpec()
	original.Inputs = []Input{{Path: "tests/expected.txt", Digest: "sha256:aaaa"}}
	tampered := oracleSpec()
	tampered.Inputs = []Input{{Path: "tests/expected.txt", Digest: "sha256:bbbb"}}
	if Fingerprint(env, original) == Fingerprint(env, tampered) {
		t.Fatal("two definitions over different oracle bytes share a fingerprint")
	}
	if Fingerprint(env, original) == "" {
		t.Fatal("a git workspace must have a fingerprint")
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// EVAL-01 counterexample: past the hashing caps a rewrite that preserves size and modification time
// is indistinguishable by stat. The environment therefore carries no fingerprint at all, and a
// confirmation cannot be recorded or reused — never a stat-trusted one.
func TestTruncatedWorkspaceHasNoConfirmationFingerprint(t *testing.T) {
	savedFiles, savedBytes := fingerprintMaxFiles, fingerprintMaxBytes
	fingerprintMaxFiles, fingerprintMaxBytes = 4, 64
	t.Cleanup(func() { fingerprintMaxFiles, fingerprintMaxBytes = savedFiles, savedBytes })

	f := newFixture(t, false)
	f.write("a.txt", "1\n")
	big := filepath.Join(f.dir, "big.bin")
	if err := os.WriteFile(big, []byte(strings.Repeat("A", 128)), 0o644); err != nil {
		t.Fatal(err)
	}
	past, err := os.Stat(big)
	if err != nil {
		t.Fatal(err)
	}
	env, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !env.Truncated || env.String() != "" {
		t.Fatalf("an over-cap workspace reported a fingerprint: %+v", env)
	}
	if Fingerprint(env, statusOK) != "" {
		t.Fatal("a truncated environment produced a reusable fingerprint")
	}
	// The same bytes with a different meaning, size and mtime preserved: no fingerprint, so no
	// confirmation can be tied to this state.
	if err := os.WriteFile(big, []byte(strings.Repeat("B", 128)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(big, past.ModTime(), past.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.String() != "" {
		t.Fatalf("an over-cap rewrite produced a fingerprint: %q", after.String())
	}
	claim := verify.Claim{ID: "c1", Kind: verify.ClaimContract, Statement: "the result is right", Required: true,
		Source: verify.ClaimSource{Kind: verify.SourceKindUser}, Evidence: []string{"check:x@eval:1"}}
	if err := claim.Settle(verify.ClaimConfirmed, verify.ClaimSource{Kind: verify.SourceKindUser, Ref: "u"},
		after.String(), []string{"check:x@eval:1"}, "", time.Now().UTC()); err == nil {
		t.Fatal("a confirmation was recorded without a workspace fingerprint")
	}
}

// EVAL-02 counterexample: a script reads an oracle from inside itself, and the oracle is gitignored.
// Command-derived discovery cannot see it, so the host declares it: the declared file is approved
// content, and rewriting it refuses the check.
func TestDeclaredOracleIsBoundWhenAScriptReadsItIndirectly(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(".gitignore", "oracle.txt\n")
	f.write("scripts/check.py", "import pathlib, sys\nsys.exit(0 if pathlib.Path('answer.txt').read_text() == pathlib.Path('oracle.txt').read_text() else 1)\n")
	f.write("oracle.txt", "42\n")
	f.git("add", ".gitignore", "scripts/check.py")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "checker")

	spec := Spec{ID: "answer", Kind: KindSemantic, Command: "python3 scripts/check.py",
		Timeout: 30 * time.Second, ApprovalEpoch: 1, Files: []string{"oracle.txt"}}
	resolved := ResolveInputs(f.dir, []Spec{spec})[0]
	var bound []string
	for _, in := range resolved.Inputs {
		bound = append(bound, in.Path)
	}
	if !containsString(bound, "oracle.txt") || !containsString(bound, "scripts/check.py") {
		t.Fatalf("the declared oracle or the script is not bound: %v", bound)
	}
	if changed := f.ev.changedInput(resolved); changed != "" {
		t.Fatalf("the approved content is reported as changed: %q", changed)
	}
	f.write("oracle.txt", "41\n")
	if changed := f.ev.changedInput(resolved); changed != "oracle.txt" {
		t.Fatalf("a rewritten declared oracle was accepted: changed=%q", changed)
	}
	if verdict := f.decide(resolved); verdict.Passed || verdict.Result.Status != StatusSpecUnapproved {
		t.Fatalf("a tampered declared oracle decided the check: %+v", verdict)
	}
	// Without the declaration nothing binds it: the limitation is the host's contract, not a guess.
	undeclared := spec
	undeclared.Files = nil
	plain := ResolveInputs(f.dir, []Spec{undeclared})[0]
	for _, in := range plain.Inputs {
		if in.Path == "oracle.txt" {
			t.Fatalf("an undeclared, gitignored oracle was bound by inference: %v", plain.Inputs)
		}
	}
}

// EVAL-02: an oracle is declared, never inferred. A semantic check that names a data file but does not
// declare it decides nothing — even when the file exists and the repository tracks it — because the
// runtime cannot know whether that word is the expected truth or a deliverable.
func TestUndeclaredSemanticOracleDecidesNothing(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("tests/expected.txt", "42\n")
	f.write("answer.txt", "42\n")
	f.git("add", "tests/expected.txt", "answer.txt")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "data")

	undeclared := Spec{ID: "answer", Kind: KindSemantic, Command: "diff -q answer.txt tests/expected.txt",
		Timeout: 30 * time.Second, ApprovalEpoch: 1} // Files nil: not declared
	verdict := f.decide(undeclared)
	if verdict.Passed || verdict.Result.Status != StatusSpecUnapproved {
		t.Fatalf("an undeclared oracle decided the semantic level: %+v", verdict)
	}
	if !strings.Contains(verdict.Tail, "does not declare the files it reads") {
		t.Fatalf("tail=%q", verdict.Tail)
	}
	if rows := f.rows(); len(rows) != 0 {
		t.Fatalf("the check ran without an approved oracle: %+v", rows)
	}
	if ok, _, err := f.ev.Satisfied(context.Background(), undeclared); ok || err != nil {
		t.Fatalf("an undeclared semantic check satisfied: ok=%v err=%v", ok, err)
	}
	if Approved(undeclared) {
		t.Fatal("an undeclared semantic definition is approved")
	}

	// The self-contained mode is declared, not inferred: "files": [] is a definition the host approved.
	selfContained := undeclared
	selfContained.Files, selfContained.FilesDeclared = []string{}, true
	if !Approved(selfContained) || !selfContained.DeclaresOracle() {
		t.Fatal(`"files": [] was not accepted as the explicit self-contained declaration`)
	}
	if got := f.decide(selfContained); got.Result.Status != StatusPassed || !got.Passed {
		t.Fatalf("a self-contained semantic check did not run: %+v", got)
	}

	// Adding the declaration is a definition change: the earlier approval does not carry over.
	if SpecDigest(undeclared) == SpecDigest(selfContained) {
		t.Fatal("the declaration is not part of the definition's digest")
	}
}
