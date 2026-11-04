package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// finishableWorkflow can finish from its first step, so a verified delivery is the last obligation and
// the evidence rule resolves the thread done.
const finishableWorkflow = `{"id":"wf","facts":{"ready":true},"actions":{"work":{"terminal":true,"completeWhen":{"fact":"ready","equals":true},"allowedTools":["read_file"]}}}`

// faultSQL runs statements on the engine's database through a separate connection, as an outside fault.
func faultSQL(t *testing.T, engine *Engine, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func threadResolution(t *testing.T, engine *Engine, id string) string {
	t.Helper()
	view, err := engine.ReadThread(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return view.Resolution
}

func hasIssue(r *DeliveryResult, phase string) bool {
	return r != nil && slices.ContainsFunc(r.Issues, func(is DeliveryIssue) bool { return is.Phase == phase })
}

// A retry whose outcome cannot be saved leaves pending in the store, not the earlier verified: the
// obligation stays, the evidence resolution is withdrawn, a restart reads pending and a later retry
// records the real outcome. Nothing reruns the Agent.
func TestDeliveryRetrySaveFailureSupersedesVerified(t *testing.T) {
	engine, id := notebookEngine(t)
	ctx := context.Background()
	file := filepath.Join(engine.workspace, "result.txt")
	if err := os.WriteFile(file, []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	r, err := engine.runAgentWith(ctx, id, p, AgentOptions{Workflow: json.RawMessage(finishableWorkflow), Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}})
	if err != nil || r.Status != "completed" || r.Delivery.Status != DeliveryVerified {
		t.Fatalf("first run: %+v delivery=%+v %v", r, r.Delivery, err)
	}
	if got := threadResolution(t, engine, id); got != taskstate.ResolutionDone {
		t.Fatalf("verified delivery did not resolve: %q", got)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}

	// The start of a retry cannot be recorded: the attempt does not run and says so.
	faultSQL(t, engine, `CREATE TRIGGER reject_delivery BEFORE INSERT ON task_states WHEN NEW.reason='delivery'
BEGIN SELECT RAISE(ABORT,'forced delivery write failure'); END`)
	if got, err := engine.VerifyDelivery(ctx, id); err == nil || got != nil {
		t.Fatalf("unrecorded retry ran: %+v %v", got, err)
	}

	// The start is recorded, the outcome is not.
	faultSQL(t, engine, `DROP TRIGGER reject_delivery`, `CREATE TRIGGER reject_settled BEFORE INSERT ON task_states
WHEN NEW.reason='delivery' AND json_extract(NEW.json,'$.deliveryResult.status')!='pending'
BEGIN SELECT RAISE(ABORT,'forced settled write failure'); END`)
	got, err := engine.VerifyDelivery(ctx, id)
	if err != nil || got.Status != DeliveryUnavailable || !hasIssue(got, "persistence") || !slices.Contains(got.Missing, "regular file required: result.txt") {
		t.Fatalf("unsaved retry: %+v %v", got, err)
	}
	info, err := engine.TaskState(ctx, id)
	if err != nil || info.State.DeliveryResult.Status != DeliveryPending || info.State.Resolution.State != taskstate.ResolutionOpen {
		t.Fatalf("stale verification visible: %+v %v", info.State.DeliveryResult, err)
	}
	if !slices.Contains(info.Obligations, "delivery") {
		t.Fatalf("delivery obligation discharged by a stale result: %v", info.Obligations)
	}
	if got := threadResolution(t, engine, id); got == taskstate.ResolutionDone {
		t.Fatalf("read_thread still done after an unsaved retry")
	}

	workspace := engine.workspace
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if info, err := reopened.TaskState(ctx, id); err != nil || info.State.DeliveryResult.Status != DeliveryPending {
		t.Fatalf("restart restored a stale result: %+v %v", info.State.DeliveryResult, err)
	}
	faultSQL(t, reopened, `DROP TRIGGER reject_settled`)
	got, err = reopened.VerifyDelivery(ctx, id)
	if err != nil || got.Status != DeliveryFailed {
		t.Fatalf("recovered retry: %+v %v", got, err)
	}
	if info, err := reopened.TaskState(ctx, id); err != nil || info.State.DeliveryResult.Status != DeliveryFailed || info.State.DeliveryResult.RunID != r.Delivery.RunID {
		t.Fatalf("recovered record: %+v %v", info.State.DeliveryResult, err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("verification reran the Agent: %d requests", len(p.requests))
	}
}

// A host resolution queued before a settlement is applied and consumed by it: the evidence rule sees
// it and never replaces it, so the stored state, read_thread and the host view agree. One queued after
// the settlement is applied by the next one, again without being overridden.
func TestDeliveryQueuedHostResolutionWinsOverAutoDone(t *testing.T) {
	for _, tc := range []struct{ queued, want, by string }{
		{taskstate.ResolutionDropped, taskstate.ResolutionDropped, taskstate.ByUser},
		{taskstate.ResolutionDone, taskstate.ResolutionDone, taskstate.ByUser},
	} {
		t.Run(tc.queued, func(t *testing.T) {
			engine, id := notebookEngine(t)
			ctx := context.Background()
			p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
			r, err := engine.runAgentWith(ctx, id, p, AgentOptions{Workflow: json.RawMessage(finishableWorkflow), Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}})
			if err != nil || r.Status != "completed" || r.Delivery.Status != DeliveryFailed {
				t.Fatalf("run: %+v delivery=%+v %v", r, r.Delivery, err)
			}
			if _, err := engine.ResolveThread(ctx, id, tc.queued, "owner"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(engine.workspace, "result.txt"), []byte("done"), 0600); err != nil {
				t.Fatal(err)
			}
			for attempt := range 2 { // a repeated settlement keeps the user's resolution
				got, err := engine.VerifyDelivery(ctx, id)
				if err != nil || got.Status != DeliveryVerified {
					t.Fatalf("attempt %d: %+v %v", attempt, got, err)
				}
				info, err := engine.TaskState(ctx, id)
				if err != nil || info.PendingEdits != 0 || info.State.Resolution.State != tc.want || info.State.Resolution.By != tc.by {
					t.Fatalf("attempt %d host view: %+v pending=%d %v", attempt, info.State.Resolution, info.PendingEdits, err)
				}
				if got := threadResolution(t, engine, id); got != tc.want {
					t.Fatalf("attempt %d read_thread=%q, host view %q", attempt, got, tc.want)
				}
				row, err := engine.store.LatestTaskState(ctx, id)
				if err != nil || !strings.Contains(string(row.JSON), `"by":"`+tc.by+`"`) {
					t.Fatalf("attempt %d stored: %s %v", attempt, row.JSON, err)
				}
			}
		})
	}

	// The resolution arrives after the boundary: the settlement resolved done by evidence first.
	engine, id := notebookEngine(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(engine.workspace, "result.txt"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	if r, err := engine.runAgentWith(ctx, id, p, AgentOptions{Workflow: json.RawMessage(finishableWorkflow), Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}}); err != nil || r.Delivery.Status != DeliveryVerified {
		t.Fatalf("run: %+v %v", r, err)
	}
	if got := taskResolution(t, engine, id).State.Resolution; got.State != taskstate.ResolutionDone || got.By != taskstate.ByEvidence {
		t.Fatalf("evidence resolution: %+v", got)
	}
	if _, err := engine.ResolveThread(ctx, id, taskstate.ResolutionDropped, "owner"); err != nil {
		t.Fatal(err)
	}
	if got := taskResolution(t, engine, id).State.Resolution; got.State != taskstate.ResolutionDropped {
		t.Fatalf("late resolution not shown: %+v", got)
	}
	if got, err := engine.VerifyDelivery(ctx, id); err != nil || got.Status != DeliveryVerified {
		t.Fatalf("settle after late resolution: %+v %v", got, err)
	}
	if got := threadResolution(t, engine, id); got != taskstate.ResolutionDropped {
		t.Fatalf("late user resolution overridden: read_thread=%q", got)
	}
}

// corruptOnCompletion makes the stored Task State undecodable the moment the execution commits
// completed, after the loop's last Task State write: only the settlement reads it.
func corruptOnCompletion(t *testing.T, engine *Engine, id string) {
	faultSQL(t, engine, `CREATE TRIGGER corrupt_state AFTER UPDATE OF status ON sessions
WHEN NEW.id='`+id+`' AND NEW.status IN ('completed','failed')
BEGIN INSERT INTO task_states(session_id, version, created_at, reason, json)
SELECT NEW.id, COALESCE(MAX(version),0)+1, 'x', 'fault', '[]' FROM task_states WHERE session_id=NEW.id; END`)
}

// Contract presence is what the Start recorded, never inferred from a later read: an unreadable state
// gives a run without a contract no delivery, and a run with one its pending record or an unavailable
// verification, never a fabricated outcome.
func TestDeliveryStateReadFailureFabricatesNothing(t *testing.T) {
	authFailure := func(context.Context, int) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAuth, Err: errors.New("unauthorized")}
	}
	for _, tc := range []struct {
		name     string
		contract *DeliveryContract
		fail     bool
		want     string
	}{
		{"failed without contract", nil, true, ""},
		{"completed without contract", nil, false, ""},
		{"failed with contract", &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}, true, DeliveryPending},
		{"completed with contract", &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}, false, DeliveryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, id := notebookEngine(t)
			corruptOnCompletion(t, engine, id)
			var p protocol.Provider = &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
			if tc.fail {
				p = &deliveryStepProvider{next: authFailure}
			}
			events, stop := engine.Events(64)
			defer stop()
			r, err := engine.runAgentWith(context.Background(), id, p, AgentOptions{Delivery: tc.contract})
			if tc.fail != (err != nil) || (tc.fail && !strings.Contains(err.Error(), "unauthorized")) {
				t.Fatalf("execution outcome changed: %+v %v", r, err)
			}
			switch {
			case tc.want == "" && r.Delivery != nil:
				t.Fatalf("fabricated delivery: %+v", r.Delivery)
			case tc.want != "" && (r.Delivery == nil || r.Delivery.Status != tc.want || r.Delivery.RunID == ""):
				t.Fatalf("delivery=%+v want %s", r.Delivery, tc.want)
			case tc.want == DeliveryUnavailable && !hasIssue(r.Delivery, "state"):
				t.Fatalf("state failure not reported: %+v", r.Delivery)
			}
			published := drainType(events, "delivery.result")
			if wantEvent := tc.want == DeliveryUnavailable; (published > 0) != wantEvent {
				t.Fatalf("delivery.result events=%d", published)
			}
		})
	}
}

