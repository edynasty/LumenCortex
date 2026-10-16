package evaluator

import (
	"context"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func TestSatisfies(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	tests := []struct {
		name    string
		wu      *taskstate.WorkUnit
		want    bool
		wantErr bool
	}{
		{
			name:    "nil work unit",
			wu:      nil,
			want:    false,
			wantErr: true,
		},
		{
			name: "no required evidence, status complete",
			wu: &taskstate.WorkUnit{
				ID:        "wu1",
				Status:    taskstate.WorkCompleted,
				CreatedAt: now,
				UpdatedAt: now,
			},
			want: true,
		},
		{
			name: "no required evidence, status in_progress",
			wu: &taskstate.WorkUnit{
				ID:        "wu1",
				Status:    taskstate.WorkActive,
				CreatedAt: now,
				UpdatedAt: now,
			},
			want: false,
		},
		{
			name: "required evidence met",
			wu: &taskstate.WorkUnit{
				ID:               "wu1",
				Status:           taskstate.WorkCompleted,
				RequiredEvidence: []string{"tests pass"},
				Evidence: []taskstate.EvidenceRef{
					{Requirement: "tests pass", Summary: "all tests passed", Ref: "test-output.txt"},
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
			want: true,
		},
		{
			name: "required evidence missing",
			wu: &taskstate.WorkUnit{
				ID:               "wu1",
				Status:           taskstate.WorkCompleted,
				RequiredEvidence: []string{"tests pass"},
				Evidence:         []taskstate.EvidenceRef{},
				CreatedAt:        now,
				UpdatedAt:        now,
			},
			want: false,
		},
		{
			name: "verification failed",
			wu: &taskstate.WorkUnit{
				ID:               "wu1",
				Status:           taskstate.WorkCompleted,
				RequiredEvidence: []string{"tests pass"},
				Evidence: []taskstate.EvidenceRef{
					{Requirement: "tests pass", Summary: "tests executed", Ref: "test-output.txt"},
				},
				Verification: []string{"run test suite"},
				VerificationResults: []taskstate.VerificationResult{
					{Check: "run test suite", Status: "failed", Detail: "1 test failed"},
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
			want: false,
		},
		{
			name: "verification passed",
			wu: &taskstate.WorkUnit{
				ID:               "wu1",
				Status:           taskstate.WorkCompleted,
				RequiredEvidence: []string{"tests pass"},
				Evidence: []taskstate.EvidenceRef{
					{Requirement: "tests pass", Summary: "tests passed", Ref: "test-output.txt"},
				},
				Verification: []string{"run test suite"},
				VerificationResults: []taskstate.VerificationResult{
					{Check: "run test suite", Status: "passed", Detail: "all tests passed"},
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Satisfies(ctx, tt.wu)
			if (err != nil) != tt.wantErr {
				t.Errorf("Satisfies() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Satisfies() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckVerdictFor(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		requirement string
		evidence    []taskstate.EvidenceRef
		results     []taskstate.VerificationResult
		want        CheckVerdict
		wantErr     bool
	}{
		{
			name:        "empty requirement",
			requirement: "",
			want:        VerdictSkipped,
		},
		{
			name:        "no evidence",
			requirement: "test passes",
			evidence:    []taskstate.EvidenceRef{},
			want:        VerdictPending,
		},
		{
			name:        "evidence, no verification result",
			requirement: "test passes",
			evidence: []taskstate.EvidenceRef{
				{Requirement: "test passes", Summary: "ran tests"},
			},
			want: VerdictPass,
		},
		{
			name:        "verification passed",
			requirement: "test passes",
			evidence: []taskstate.EvidenceRef{
				{Requirement: "test passes", Summary: "ran tests"},
			},
			results: []taskstate.VerificationResult{
				{Check: "test passes", Status: "passed"},
			},
			want: VerdictPass,
		},
		{
			name:        "verification failed",
			requirement: "test passes",
			evidence: []taskstate.EvidenceRef{
				{Requirement: "test passes", Summary: "ran tests"},
			},
			results: []taskstate.VerificationResult{
				{Check: "test passes", Status: "failed"},
			},
			want: VerdictFail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckVerdictFor(ctx, tt.requirement, tt.evidence, tt.results)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckVerdictFor() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("CheckVerdictFor() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvaluateWorkUnit(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	tests := []struct {
		name         string
		wu           *taskstate.WorkUnit
		wantComplete bool
		wantChecks   int
		wantErr      bool
	}{
		{
			name:    "nil work unit",
			wu:      nil,
			wantErr: true,
		},
		{
			name: "no required evidence, complete",
			wu: &taskstate.WorkUnit{
				ID:        "wu1",
				Status:    taskstate.WorkCompleted,
				CreatedAt: now,
				UpdatedAt: now,
			},
			wantComplete: true,
			wantChecks:   0,
		},
		{
			name: "all checks pass",
			wu: &taskstate.WorkUnit{
				ID:               "wu1",
				Status:           taskstate.WorkCompleted,
				RequiredEvidence: []string{"test 1", "test 2"},
				Evidence: []taskstate.EvidenceRef{
					{Requirement: "test 1", Summary: "test 1 output"},
					{Requirement: "test 2", Summary: "test 2 output"},
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
			wantComplete: true,
			wantChecks:   2,
		},
		{
			name: "one check pending",
			wu: &taskstate.WorkUnit{
				ID:               "wu1",
				Status:           taskstate.WorkCompleted,
				RequiredEvidence: []string{"test 1", "test 2"},
				Evidence: []taskstate.EvidenceRef{
					{Requirement: "test 1", Summary: "test 1 output"},
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
			wantComplete: false,
			wantChecks:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvaluateWorkUnit(ctx, tt.wu)
			if (err != nil) != tt.wantErr {
				t.Errorf("EvaluateWorkUnit() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if got.Complete != tt.wantComplete {
				t.Errorf("EvaluateWorkUnit() Complete = %v, want %v", got.Complete, tt.wantComplete)
			}
			if len(got.Checks) != tt.wantChecks {
				t.Errorf("EvaluateWorkUnit() checks count = %v, want %v", len(got.Checks), tt.wantChecks)
			}
		})
	}
}
