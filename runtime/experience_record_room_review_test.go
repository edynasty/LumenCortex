package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// expRoomChecks adds two file conditions to the Experience fixture's checks, so that one record can
// state the three conditions a definition may have.
const expRoomChecks = `{
	"port": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 8080", "required": false},
	"port9090": {"kind": "test", "command": ". ./config.env && test \"$PORT\" = 9090", "required": false},
	"reused": {"kind": "test", "command": "test -f build/reused", "required": false},
	"config": {"kind": "test", "command": "test -f config.env", "required": false},
	"defaults": {"kind": "test", "command": "test -f defaults.env", "required": false}
}`

const expRoomCommand = "if test -f build/block; then rm -f build/reused; else printf ready > build/reused; fi"

// expRoomValidated is a validated record at the legal size of a detailed lesson: three conditions and
// a strategy of steps long sentences. Its procedure removes the expected artifact when build/block
// exists, so a consumer can observe a counterexample by applying it faithfully.
func expRoomValidated(t *testing.T, f *cognitionFixture, pattern string, steps int) CognitiveNode {
	t.Helper()
	setProjectChecks(t, f.root, expRoomChecks)
	approveChecks(t, f.engine)
	f.bindPorts(t)
	f.bind(t, expArtifactFact("present"), "reused")
	config := knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: "config.env", Value: "present"}
	defaults := knowledge.Assertion{Kind: knowledge.AssertFileExists, Target: "defaults.env", Value: "present"}
	f.bind(t, config, "config")
	f.bind(t, defaults, "defaults")
	writeFile(t, f.root+"/build/fixture", "conditional procedure workspace")
	origin := expNewSession(t, f, pattern)
	result, err := f.engine.runAgentWith(context.Background(), origin, expCommandThenFinal(expRoomCommand, "conditional origin"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("fixture: origin %+v %v", result, err)
	}
	proposal := expProposal(pattern)
	proposal.Preconditions = []CognitiveAssertion{portFact("8080"), config, defaults}
	proposal.Strategy = make([]string, steps)
	for i := range proposal.Strategy {
		proposal.Strategy[i] = fmt.Sprintf("Step %d of the procedure. ", i+1) + strings.TrimSpace(strings.Repeat("Check the marker state before the build writes it and report what was observed. ", 6))
	}
	n := expValidate(t, f, origin, expExtract(t, f, origin, proposal))
	if n.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("fixture: state %s (%+v)", n.Experience.State, n.Experience.Validation)
	}
	return n
}

type expRoomUse struct{ consumer, adoption string }

// expRoomAdopt adopts n for new consumers until the record admits no further use.
func expRoomAdopt(t *testing.T, f *cognitionFixture, n CognitiveNode) []expRoomUse {
	t.Helper()
	ctx := context.Background()
	var uses []expRoomUse
	version := n.Version
	for len(uses) < knowledge.MaxExperienceUses {
		consumer := expNewSession(t, f, "consumer of a record close to its storage bound")
		adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, version)
		if err != nil {
			if !errors.Is(err, ErrExperienceNotApplicable) {
				t.Fatalf("adoption %d was refused without naming the record's bound: %v", len(uses)+1, err)
			}
			matches, err := f.engine.RecallExperiences(ctx, consumer, "marker procedure detailed strategy", 5)
			if err != nil || len(matches) != 1 || matches[0].ID != n.ID || matches[0].Applicability != "use_limit_reached" {
				t.Fatalf("a record that admits no further use is recalled as adoptable: %+v %v", matches, err)
			}
			break
		}
		uses, version = append(uses, expRoomUse{consumer, adoption.ID}), adoption.Version
	}
	if len(uses) < 2 {
		t.Fatalf("fixture: %d uses admitted", len(uses))
	}
	return uses
}

