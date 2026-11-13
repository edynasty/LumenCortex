package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// These tests drive the Experimental Experience pipeline through the same real Engine, runs, checks
// and git workspace the rest of the runtime tests use. Scripted providers stand in for models; tool
// effects, check results and stored revisions are real.

const experienceChecks = `{
	"port": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 8080", "required": false},
	"port9090": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 9090", "required": false},
	"reused": {"kind": "test", "command": "test -f build/reused", "required": false}
}`

func expArtifactFact(value string) knowledge.Assertion {
	return knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: "build/reused", Value: value}
}

func expProposal(pattern string) ExperienceProposal {
	return ExperienceProposal{
		ProblemPattern:  pattern,
		ProblemType:     "build",
		Context:         "Local workspace whose service port comes from config.env.",
		ProposedCause:   "The reusable build marker was not written yet.",
		Preconditions:   []CognitiveAssertion{portFact("8080")},
		Strategy:        []string{"Write build/reused with write_file before reporting completion."},
		ExpectedOutcome: expArtifactFact("present"),
		ExpectedResult:  "passed",
		CitedPaths:      []string{"config.env"},
	}
}

// expInstallChecks adds the file-existence outcome check and re-binds the fixture's port checks to
// the current approved set. It is explicit user approval, not a forged model grant.
func expInstallChecks(t *testing.T, f *cognitionFixture) {
	t.Helper()
	setProjectChecks(t, f.root, experienceChecks)
	approveChecks(t, f.engine)
	f.bindPorts(t)
	f.bind(t, expArtifactFact("present"), "reused")
}

func expWriteThenFinal(path, content, final string) *cognitionProvider {
	return &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 0 {
			args, _ := json.Marshal(map[string]string{"path": path, "content": content})
			return cogCall("write_file", string(args)), nil
		}
		return cogFinal(final), nil
	}}
}

func expWriteThenFail(path, content string) *cognitionProvider {
	return &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 0 {
			args, _ := json.Marshal(map[string]string{"path": path, "content": content})
			return cogCall("write_file", string(args)), nil
		}
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAuth, Err: errors.New("scripted terminal origin failure")}
	}}
}

func expCommandThenFinal(command, final string) *cognitionProvider {
	return &cognitionProvider{respond: func(n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 0 {
			args, _ := json.Marshal(map[string]string{"command": command})
			return cogCall("bash", string(args)), nil
		}
		return cogFinal(final), nil
	}}
}

