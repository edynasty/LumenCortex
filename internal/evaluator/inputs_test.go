package evaluator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// commandPaths finds the file a command executes however the command spells it, and nothing a program
// only reads: arguments, inline code, redirection targets, option arguments, expansions and commands
// looked up in PATH. The spellings include those the FIX_REVIEW F1/F7 reproductions used.
func TestCommandPathsFindsTheExecutedFiles(t *testing.T) {
	for _, c := range []struct {
		command string
		want    []string
	}{
		{"sh scripts/a.sh", []string{"scripts/a.sh"}},
		{"./check.sh", []string{"./check.sh"}},
		{"go vet ./... && ./check.sh > out.txt", []string{"./check.sh"}},
		{`bash -c 'sh ./scripts/a.sh && go test ./...'`, []string{"./scripts/a.sh"}},
		{"bash -ec 'sh scripts/a.sh'", []string{"scripts/a.sh"}},
		{"FOO=1 env bash -e -x scripts/a.sh", []string{"scripts/a.sh"}},
		{"bash -o pipefail scripts/a.sh", []string{"scripts/a.sh"}},
		{"bash -euo pipefail scripts/a.sh arg.txt", []string{"scripts/a.sh"}},
		{"bash --rcfile rc scripts/a.sh", []string{"scripts/a.sh"}},
		{"bash -- scripts/a.sh", []string{"scripts/a.sh"}},
		{"env -i sh scripts/a.sh", []string{"scripts/a.sh"}},
		{"env -u HOME X=1 sh scripts/a.sh", []string{"scripts/a.sh"}},
		{"/usr/bin/env bash scripts/a.sh", []string{"scripts/a.sh"}},
		{"nice -n 5 sh scripts/a.sh", []string{"scripts/a.sh"}},
		{"timeout -s KILL 30 sh scripts/a.sh", []string{"scripts/a.sh"}},
		{"exec sh scripts/a.sh", []string{"scripts/a.sh"}},
		{"if true; then . scripts/a.sh; fi", []string{"scripts/a.sh"}},
		{"source scripts/a.sh", []string{"scripts/a.sh"}},
		{`sh "scripts/my gate.sh"`, []string{"scripts/my gate.sh"}},
		{"sh scripts/a.sh &> out.txt; sh scripts/b.sh >& log.txt 2>&1", []string{"scripts/a.sh", "scripts/b.sh"}},
		{"python3 -W error tools/check.py data.json", []string{"tools/check.py"}},
		{"python3 -u -m pytest tests/test_x.py", nil},
		{`python3 -c "import json; json.load(open('data.json'))"`, nil},
		{"node -e 'require(\"./x.js\")'", nil},
		{"deno run --allow-read tools/check.ts", []string{"tools/check.ts"}},
		{"echo $(sh scripts/a.sh)", []string{"scripts/a.sh"}},
		{"jq '.a | .b' data.json", nil},
		{"grep -q ok status.txt && test -f scripts/a.sh", nil},
		{"bash -s < scripts/a.sh", []string{"scripts/a.sh"}},
		{"sh < scripts/a.sh", []string{"scripts/a.sh"}},
		{"sh scripts/a.sh < input.txt", []string{"scripts/a.sh"}},
		{"python3 < tools/check.py", []string{"tools/check.py"}},
		{"jq . < data.json", nil},
		{"go test ./... && make check", nil},
		{"sh $HOME/a.sh; sh scripts/*.sh; sh ~/a.sh", nil},
		{"golangci-lint run --config=.golangci.yml ./...", nil},
		{"sh 2> err.txt scripts/a.sh", []string{"scripts/a.sh"}},
		{"> log.txt ./check.sh", []string{"./check.sh"}},
	} {
		if got := commandPaths(c.command); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.command, got, c.want)
		}
	}
}

func inputsOf(s Spec) map[string]string {
	out := map[string]string{}
	for _, in := range s.Inputs {
		out[in.Path] = in.Digest
	}
	return out
}

