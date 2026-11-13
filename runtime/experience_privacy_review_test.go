package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// A shared definition is not a grant to inspect another consumer's runs, checks or feedback.
// Exercise both public read APIs and mutation responses; filtering only ReadExperience is insufficient.
func TestExperienceFeedbackDetailsArePrivateToConsumer(t *testing.T) {
	ctx := context.Background()
	engine, root := cognitiveOpenUpgradeProject(t)
	f := &cognitionFixture{engine: engine, root: root}
	t.Cleanup(func() { _ = f.engine.Close() })
	n, origin := expValidated(t, f, "private consumer feedback")
	first := expNewSession(t, f, "first private consumer")
	second := expNewSession(t, f, "second private consumer")
	unrelated := expNewSession(t, f, "definition-only reader")
	adoptFirst, err := f.engine.AdoptExperience(ctx, first, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	adoptSecond, err := f.engine.AdoptExperience(ctx, second, n.ID, adoptFirst.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate := func(label, reader string, got CognitiveNode, wantUse string) {
		t.Helper()
		if got.Experience == nil {
			t.Fatalf("%s: missing shared definition", label)
		}
		x := got.Experience
		if x.Origin.Source.SessionID != origin || x.Definition.ProblemPattern != n.Experience.Definition.ProblemPattern {
			t.Fatalf("%s: lost the shared origin or definition", label)
		}
		wantCount := 0
		if wantUse != "" {
			wantCount = 1
		}
		if len(x.Uses) != wantCount {
			t.Errorf("%s: returned %d uses to %q, want %d private uses", label, len(x.Uses), reader, wantCount)
		}
		for _, use := range x.Uses {
			if use.ConsumerID != reader || use.ID != wantUse {
				t.Errorf("%s: exposed another consumer's adoption %s", label, use.ID)
			}
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range []string{first, second} {
			if other != reader && strings.Contains(string(raw), other) {
				t.Errorf("%s: response contains private identity %s", label, other)
			}
		}
	}

	expApply(t, f, second, n, adoptSecond.ID)
	secondFeedback, err := f.engine.RecordExperienceFeedback(ctx, second, n.ID, adoptSecond.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate("second feedback return", second, secondFeedback, adoptSecond.ID)
	expApply(t, f, first, n, adoptFirst.ID)
	firstFeedback, err := f.engine.RecordExperienceFeedback(ctx, first, n.ID, adoptFirst.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate("first feedback return", first, firstFeedback, adoptFirst.ID)
	stored := expNodeJSON(t, f, n.ID, firstFeedback.Version)
	complete, err := knowledge.DecodeNode(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete.Experience.Uses) != 2 || complete.Experience.Statistics().Successes != 2 {
		t.Fatalf("private feedback isolation lost durable history: %+v", complete.Experience.Statistics())
	}

	checkReads := func() {
		t.Helper()
		for name, read := range map[string]func(context.Context, string, string, int64) (CognitiveNode, error){
			"experience": f.engine.ReadExperience,
			"graph node": f.engine.ReadCognitiveNode,
		} {
			for _, reader := range []string{first, second, origin, unrelated, ""} {
				for _, version := range []int64{0, secondFeedback.Version} {
					got, err := read(ctx, reader, n.ID, version)
					if err != nil {
						t.Fatalf("%s reader %s version %d: %v", name, reader, version, err)
					}
					want := ""
					if reader == first {
						want = adoptFirst.ID
					} else if reader == second {
						want = adoptSecond.ID
					}
					assertPrivate(name, reader, got, want)
					if want != "" && len(got.Experience.Uses) == 1 {
						use := got.Experience.Uses[0]
						if reader == first && version == secondFeedback.Version {
							if use.Feedback != nil {
								t.Error("historical read acquired later feedback")
							}
						} else if use.Feedback == nil || use.Feedback.Outcome != "success" || use.Feedback.Observation == nil || use.Feedback.Observation.Evidence.SessionID != reader {
							t.Errorf("own qualified feedback was lost: %+v", use.Feedback)
						}
					}
				}
			}
		}
	}
	checkReads()
	if got := expNodeJSON(t, f, n.ID, firstFeedback.Version); got != stored {
		t.Fatal("public reads rewrote the immutable private record")
	}
	for _, item := range []struct{ reader, adoption string }{{first, adoptFirst.ID}, {second, adoptSecond.ID}} {
		repeated, err := f.engine.RecordExperienceFeedback(ctx, item.reader, n.ID, item.adoption)
		if err != nil || repeated.Version != firstFeedback.Version {
			t.Fatalf("repeat feedback changed history: version=%d err=%v", repeated.Version, err)
		}
		assertPrivate("idempotent feedback return", item.reader, repeated, item.adoption)
	}
	assertPrivate("duplicate extraction", origin, expExtract(t, f, origin, n.Experience.Definition), "")

	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	f.engine, err = Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	checkReads()
	validated, err := f.engine.ValidateExperience(ctx, origin, n.ID, firstFeedback.Version)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate("validation return", origin, validated, "")
	proposal := n.Experience.Definition
	proposal.Strategy = []string{"Confirm the service port, then write build/reused with write_file."}
	revised, err := f.engine.ReviseExperience(ctx, origin, n.ID, validated.Version, proposal)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivate("definition revision return", origin, revised, "")
	after, err := knowledge.DecodeNode(expNodeJSON(t, f, n.ID, revised.Version))
	if err != nil || !reflect.DeepEqual(after.Experience.Uses, complete.Experience.Uses) {
		t.Fatalf("origin revision erased consumers' immutable feedback: %v", err)
	}
}

// A real read_thread tool used on one's own thread must not reintroduce a lineage edge to the
// aggregate record owner. That owner contains other consumers' private observations.
func TestExperienceOwnThreadReadDoesNotSubscribeToCoConsumers(t *testing.T) {
	for _, op := range []string{"revoke", "delete"} {
		t.Run(op, func(t *testing.T) {
			ctx := context.Background()
			f := newCognitionFixture(t, false)
			n, origin := expValidated(t, f, "consumer context provenance")
			kept := expNewSession(t, f, "kept consumer")
			withdrawn := expNewSession(t, f, "withdrawn co-consumer")
			adopted, err := f.engine.AdoptExperience(ctx, kept, n.ID, n.Version)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine.AdoptExperience(ctx, withdrawn, n.ID, adopted.Version); err != nil {
				t.Fatal(err)
			}
			reader := &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if step == 0 {
					args, _ := json.Marshal(map[string]string{"id": kept})
					return cogCall("read_thread", string(args)), nil
				}
				return cogFinal("read own thread"), nil
			}}
			expMustRunSession(t, f, kept, reader)
			if op == "delete" {
				if _, err := f.engine.DeleteSession(ctx, withdrawn, WorktreeCleanup{}); err != nil {
					t.Fatal(err)
				}
			} else {
				report, err := f.engine.RevokeSource(ctx, SourceSession, withdrawn, "privacy_review")
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(report.Sessions, kept) {
					t.Error("read_thread made an unexposed co-consumer part of the exposure report")
				}
			}
			if fenced, err := f.engine.store.SessionWithdrawn(ctx, kept); err != nil || fenced {
				t.Errorf("own thread read caused collateral withdrawal: fenced=%v err=%v", fenced, err)
			}
			if _, err := f.engine.GetContextGraph(ctx, kept); err != nil {
				t.Errorf("unexposed consumer lost its context: %v", err)
			}
			// Do not bypass origin revocation to preserve the consumer.
			if _, err := f.engine.RevokeSource(ctx, SourceSession, origin, "privacy_review"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine.GetContextGraph(ctx, kept); !errors.Is(err, session.ErrFenced) {
				t.Errorf("origin withdrawal did not fence its consumer: %v", err)
			}
		})
	}
}

// A reader that actually receives a counterexample warning differs from a co-consumer that only
// received the origin strategy. Keep that real derivation without revealing the reporter's identity.
func TestExperienceCounterexampleWarningRetainsWithdrawal(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, false)
	n, _ := expConditionalValidated(t, f, "counterexample warning provenance")
	readerID := expNewSession(t, f, "warning reader")
	reporter := expNewSession(t, f, "private counterexample reporter")
	first, err := f.engine.AdoptExperience(ctx, readerID, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.engine.AdoptExperience(ctx, reporter, n.ID, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	result, err := expApplyCounterexample(t, f, reporter, n, second.ID)
	if err != nil || result.Status != "completed" {
		t.Fatalf("counterexample run: %+v %v", result, err)
	}
	fed, err := f.engine.RecordExperienceFeedback(ctx, reporter, n.ID, second.ID)
	if err != nil || fed.Experience == nil || fed.Experience.State != knowledge.ExperienceNeedsRevision {
		t.Fatalf("counterexample feedback: %+v %v", fed.Experience, err)
	}
	view, err := f.engine.GetContextGraph(ctx, readerID)
	if err != nil {
		t.Fatal(err)
	}
	locator, ok := expContextNode(view, "experience:"+n.ID)
	if !ok || locator.Status != knowledge.ExperienceNeedsRevision || locator.Validation != "contested" {
		t.Fatalf("warning missing from context: %+v", locator)
	}
	raw, err := json.Marshal(view)
	if err != nil || strings.Contains(string(raw), reporter) {
		t.Fatalf("warning exposed its private reporter: %v", err)
	}
	p := &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			args, _ := json.Marshal(map[string]string{"id": readerID})
			return cogCall("read_thread", string(args)), nil
		}
		return cogFinal("read counterexample warning"), nil
	}}
	expMustRunSession(t, f, readerID, p)
	rev, err := f.engine.RevokeSource(ctx, SourceSession, reporter, "privacy_review")
	if err != nil || !slices.Contains(rev.Sessions, readerID) {
		t.Fatalf("consumed warning lost its withdrawal lineage: %+v %v", rev.Sessions, err)
	}
	if _, err := f.engine.GetContextGraph(ctx, readerID); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("warning reader remained admitted after source withdrawal: %v", err)
	}
}
