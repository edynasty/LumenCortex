package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

func runPreAdmissionWindowOverflow(t *testing.T, e *Engine, sessionID, input string) AgentResult {
	t.Helper()
	oversizedPrompt := strings.Repeat("window_overflow_padding ", 140000)
	res, _ := e.runAgentWith(context.Background(), sessionID, cogAnswer("unreachable"), AgentOptions{
		Input:        input,
		SystemPrompt: oversizedPrompt,
		MaxSteps:     4,
		Policy:       "workspace",
	})
	if res.Status != "failed" {
		t.Fatalf("fixture: expected pre-admission failed status, got %+v", res)
	}
	return res
}

// ICRA-002 (Origin & Proposal Misattribution). When a second run commits BeginRun (advancing
// SourceVersion) and then fails in Loop.Run before admitting a main request envelope (for example
// KindWindowOverflow), sourceRunSQL fell back to the previous run's RunID. ExperienceOrigin then
// spliced Run 2's failed status and digest onto Run 1's completed execution trace, and
// finalizeCognitiveRun extracted an interrupted Run 1 proposal instead of refusing it as stale.
func TestPreAdmissionRunFailureDoesNotSplicePreviousRunTraceOrFinalizeStaleProposal(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	expInstallChecks(t, f)

	origin := expNewSession(t, f, "origin whose second run overflows window")
	first, err := f.engine.runAgentWith(ctx, origin, &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("write_file", `{"path":"build/reused","content":"ready"}`), nil
		}
		return cogFinal("completed first run"), nil
	}}, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || first.Status != "completed" {
		t.Fatalf("first run: %+v %v", first, err)
	}
	beforeRef, err := f.engine.store.CognitiveSource(ctx, origin)
	if err != nil || beforeRef.RunID == "" {
		t.Fatalf("before source: %+v %v", beforeRef, err)
	}

	runPreAdmissionWindowOverflow(t, f.engine, origin, "second turn that overflows before main admission")

	afterOrigin, err := f.engine.store.ExperienceOrigin(ctx, origin)
	if err != nil {
		t.Fatalf("ExperienceOrigin after pre-admission failed run: %v", err)
	}
	if afterOrigin.Source.RunID == beforeRef.RunID {
		t.Errorf("pre-admission failed run reused previous RunID %s for new digest %s", afterOrigin.Source.RunID, afterOrigin.Source.Digest)
	}
	if len(afterOrigin.Trace.Steps) != 0 {
		t.Errorf("ExperienceOrigin spliced %d step(s) from Run 1 onto failed Run 2: %+v", len(afterOrigin.Trace.Steps), afterOrigin.Trace.Steps)
	}

	// Interrupted Run 1 queued a proposal; a pre-admission failed Run 2 must refuse it (p.RunID != source.RunID),
	// never extract and validate Run 1's trace over Run 2's failed session.
	proposer := expNewSession(t, f, "interrupted proposer followed by pre-admission failure")
	warning := expProposal("stale interrupted proposal")
	warning.ExpectedResult = "failed"
	rawProposal, _ := json.Marshal(map[string]any{"proposal": warning})
	interrupted, _ := f.engine.runAgentWith(ctx, proposer, &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			return cogCall("read_file", `{"path":"config.env"}`), nil
		}
		return cogCall("propose_experience", string(rawProposal)), nil
	}}, AgentOptions{MaxSteps: 2, Policy: "workspace"})
	if interrupted.Status != "interrupted" {
		t.Fatalf("fixture: expected interrupted proposer run, got %+v", interrupted)
	}

	second := runPreAdmissionWindowOverflow(t, f.engine, proposer, "second turn fails pre-admission")
	if second.Cognition != nil && second.Cognition.Extracted != 0 {
		t.Errorf("interrupted Run 1 proposal was extracted (%d) during pre-admission failed Run 2", second.Cognition.Extracted)
	}
	pending, err := f.engine.store.PendingExperienceProposals(ctx, proposer)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("stale proposal remained pending (%d) instead of being refused", len(pending))
	}
}

