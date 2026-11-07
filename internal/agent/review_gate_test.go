package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

type resultReviewer struct {
	result verify.ReviewResult
	err    error
	calls  int
}

func (r *resultReviewer) Review(context.Context, verify.ReviewRequest) (verify.ReviewResult, error) {
	r.calls++
	return r.result, r.err
}

func TestInvalidReviewBlocksCompletionAndKeepsUsage(t *testing.T) {
	for _, test := range []struct {
		name     string
		findings []verify.ReviewFinding
		err      error
	}{
		{"contradictory pass", []verify.ReviewFinding{{Severity: verify.SeverityMajor, Issue: "wrong result"}}, nil},
		{"unknown severity", []verify.ReviewFinding{{Severity: "unrecognized", Issue: "wrong result"}}, nil},
		{"empty issue", []verify.ReviewFinding{{Severity: verify.SeverityMinor}}, nil},
		{"failed response with usage", nil, errors.New("invalid response")},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newRunningStore("review")
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{
				{Message: protocol.Message{Content: "done"}},
				{Message: protocol.Message{Content: "still done"}},
			}}
			reviewer := &resultReviewer{result: verify.ReviewResult{
				Passed: true, Source: verify.SourceIndependent, Model: "test/review", RequestID: "review-1",
				Findings: test.findings, Usage: protocol.Usage{TotalTokens: 47}, CostKnown: true, CostUSD: 0.125,
			}, err: test.err}
			loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Reviewer: reviewer}
			result, err := loop.Run(context.Background(), "review", Options{
				Session: testRef, MaxSteps: 5, Verification: VerificationOptions{Review: true, MaxRepairs: 1},
			})
			if !errors.Is(err, ErrSemanticUnverified) || result.Status != StatusInterrupted {
				t.Fatalf("invalid review accepted: result=%+v err=%v", result, err)
			}
			if reviewer.calls != 1 || result.Usage.TotalTokens != 47 || result.Spend.KnownCostUSD != 0.125 {
				t.Fatalf("rejected review accounting: calls=%d usage=%+v spend=%+v", reviewer.calls, result.Usage, result.Spend)
			}
			record := store.ts.Verification
			if record == nil || record.Review == nil || record.Review.Passed || record.Review.RequestID != "review-1" {
				t.Fatalf("failed review was lost: %+v", record)
			}
			if len(completionBlocked(store)) != 1 {
				t.Fatalf("missing bounded repair feedback: %+v", completionBlocked(store))
			}
		})
	}
}

func TestReviewFailureCanBeRepairedWithFreshHostEvidence(t *testing.T) {
	for _, test := range []struct {
		name, fingerprint string
		hostPass          bool
	}{
		{"unchanged", "before", true},
		{"unknown fingerprint", "", true},
		{"changed without evidence", "after", false},
		{"verified repair", "after", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newRunningStore("repair-review")
			fingerprint := "before"
			reviewer := &resultReviewer{result: verify.ReviewResult{Passed: false, Source: verify.SourceIndependent,
				Findings: []verify.ReviewFinding{{Severity: verify.SeverityMajor, Issue: "the result is wrong"}}}}
			loop := Loop{Store: store, Reviewer: reviewer, Evidence: func(context.Context) (taskstate.Evidence, error) {
				return taskstate.Evidence{Fingerprint: fingerprint}, nil
			}}
			r := &run{ts: store.ts, spend: &Spend{}}
			opts := Options{Verification: VerificationOptions{Review: true, MaxRepairs: 2}}
			feedback, cause := loop.verificationGate(context.Background(), "repair-review", r, opts, 1, "done", nil, nil)
			if feedback == "" || cause != nil {
				t.Fatalf("first failed review: %q %v", feedback, cause)
			}
			fingerprint = test.fingerprint
			if test.hostPass {
				loop.Verifier = &scriptedVerifier{results: [][]verify.Verdict{{{Level: verify.LevelSemantic, Source: verify.SourceIndependent, Status: verify.StatusPassed, Check: "real-host-check"}}}}
			}
			feedback, cause = loop.verificationGate(context.Background(), "repair-review", r, opts, 2, "repaired", nil, nil)
			wantPass := test.fingerprint == "after" && test.hostPass
			if (feedback == "" && cause == nil) != wantPass {
				t.Fatalf("feedback=%q cause=%v wantPass=%v", feedback, cause, wantPass)
			}
			if reviewer.calls != 1 || r.ts.Verification.Review == nil || r.ts.Verification.Review.Passed {
				t.Fatal("review retried or failed history erased")
			}
		})
	}
}

func TestUnadmittedReviewFailureDoesNotInventModelRequest(t *testing.T) {
	store := newRunningStore("unadmitted-review")
	loop := Loop{Store: store, Reviewer: &resultReviewer{err: errors.New("no configured reviewer")}}
	r := &run{ts: store.ts, spend: &Spend{}}
	feedback, cause := loop.verificationGate(context.Background(), "unadmitted-review", r,
		Options{Verification: VerificationOptions{Review: true, MaxRepairs: 1}}, 1, "done", nil, nil)
	if feedback == "" || cause != nil {
		t.Fatalf("failure did not block: %q %v", feedback, cause)
	}
	if r.spend.ModelRequests != 0 || r.spend.UnknownCostRequests != 0 || r.spend.KnownTokens != 0 {
		t.Fatalf("invented consumption: %+v", r.spend)
	}
}