func expRunProvider(t *testing.T, f *cognitionFixture, goal string, p protocol.Provider) (string, AgentResult, error) {
	t.Helper()
	handle, err := f.engine.NewSession(context.Background(), SessionOptions{Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := f.engine.runAgentWith(context.Background(), handle.ID, p, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	return handle.ID, result, runErr
}

func expMustRun(t *testing.T, f *cognitionFixture, goal string, p protocol.Provider) string {
	t.Helper()
	id, result, err := expRunProvider(t, f, goal, p)
	if err != nil || result.Status != "completed" {
		t.Fatalf("run %s = %+v, %v; want completed", id, result, err)
	}
	return id
}

func expNewSession(t *testing.T, f *cognitionFixture, goal string) string {
	t.Helper()
	handle, err := f.engine.NewSession(context.Background(), SessionOptions{Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	return handle.ID
}

func expMustRunSession(t *testing.T, f *cognitionFixture, id string, p protocol.Provider) {
	t.Helper()
	result, err := f.engine.runAgentWith(context.Background(), id, p, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("run %s = %+v, %v; want completed", id, result, err)
	}
}

func expOrigin(t *testing.T, f *cognitionFixture, pattern string) string {
	t.Helper()
	return expMustRun(t, f, pattern, expWriteThenFinal("build/reused", "ready", "origin completed"))
}

func expExtract(t *testing.T, f *cognitionFixture, origin string, proposal ExperienceProposal) CognitiveNode {
	t.Helper()
	n, err := f.engine.ExtractExperience(context.Background(), origin, ExperienceExtraction{
		ScopeKind: string(knowledge.ScopeProject), Proposal: proposal, Valid: CognitiveValidTime{}})
	if err != nil {
		t.Fatalf("extract %q from %s: %v", proposal.ProblemPattern, origin, err)
	}
	return n
}

func expValidate(t *testing.T, f *cognitionFixture, origin string, n CognitiveNode) CognitiveNode {
	t.Helper()
	got, err := f.engine.ValidateExperience(context.Background(), origin, n.ID, n.Version)
	if err != nil {
		t.Fatalf("validate %s at %d: %v", n.ID, n.Version, err)
	}
	return got
}

func expValidated(t *testing.T, f *cognitionFixture, pattern string) (CognitiveNode, string) {
	t.Helper()
	expInstallChecks(t, f)
	origin := expOrigin(t, f, pattern)
	n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal(pattern)))
	if n.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("%s state = %s, want validated (%+v)", n.ID, n.Experience.State, n.Experience.Validation)
	}
	return n, origin
}

func expContextNode(view ContextGraphView, id string) (ContextNode, bool) {
	for _, node := range view.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return ContextNode{}, false
}

func expNodeJSON(t *testing.T, f *cognitionFixture, id string, version int64) string {
	t.Helper()
	db := cognitiveOpenReadOnly(t, f.engine.store.Path())
	defer db.Close()
	var raw string
	if err := db.QueryRow(`SELECT json FROM cognition_nodes WHERE id=? AND version=?`, id, version).Scan(&raw); err != nil {
		t.Fatalf("read stored experience %s@%d: %v", id, version, err)
	}
	return raw
}

func expProposalJSON(t *testing.T, p ExperienceProposal) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestExperienceOriginPromotionIsIdempotentAndUnforgeable(t *testing.T) {
	f := newCognitionFixture(t, false)
	expInstallChecks(t, f)
	const finalSentinel = "EXPERIENCE-RAW-FINAL-9f2a7c"
	origin := expMustRun(t, f, "write reusable build marker", expWriteThenFinal("build/reused", "ready", finalSentinel))
	proposal := expProposal("reusable build marker is missing")

	first := expExtract(t, f, origin, proposal)
	if first.Experience.State != knowledge.ExperienceCandidate || first.Experience.Validation.Method != "" {
		t.Fatalf("new extraction is not an unverified candidate: %+v", first.Experience)
	}
	if len(first.Sources) != 1 || first.Sources[0].SessionID != origin || first.Experience.Origin.Source.RunID == "" ||
		!strings.HasPrefix(first.Experience.Origin.TaskRef, "task_state:"+origin+"@") {
		t.Fatalf("origin/source lineage = %+v", first.Experience.Origin)
	}
	stepSeen := false
	for _, step := range first.Experience.Origin.Trace.Steps {
		if step.Tool == "write_file" && step.Outcome == "completed" && strings.HasPrefix(step.ArgsDigest, "sha256:") && step.ExecutionID != "" {
			stepSeen = true
		}
	}
	if !stepSeen {
		t.Fatalf("origin trace has no real write_file execution: %+v", first.Experience.Origin.Trace.Steps)
	}

	again := expExtract(t, f, origin, proposal)
	if again.ID != first.ID || again.Version != first.Version {
		t.Fatalf("duplicate extraction created a revision: %s@%d -> %s@%d", first.ID, first.Version, again.ID, again.Version)
	}
	if rows := countRows(t, f.engine, `SELECT COUNT(*) FROM cognition_nodes WHERE id=?`, first.ID); rows != 1 {
		t.Fatalf("duplicate extraction stored %d revisions, want 1", rows)
	}

	validated := expValidate(t, f, origin, first)
	if validated.Experience.State != knowledge.ExperienceValidated || validated.Experience.Validation.Outcome == nil ||
		validated.Experience.Validation.Outcome.Evidence.Verification != "passed" || len(validated.Experience.Validation.Conditions) != 1 {
		t.Fatalf("validated experience lacks qualified conditional evidence: %+v", validated.Experience.Validation)
	}
	if validated.Experience.Validation.Conditions[0].Evidence.SessionID != origin ||
		validated.Experience.Validation.Conditions[0].Evidence.ResultSeq < 1 ||
		validated.Experience.Validation.Outcome.Evidence.SessionID != origin {
		t.Fatalf("validation evidence is not the origin's recorded checks: %+v", validated.Experience.Validation)
	}
	if checks := countRows(t, f.engine, `SELECT COUNT(*) FROM eval_results WHERE session_id=? AND check_id IN ('port','reused')`, origin); checks < 2 {
		t.Fatalf("origin session stored %d of its condition/outcome check results", checks)
	}

	afterValidation := expExtract(t, f, origin, proposal)
	if afterValidation.ID != validated.ID || afterValidation.Version != validated.Version ||
		afterValidation.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("duplicate extraction rewrote a validated experience: %+v", afterValidation)
	}

	// The extractor boundary accepts only untrusted definition fields. State, hidden reasoning and
	// absolute/private citations are refused before any record exists.
	for name, mutate := range map[string]func(*ExperienceProposal){
		"secret strategy":  func(p *ExperienceProposal) { p.Strategy = []string{"use token ghp_0123456789abcdefghij"} },
		"private citation": func(p *ExperienceProposal) { p.CitedPaths = []string{"/Users/alice/.ssh/id_rsa"} },
		"absolute context": func(p *ExperienceProposal) { p.Context = "read /etc/passwd" },
	} {
		t.Run(name, func(t *testing.T) {
			p := expProposal("rejected unsafe proposal")
			mutate(&p)
			if _, err := f.engine.ExtractExperience(context.Background(), origin, ExperienceExtraction{ScopeKind: string(knowledge.ScopeProject), Proposal: p}); err == nil {
				t.Fatalf("unsafe proposal with %s was accepted", name)
			}
		})
	}
	for _, field := range []string{"state", "hiddenReasoning", "checkIds"} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(expProposalJSON(t, expProposal("forged authority"))), &object); err != nil {
			t.Fatal(err)
		}
		object[field] = json.RawMessage(`"validated"`)
		raw, _ := json.Marshal(object)
		if _, err := ParseExperienceProposal(string(raw)); err == nil {
			t.Fatalf("proposal carrying authority field %q was accepted", field)
		}
	}
	host := validated
	host.Title = "forged host experience"
	if _, err := f.engine.PutCognitiveNode(context.Background(), origin, host, validated.Version); err == nil {
		t.Fatal("ordinary host graph API accepted an Experience payload")
	}
	if latest, err := f.engine.ReadExperience(context.Background(), origin, validated.ID, 0); err != nil || latest.Version != validated.Version {
		t.Fatalf("forged host write changed the experience: %+v %v", latest, err)
	}

	// Only the definition is stored under the experience record: the origin's final answer and model
	// prose are not copied into it.
	raw := expNodeJSON(t, f, validated.ID, validated.Version)
	for _, forbidden := range []string{finalSentinel, "hiddenReasoning", "ghp_", "/Users/alice"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("stored experience contains %q: %s", forbidden, raw)
		}
	}
}

