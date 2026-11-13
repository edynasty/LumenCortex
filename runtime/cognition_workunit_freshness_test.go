package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

// freshnessRun stops at each provider request. The test drives the real tool loop
// across other runs' committed check results, without timing sleeps or product hooks.
type freshnessRun struct {
	id       string
	reads    int
	serial   uint64
	ctx      context.Context
	cancel   context.CancelFunc
	requests chan protocol.ProviderRequest
	replies  chan protocol.ProviderResponse
	done     chan struct{}
	err      error
}

var freshnessRunSerial atomic.Uint64

func startFreshnessRun(t *testing.T, e *Engine) *freshnessRun {
	t.Helper()
	h, err := e.NewSession(context.Background(), SessionOptions{Goal: "check cognition across a concurrent Work Unit"})
	if err != nil {
		t.Fatal(err)
	}
	return startFreshnessRunIn(t, e, h.ID)
}

func startFreshnessRunIn(t *testing.T, e *Engine, sessionID string) *freshnessRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	r := &freshnessRun{id: sessionID, serial: freshnessRunSerial.Add(1), ctx: ctx, cancel: cancel, requests: make(chan protocol.ProviderRequest), replies: make(chan protocol.ProviderResponse), done: make(chan struct{})}
	p := &cognitionProvider{respond: func(_ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		select {
		case r.requests <- req:
		case <-ctx.Done():
			return protocol.ProviderResponse{}, ctx.Err()
		}
		select {
		case response := <-r.replies:
			return response, nil
		case <-ctx.Done():
			return protocol.ProviderResponse{}, ctx.Err()
		}
	}}
	model := bind(e, p)
	go func() {
		defer close(r.done)
		result, err := e.RunAgent(ctx, r.id, AgentOptions{ModelRef: model, MaxSteps: 30, Policy: "workspace", Interactive: true})
		if err == nil && result.Status != "completed" {
			err = fmt.Errorf("run ended %s", result.Status)
		}
		r.err = err
	}()
	t.Cleanup(func() { cancel(); <-r.done })
	r.next(t)
	return r
}

func (r *freshnessRun) next(t *testing.T) protocol.ProviderRequest {
	t.Helper()
	select {
	case request := <-r.requests:
		return request
	case <-r.done:
		t.Fatalf("run ended before its next request: %v", r.err)
	case <-r.ctx.Done():
		t.Fatal(r.ctx.Err())
	}
	return protocol.ProviderRequest{}
}

func (r *freshnessRun) send(t *testing.T, response protocol.ProviderResponse) {
	t.Helper()
	select {
	case r.replies <- response:
	case <-r.done:
		t.Fatalf("run ended before its next tool: %v", r.err)
	case <-r.ctx.Done():
		t.Fatal(r.ctx.Err())
	}
}

func (r *freshnessRun) tool(t *testing.T, id, name, args string) protocol.ProviderRequest {
	t.Helper()
	r.send(t, cogStep(id, name, args))
	return r.next(t)
}

func (r *freshnessRun) read(t *testing.T, ref string) KnowledgeView {
	t.Helper()
	r.reads++
	call := fmt.Sprintf("read-%d-%d", r.serial, r.reads)
	request := r.tool(t, call, "read_knowledge", `{"ref":"`+ref+`"}`)
	return viewIn(t, request, call)
}

func (r *freshnessRun) finish(t *testing.T) {
	t.Helper()
	r.send(t, cogFinal("check complete"))
	select {
	case <-r.done:
		if r.err != nil {
			t.Fatal(r.err)
		}
	case <-r.ctx.Done():
		t.Fatal(r.ctx.Err())
	}
}

func workUnitFreshnessFixture(t *testing.T) (*cognitionFixture, string, string) {
	t.Helper()
	external := filepath.Join(t.TempDir(), "service-port")
	writeFile(t, external, "8080\n")
	checks := fmt.Sprintf(`{"port":{"kind":"test","command":%q,"required":false}}`, "test \"$(cat "+quoteCheckPath(external)+")\" = 8080")
	f, id := workUnitFreshnessChecks(t, checks)
	return f, id, external
}

