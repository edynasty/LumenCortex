package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

// These tests are the runtime-level regressions of the evaluator defects an independent review found:
// a claim confirmation accepted at a constant fingerprint (EVAL-01) and a semantic check that passed
// after the model rewrote the host's oracle (EVAL-02). Both run the real Engine: SQLite store, real
// evaluator, real tools.

// writeThenFinish is a model that writes files and then answers, so a run can produce a wrong
// deliverable and tamper with a check's oracle.
type writeThenFinish struct {
	writes []map[string]string
	step   int
}

func (p *writeThenFinish) Model() string { return "write-then-finish" }

func (p *writeThenFinish) Complete(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.step++
	if p.step <= len(p.writes) {
		write := p.writes[p.step-1]
		raw, err := json.Marshal(map[string]any{"path": write["path"], "content": write["content"]})
		if err != nil {
			return protocol.ProviderResponse{}, err
		}
		return protocol.ProviderResponse{
			Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
				{ID: "w" + write["path"], Name: "write_file", Arguments: raw}}},
			FinishReason: "tool_calls",
		}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
}

// EVAL-01: outside git a confirmation is tied to the workspace's content, not to a constant, so a
// change to the deliverable the claim is about stops it counting.
func TestNonGitClaimConfirmationFollowsWorkspaceContent(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "non-git"})
	if err != nil {
		t.Fatal(err)
	}
	answer := filepath.Join(engine.workspace, "answer.txt")
	if err := os.WriteFile(answer, []byte("41\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := engine.workspaceFingerprint(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before == "" || before == "+@local" {
		t.Fatalf("a non-git workspace has no content fingerprint: %q", before)
	}
	added, err := engine.AddSessionClaim(ctx, handle.ID, ClaimView{Kind: "contract", Statement: "answer is 42", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := engine.SettleSessionClaim(ctx, handle.ID, added.ID, "confirmed", []string{"check:answer@eval:1"}, "user-1")
	if err != nil {
		t.Fatalf("confirming at the workspace's fingerprint failed: %v", err)
	}
	if settled.Status != "confirmed" || settled.Fingerprint != before {
		t.Fatalf("settled=%+v", settled)
	}
	view, err := engine.SessionVerification(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Claims.Confirmed != 1 {
		t.Fatalf("a fresh confirmation does not count: %+v", view.Claims)
	}

	if err := os.WriteFile(answer, []byte("completely different\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := engine.workspaceFingerprint(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatalf("the deliverable changed but the fingerprint did not: %q", after)
	}
	view, err = engine.SessionVerification(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Claims.Confirmed != 0 || view.Claims.Stale != 1 {
		t.Fatalf("a confirmation survived a change to the file it is about: %+v", view.Claims)
	}
}

// EVAL-01: a gitignored deliverable is still the content a confirmation is about. The reviewed
// fingerprint hashed only the diff and the untracked non-ignored files, so this edit was invisible.
func TestIgnoredDeliverableInvalidatesAConfirmation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ws := t.TempDir()
	initGitRepo(t, ws)
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".gitignore"}, {"commit", "-m", "ignore"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(ws, "out", "report.csv")
	if err := os.WriteFile(report, []byte("usd,100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := Open(Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "report"})
	if err != nil {
		t.Fatal(err)
	}
	added, err := engine.AddSessionClaim(ctx, handle.ID, ClaimView{Kind: "contract", Statement: "out/report.csv is in USD", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	before, err := engine.workspaceFingerprint(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SettleSessionClaim(ctx, handle.ID, added.ID, "confirmed", []string{"check:units@eval:1"}, "user-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("eur,93\n"), 0o644); err != nil { // same length, other bytes
		t.Fatal(err)
	}
	after, err := engine.workspaceFingerprint(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatalf("the gitignored deliverable changed but the fingerprint did not: %q", after)
	}
	view, err := engine.SessionVerification(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Claims.Confirmed != 0 || view.Claims.Stale != 1 {
		t.Fatalf("a confirmation survived a change to the gitignored file it is about: %+v", view.Claims)
	}
}

// EVAL-02: the host commits the expected answer, approves the semantic check, and the model writes a
// wrong deliverable and rewrites the oracle to match it. The reviewed runtime passed the check; the
// approval now covers the oracle's bytes, so the check decides nothing and the run cannot complete.
func TestTamperedOracleCannotSatisfyASemanticCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("diff"); err != nil {
		t.Skip("diff not installed")
	}
	root := t.TempDir()
	initGitRepo(t, root)
	useChecks(t, `{"answer": {"kind": "semantic", "command": "diff -q answer.txt tests/expected.txt", "required": false,
		"files": ["tests/expected.txt"]}}`)
	if err := os.MkdirAll(filepath.Join(root, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "expected.txt"), []byte("42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tests/expected.txt"}, {"commit", "-m", "host oracle"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	approveChecks(t, engine)
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "produce 42"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &writeThenFinish{writes: []map[string]string{
		{"path": "answer.txt", "content": "41\n"},
		{"path": "tests/expected.txt", "content": "41\n"}, // the tampered oracle
	}}
	result, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{
		Policy: "full", MaxSteps: 8, Retry: &config.Retry{Disabled: true},
		Verification: &VerificationOptions{RequireSemantic: true, MaxRepairs: 1},
	})
	answer, readErr := os.ReadFile(filepath.Join(root, "answer.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if result.Status == "completed" {
		t.Fatalf("the semantic check passed with the wrong answer %q after the model rewrote the oracle", answer)
	}
	if !strings.Contains(string(answer), "41") {
		t.Fatalf("the test did not exercise the wrong answer: %q", answer)
	}
	// The rewritten oracle is a definition change, not a result: the check does not run against
	// content nobody approved, and the run ends without accepting the wrong answer.
	if err == nil {
		t.Fatal("a run rejected at its completion gate must report why")
	}
	if !errors.Is(err, agent.ErrChecksUnapproved) && !errors.Is(err, agent.ErrSemanticUnverified) {
		t.Fatalf("err=%v", err)
	}
	state, stateErr := engine.loadTaskState(context.Background(), handle.ID)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if state.Verification != nil && state.Verification.Status == string(verify.StatusPassed) {
		t.Fatalf("verification=%+v", state.Verification)
	}
}

// EVAL-01 counterexample, end to end: a workspace with more ignored content than a fingerprint hashes
// gets no fingerprint at all, so the host cannot even record a confirmation for it. The alternative —
// a digest of path, size and mtime — would accept a same-size, same-mtime rewrite.
func TestOverCapWorkspaceRefusesAClaimConfirmation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ws := t.TempDir()
	initGitRepo(t, ws)
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("out/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".gitignore"}, {"commit", "-m", "ignore"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	// More than the fingerprint's byte cap in one ignored file.
	big := filepath.Join(ws, "out", "blob.bin")
	if err := os.WriteFile(big, make([]byte, 17<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := Open(Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "huge"})
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint, err := engine.workspaceFingerprint(ctx, handle.ID); err != nil || fingerprint != "" {
		t.Fatalf("an over-cap workspace reported a fingerprint %q (err=%v)", fingerprint, err)
	}
	added, err := engine.AddSessionClaim(ctx, handle.ID, ClaimView{Kind: "contract", Statement: "the report is right", Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SettleSessionClaim(ctx, handle.ID, added.ID, "confirmed", []string{"check:x@eval:1"}, "user-1"); err == nil {
		t.Fatal("a confirmation was recorded without a workspace fingerprint")
	}
}
