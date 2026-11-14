package runtime

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

// ICRA-001. When a reader session runs assess_cognition over propositions owned by or derived from
// another session, CommitCognitiveDecision stores the serialized CognitiveAssessment in
// cognition_decisions under the reader's session_id, including the source proposition's structured
// Assertion (Target path, Key, Value), Evidence refs, node IDs and sourceSessions. DeleteSession on
// the source fenced the reader and scrubbed its tool-result transcript row, yet left the reader's
// cognition_decisions row intact in plaintext at rest after VACUUM and WAL truncation, and omitted
// it from DeletionPreview.Rows["cognition_decisions"].
func TestDeleteSessionPurgesForeignCognitiveDecisionsCitingDeletedSource(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()

	const (
		secretPath = "secrets/ICRA001-PRIVATE-PATH-9f4c2a.env"
		secretKey  = "ICRA001_SECRET_KEY_9f4c2a"
		secretVal  = "ICRA001_SECRET_VAL_9f4c2a"
	)

	owner := f.run(t, "record private configuration claim", cogAnswer("recorded"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	secretAssertion := CognitiveAssertion{
		Kind:   knowledge.AssertConfigValue,
		Target: secretPath,
		Key:    secretKey,
		Value:  secretVal,
	}
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{
		ID:        "icra001-secret-belief",
		Kind:      "belief",
		Title:     "private gateway secret setting",
		Scope:     scope,
		Assertion: &secretAssertion,
	}, 0)

	unrelated := f.run(t, "record unrelated claim", cogAnswer("unrelated"))
	unrelatedAssertion := CognitiveAssertion{
		Kind:   knowledge.AssertConfigValue,
		Target: "public/app.env",
		Key:    "PUBLIC_MODE",
		Value:  "enabled",
	}
	cognitiveMustPut(t, f.engine, unrelated, CognitiveNode{
		ID:        "icra001-unrelated-belief",
		Kind:      "belief",
		Title:     "public mode setting",
		Scope:     cognitiveMustScope(t, f.engine, unrelated, "project"),
		Assertion: &unrelatedAssertion,
	}, 0)

	reader := expNewSession(t, f, "assess private configuration")
	expMustRunSession(t, f, reader, &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("assess_cognition", `{"query":"private gateway","ids":["icra001-secret-belief"],"action":"gather_evidence"}`), nil
		}
		return cogFinal("need independent check"), nil
	}})

	unrelatedReader := expNewSession(t, f, "assess unrelated configuration")
	expMustRunSession(t, f, unrelatedReader, &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("assess_cognition", `{"query":"public mode","ids":["icra001-unrelated-belief"],"action":"gather_evidence"}`), nil
		}
		return cogFinal("unrelated assessed"), nil
	}})

	if got := countRows(t, f.engine, `SELECT count(*) FROM cognition_decisions WHERE session_id = ?`, reader); got != 1 {
		t.Fatalf("fixture: reader decision rows = %d, want 1", got)
	}
	stateBefore := readStateBytes(t, f.engine.store.Path())
	for _, sentinel := range []string{secretPath, secretKey, secretVal} {
		if !strings.Contains(stateBefore, sentinel) {
			t.Fatalf("fixture: sentinel %q missing before deletion; byte scan would prove nothing", sentinel)
		}
	}

	preview, err := f.engine.DeletionPreview(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if got := preview.Rows["cognition_decisions"]; got != 1 {
		t.Errorf("DeletionPreview.Rows[\"cognition_decisions\"] = %d, want 1 foreign decision citing owner", got)
	}

	if _, err := f.engine.DeleteSession(ctx, owner, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, f.engine, `SELECT count(*) FROM cognition_decisions WHERE session_id = ?`, reader); got != 0 {
		t.Errorf("foreign cognition_decisions rows citing deleted source survived DeleteSession: %d", got)
	}
	if got := countRows(t, f.engine, `SELECT count(*) FROM cognition_decisions WHERE session_id = ?`, unrelatedReader); got != 1 {
		t.Errorf("unrelated cognition_decisions row was lost: got %d, want 1", got)
	}

	for _, file := range []string{f.engine.store.Path(), f.engine.store.Path() + "-wal", f.engine.store.Path() + "-shm"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		for _, sentinel := range []string{secretPath, secretKey, secretVal} {
			if strings.Contains(string(raw), sentinel) {
				t.Errorf("private sentinel %q survived DeleteSession in %s", sentinel, file)
			}
		}
	}
}

// ICRA-001 (Experience owner deletion). A validated Experience node's session_id is its dedicated
// owner session, while CognitiveAssessment.SourceSessions names the origin session unless the
// Experience is in needs_revision. Deleting the Experience owner session must also purge any
// foreign cognition_decisions row that assessed that Experience node.
func TestDeleteExperienceOwnerPurgesForeignCognitiveDecisions(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()

	const expSecretTarget = "build/ICRA001-EXP-SECRET-7b1e"
	setProjectChecks(t, f.root, `{
		"port": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 8080", "required": false},
		"port9090": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 9090", "required": false},
		"secret_out": {"kind": "test", "command": "test -f `+expSecretTarget+`", "required": false}
	}`)
	approveChecks(t, f.engine)
	f.bindPorts(t)
	secretOutcome := knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: expSecretTarget, Value: "present"}
	f.bind(t, secretOutcome, "secret_out")

	origin := expNewSession(t, f, "origin of secret marker experience")
	res, err := f.engine.runAgentWith(ctx, origin, expCommandThenFinal("mkdir -p build && printf ok > "+expSecretTarget, "done"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || res.Status != "completed" {
		t.Fatalf("origin run: %+v %v", res, err)
	}
	proposal := expProposal("secret marker procedure")
	proposal.ExpectedOutcome = secretOutcome
	n := expValidate(t, f, origin, expExtract(t, f, origin, proposal))
	if n.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("fixture: state = %s", n.Experience.State)
	}

	reader := expNewSession(t, f, "reader assessing validated experience")
	expMustRunSession(t, f, reader, &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("assess_cognition", `{"ids":["`+n.ID+`"],"action":"gather_evidence"}`), nil
		}
		return cogFinal("assessed"), nil
	}})
	if got := countRows(t, f.engine, `SELECT count(*) FROM cognition_decisions WHERE session_id = ?`, reader); got != 1 {
		t.Fatalf("fixture: reader decision rows = %d, want 1", got)
	}

	preview, err := f.engine.DeletionPreview(ctx, n.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := preview.Rows["cognition_decisions"]; got != 1 {
		t.Errorf("DeletionPreview.Rows[\"cognition_decisions\"] for Experience owner = %d, want 1", got)
	}
	if _, err := f.engine.DeleteSession(ctx, n.SessionID, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, f.engine, `SELECT count(*) FROM cognition_decisions WHERE session_id = ?`, reader); got != 0 {
		t.Errorf("foreign cognition_decisions row citing deleted Experience owner survived: %d", got)
	}
}