func drainType(events <-chan Event, eventType string) int {
	n := 0
	for {
		select {
		case ev := <-events:
			if ev.Type == eventType {
				n++
			}
		default:
			return n
		}
	}
}

// An asynchronous run's settlement failure reaches the host as a delivery.result event before
// run.stopped; the Agent stays completed.
func TestDeliveryAsyncSettlementFailureIsPublished(t *testing.T) {
	engine, id := notebookEngine(t)
	corruptOnCompletion(t, engine, id)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	events, stop := engine.Events(256)
	defer stop()
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	if _, err := supervisor.startWith(context.Background(), id, p, AgentOptions{Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result.txt"}}}}); err != nil {
		t.Fatal(err)
	}
	var result *DeliveryResult
	deadline := time.After(10 * time.Second)
	for stopped := false; !stopped; {
		select {
		case ev := <-events:
			switch {
			case ev.SessionID != id:
			case ev.Type == "delivery.result":
				result = &DeliveryResult{}
				if err := json.Unmarshal(ev.Data, result); err != nil {
					t.Fatal(err)
				}
			case ev.Type == "run.stopped":
				stopped = true
			}
		case <-deadline:
			t.Fatal("run did not stop")
		}
	}
	if result == nil || result.Status != DeliveryUnavailable || !hasIssue(result, "state") {
		t.Fatalf("settlement failure not published: %+v", result)
	}
	row, err := engine.store.Get(context.Background(), id)
	if err != nil || row.Status != "completed" || len(p.requests) != 1 {
		t.Fatalf("execution changed: %+v requests=%d %v", row, len(p.requests), err)
	}
}

// A child's failed delivery keeps the parent's tasks obligation across a restart, and the child's
// verification-only retry on the restarted engine discharges it without running the child again.
func TestDeliveryChildFailureObligationSurvivesRestart(t *testing.T) {
	engine, child := notebookEngine(t)
	ctx := context.Background()
	p := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("done")}}
	if r, err := engine.runAgentWith(ctx, child, p, AgentOptions{Delivery: &DeliveryContract{Files: []DeliveryFile{{Path: "result"}}}}); err != nil || r.Delivery.Status != DeliveryFailed {
		t.Fatalf("child: %+v %v", r, err)
	}
	parent, err := engine.NewSession(ctx, SessionOptions{Goal: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.store.AddRelation(ctx, session.Relation{ParentSessionID: parent.ID, ChildSessionID: child, Kind: RelationExecution}); err != nil {
		t.Fatal(err)
	}
	workspace := engine.workspace
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	info, err := reopened.TaskState(ctx, parent.ID)
	if err != nil || !slices.Contains(info.Obligations, taskstate.ObligationTasks) || info.State.Resolution.State != taskstate.ResolutionOpen {
		t.Fatalf("restart lost the child's failed delivery: %+v %v", info.Obligations, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "result"), []byte("delivered"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.VerifyDelivery(ctx, child); err != nil || got.Status != DeliveryVerified {
		t.Fatalf("child retry: %+v %v", got, err)
	}
	if info, err := reopened.TaskState(ctx, parent.ID); err != nil || slices.Contains(info.Obligations, taskstate.ObligationTasks) {
		t.Fatalf("verified child still an obligation: %+v %v", info.Obligations, err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("child rerun: %d requests", len(p.requests))
	}
}