func expRoomSize(t *testing.T, f *cognitionFixture, n CognitiveNode) int {
	t.Helper()
	current, err := f.engine.ReadExperience(context.Background(), n.Experience.Origin.Source.SessionID, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(expNodeJSON(t, f, n.ID, current.Version))
}

var expRoomActions = []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": expRoomCommand}}}

// FFR-017. One record holds every use of an Experience and, once a use settles, its feedback with
// the outcome observation; a node is at most 64 KiB (knowledge.MaxGraphRecordBytes, and the schema).
// An adoption was admitted whenever the record could hold the adoption itself. A record with three
// conditions and a detailed strategy then admitted uses whose feedback no longer fit: the settlement
// of a use that was already applied failed "node exceeds storage bound", and its checked outcome, a
// counterexample included, could never be recorded.
//
// Every use a record admits must be able to record its checked outcome. The room is decided at the
// admission: a consumer that is refused has applied nothing yet.
func TestExperienceAdmitsOnlyUsesWhoseFeedbackItsRecordCanHold(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n := expRoomValidated(t, f, "marker procedure with a detailed strategy", 7)
	validated := expRoomSize(t, f, n)
	uses := expRoomAdopt(t, f, n)
	adopted := expRoomSize(t, f, n)

	for i, u := range uses {
		last := i == len(uses)-1
		if last {
			// The last admitted consumer meets the counterexample: the faithful procedure removes the artifact.
			writeFile(t, f.root+"/build/block", "blocked")
		}
		result, err := f.engine.runAgentWith(ctx, u.consumer, completionApplyProvider(t, n, u.adoption, expRoomActions), AgentOptions{MaxSteps: 6, Policy: "full"})
		if err != nil || result.Status != "completed" {
			t.Fatalf("consumer %d of %d: %+v %v", i+1, len(uses), result, err)
		}
		if result.Cognition == nil || result.Cognition.Feedback != 1 || len(result.Cognition.Errors) != 0 {
			t.Fatalf("use %d of %d was admitted (record %d bytes after %d adoptions, bound %d) but its checked outcome could not be recorded: %+v",
				i+1, len(uses), adopted, len(uses), knowledge.MaxGraphRecordBytes, result.Cognition)
		}
		got, err := f.engine.ReadExperience(ctx, u.consumer, n.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Experience.Uses) != 1 || got.Experience.Uses[0].Feedback == nil || got.Experience.Uses[0].Feedback.Observation == nil {
			t.Fatalf("use %d: feedback = %+v", i+1, got.Experience.Uses)
		}
		fb := got.Experience.Uses[0].Feedback
		if !last && (fb.Outcome != "success" || !fb.Used) {
			t.Fatalf("use %d: feedback = %+v", i+1, fb)
		}
		if last && (fb.Outcome != "failure" || !fb.RevisionNeeded || got.Experience.State != knowledge.ExperienceNeedsRevision) {
			t.Fatalf("the counterexample of the last admitted use was lost: %+v state=%s", fb, got.Experience.State)
		}
	}
	t.Logf("record: %d bytes validated, %d after %d adoptions, %d settled, bound %d", validated, adopted, len(uses), expRoomSize(t, f, n), knowledge.MaxGraphRecordBytes)
}

// FFR-017, the host's side. A lifecycle revision is measured against the same 64 KiB: a record its
// uses had filled refused the revocation of a procedure the host found wrong ("invalid lifecycle
// revision") when the reason was long, and stayed adoptable by its identity's history.
//
// A record takes the host's lifecycle decision with any reason the API accepts, however many uses it
// admitted and settled.
func TestExperienceRecordFilledByItsUsesCanStillBeRevoked(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n := expRoomValidated(t, f, "marker procedure with a detailed strategy", 7)
	uses := expRoomAdopt(t, f, n)
	for i, u := range uses {
		// Whether every outcome could be recorded is the other test's question.
		result, err := f.engine.runAgentWith(ctx, u.consumer, completionApplyProvider(t, n, u.adoption, expRoomActions), AgentOptions{MaxSteps: 6, Policy: "full"})
		if err != nil || result.Status != "completed" {
			t.Fatalf("consumer %d of %d: %+v %v", i+1, len(uses), result, err)
		}
	}
	filled := expRoomSize(t, f, n)

	origin := n.Experience.Origin.Source.SessionID
	current, err := f.engine.ReadExperience(ctx, origin, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The widest reason the API accepts: each of its 512 bytes takes six in the stored JSON.
	revoked, err := f.engine.SetCognitiveLifecycle(ctx, origin, n.ID, current.Version, "revoke", strings.Repeat("<", 512))
	if err != nil {
		t.Fatalf("a record its %d uses filled (%d bytes, bound %d) refused the host's revocation: %v", len(uses), filled, knowledge.MaxGraphRecordBytes, err)
	}
	if revoked.Status != "invalid" || revoked.Times.InvalidatedAt.IsZero() {
		t.Fatalf("revocation = %s %+v", revoked.Status, revoked.Times)
	}
	consumer := expNewSession(t, f, "consumer after the revocation")
	if _, err := f.engine.AdoptExperience(ctx, consumer, n.ID, revoked.Version); err == nil {
		t.Fatal("a revoked record was adopted")
	}
	t.Logf("record: %d bytes after %d uses, %d revoked, bound %d", filled, len(uses), expRoomSize(t, f, n), knowledge.MaxGraphRecordBytes)
}