func TestExperienceFailedOriginWarningAndSuccessRefusal(t *testing.T) {
	f := newCognitionFixture(t, false)
	expInstallChecks(t, f)
	origin, result, err := expRunProvider(t, f, "failing build origin", expWriteThenFail("build/reused", "partial"))
	if err == nil || result.Status != "failed" {
		t.Fatalf("scripted failed origin = %+v, %v; want failed", result, err)
	}
	if file, err := os.Stat(filepath.Join(f.root, "build/reused")); err != nil || file.IsDir() {
		t.Fatalf("failed origin did not execute its real tool before failing: %v", err)
	}

	warning := expProposal("port 9090 is known to fail")
	warning.ExpectedOutcome = portFact("9090")
	warning.ExpectedResult = "failed"
	warningNode := expExtract(t, f, origin, warning)
	if warningNode.Experience.Origin.Outcome != "failed" || len(warningNode.Experience.Origin.Trace.Steps) == 0 ||
		warningNode.Experience.Origin.Trace.Incomplete {
		t.Fatalf("failed origin trace was not recorded as a real closed episode: %+v", warningNode.Experience.Origin)
	}
	node := expValidate(t, f, origin, warningNode)
	if node.Experience.Origin.Outcome != "failed" || node.Experience.State != knowledge.ExperienceValidated ||
		node.Experience.Validation.Outcome == nil || node.Experience.Validation.Outcome.Evidence.Verification != "failed" {
		t.Fatalf("negative warning did not validate against its failed origin: %+v", node.Experience)
	}

	success := expProposal("failed origin cannot claim success")
	success.ExpectedOutcome = portFact("8080")
	success.ExpectedResult = "passed"
	rejected := expValidate(t, f, origin, expExtract(t, f, origin, success))
	if rejected.Experience.State != knowledge.ExperienceRejected || rejected.Experience.Validation.Reason != "failed_origin_is_not_success" ||
		rejected.Experience.Validation.Outcome != nil {
		t.Fatalf("failed origin promoted an expected success: %+v", rejected.Experience.Validation)
	}
	consumer := expMustRun(t, f, "consumer of rejected warning", cogAnswer("cannot use it"))
	if _, err := f.engine.AdoptExperience(context.Background(), consumer, rejected.ID, rejected.Version); !errors.Is(err, ErrExperienceNotApplicable) {
		t.Fatalf("rejected experience adopted: %v", err)
	}
}