func workUnitFreshnessChecks(t *testing.T, checks string) (*cognitionFixture, string) {
	t.Helper()
	f := newCognitionFixture(t, true)
	f.setChecks(t, checks, map[string]knowledge.Assertion{"port": portFact("8080")})
	runGitIn(t, f.root, "add", "lumencortex.json")
	runGitIn(t, f.root, "commit", "-m", "approved checks")
	source := f.run(t, "record the service port", cogAnswer("8080"))
	adopted := f.adoptFacts(t, source, portFact("8080"))
	if adopted.Qualified != 1 || len(adopted.Inserted) != 1 {
		t.Fatalf("adoption: %+v", adopted)
	}
	f.answer = func([]string) string { return `{"candidates":[]}` }
	return f, adopted.Inserted[0]
}

func workUnitPortCheck(t *testing.T, e *Engine, r *freshnessRun, unit, want string) CheckResult {
	t.Helper()
	prepareFreshnessUnit(t, r, unit, "port")
	request := r.tool(t, unit+"-decide", "work_unit_update", `{"id":"`+unit+`","status":"completed"}`)
	row := freshnessUnitResult(t, e, r.id, unit, "port")
	if row.Status != want || row.EpisodeStatus != evaluator.EpisodeClosed || row.Provenance != evaluator.ProvenanceRuntime ||
		row.Fingerprint == "" || row.PreFingerprint != row.Fingerprint || row.PostFingerprint != row.Fingerprint {
		t.Fatalf("Work Unit did not persist a qualified %s: %+v", want, row)
	}
	if want == evaluator.StatusFailed && !strings.Contains(lastToolResult(request, unit+"-decide"), "port: failed") {
		t.Fatalf("failed Work Unit was not refused: %s", lastToolResult(request, unit+"-decide"))
	}
	return row
}

func prepareFreshnessUnit(t *testing.T, r *freshnessRun, unit string, checks ...string) {
	t.Helper()
	r.tool(t, unit+"-invalidate", "write_file", `{"path":"defaults.env","content":"BASE=8080\n"}`)
	args, err := json.Marshal(map[string]any{"id": unit, "goal": "check the service port", "checks": checks})
	if err != nil {
		t.Fatal(err)
	}
	r.tool(t, unit+"-create", "work_unit_create", string(args))
}

func freshnessUnitResult(t *testing.T, e *Engine, sessionID, unit, check string) CheckResult {
	t.Helper()
	rows, err := e.CheckResults(context.Background(), sessionID, -1, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.WorkUnitID != unit || row.Check != check {
			continue
		}
		return row
	}
	t.Fatalf("no persisted Work Unit result for %s/%s: %+v", unit, check, rows)
	return CheckResult{}
}

// REVIEW-F04-01: both runs establish PASS before only Y invalidates its evaluator
// and executes a failing Work Unit. The failure is not a cognition revalidation.
func TestCognitionWorkUnitFailureSupersedesOtherRunPass(t *testing.T) {
	f, id, external := workUnitFreshnessFixture(t)
	x := startFreshnessRun(t, f.engine)
	if before := x.read(t, id); before.Applicability != knowledge.Current {
		t.Fatalf("X never established current: %+v", before)
	}
	old := f.observations(t, id)[0]
	y := startFreshnessRun(t, f.engine)
	if before := y.read(t, id); before.Applicability != knowledge.Current {
		t.Fatalf("Y never established current: %+v", before)
	}
	writeFile(t, external, "9090\n")
	failed := workUnitPortCheck(t, f.engine, y, "verify-port", evaluator.StatusFailed)
	if failed.Fingerprint != old.Fingerprint || failed.SpecDigest != old.SpecDigest || failed.ApprovalEpoch != old.ApprovalEpoch {
		t.Fatalf("failure is not at X's state: old=%+v failed=%+v", old, failed)
	}
	for _, observation := range f.observations(t, id) {
		if observation.SessionID == y.id && observation.ResultSeq == failed.Seq {
			t.Fatal("fixture incorrectly linked the Work Unit failure as a cognition revalidation")
		}
	}
	after := x.read(t, id)
	t.Logf("X before=current; Y Work Unit failure seq=%d is closed at identical fingerprint/spec/approval; X after=%s", failed.Seq, after.Applicability)
	if after.Applicability != knowledge.Unknown {
		t.Errorf("persisted Work Unit failure must make X unknown, not %s", after.Applicability)
	}
	if own := y.read(t, id); own.Applicability != knowledge.Stale {
		t.Errorf("Y must use its own failed check as stale: %+v", own)
	}
	writeFile(t, external, "8080\n")
	passed := workUnitPortCheck(t, f.engine, x, "recover-port", evaluator.StatusPassed)
	if passed.Fingerprint != failed.Fingerprint {
		t.Fatalf("recovery changed the fingerprint: %+v", passed)
	}
	if recovered := x.read(t, id); recovered.Applicability != knowledge.Current {
		t.Errorf("X's own fresh Work Unit PASS did not restore current: %+v", recovered)
	}
	if other := y.read(t, id); other.Applicability != knowledge.Stale {
		t.Errorf("Y borrowed X's new PASS: %+v", other)
	}

	y.tool(t, "cancel", "work_unit_update", `{"id":"verify-port","status":"cancelled","summary":"the failed check is recorded"}`)
	y.finish(t)
	x.finish(t)
}

