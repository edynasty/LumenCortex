package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func TestCognitionCommitRefusesRevokedSupportAtomically(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	finish(t, s, "a", "completed")
	lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	assertion := knowledge.Assertion{Kind: knowledge.AssertPortValue, Target: "service", Value: "8080"}
	epoch, err := s.ApproveCheckSet(ctx, CheckSetApproval{ProjectID: "project", SetDigest: "set", Specs: []ApprovedSpec{{CheckID: "port", SpecDigest: "sha256:spec"}}, ApprovedBy: ApprovedByUser, Actor: "host"})
	if err != nil {
		t.Fatal(err)
	}
	binding := BindingApproval{ProjectID: "project", ConstraintRef: assertion.Ref(), ConstraintDigest: assertion.ValueDigest(), CheckID: "port", SpecDigest: "sha256:spec", BindingDigest: "sha256:binding", ApprovedBy: ApprovedByUser, Actor: "host"}
	if _, err := s.ApproveConstraintBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	putEvalRow(t, s, "m", 1, "port", "passed", "closed", epoch, "f1", "f1")
	c := candidate("kc_supported", "8080")
	c.Assertion, c.BindingDigest, c.CheckIDs, c.Status = &assertion, binding.BindingDigest, []string{"port"}, knowledge.StatusQualified
	commit := commitFor(lease, 1, "m", c)
	commit.Observations = []ObservationLink{{CandidateID: c.ID, SessionID: "m", ResultSeq: 1, Purpose: ObservationAdoption, Scope: "/project"}}
	if _, err := s.RevokeConstraintBinding(ctx, "project", binding.BindingDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitMaintenance(ctx, commit, t0); !errors.Is(err, ErrNotQualified) {
		t.Fatalf("revoked binding committed qualified support: %v", err)
	}
	if st := state(t, s); st.Cursor != 0 || st.Dirty != 1 || st.LeaseID != lease.LeaseID {
		t.Fatalf("refused support advanced state: %+v", st)
	}
	if _, err := s.Candidate(ctx, c.ID); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("refused support left candidate: %v", err)
	}
	if rows, err := s.Observations(ctx, c.ID); err != nil || len(rows) != 0 {
		t.Fatalf("refused support left observation: %+v %v", rows, err)
	}
	if _, err := s.ApproveConstraintBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if result, err := s.CommitMaintenance(ctx, commit, t0); err != nil || len(result.Inserted) != 1 {
		t.Fatalf("reapproved support retry: %+v %v", result, err)
	}
}