func TestExperienceEvidenceAndConditionsGatePromotion(t *testing.T) {
	f := newCognitionFixture(t, false)
	expInstallChecks(t, f)
	origin := expOrigin(t, f, "evidence gate origin")

	missing := expProposal("unbound outcome cannot promote")
	missing.ExpectedOutcome = knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: "build/unbound", Value: "present"}
	node := expExtract(t, f, origin, missing)
	node = expValidate(t, f, origin, node)
	if node.Experience.State != knowledge.ExperienceNeedsEvidence || node.Experience.Validation.Reason != "outcome_not_qualified" ||
		node.Experience.Validation.Outcome != nil {
		t.Fatalf("unbound evidence promoted or lost its reason: %+v", node.Experience.Validation)
	}

	failed := expProposal("failed outcome is a counterexample")
	failed.ExpectedOutcome = portFact("9090")
	node = expValidate(t, f, origin, expExtract(t, f, origin, failed))
	if node.Experience.State != knowledge.ExperienceRejected || node.Experience.Validation.Reason != "outcome_counterexample" {
		t.Fatalf("failed evidence promoted: %+v", node.Experience.Validation)
	}

	valid := expProposal("conditions must still hold at reuse")
	validNode := expValidate(t, f, origin, expExtract(t, f, origin, valid))
	if validNode.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("setup experience not validated: %+v", validNode.Experience.Validation)
	}
	consumer := expNewSession(t, f, "consumer with wrong condition")
	writeFile(t, filepath.Join(f.root, "defaults.env"), "BASE=9090\n")
	if _, err := f.engine.AdoptExperience(context.Background(), consumer, validNode.ID, validNode.Version); !errors.Is(err, ErrExperienceNotApplicable) {
		t.Fatalf("adoption with a failing condition = %v, want ErrExperienceNotApplicable", err)
	}
	after, err := f.engine.ReadExperience(context.Background(), origin, validNode.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Experience.Uses) != 0 || after.Version != validNode.Version {
		t.Fatalf("failed condition check partially promoted an adoption: version %d uses %d", after.Version, len(after.Experience.Uses))
	}
}

