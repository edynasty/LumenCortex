package evaluator

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func inputPaths(s Spec) []string {
	var out []string
	for _, in := range s.Inputs {
		out = append(out, in.Path)
	}
	return out
}

// The inputs of a definition are the workspace files its command executes, however the command spells
// them, and the schema of a schema check. The files a program reads as arguments, directories, files
// outside the workspace, expansions and the files a schema check validates are not.
func TestResolveInputsNamesTheFilesADefinitionRuns(t *testing.T) {
	f := newFixture(t, false)
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("scripts/a.sh", "exit 0\n")
	f.write("check.sh", "exit 0\n")
	f.write(".golangci.yml", "linters: {}\n")
	f.write("schema.json", `{"type": "object"}`)
	f.write("data.json", `{}`)
	outside := filepath.Join(t.TempDir(), "outside.sh")
	if err := os.WriteFile(outside, []byte("exit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.dir, "link.sh")); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.EvalSymlinks(filepath.Join(f.dir, "check.sh"))
	for _, c := range []struct {
		name string
		spec Spec
		want []string
	}{
		{"script argument", Spec{Kind: KindTest, Command: "sh scripts/a.sh"}, []string{"scripts/a.sh"}},
		{"dot-slash program", Spec{Kind: KindTest, Command: "./check.sh"}, []string{"check.sh"}},
		{"outside and expanded", Spec{Kind: KindTest, Command: "sh " + outside + "; sh ../x.sh; sh $HOME/a.sh; sh scripts/*.sh"}, nil},
		{"nested shell", Spec{Kind: KindTest, Command: `bash -c 'sh ./scripts/a.sh && go test ./...'`}, []string{"scripts/a.sh"}},
		{"interpreter options", Spec{Kind: KindTest, Command: "FOO=1 env bash -e -x scripts/a.sh"}, []string{"scripts/a.sh"}},
		{"after an operator", Spec{Kind: KindTest, Command: "go vet ./... && ./check.sh > out.txt"}, []string{"check.sh"}},
		{"absolute inside", Spec{Kind: KindTest, Command: "sh " + abs}, []string{"check.sh"}},
		{"repeated", Spec{Kind: KindTest, Command: "sh scripts/a.sh; sh scripts//a.sh"}, []string{"scripts/a.sh"}},
		{"data arguments", Spec{Kind: KindTest, Command: "grep -q ok check.sh && test -f scripts/a.sh"}, nil},
		{"tool configuration", Spec{Kind: KindBuild, Command: "golangci-lint run --config=.golangci.yml ./..."}, nil},
		{"directories only", Spec{Kind: KindTest, Command: "go test ./... && ls scripts"}, nil},
		{"symlink leaving the workspace", Spec{Kind: KindTest, Command: "sh link.sh"}, nil},
		{"schema", Spec{Kind: KindSchema, Schema: "schema.json", Files: []string{"data.json"}}, []string{"schema.json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := inputPaths(ResolveInputs(f.dir, []Spec{c.spec})[0])
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("inputs %v, want %v", got, c.want)
			}
		})
	}
	missing := ResolveInputs(f.dir, []Spec{{Kind: KindSchema, Schema: "gone.json", Files: []string{"data.json"}}})[0]
	if len(missing.Inputs) != 1 || missing.Inputs[0].Digest != AbsentInput {
		t.Fatalf("a missing schema: %+v", missing.Inputs)
	}
	plain := Spec{ID: "p", Kind: KindTest, Command: "go test ./..."}
	if SpecDigest(ResolveInputs(f.dir, []Spec{plain})[0]) != SpecDigest(plain) {
		t.Fatal("a definition without inputs digests differently")
	}
}

// gateScript is an approved check that runs a workspace script, its inputs read as the runtime reads
// them when the set is approved.
func (f *fixture) gateScript() Spec {
	f.t.Helper()
	spec := Spec{ID: "gate", Kind: KindTest, Command: "sh scripts/gate.sh", Timeout: 30 * time.Second, Required: true, ApprovalEpoch: 1}
	return ResolveInputs(f.dir, []Spec{spec})[0]
}

// CHECKS-002: the approved definition runs scripts/gate.sh. Rewriting the script to pass changes the
// definition's digest, and a definition approved with the old content is not run against the new one:
// it is decided spec_unapproved, naming the script, and nothing is stored.
func TestRewritingTheScriptOfAnApprovedCheckDoesNotPassIt(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("scripts/gate.sh", "grep -q ok status.txt\n")
	f.git("add", "scripts/gate.sh")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "gate")
	approved := f.gateScript()
	if v := f.decide(approved); v.Passed || v.Result.Status != StatusFailed {
		t.Fatalf("the approved script on a bad tree: %+v", v)
	}

	f.write("scripts/gate.sh", "exit 0\n")
	before := len(f.rows())
	v := f.decide(approved)
	if v.Passed || v.Result.Status != StatusSpecUnapproved || !strings.Contains(v.Tail, "scripts/gate.sh") {
		t.Fatalf("the rewritten script: %+v", v)
	}
	if len(f.rows()) != before {
		t.Fatal("the rewritten script ran")
	}
	if SpecDigest(f.gateScript()) == SpecDigest(approved) {
		t.Fatal("the rewritten script does not change the definition's digest")
	}

	f.write("scripts/gate.sh", "grep -q ok status.txt\n")
	f.write("status.txt", "ok\n")
	if v := f.decide(approved); !v.Passed || v.Episode != EpisodeClosed {
		t.Fatalf("the approved content restored: %+v", v)
	}
}

// A script that rewrites itself while the check runs leaves the episode spec_unapproved even when the
// script is ignored by git, so the workspace snapshot cannot see the change: the pass counts for
// nothing and is not reused.
func TestAScriptChangedDuringTheEpisodeCountsForNothing(t *testing.T) {
	f := newFixture(t, true)
	if err := os.MkdirAll(filepath.Join(f.dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(".gitignore", "scripts/\n")
	f.write("status.txt", "ok\n")
	f.git("add", ".gitignore", "status.txt")
	f.git("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "ignore")
	f.write("scripts/gate.sh", "grep -q ok status.txt && echo 'exit 0' > scripts/gate.sh\n")
	approved := f.gateScript()
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