// ICRA-002 (Settlement & Recovery). Covers three consumer failure modes when a started run fails
// before main request admission:
//  1. Host adoption after R1 settles cleanly as unused unknown when R2 fails pre-admission (not stuck not-due).
//  2. First-run consumer with a host adoption settles cleanly without emitting cognition.recovery.failed on Engine reopen.
//  3. Interrupted R1 tool adoption + application settles as adoption_run_left_unsettled (Used=false), never Used=true or refuting the shared Experience.
func TestPreAdmissionRunFailureSettlesConsumerAdoptionsWithoutFalseAttributionOrRecoveryLoop(t *testing.T) {
	t.Run("host adoption after completed run settles when next run fails pre-admission", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		ctx := context.Background()
		n, _ := expValidated(t, f, "host adoption before pre-admission failure")
		consumer := expMustRun(t, f, "consumer baseline run", cogAnswer("baseline done"))
		adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
		if err != nil {
			t.Fatal(err)
		}

		res := runPreAdmissionWindowOverflow(t, f.engine, consumer, "turn that fails pre-admission")
		if res.Cognition == nil || res.Cognition.Feedback != 1 || len(res.Cognition.Errors) != 0 {
			t.Fatalf("host adoption did not settle on terminal pre-admission failure: %+v", res.Cognition)
		}
		got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if fb := got.Experience.Uses[0].Feedback; fb == nil || fb.Used || fb.Outcome != "unknown" {
			t.Fatalf("feedback = %+v, want settled unused unknown", fb)
		}
		_ = adoption
	})

	t.Run("first-run pre-admission failure settles without perpetual recovery failure", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		ctx := context.Background()
		n, _ := expValidated(t, f, "first run pre-admission failure")
		consumer := expNewSession(t, f, "fresh consumer never run before")
		if _, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version); err != nil {
			t.Fatal(err)
		}

		res := runPreAdmissionWindowOverflow(t, f.engine, consumer, "first turn fails pre-admission")
		if res.Cognition == nil || res.Cognition.Feedback != 1 || len(res.Cognition.Errors) != 0 {
			t.Fatalf("first-run adoption failed settlement: %+v", res.Cognition)
		}
		events, cancel := f.engine.Events(64)
		defer cancel()
		if err := f.engine.recoverCognitiveRuns(ctx); err != nil {
			t.Fatal(err)
		}
		for drained := false; !drained; {
			select {
			case ev := <-events:
				if ev.Type == "cognition.recovery.failed" {
					t.Fatalf("unexpected cognition.recovery.failed: %s", ev.Data)
				}
			default:
				drained = true
			}
		}
	})

	t.Run("interrupted tool application is not credited or refuted by next run pre-admission failure", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		ctx := context.Background()
		n, _ := expConditionalValidated(t, f, "interrupted counterexample before pre-admission failure")
		writeFile(t, f.root+"/build/block", "blocked")

		consumer := expNewSession(t, f, "consumer interrupted after apply_experience")
		const cmd = "if test -f build/block; then rm -f build/reused; else printf ready > build/reused; fi"
		var adoptionID string
		interrupted, _ := f.engine.runAgentWith(ctx, consumer, &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			if step == 0 {
				raw, _ := json.Marshal(map[string]any{"id": n.ID, "version": n.Version})
				return cogCall("adopt_experience", string(raw)), nil
			}
			var ad ExperienceAdoption
			if err := json.Unmarshal([]byte(lastToolResult(req, "call_1")), &ad); err != nil {
				return protocol.ProviderResponse{}, err
			}
			adoptionID = ad.ID
			raw, _ := json.Marshal(map[string]any{
				"id":         n.ID,
				"adoptionId": adoptionID,
				"actions":    []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": cmd}}},
			})
			return cogCall("apply_experience", string(raw)), nil
		}}, AgentOptions{MaxSteps: 2, Policy: "full"})
		if interrupted.Status != "interrupted" {
			t.Fatalf("fixture: expected interrupted R1, got %+v", interrupted)
		}

		res := runPreAdmissionWindowOverflow(t, f.engine, consumer, "R2 fails before main admission")
		if res.Cognition == nil || res.Cognition.Feedback != 1 || len(res.Cognition.Errors) != 0 {
			t.Fatalf("R2 settlement report = %+v", res.Cognition)
		}
		got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		fb := got.Experience.Uses[0].Feedback
		if fb == nil || fb.Used || fb.Outcome != "unknown" || fb.Reason != "adoption_run_left_unsettled" {
			t.Fatalf("interrupted R1 application misattributed during R2 pre-admission failure: %+v", fb)
		}
		if got.Experience.State != knowledge.ExperienceValidated {
			t.Fatalf("shared Experience state changed to %s from an unsettled interrupted run", got.Experience.State)
		}
	})
}