func TestCognitionWorkUnitFailureOtherWorkspace(t *testing.T) {
	f, id, external := workUnitFreshnessFixture(t)
	x := startFreshnessRun(t, f.engine)
	if before := x.read(t, id); before.Applicability != knowledge.Current {
		t.Fatalf("X never established current: %+v", before)
	}
	old := f.observations(t, id)[0]
	h, err := f.engine.NewSession(context.Background(), SessionOptions{Goal: "verify the service in another worktree"})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := f.engine.AttachWorktree(context.Background(), h.ID, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	y := startFreshnessRunIn(t, f.engine, h.ID)
	if before := y.read(t, id); before.Applicability != knowledge.Current {
		t.Fatalf("Y never established current: %+v", before)
	}
	writeFile(t, external, "9090\n")
	failed := workUnitPortCheck(t, f.engine, y, "other-worktree", evaluator.StatusFailed)
	if failed.SpecDigest != old.SpecDigest || failed.ApprovalEpoch != old.ApprovalEpoch || failed.Fingerprint == old.Fingerprint ||
		!strings.Contains(failed.Fingerprint, "@"+wt.Path+"#") {
		t.Fatalf("failure was not isolated only by workspace: %+v versus %+v", failed, old)
	}
	if after := x.read(t, id); after.Applicability != knowledge.Current {
		t.Errorf("other workspace failure invalidated X: %+v", after)
	}
	y.tool(t, "cancel", "work_unit_update", `{"id":"other-worktree","status":"cancelled"}`)
	y.finish(t)
	x.finish(t)
}

// gate is an external check dependency, not a product test hook. The first
// check has already persisted its open result when curl reaches this handler.
func freshnessEpisodeGate(t *testing.T) (string, <-chan struct{}, chan<- struct{}) {
	t.Helper()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		select {
		case <-release:
			fmt.Fprintln(w, "released")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, entered, release
}

// Reads straddle the real CloseEpisode transaction. Open/cancelled/changed
// episodes are not failures; a stable close makes the very next read unknown.
func TestCognitionWorkUnitFailureEpisodeInterleaving(t *testing.T) {
	for _, mode := range []string{"closed", "interrupted", "scope_changed", "spec_unapproved"} {
		t.Run(mode, func(t *testing.T) {
			url, entered, release := freshnessEpisodeGate(t)
			external := filepath.Join(t.TempDir(), "service-port")
			writeFile(t, external, "8080\n")
			command := "test \"$(cat " + quoteCheckPath(external) + ")\" = 8080"
			checks := fmt.Sprintf(`{"port":{"kind":"test","command":%q,"required":false},"gate":{"kind":"test","command":%q,"required":false}}`, command, "curl --noproxy '*' -fsS '"+url+"'")
			f, id := workUnitFreshnessChecks(t, checks)
			x := startFreshnessRun(t, f.engine)
			old := f.observations(t, id)[0]
			y := startFreshnessRun(t, f.engine)
			if x.read(t, id).Applicability != knowledge.Current || y.read(t, id).Applicability != knowledge.Current {
				t.Fatal("both runs must establish their own PASS")
			}
			writeFile(t, external, "9090\n")
			prepareFreshnessUnit(t, y, "gated", "port", "gate")
			y.send(t, cogStep("decide", "work_unit_update", `{"id":"gated","status":"completed"}`))
			select {
			case <-entered:
			case <-y.done:
				t.Fatalf("Y ended before the gate: %v", y.err)
			case <-y.ctx.Done():
				t.Fatal(y.ctx.Err())
			}
			open := freshnessUnitResult(t, f.engine, y.id, "gated", "port")
			if open.Status != evaluator.StatusFailed || open.EpisodeStatus != evaluator.EpisodeOpen || open.Fingerprint != old.Fingerprint {
				t.Fatalf("gate did not hold a real same-state open failure: %+v", open)
			}
			if v := x.read(t, id); v.Applicability != knowledge.Current {
				t.Fatalf("unclosed failure invalidated X: %+v", v)
			}
			switch mode {
			case "interrupted":
				y.cancel()
				<-y.done
			case "scope_changed":
				f.write(t, "defaults.env", "BASE=9090\n")
			case "spec_unapproved":
				approveChecks(t, f.engine)
			}
			if mode != "interrupted" {
				release <- struct{}{}
				y.next(t)
			}
			closed := freshnessUnitResult(t, f.engine, y.id, "gated", "port")
			if closed.Status != evaluator.StatusFailed || closed.EpisodeStatus != mode {
				t.Fatalf("wrong final episode: %+v", closed)
			}
			if mode == "scope_changed" {
				// Restoring the workspace makes X's earlier fingerprint applicable
				// again, but must not qualify Y's moved-scope failure retroactively.
				f.write(t, "defaults.env", "BASE=8080\n")
			}
			if mode == "spec_unapproved" {
				f.bind(t, portFact("8080"), "port")
				writeFile(t, external, "8080\n")
				workUnitPortCheck(t, f.engine, x, "current-approval", evaluator.StatusPassed)
			}
			want := knowledge.Current
			if mode == "closed" {
				want = knowledge.Unknown
			}
			for range 3 {
				if v := x.read(t, id); v.Applicability != want {
					t.Fatalf("after %s, got %s; want %s", mode, v.Applicability, want)
				}
			}
			if mode != "interrupted" {
				y.tool(t, "cancel", "work_unit_update", `{"id":"gated","status":"cancelled"}`)
				y.finish(t)
			}
			x.finish(t)
		})
	}
}

// A reopened Engine must retain the qualified failure without reviving the
// completed run's old PASS. A resumed run verifies for itself before recovery.
func TestCognitionWorkUnitFailureSurvivesEngineReopen(t *testing.T) {
	f, id, external := workUnitFreshnessFixture(t)
	x := startFreshnessRun(t, f.engine)
	if x.read(t, id).Applicability != knowledge.Current {
		t.Fatal("X never established current")
	}
	y := startFreshnessRun(t, f.engine)
	writeFile(t, external, "9090\n")
	failed := workUnitPortCheck(t, f.engine, y, "verify-port", evaluator.StatusFailed)
	y.tool(t, "cancel", "work_unit_update", `{"id":"verify-port","status":"cancelled"}`)
	y.finish(t)
	if x.read(t, id).Applicability != knowledge.Unknown {
		t.Fatal("persisted failure did not invalidate X")
	}
	x.finish(t)
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Workspace: f.root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	rows, err := reopened.CheckResults(context.Background(), y.id, -1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var retained bool
	for _, row := range rows {
		if row.Seq == failed.Seq {
			retained = row.Status == evaluator.StatusFailed && row.EpisodeStatus == evaluator.EpisodeClosed &&
				row.Fingerprint == failed.Fingerprint && row.SpecDigest == failed.SpecDigest && row.ApprovalEpoch == failed.ApprovalEpoch
		}
	}
	if !retained {
		t.Fatalf("reopen lost the qualified failure: %+v", rows)
	}
	view, err := reopened.ReadKnowledge(context.Background(), id)
	if err != nil || view.(KnowledgeView).Applicability != knowledge.Unknown {
		t.Fatalf("reopened host reused a completed run: %+v %v", view, err)
	}
	resumed := startFreshnessRunIn(t, reopened, x.id)
	if resumed.read(t, id).Applicability != knowledge.Stale {
		t.Fatal("restarted Run must check for itself and see the real failure")
	}
	writeFile(t, external, "8080\n")
	workUnitPortCheck(t, reopened, resumed, "restore-port", evaluator.StatusPassed)
	if resumed.read(t, id).Applicability != knowledge.Current {
		t.Fatal("restarted Run's new PASS did not recover current")
	}
	resumed.finish(t)
}

func TestCognitionWorkUnitFailureForeignRecoveryDoesNotReviveOldPass(t *testing.T) {
	f, id, external := workUnitFreshnessFixture(t)
	x := startFreshnessRun(t, f.engine)
	y := startFreshnessRun(t, f.engine)
	if x.read(t, id).Applicability != knowledge.Current || y.read(t, id).Applicability != knowledge.Current {
		t.Fatal("both runs must establish their own PASS")
	}
	writeFile(t, external, "9090\n")
	workUnitPortCheck(t, f.engine, y, "failure", evaluator.StatusFailed)
	if x.read(t, id).Applicability != knowledge.Unknown {
		t.Fatal("failure did not invalidate X")
	}
	writeFile(t, external, "8080\n")
	workUnitPortCheck(t, f.engine, y, "foreign-recovery", evaluator.StatusPassed)
	if y.read(t, id).Applicability != knowledge.Current {
		t.Fatal("Y's own new PASS did not recover")
	}
	if v := x.read(t, id); v.Applicability != knowledge.Unknown {
		t.Errorf("Y's newer PASS revived X's superseded PASS: %+v", v)
	}
	y.tool(t, "cancel-failure", "work_unit_update", `{"id":"failure","status":"cancelled"}`)
	y.finish(t)
	x.finish(t)
}

func TestCognitionWorkUnitFailureDifferentCheck(t *testing.T) {
	external := filepath.Join(t.TempDir(), "service-port")
	writeFile(t, external, "8080\n")
	command := "test \"$(cat " + quoteCheckPath(external) + ")\" = 8080"
	checks := fmt.Sprintf(`{"port":{"kind":"test","command":%q,"required":false},"other":{"kind":"test","command":%q,"required":false}}`, command, command)
	f, id := workUnitFreshnessChecks(t, checks)
	x := startFreshnessRun(t, f.engine)
	if x.read(t, id).Applicability != knowledge.Current {
		t.Fatal("X never established current")
	}
	old := f.observations(t, id)[0]
	y := startFreshnessRun(t, f.engine)
	writeFile(t, external, "9090\n")
	prepareFreshnessUnit(t, y, "unrelated", "other")
	y.tool(t, "unrelated-decide", "work_unit_update", `{"id":"unrelated","status":"completed"}`)
	failed := freshnessUnitResult(t, f.engine, y.id, "unrelated", "other")
	if failed.Status != evaluator.StatusFailed || failed.EpisodeStatus != evaluator.EpisodeClosed ||
		failed.Fingerprint != old.Fingerprint || failed.ApprovalEpoch != old.ApprovalEpoch || failed.SpecDigest == old.SpecDigest {
		t.Fatalf("unrelated check must differ by its definition, not fingerprint/approval: %+v versus %+v", failed, old)
	}
	if v := x.read(t, id); v.Applicability != knowledge.Current {
		t.Errorf("different check invalidated X: %+v", v)
	}
	y.tool(t, "cancel", "work_unit_update", `{"id":"unrelated","status":"cancelled"}`)
	y.finish(t)
	x.finish(t)
}

// Host changes go through the real approval API. Single-field comparisons are
// additionally covered at the Store boundary: real reapproval is monotonic and
// a definition change necessarily changes its approval too.
func TestCognitionWorkUnitFailureDefinitionAndApprovalIsolation(t *testing.T) {
	for _, mode := range []string{"approval_epoch", "spec_digest", "input_version"} {
		t.Run(mode, func(t *testing.T) {
			external := filepath.Join(t.TempDir(), "service-port")
			writeFile(t, external, "8080\n")
			f := newCognitionFixture(t, true)
			f.write(t, "check.sh", "test \"$(cat "+quoteCheckPath(external)+")\" = 8080\n")
			checks := `{"port":{"kind":"test","command":"sh check.sh","required":false,"timeoutSeconds":20}}`
			f.setChecks(t, checks, map[string]knowledge.Assertion{"port": portFact("8080")})
			runGitIn(t, f.root, "add", "lumencortex.json", "check.sh")
			runGitIn(t, f.root, "commit", "-m", "check inputs")
			source := f.run(t, "record the service port", cogAnswer("8080"))
			adopted := f.adoptFacts(t, source, portFact("8080"))
			if adopted.Qualified != 1 || len(adopted.Inserted) != 1 {
				t.Fatalf("adoption: %+v", adopted)
			}
			id := adopted.Inserted[0]
			f.answer = func([]string) string { return `{"candidates":[]}` }
			x := startFreshnessRun(t, f.engine)
			y := startFreshnessRun(t, f.engine)
			writeFile(t, external, "9090\n")
			failed := workUnitPortCheck(t, f.engine, y, "old-definition", evaluator.StatusFailed)
			if x.read(t, id).Applicability != knowledge.Unknown {
				t.Fatal("old failure did not invalidate X")
			}
			switch mode {
			case "spec_digest":
				// Timeout belongs to SpecDigest but not the check fingerprint.
				setProjectChecks(t, f.root, strings.Replace(checks, "20", "30", 1))
			case "input_version":
				f.write(t, "check.sh", "# approved revision two\ntest \"$(cat "+quoteCheckPath(external)+")\" = 8080\n")
			}
			approveChecks(t, f.engine)
			f.bind(t, portFact("8080"), "port")
			writeFile(t, external, "8080\n")
			passed := workUnitPortCheck(t, f.engine, x, "new-definition", evaluator.StatusPassed)
			if passed.ApprovalEpoch == failed.ApprovalEpoch || (passed.SpecDigest == failed.SpecDigest) != (mode == "approval_epoch") {
				t.Fatalf("definition/approval transition missing: old=%+v new=%+v", failed, passed)
			}
			if mode == "approval_epoch" && passed.Fingerprint != failed.Fingerprint {
				t.Fatal("approval-only transition unexpectedly changed the fingerprint")
			}
			if mode != "approval_epoch" {
				// The old candidate remains bound to the old definition. A new
				// approved definition must not silently give it new authority.
				if v := x.read(t, id); v.Applicability != knowledge.CandidateOnly {
					t.Fatalf("old candidate acquired another check definition: %+v", v)
				}
				// A new source version creates support under the new binding;
				// the original source's processed version is not maintained again.
				newSource := f.run(t, "record the newly approved service port", cogAnswer("8080"))
				replacement := f.adoptFacts(t, newSource, portFact("8080"))
				if replacement.Qualified != 1 || len(replacement.Inserted) != 1 {
					t.Fatalf("new-definition adoption: %+v", replacement)
				}
				id = replacement.Inserted[0]
			}
			if v := x.read(t, id); v.Applicability != knowledge.Current {
				t.Errorf("old definition/approval failure contaminated new evidence: %+v", v)
			}
			y.tool(t, "cancel", "work_unit_update", `{"id":"old-definition","status":"cancelled"}`)
			y.finish(t)
			x.finish(t)
		})
	}
}

func TestCognitionWorkUnitFailureIgnoresExecutionError(t *testing.T) {
	external := filepath.Join(t.TempDir(), "mode")
	writeFile(t, external, "fast\n")
	command := "if test \"$(cat " + quoteCheckPath(external) + ")\" = slow; then sleep 30; fi"
	f, id := workUnitFreshnessChecks(t, fmt.Sprintf(`{"port":{"kind":"test","command":%q,"required":false,"timeoutSeconds":1}}`, command))
	x := startFreshnessRun(t, f.engine)
	y := startFreshnessRun(t, f.engine)
	if x.read(t, id).Applicability != knowledge.Current {
		t.Fatal("X never established current")
	}
	writeFile(t, external, "slow\n")
	prepareFreshnessUnit(t, y, "timeout", "port")
	y.tool(t, "decide", "work_unit_update", `{"id":"timeout","status":"completed"}`)
	result := freshnessUnitResult(t, f.engine, y.id, "timeout", "port")
	if result.Status != evaluator.StatusError || result.EpisodeStatus != evaluator.EpisodeClosed {
		t.Fatalf("expected closed execution error, not a check failure: %+v", result)
	}
	if v := x.read(t, id); v.Applicability != knowledge.Current {
		t.Errorf("execution error invalidated X as if it were a real failure: %+v", v)
	}
	y.tool(t, "cancel", "work_unit_update", `{"id":"timeout","status":"cancelled"}`)
	y.finish(t)
	x.finish(t)
}
