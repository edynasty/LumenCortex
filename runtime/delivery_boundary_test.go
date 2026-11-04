package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// Inject an edit at the committed pending boundary, after VerifyDelivery's first
// read. A failed final write must preserve that edit for the next verification.
func TestDeliveryHostEditAtRetryBoundarySurvivesFailedSave(t *testing.T) {
	engine, id := notebookEngine(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(engine.workspace, "result"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	r, err := engine.runAgentWith(ctx, id, p, AgentOptions{Workflow: json.RawMessage(finishableWorkflow), Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result"}}}})
	if err != nil || r.Delivery.Status != DeliveryVerified {
		t.Fatalf("initial %+v %v", r, err)
	}
	faultSQL(t, engine, `CREATE TRIGGER queue_drop AFTER INSERT ON task_states
WHEN NEW.reason='delivery' AND json_extract(NEW.json,'$.deliveryResult.status')='pending'
BEGIN INSERT INTO task_state_inbox(session_id,created_at,json) VALUES(NEW.session_id,'2026-11-04T00:00:00Z','{"resolve":{"state":"dropped","actor":"owner"}}'); END`,
		`CREATE TRIGGER fail_settlement BEFORE INSERT ON task_states
WHEN NEW.reason='delivery' AND json_extract(NEW.json,'$.deliveryResult.status')!='pending'
BEGIN SELECT RAISE(ABORT,'boundary save fault'); END`)
	got, err := engine.VerifyDelivery(ctx, id)
	if err != nil || got.Status != DeliveryUnavailable || !hasIssue(got, "persistence") {
		t.Fatalf("fault %+v %v", got, err)
	}
	info, err := engine.TaskState(ctx, id)
	if err != nil || info.PendingEdits != 1 || info.State.DeliveryResult.Status != DeliveryPending || info.State.Resolution.State != taskstate.ResolutionDropped {
		t.Fatalf("queued decision lost: %+v %v", info, err)
	}
	faultSQL(t, engine, `DROP TRIGGER queue_drop`, `DROP TRIGGER fail_settlement`)
	got, err = engine.VerifyDelivery(ctx, id)
	if err != nil || got.Status != DeliveryVerified {
		t.Fatalf("retry %+v %v", got, err)
	}
	info, err = engine.TaskState(ctx, id)
	if err != nil || info.PendingEdits != 0 || info.State.Resolution.By != taskstate.ByUser || threadResolution(t, engine, id) != taskstate.ResolutionDropped {
		t.Fatalf("host decision overridden: %+v %v", info, err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("agent replayed: %d", len(p.requests))
	}
}

func TestDeliveryRetryStateReadFailureRetainsPending(t *testing.T) {
	engine, id := notebookEngine(t)
	if err := os.WriteFile(filepath.Join(engine.workspace, "result"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	initial, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result"}}}})
	if err != nil || initial.Delivery.Status != DeliveryVerified {
		t.Fatalf("initial %+v %v", initial, err)
	}
	// Simulate loss immediately after the durable retry-start transaction: the
	// load failure cannot restore previous verified, and recovery is verification-only.
	faultSQL(t, engine, `CREATE TRIGGER corrupt_retry AFTER INSERT ON task_states
WHEN NEW.reason='delivery' AND json_extract(NEW.json,'$.deliveryResult.status')='pending'
BEGIN INSERT INTO task_states(session_id,version,created_at,reason,json) VALUES(NEW.session_id,NEW.version+1,'x','fault','[]'); END`)
	result, err := engine.VerifyDelivery(context.Background(), id)
	if err != nil || result.Status != DeliveryUnavailable || !hasIssue(result, "state") {
		t.Fatalf("load fault %+v %v", result, err)
	}
	faultSQL(t, engine, `DROP TRIGGER corrupt_retry`, `DELETE FROM task_states WHERE reason='fault'`)
	info, err := engine.TaskState(context.Background(), id)
	if err != nil || info.State.DeliveryResult.Status != DeliveryPending || !slices.Contains(info.Obligations, "delivery") {
		t.Fatalf("restored stale success %+v %v", info, err)
	}
	result, err = engine.VerifyDelivery(context.Background(), id)
	if err != nil || result.Status != DeliveryVerified || result.RunID != initial.Delivery.RunID || len(p.requests) != 1 {
		t.Fatalf("recovery %+v %v requests=%d", result, err, len(p.requests))
	}
}

// Contract presence and pending RunID survive each exit independently of the
// execution error. All provider responses are local fixtures.
func TestDeliveryPendingAcrossExecutionExitKinds(t *testing.T) {
	for _, kind := range []string{"cancel", "deadline", "steps", "retry", "panic"} {
		t.Run(kind, func(t *testing.T) {
			engine, id := notebookEngine(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := AgentOptions{Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result"}}}}
			p := &deliveryStepProvider{next: func(ctx context.Context, _ int) (protocol.ProviderResponse, error) {
				switch kind {
				case "cancel":
					cancel()
					return protocol.ProviderResponse{}, ctx.Err()
				case "deadline":
					<-ctx.Done()
					return protocol.ProviderResponse{}, ctx.Err()
				case "steps":
					return toolCallResponse("read", "list_dir", `{"path":"."}`), nil
				case "retry":
					return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, Err: errors.New("offline fixture")}
				default:
					panic("fixture panic")
				}
			}}
			if kind == "deadline" {
				deadline := time.Now().Add(300 * time.Millisecond)
				opts.Deadline = &deadline
			}
			if kind == "steps" {
				opts.MaxSteps = 1
			}
			if kind == "retry" {
				opts.Retry = &config.Retry{MaxRetries: 1, BaseDelayMs: 1, MaxDelayMs: 1}
			}
			result, err := engine.runAgentWith(ctx, id, p, opts)
			if err == nil || result.Status == "completed" || result.Delivery == nil || result.Delivery.Status != DeliveryPending || result.Delivery.RunID == "" {
				t.Fatalf("exit lost pending: %+v %v", result, err)
			}
			info, err := engine.TaskState(context.Background(), id)
			if err != nil || info.State.DeliveryResult.Status != DeliveryPending || info.State.DeliveryResult.RunID != result.Delivery.RunID {
				t.Fatalf("stored mismatch: %+v %v", info, err)
			}
			if _, err := engine.VerifyDelivery(context.Background(), id); err == nil {
				t.Fatal("noncompleted execution was verified")
			}
		})
	}
}