func TestExperienceIndependentReuseExecutesToolAndRecordsChecks(t *testing.T) {
	f := newCognitionFixture(t, false)
	expInstallChecks(t, f)
	origin := expOrigin(t, f, "A wrote the reusable build marker")
	node := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("reusable build marker is missing")))
	if node.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("origin experience not validated: %+v", node.Experience.Validation)
	}
	if !reflect.DeepEqual(node.Sources, []knowledge.SourceRef{node.Experience.Origin.Source}) {
		t.Fatalf("experience sources = %+v, want only the origin source", node.Sources)
	}

	consumer := expNewSession(t, f, "B reuses the build marker experience")
	if consumer == origin {
		t.Fatal("consumer session is the origin")
	}
	adoption, err := f.engine.AdoptExperience(context.Background(), consumer, node.ID, node.Version)
	if err != nil {
		t.Fatalf("adopt in independent session: %v", err)
	}
	if adoption.ExperienceID != node.ID || adoption.DefinitionVersion != node.Experience.DefinitionVersion ||
		len(adoption.Conditions) != 1 || adoption.Conditions[0].Evidence.SessionID != consumer || adoption.Conditions[0].Evidence.ResultSeq < 1 {
		t.Fatalf("adoption did not run/record the consumer condition: %+v", adoption)
	}
	if checks := countRows(t, f.engine, `SELECT COUNT(*) FROM eval_results WHERE session_id=? AND check_id='port'`, consumer); checks < 1 {
		t.Fatal("adoption condition check was not recorded in the consumer session")
	}

	// Before B executes anything, its context graph advertises the adopted experience as a locator,
	// not as strategy text copied from A's chat.
	view, err := f.engine.GetContextGraph(context.Background(), consumer)
	if err != nil {
		t.Fatalf("consumer context graph before run: %v", err)
	}
	locator, ok := expContextNode(view, "experience:"+node.ID)
	if !ok || locator.Kind != "experience" || locator.Status != knowledge.ExperienceValidated ||
		locator.Freshness != "requires_condition_check" || locator.SourceRef != "cognition:"+node.ID+"@"+itoa(adoption.Version) {
		t.Fatalf("adopted experience locator = %+v, present=%v; view=%+v", locator, ok, view.Nodes)
	}
	for _, text := range node.Experience.Definition.Strategy {
		if strings.Contains(locator.Summary, text) {
			t.Fatalf("context graph copied strategy text into B: %q", locator.Summary)
		}
	}

	matches, err := f.engine.RecallExperiences(context.Background(), consumer, node.Experience.Definition.ProblemPattern, 4)
	if err != nil || len(matches) != 1 || matches[0].ID != node.ID || matches[0].Applicability != "requires_condition_check" {
		t.Fatalf("exact recall = %+v, %v", matches, err)
	}
	other, err := f.engine.RecallExperiences(context.Background(), consumer, "a deliberately different problem", 4)
	if err != nil || len(other) != 0 {
		t.Fatalf("mismatched pattern recalled %+v, %v", other, err)
	}

	// Prove B's ordinary run, not a chat copy, produces the outcome the record checks: remove the
	// origin artifact first, then let B's write_file recreate it.
	artifact := filepath.Join(f.root, "build/reused")
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("artifact still present before independent execution: %v", err)
	}
	expApply(t, f, consumer, node, adoption.ID)
	if info, err := os.Stat(artifact); err != nil || info.IsDir() {
		t.Fatalf("B did not execute the real write tool: %v", err)
	}

	fed, err := f.engine.RecordExperienceFeedback(context.Background(), consumer, node.ID, adoption.ID)
	if err != nil {
		t.Fatalf("feedback after independent run: %v", err)
	}
	if len(fed.Experience.Uses) != 1 {
		t.Fatalf("feedback recorded %d uses, want 1", len(fed.Experience.Uses))
	}
	use := fed.Experience.Uses[0]
	if use.Feedback == nil || use.Feedback.Outcome != "success" || use.Feedback.Source.SessionID != consumer ||
		use.Feedback.Source.RunID == "" || use.Feedback.Observation == nil ||
		use.Feedback.Observation.Evidence.SessionID != consumer || use.Feedback.Observation.Evidence.ResultSeq < 1 {
		t.Fatalf("feedback is not backed by B's later terminal check: %+v", use)
	}
	stats := fed.Experience.Statistics()
	if stats.Adoptions != 1 || stats.Successes != 1 || stats.Failures != 0 || stats.Unconfirmed != 0 {
		t.Fatalf("statistics = %+v", stats)
	}
	replayed, err := f.engine.RecordExperienceFeedback(context.Background(), consumer, node.ID, adoption.ID)
	if err != nil || replayed.Version != fed.Version || !reflect.DeepEqual(replayed.Experience, fed.Experience) {
		t.Fatalf("repeat feedback was not idempotent: version %d -> %d, err=%v", fed.Version, replayed.Version, err)
	}
	if checks := countRows(t, f.engine, `SELECT COUNT(*) FROM eval_results WHERE session_id=? AND check_id='reused'`, consumer); checks < 1 {
		t.Fatal("feedback outcome check was not recorded in B")
	}
}