// ResolveInputs binds what a definition executes, relative to the workspace root: a present file by
// its content, a missing one as absent (FIX_REVIEW F3), a file outside the workspace or reached
// through a symlink leaving it never by content. Files git ignores (a binary the check builds) are not
// inputs (F2). A schema check binds its schema and the files it $refs (F5), not the files it validates.
func TestResolveInputsBindsWhatADefinitionRuns(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("scripts/a.sh", "exit 0\n")
	f.write(".gitignore", "bin/\n")
	if err := os.MkdirAll(filepath.Join(f.dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("bin/x", "binary\n")
	f.write("schema.json", `{"type": "object", "properties": {"a": {"$ref": "defs/a.json#/x"}, "b": {"$ref": "#/definitions/b"}, "c": {"$ref": "https://example.test/c.json"}}}`)
	if err := os.MkdirAll(filepath.Join(f.dir, "defs"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("defs/a.json", `{"x": {"$ref": "../shared.json"}}`)
	f.write("data.json", `{}`)
	outside := filepath.Join(t.TempDir(), "outside.sh")
	if err := os.WriteFile(outside, []byte("exit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.dir, "link.sh")); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.EvalSymlinks(filepath.Join(f.dir, "scripts", "a.sh"))
	for _, c := range []struct {
		name string
		spec Spec
		want map[string]bool // path -> present
	}{
		{"present script", Spec{Kind: KindTest, Command: "sh scripts/a.sh"}, map[string]bool{"scripts/a.sh": true}},
		{"absolute inside", Spec{Kind: KindTest, Command: "sh " + abs}, map[string]bool{"scripts/a.sh": true}},
		{"missing script", Spec{Kind: KindTest, Command: "sh scripts/new.sh"}, map[string]bool{"scripts/new.sh": false}},
		{"outside", Spec{Kind: KindTest, Command: "sh " + outside + "; sh ../x.sh"}, map[string]bool{}},
		{"symlink leaving the workspace", Spec{Kind: KindTest, Command: "sh link.sh"}, map[string]bool{"link.sh": false}},
		{"ignored build output", Spec{Kind: KindBuild, Command: "go build -o bin/x . && ./bin/x"}, map[string]bool{}},
		{"schema and its refs", Spec{Kind: KindSchema, Schema: "schema.json", Files: []string{"data.json"}},
			map[string]bool{"schema.json": true, "defs/a.json": true, "shared.json": false}},
		{"missing schema", Spec{Kind: KindSchema, Schema: "gone.json", Files: []string{"data.json"}}, map[string]bool{"gone.json": false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := inputsOf(ResolveInputs(f.dir, []Spec{c.spec})[0])
			if len(got) != len(c.want) {
				t.Fatalf("inputs %v, want %v", got, c.want)
			}
			for p, present := range c.want {
				d, ok := got[p]
				if !ok || (d != AbsentInput) != present {
					t.Fatalf("inputs %v, want %v", got, c.want)
				}
			}
		})
	}
	plain := Spec{ID: "p", Kind: KindTest, Command: "go test ./..."}
	if SpecDigest(ResolveInputs(f.dir, []Spec{plain})[0]) != SpecDigest(plain) {
		t.Fatal("a definition without inputs digests differently")
	}
}

// gateScript is an approved check that runs a workspace script, its inputs read as the runtime reads
// them when the set is approved.
func (f *fixture) gateScript(command string) Spec {
	f.t.Helper()
	spec := Spec{ID: "gate", Kind: KindTest, Command: command, Timeout: 30 * time.Second, Required: true, ApprovalEpoch: 1}
	return ResolveInputs(f.dir, []Spec{spec})[0]
}

func (f *fixture) commitScript(body string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.write("scripts/gate.sh", body)
	f.git("add", "scripts/gate.sh")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "gate")
}

// CHECKS-002: the approved definition runs scripts/gate.sh. Rewriting the script to pass changes the
// definition's digest, and a definition approved with the old content is not run against the new one:
// it is decided spec_unapproved, naming the script, nothing is stored, a person cannot accept it and
// no stored pass satisfies it (FIX_REVIEW F4). Restoring the content makes it count again.
func TestRewritingTheScriptOfAnApprovedCheckDoesNotPassIt(t *testing.T) {
	f := newFixture(t, true)
	f.commitScript("grep -q ok status.txt\n")
	f.write("status.txt", "ok\n")
	approved := f.gateScript("bash -euo pipefail scripts/gate.sh")
	if v := f.decide(approved); !v.Passed {
		t.Fatalf("the approved script: %+v", v)
	}
	f.write("status.txt", "bad\n")

	f.write("scripts/gate.sh", "exit 0\n")
	before := len(f.rows())
	v := f.decide(approved)
	if v.Passed || v.Result.Status != StatusSpecUnapproved || !strings.Contains(v.Tail, "scripts/gate.sh") {
		t.Fatalf("the rewritten script: %+v", v)
	}
	if len(f.rows()) != before {
		t.Fatal("the rewritten script ran")
	}
	if SpecDigest(f.gateScript("bash -euo pipefail scripts/gate.sh")) == SpecDigest(approved) {
		t.Fatal("the rewritten script does not change the definition's digest")
	}
	if _, err := f.ev.Accept(context.Background(), approved, "desktop-user"); !errors.Is(err, ErrSpecUnapproved) || !strings.Contains(err.Error(), "scripts/gate.sh") {
		t.Fatalf("accepting under the rewritten script: %v", err)
	}

	f.write("scripts/gate.sh", "grep -q ok status.txt\n")
	if _, err := f.ev.Accept(context.Background(), approved, "desktop-user"); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := f.ev.Satisfied(context.Background(), approved); !ok {
		t.Fatal("the acceptance under the approved script does not satisfy")
	}
	f.write("scripts/gate.sh", "exit 0\n")
	if ok, _, _ := f.ev.Satisfied(context.Background(), approved); ok {
		t.Fatal("a pass satisfies while the script is not the approved one")
	}
}

// A script approved while absent is bound as absent: creating it is a change, so a script the agent
// writes is not run as the approved check (FIX_REVIEW F3).
func TestAScriptCreatedAfterApprovalIsNotApproved(t *testing.T) {
	f := newFixture(t, true)
	approved := f.gateScript("sh scripts/new.sh")
	if v := f.decide(approved); v.Passed || v.Result.Status != StatusFailed {
		t.Fatalf("the absent script: %+v", v)
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("scripts/new.sh", "exit 0\n")
	if v := f.decide(approved); v.Passed || v.Result.Status != StatusSpecUnapproved || !strings.Contains(v.Tail, "scripts/new.sh") {
		t.Fatalf("the created script: %+v", v)
	}
}

// A script that rewrites itself while the check runs leaves the episode spec_unapproved: the input is
// compared again after the checks ran, before the workspace scope, so the pass counts for nothing and
// is not reused.
func TestAScriptChangedDuringTheEpisodeCountsForNothing(t *testing.T) {
	f := newFixture(t, true)
	f.write("status.txt", "ok\n")
	f.commitScript("grep -q ok status.txt && echo 'exit 0' > scripts/gate.sh\n")
	approved := f.gateScript("sh scripts/gate.sh")
	v := f.decide(approved)
	if v.Passed || v.Result.Status != StatusPassed || v.Episode != EpisodeSpecUnapproved {
		t.Fatalf("a self-rewriting script: %+v", v)
	}
	if row := f.rowBySeq(v.Result.Seq); row.EpisodeStatus != EpisodeSpecUnapproved {
		t.Fatalf("its row: %+v", row)
	}
	if ok, _, _ := f.ev.Satisfied(context.Background(), approved); ok {
		t.Fatal("the pass satisfies")
	}
}

// A stored pass satisfies only while the inputs match. An untracked script bound at approval and
// ignored afterwards no longer moves the fingerprint, so rewriting it leaves the fingerprint of the
// pass current: the input comparison is what refuses it.
func TestAPassDoesNotSatisfyOnceAnIgnoredInputChanged(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.write("scripts/gate.sh", "exit 0\n")
	approved := f.gateScript("sh scripts/gate.sh")
	f.write(".gitignore", "scripts/gate.sh\n")
	if v := f.decide(approved); !v.Passed {
		t.Fatalf("the approved script: %+v", v)
	}
	env, err := f.ev.Environment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.write("scripts/gate.sh", "echo rewritten; exit 0\n")
	if after, err := f.ev.Environment(context.Background()); err != nil || Fingerprint(after, approved) != Fingerprint(env, approved) {
		t.Fatalf("the ignored script moved the fingerprint (%v)", err)
	}
	if ok, _, _ := f.ev.Satisfied(context.Background(), approved); ok {
		t.Fatal("a pass satisfies while the ignored script is not the approved one")
	}
}
