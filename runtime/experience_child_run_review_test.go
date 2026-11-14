package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

// FFR-015. A child run is offered the Experience tools of every run but the maintainer's, and what
// they record is durable: an adoption is a use of the shared record, an application a receipt, a
// proposal a pending row. Only a top-level run settled them when it ended. A child's stayed open
// until the next Engine open, whose recovery pass settles every session, so in a host that keeps
// running a procedure a child really applied was never checked: a success was not credited, and a
// counterexample left the refuted record validated for every later consumer. A lesson a child
// proposed stayed pending after the terminal run its tool result asks for.
func TestExperienceWorkOfAChildRunSettlesWhenTheRunEnds(t *testing.T) {
	for _, tt := range []struct {
		name            string
		refute          bool
		outcome, reason string
		state           string
		proposals       int
	}{
		{"an applied procedure is credited and a proposed lesson is finalized", false, "success", "expected_outcome_verified", knowledge.ExperienceValidated, 1},
		{"a counterexample stops the record", true, "failure", "outcome_counterexample", knowledge.ExperienceNeedsRevision, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newCognitionFixture(t, true)
			ctx := context.Background()
			expInstallChecks(t, f)
			marker := filepath.Join(f.root, "build/reused")
			writeFile(t, marker, "ready")
			origin := expMustRun(t, f, "inspect the configuration", &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if step == 0 {
					return cogCall("read_file", `{"path":"config.env"}`), nil
				}
				return cogFinal("configuration inspected"), nil
			}})
			proposal := expProposal("procedure a child applies")
			proposal.Strategy = []string{"Read config.env with read_file before reporting completion."}
			n := expValidate(t, f, origin, expExtract(t, f, origin, proposal))
			if n.Experience.State != knowledge.ExperienceValidated {
				t.Fatalf("fixture: %s %+v", n.Experience.State, n.Experience.Validation)
			}
			if tt.refute {
				// The outcome the origin observed no longer holds where the child applies the procedure.
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			}

			supervisor, err := NewRunSupervisor(f.engine)
			if err != nil {
				t.Fatal(err)
			}
			defer supervisor.Close()
			var mu sync.Mutex
			var child, applied string
			parentRequests := 0
			p := &cognitionProvider{respond: func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				mu.Lock()
				defer mu.Unlock()
				if hasTool(req, "spawn_subagent") {
					parentRequests++
					switch parentRequests {
					case 1:
						return toolCalls(tc("spawn", "spawn_subagent", `{"goal":"apply the procedure a child applies"}`)), nil
					case 2:
						var node SubagentNode
						if err := json.Unmarshal([]byte(lastToolResult(req, "spawn")), &node); err != nil {
							return protocol.ProviderResponse{}, err
						}
						child = node.SessionID
						return toolCalls(tc("wait", "wait_subagent", fmt.Sprintf(`{"child_session_id":%q,"timeout_seconds":30}`, child))), nil
					}
					return cogFinal("parent done"), nil
				}
				results := 0
				for _, m := range req.Messages {
					if m.Role == "tool" {
						results++
					}
				}
				switch results {
				case 0:
					return toolCalls(tc("adopt", "adopt_experience", fmt.Sprintf(`{"id":%q,"version":%d}`, n.ID, n.Version))), nil
				case 1:
					var adoption ExperienceAdoption
					if err := json.Unmarshal([]byte(lastToolResult(req, "adopt")), &adoption); err != nil {
						return protocol.ProviderResponse{}, err
					}
					raw, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": adoption.ID, "actions": []experienceAction{{Tool: "read_file", Arguments: map[string]any{"path": "config.env"}}}})
					return toolCalls(tc("apply", "apply_experience", string(raw))), nil
				case 2:
					applied = lastToolResult(req, "apply")
					if tt.proposals > 0 {
						lesson := expProposal("lesson a child proposes")
						lesson.Strategy = proposal.Strategy
						raw, _ := json.Marshal(map[string]any{"proposal": lesson})
						return toolCalls(tc("propose", "propose_experience", string(raw))), nil
					}
				}
				return cogFinal("child done"), nil
			}}
			parent := expNewSession(t, f, "parent that delegates the procedure")
			if _, err := supervisor.startWith(ctx, parent, p, AgentOptions{MaxSteps: 8, Policy: "workspace"}); err != nil {
				t.Fatal(err)
			}
			waitInactive(t, supervisor, parent)
			mu.Lock()
			consumer, receipt := child, applied
			mu.Unlock()
			if consumer == "" {
				t.Fatal("fixture: the parent spawned no child")
			}
			waitInactive(t, supervisor, consumer)
			if row, err := f.engine.store.Get(ctx, consumer); err != nil || row.Status != "completed" {
				t.Fatalf("fixture: child run %+v %v", row.Status, err)
			}
			var application struct {
				State string `json:"state"`
			}
			if err := json.Unmarshal([]byte(receipt), &application); err != nil || application.State != "completed" {
				t.Fatalf("fixture: the child did not apply the procedure: %s %v", receipt, err)
			}

			// The Engine keeps running: no restart, no recovery pass.
			got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Experience.Uses) != 1 {
				t.Fatalf("fixture: uses of the child %+v", got.Experience.Uses)
			}
			feedback := got.Experience.Uses[0].Feedback
			if feedback == nil {
				t.Errorf("the applied use of the ended child run is still open (record %s@%d %s)", got.ID, got.Version, got.Experience.State)
			} else if feedback.Outcome != tt.outcome || feedback.Reason != tt.reason || !feedback.Used || feedback.ApplicationID == "" {
				t.Errorf("feedback of the child = %+v, want %s/%s of its application", feedback, tt.outcome, tt.reason)
			}
			if got.Experience.State != tt.state {
				t.Errorf("record state after the child run = %s, want %s", got.Experience.State, tt.state)
			}
			if tt.refute {
				later := expNewSession(t, f, "a later consumer of the refuted procedure")
				if _, err := f.engine.AdoptExperience(ctx, later, got.ID, got.Version); err == nil {
					t.Errorf("a later consumer adopted the procedure the child refuted")
				}
			}

			pending, err := f.engine.store.PendingExperienceProposals(ctx, consumer)
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Errorf("%d proposal(s) of the ended child run are still pending", len(pending))
			}
			db := cognitiveOpenReadOnly(t, f.engine.store.Path())
			defer db.Close()
			var learned int
			if err := db.QueryRow(`SELECT count(DISTINCT id) FROM cognition_nodes WHERE kind='experience' AND json_extract(json,'$.experience.origin.source.sessionId')=?`, consumer).Scan(&learned); err != nil {
				t.Fatal(err)
			}
			if learned != tt.proposals {
				t.Errorf("records learned from the child episode = %d, want %d", learned, tt.proposals)
			}
		})
	}
}