func TestExperienceFailureFeedbackPreservesHistoryAndForcesRevision(t *testing.T) {
	f := newCognitionFixture(t, false)
	original, origin := expConditionalValidated(t, f, "reused marker can regress")
	consumer := expNewSession(t, f, "B breaks the adopted outcome")
	adoption, err := f.engine.AdoptExperience(context.Background(), consumer, original.ID, original.Version)
	if err != nil {
		t.Fatal(err)
	}
	result, err := expApplyCounterexample(t, f, consumer, original, adoption.ID)
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer counterexample run: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "build/reused")); !os.IsNotExist(err) {
		t.Fatalf("B did not remove the artifact: %v", err)
	}
	failed, err := f.engine.RecordExperienceFeedback(context.Background(), consumer, original.ID, adoption.ID)
	if err != nil {
		t.Fatalf("failure feedback: %v", err)
	}
	if failed.Experience.State != knowledge.ExperienceNeedsRevision || failed.Experience.Confidence != "contested" {
		t.Fatalf("counterexample did not contest the definition: %+v", failed.Experience)
	}
	if !reflect.DeepEqual(failed.Experience.Origin, original.Experience.Origin) ||
		!reflect.DeepEqual(failed.Experience.Validation, original.Experience.Validation) ||
		failed.Experience.Definition.Digest() != original.Experience.Definition.Digest() {
		t.Fatal("failure feedback rewrote origin or original validation")
	}
	if len(failed.Experience.Uses) != 1 || failed.Experience.Uses[0].Feedback == nil ||
		failed.Experience.Uses[0].Feedback.Outcome != "failure" || !failed.Experience.Uses[0].Feedback.RevisionNeeded {
		t.Fatalf("failure history is incomplete: %+v", failed.Experience.Uses)
	}
	historical, err := f.engine.ReadExperience(context.Background(), origin, original.ID, original.Version)
	if err != nil || historical.Experience.State != knowledge.ExperienceValidated ||
		historical.Experience.Validation.Outcome == nil || len(historical.Experience.Uses) != 0 {
		t.Fatalf("historical validated revision was rewritten: %+v, %v", historical.Experience, err)
	}
	if _, err := f.engine.ValidateExperience(context.Background(), origin, original.ID, failed.Version); err == nil {
		t.Fatal("counterexample was revalidated without a revised definition")
	}

	revisedProposal := original.Experience.Definition
	revisedProposal.Strategy = []string{"Check the port, then write build/reused with write_file before completion."}
	revised, err := f.engine.ReviseExperience(context.Background(), origin, original.ID, failed.Version, revisedProposal)
	if err != nil {
		t.Fatalf("revise definition: %v", err)
	}
	if revised.Experience.State != knowledge.ExperienceCandidate || revised.Experience.DefinitionVersion != original.Experience.DefinitionVersion+1 ||
		revised.Experience.SupersedesVersion != original.Experience.DefinitionVersion || !reflect.DeepEqual(revised.Experience.Validation, knowledge.ExperienceValidation{}) ||
		len(revised.Experience.Uses) != 0 {
		t.Fatalf("revision did not reset validation or exposed consumer feedback: %+v", revised.Experience)
	}
	consumerHistory, err := f.engine.ReadExperience(context.Background(), consumer, revised.ID, revised.Version)
	if err != nil || consumerHistory.Experience == nil || !reflect.DeepEqual(consumerHistory.Experience.Uses, failed.Experience.Uses) {
		t.Fatalf("revision did not preserve the consumer's exact failure history: %+v, %v", consumerHistory.Experience, err)
	}
	adopter := expNewSession(t, f, "revised experience before validation")
	if _, err := f.engine.AdoptExperience(context.Background(), adopter, revised.ID, revised.Version); !errors.Is(err, ErrExperienceNotApplicable) {
		t.Fatalf("unvalidated revision adopted: %v", err)
	}
	// Recreate the environment outcome; the revised definition still needs its own validation run.
	writeFile(t, filepath.Join(f.root, "build/reused"), "ready again")
	revalidated := expValidate(t, f, origin, revised)
	if revalidated.Experience.State != knowledge.ExperienceValidated || revalidated.Experience.DefinitionVersion != revised.Experience.DefinitionVersion ||
		revalidated.Experience.Validation.Outcome == nil {
		t.Fatalf("revised definition was not revalidated: %+v", revalidated.Experience.Validation)
	}
	if _, err := f.engine.AdoptExperience(context.Background(), adopter, revalidated.ID, revalidated.Version); err != nil {
		t.Fatalf("validated revision could not be adopted: %v", err)
	}
}

