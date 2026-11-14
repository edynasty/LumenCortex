package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func TestExperienceApplicationSettlementRequiresOrderedExactExecutions(t *testing.T) {
	for _, fault := range []string{"duplicate", "reverse", "other_run", "arguments", "outcome", "unknown", "observation", "valid"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			store := provStore(t)
			gapSession(t, store, "s", "idle", "")
			admit(t, store, mainAdmission("s", "r"))
			a := knowledge.Application{ID: "application", ExperienceID: "experience", AdoptionID: "adoption", SessionID: "s", RunID: "run1", State: "pending", At: time.Now().Add(-time.Minute).UTC(), Actions: []knowledge.AppliedAction{{Tool: "shell", ArgsDigest: sha("d")}, {Tool: "shell", ArgsDigest: sha("d")}}}
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`INSERT INTO cognition_applications(id,experience_id,adoption_id,session_id,run_id,state,created_at,json) VALUES(?,?,?,?,?,?,?,?)`, a.ID, a.ExperienceID, a.AdoptionID, a.SessionID, a.RunID, a.State, stamp(a.At), string(raw)); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"first", "second"} {
				admission := execAdmission("s", id, "r")
				if id == "second" && fault == "other_run" {
					admission.RunID = "another_run"
				}
				if id == "second" && fault == "arguments" {
					admission.ArgsDigest = sha("e")
				}
				if err := store.AdmitExecution(ctx, admission); err != nil {
					t.Fatal(err)
				}
				outcome := OutcomeComplete
				if id == "second" && fault == "unknown" {
					outcome = OutcomeUnknown
				}
				if _, err := store.SettleExecution(ctx, "s", ExecutionResult{ExecutionID: id, Outcome: outcome, Coverage: CoverageComplete}, nil); err != nil {
					t.Fatal(err)
				}
			}
			a.State = "completed"
			a.Actions[0].ExecutionID, a.Actions[1].ExecutionID = "first", "second"
			a.Actions[0].Outcome, a.Actions[1].Outcome = "completed", "completed"
			switch fault {
			case "duplicate":
				a.Actions[1].ExecutionID = "first"
			case "reverse":
				a.Actions[0].ExecutionID, a.Actions[1].ExecutionID = "second", "first"
			case "outcome":
				a.State, a.Actions[1].Outcome = "failed", "failed"
			case "observation":
				// An outcome that is not a qualified result of the running application.
				a.Outcome = &knowledge.ExperienceObservation{Evidence: knowledge.Evidence{SessionID: "s", ResultSeq: 1, Verification: "passed"}}
			}
			err = store.FinishExperienceApplication(ctx, a, "project")
			if fault == "valid" {
				if err != nil {
					t.Fatalf("exact sequence refused: %v", err)
				}
			} else {
				if !errors.Is(err, ErrNotQualified) {
					t.Fatalf("%s receipt accepted: %v", fault, err)
				}
				pending, err := store.ExperienceApplication(ctx, "s", "adoption")
				if err != nil || pending.State != "pending" {
					t.Fatalf("refusal rewrote admission: %+v %v", pending, err)
				}
				if fault == "unknown" {
					a.State, a.Actions[1].Outcome = "unknown", string(OutcomeUnknown)
					if err := store.FinishExperienceApplication(ctx, a, "project"); err != nil {
						t.Fatalf("unknown observation was not retained: %v", err)
					}
					if a.Executed() {
						t.Fatal("unknown sequence earned execution credit")
					}
				}
				if fault == "observation" {
					a.Outcome = nil
					if err := store.FinishExperienceApplication(ctx, a, "project"); err != nil {
						t.Fatalf("the executions were not settled without the refused observation: %v", err)
					}
					settled, err := store.ExperienceApplication(ctx, "s", "adoption")
					if err != nil || !settled.Executed() || settled.Outcome != nil {
						t.Fatalf("settled receipt = %+v %v", settled, err)
					}
				}
			}
		})
	}
}
