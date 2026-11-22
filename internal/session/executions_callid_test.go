package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

func TestExecutionAdmissionCallIDRoundTripAndConstraints(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	if err := store.Create(ctx, Session{ID: "s-callid", Goal: "callid roundtrip", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginRun(ctx, "s-callid", RunStart{
		RunID: "run-1",
		Input: &protocol.Message{Role: "user", Content: "go"},
	}); err != nil {
		t.Fatal(err)
	}
	f0, l0 := int64(0), int64(0)
	if _, err := store.AdmitRequest(ctx, RequestAdmission{
		RequestID: "req-1", SessionID: "s-callid", RunID: "run-1", Purpose: PurposeMain,
		Step: 1, FirstSeq: &f0, LastSeq: &l0, ProjectionVersion: 2, LineageComplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	aSeq, err := store.AppendAnswered(ctx, "s-callid", "req-1", protocol.Message{
		Role: "assistant",
		ToolCalls: []protocol.ToolCall{
			{ID: "call/with:special%chars 1", Name: "write_file"},
			{ID: "parent/eval:2", Name: "eval"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	// Reject ParentToolCallID without ToolCallID.
	if err := store.AdmitExecution(ctx, ExecutionAdmission{
		SessionID: "s-callid", RunID: "run-1", ExecutionID: "ex-bad", RequestID: "req-1",
		ParentToolCallID: "parent-only", ToolName: "write_file", ArgsDigest: digest, PreUnavailable: true,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid when ParentToolCallID set without ToolCallID, got %v", err)
	}

	// 1. Top-level call with special characters (/, :, %, space).
	if err := store.AdmitExecution(ctx, ExecutionAdmission{
		SessionID: "s-callid", RunID: "run-1", ExecutionID: "ex-top", RequestID: "req-1",
		ToolCallID: "call/with:special%chars 1", ToolName: "write_file", ArgsDigest: digest, PreUnavailable: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 2. Nested callback execution preserving both nested ToolCallID and ParentToolCallID.
	if err := store.AdmitExecution(ctx, ExecutionAdmission{
		SessionID: "s-callid", RunID: "run-1", ExecutionID: "ex-nested", RequestID: "req-1",
		ToolCallID: "parent/eval:2:cb1", ParentToolCallID: "parent/eval:2",
		ToolName: "write_file", ArgsDigest: digest, PreUnavailable: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 3. Legacy execution with empty ToolCallID -> NULL derived_from.
	if err := store.AdmitExecution(ctx, ExecutionAdmission{
		SessionID: "s-callid", RunID: "run-1", ExecutionID: "ex-legacy", RequestID: "req-1",
		ToolName: "bash", ArgsDigest: digest, PreUnavailable: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 4. Overlong (>512B) ToolCallID -> stored safely via overflow marker and decoded as unknown ("","").
	if err := store.AdmitExecution(ctx, ExecutionAdmission{
		SessionID: "s-callid", RunID: "run-1", ExecutionID: "ex-overlong", RequestID: "req-1",
		ToolCallID: strings.Repeat("x", 600), ToolName: "bash", ArgsDigest: digest, PreUnavailable: true,
	}); err != nil {
		t.Fatal(err)
	}

	recTop, err := store.Execution(ctx, "ex-top")
	if err != nil || recTop.ToolCallID != "call/with:special%chars 1" || recTop.ParentToolCallID != "" {
		t.Fatalf("recTop = %+v, err = %v", recTop, err)
	}
	recNested, err := store.Execution(ctx, "ex-nested")
	if err != nil || recNested.ToolCallID != "parent/eval:2:cb1" || recNested.ParentToolCallID != "parent/eval:2" {
		t.Fatalf("recNested = %+v, err = %v", recNested, err)
	}
	recLegacy, err := store.Execution(ctx, "ex-legacy")
	if err != nil || recLegacy.ToolCallID != "" || recLegacy.ParentToolCallID != "" {
		t.Fatalf("recLegacy = %+v, err = %v", recLegacy, err)
	}
	recOver, err := store.Execution(ctx, "ex-overlong")
	if err != nil || recOver.ToolCallID != "" || recOver.ParentToolCallID != "" {
		t.Fatalf("recOver = %+v, err = %v", recOver, err)
	}

	sums, err := store.ListAnsweredExecutions(ctx, "s-callid", aSeq)
	if err != nil || len(sums) != 4 {
		t.Fatalf("sums = %+v, err = %v", sums, err)
	}
	if sums[0].ToolCallID != "call/with:special%chars 1" || sums[0].ParentToolCallID != "" ||
		sums[1].ToolCallID != "parent/eval:2:cb1" || sums[1].ParentToolCallID != "parent/eval:2" ||
		sums[2].ToolCallID != "" || sums[2].ParentToolCallID != "" ||
		sums[3].ToolCallID != "" || sums[3].ParentToolCallID != "" {
		t.Fatalf("unexpected ListAnsweredExecutions call bindings: %+v", sums)
	}
}