func TestExperienceScopeIsolationAndConcurrentAdoption(t *testing.T) {
	f := newCognitionFixture(t, false)
	expInstallChecks(t, f)
	origin := expOrigin(t, f, "scope origin")

	sessionScoped := expProposal("session-only experience")
	sessionNode, err := f.engine.ExtractExperience(context.Background(), origin, ExperienceExtraction{
		ScopeKind: string(knowledge.ScopeSession), Proposal: sessionScoped})
	if err != nil {
		t.Fatal(err)
	}
	sessionNode = expValidate(t, f, origin, sessionNode)
	if _, err := f.engine.ReadExperience(context.Background(), origin, sessionNode.ID, 0); err != nil {
		t.Fatalf("origin cannot read its session-scoped experience: %v", err)
	}
	other := expNewSession(t, f, "unrelated session")
	if _, err := f.engine.ReadExperience(context.Background(), other, sessionNode.ID, 0); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("unrelated session read session-scoped experience = %v, want session.ErrFenced", err)
	}
	if matches, err := f.engine.RecallExperiences(context.Background(), other, sessionScoped.ProblemPattern, 4); err != nil || len(matches) != 0 {
		t.Fatalf("session-scoped experience leaked into unrelated recall: %+v, %v", matches, err)
	}
	if _, err := f.engine.ExtractExperience(context.Background(), origin, ExperienceExtraction{
		ScopeKind: "global", Proposal: expProposal("global experience")}); err == nil {
		t.Fatal("global experience scope was accepted")
	}

	// Two consumers at the same baseline race for the same CAS version: exactly one adoption is
	// admitted; the loser leaves no use row behind.
	node := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("concurrent adoption experience")))
	consumers := []string{expNewSession(t, f, "first concurrent consumer"), expNewSession(t, f, "second concurrent consumer")}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, consumer := range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.engine.AdoptExperience(context.Background(), consumer, node.ID, node.Version)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	succeeded, revision := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, session.ErrCognitiveRevision):
			revision++
		default:
			t.Fatalf("unexpected concurrent adoption error: %v", err)
		}
	}
	if succeeded != 1 || revision != 1 {
		t.Fatalf("concurrent adoption successes=%d revision_conflicts=%d", succeeded, revision)
	}
	read, err := f.engine.ReadExperience(context.Background(), origin, node.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := knowledge.DecodeNode(expNodeJSON(t, f, node.ID, read.Version))
	if err != nil || stored.Experience == nil || len(stored.Experience.Uses) != 1 {
		t.Fatalf("CAS left partial adoption rows: %+v, %v", stored.Experience, err)
	}

	// A cited source that changed after validation is stale: adoption and promotion fail without a
	// new use or revision.
	writeFile(t, filepath.Join(f.root, "config.env"), cognitionConfig+"# changed after validation\n")
	staleConsumer := expNewSession(t, f, "consumer after source change")
	staleBefore, err := f.engine.ReadExperience(context.Background(), origin, node.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.AdoptExperience(context.Background(), staleConsumer, node.ID, staleBefore.Version); !errors.Is(err, session.ErrSourceChanged) {
		t.Fatalf("adoption after cited source change = %v, want ErrSourceChanged", err)
	}
	staleAfter, err := f.engine.ReadExperience(context.Background(), origin, node.ID, 0)
	if err != nil || staleAfter.Version != staleBefore.Version || len(staleAfter.Experience.Uses) != len(staleBefore.Experience.Uses) {
		t.Fatalf("stale source path wrote a partial adoption: before=%d/%d after=%d/%d err=%v", staleBefore.Version, len(staleBefore.Experience.Uses), staleAfter.Version, len(staleAfter.Experience.Uses), err)
	}
	writeFile(t, filepath.Join(f.root, "config.env"), cognitionConfig)

	// A revoked approval removes authority without rewriting the stored experience.
	binding := f.bind(t, portFact("8080"), "port")
	if _, err := f.engine.RevokeApproval(context.Background(), RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: binding.BindingDigest}); err != nil {
		t.Fatal(err)
	}
	before, err := f.engine.ReadExperience(context.Background(), origin, node.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	lateConsumer := expNewSession(t, f, "consumer after revocation")
	if _, err := f.engine.AdoptExperience(context.Background(), lateConsumer, node.ID, before.Version); err == nil {
		t.Fatal("adoption succeeded without its approved condition binding")
	}
	unadoptedView, err := f.engine.GetContextGraph(context.Background(), lateConsumer)
	if err != nil {
		t.Fatal(err)
	}
	if locator, ok := expContextNode(unadoptedView, "experience:"+node.ID); ok || locator.Kind == "experience" {
		t.Fatalf("failed adoption/failed conditions still exposed an experience locator: %+v", locator)
	}
	after, err := f.engine.ReadExperience(context.Background(), origin, node.ID, 0)
	if err != nil || after.Version != before.Version || len(after.Experience.Uses) != len(before.Experience.Uses) {
		t.Fatalf("failed approval path left a partial adoption: before=%d/%d after=%d/%d err=%v", before.Version, len(before.Experience.Uses), after.Version, len(after.Experience.Uses), err)
	}
}

func TestExperiencePrivacyDeletionErasesHistory(t *testing.T) {
	for _, deleted := range []string{"origin", "consumer"} {
		t.Run(deleted, func(t *testing.T) {
			f := newCognitionFixture(t, false)
			original, origin := expValidated(t, f, "deletable experience")
			consumer := expNewSession(t, f, "consumer history")
			unexposed := expNewSession(t, f, "unrelated unexposed reader")
			adoption, err := f.engine.AdoptExperience(context.Background(), consumer, original.ID, original.Version)
			if err != nil {
				t.Fatal(err)
			}
			expApply(t, f, consumer, original, adoption.ID)
			if _, err := f.engine.RecordExperienceFeedback(context.Background(), consumer, original.ID, adoption.ID); err != nil {
				t.Fatal(err)
			}
			if rows := countRows(t, f.engine, `SELECT COUNT(*) FROM cognition_nodes WHERE id=?`, original.ID); rows != 4 {
				t.Fatalf("expected candidate, validated, adoption and feedback revisions, got %d", rows)
			}
			target := origin
			if deleted == "consumer" {
				target = consumer
			}
			if _, err := f.engine.DeleteSession(context.Background(), target, WorktreeCleanup{}); err != nil {
				t.Fatal(err)
			}
			if rows := countRows(t, f.engine, `SELECT COUNT(*) FROM cognition_nodes WHERE id=?`, original.ID); rows != 0 {
				t.Fatalf("%d experience revisions survived privacy deletion", rows)
			}
			if deleted == "origin" {
				if _, err := f.engine.ReadExperience(context.Background(), consumer, original.ID, 0); !errors.Is(err, session.ErrFenced) {
					t.Fatalf("exposed consumer not fenced: %v", err)
				}
			}
			if _, err := f.engine.ReadExperience(context.Background(), unexposed, original.ID, 0); !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("deleted experience read by unexposed reader = %v", err)
			}
			if matches, err := f.engine.RecallExperiences(context.Background(), unexposed, original.Experience.Definition.ProblemPattern, 4); err != nil || len(matches) != 0 {
				t.Fatalf("deleted experience still recalled: %+v, %v", matches, err)
			}
			if _, _, err := f.engine.Session(context.Background(), unexposed); err != nil {
				t.Fatalf("unrelated session affected: %v", err)
			}
		})
	}
}

func TestExperienceSurvivesEngineReopen(t *testing.T) {
	engine, root := cognitiveOpenUpgradeProject(t)
	current, closed := engine, false
	t.Cleanup(func() {
		if !closed {
			_ = current.Close()
		}
	})
	f := &cognitionFixture{engine: engine, root: root}
	expInstallChecks(t, f)
	origin := expOrigin(t, f, "reopen experience")
	node := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("reopen experience")))

	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	current, closed = reopened, false
	got, err := reopened.ReadExperience(context.Background(), origin, node.ID, 0)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if got.Version != node.Version || got.Experience.State != knowledge.ExperienceValidated ||
		!reflect.DeepEqual(got.Experience.Origin, node.Experience.Origin) ||
		!reflect.DeepEqual(got.Experience.Validation, node.Experience.Validation) ||
		!reflect.DeepEqual(got.Sources, node.Sources) {
		t.Fatalf("reopen changed experience: %+v", got.Experience)
	}
	matches, err := reopened.RecallExperiences(context.Background(), origin, node.Experience.Definition.ProblemPattern, 4)
	if err != nil || len(matches) != 1 || matches[0].ID != node.ID || matches[0].Version != node.Version {
		t.Fatalf("recall after reopen = %+v, %v", matches, err)
	}
}
