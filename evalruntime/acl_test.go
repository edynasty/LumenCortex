package evalruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

func TestSessionsAreIsolatedBetweenOwners(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	a := h.create("fixture", "owner-a", "owner-a")
	b := h.create("fixture", "owner-b", "owner-b")
	if a.Workspace == b.Workspace {
		t.Fatalf("two kernels share a workspace")
	}
	h.expectCompleted(a, "owner-a", "let secret = 42")

	// B may not execute in A's kernel, read its history or see it.
	if _, err := h.m.Execute(ctx, evalapi.Request{ID: "x", SessionID: a.ID, CellID: "c", Code: "1", AgentID: "owner-b"}, nil); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("cross-owner execute = %v", err)
	}
	if _, err := h.m.Results(ctx, a.ID, "owner-b"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("cross-owner results = %v", err)
	}
	if _, err := h.m.Session(ctx, a.ID, "owner-b"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("cross-owner session = %v", err)
	}
	visible, err := h.m.Sessions(ctx, "owner-b")
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	for _, item := range visible {
		if item.ID == a.ID {
			t.Fatalf("owner-b sees owner-a's kernel")
		}
	}
	if result := h.exec(b, "owner-b", "secret", evalapi.Budget{}); result.Succeeded() {
		t.Fatalf("owner-b resolved owner-a's variable: %+v", result)
	}
}

func TestGrantRoles(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	session := h.create("fixture", "owner-a", "owner-a")
	h.expectCompleted(session, "owner-a", "let x = 10")

	// Workspace widening is refused.
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "executor-b", Role: evalapi.RoleObserver, Workspace: h.workspace, Permission: session.Permission,
	}); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("grant with a broader workspace = %v", err)
	}
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "executor-b", Role: evalapi.RoleObserver, Workspace: session.Workspace, Permission: "full",
	}); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("grant with a broader permission = %v", err)
	}
	// Ownership cannot be granted.
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "executor-b", Role: evalapi.RoleOwner, Workspace: session.Workspace, Permission: session.Permission,
	}); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("grant of OWNER = %v", err)
	}

	// A kernel executes only for its owner: an executor grant is refused, because
	// it would run another actor inside the owner's process.
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "executor-b", Role: "EXECUTOR", Workspace: session.Workspace, Permission: session.Permission,
	}); !errors.Is(err, evalapi.ErrInvalid) {
		t.Fatalf("removed executor role = %v, want ErrInvalid", err)
	}
	// The same actor shares data read-only instead.
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "executor-b", Role: evalapi.RoleObserver, Workspace: session.Workspace, Permission: session.Permission,
	}); err != nil {
		t.Fatalf("grant observer: %v", err)
	}
	if _, err := h.m.Execute(ctx, evalapi.Request{ID: "cross", SessionID: session.ID, CellID: "c", Code: "1", AgentID: "executor-b"}, nil); !errors.Is(err, evalapi.ErrAuthorityDomain) {
		t.Fatalf("cross-domain execute = %v, want ErrAuthorityDomain", err)
	}
	if _, err := h.m.Results(ctx, session.ID, "executor-b"); err != nil {
		t.Fatalf("a read grant cannot read results: %v", err)
	}
	if err := h.m.Reset(ctx, session.ID, "executor-b"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("executor reset = %v", err)
	}
	if err := h.m.Close(ctx, session.ID, "executor-b"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("executor close = %v", err)
	}
	if err := h.m.Grant(ctx, session.ID, "executor-b", evalapi.Grant{AgentID: "executor-c", Role: evalapi.RoleObserver}); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("grantee regrant = %v", err)
	}

	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "observer-c", Role: evalapi.RoleObserver, Workspace: session.Workspace, Permission: session.Permission,
	}); err != nil {
		t.Fatalf("grant observer: %v", err)
	}
	if _, err := h.m.Results(ctx, session.ID, "observer-c"); err != nil {
		t.Fatalf("observer cannot read results: %v", err)
	}
	if _, err := h.m.Execute(ctx, evalapi.Request{ID: "obs", SessionID: session.ID, CellID: "c", Code: "1", AgentID: "observer-c"}, nil); !errors.Is(err, evalapi.ErrAuthorityDomain) {
		t.Fatalf("observer execute = %v", err)
	}
	if _, err := h.m.InterruptExecution(ctx, session.ID, "observer-c", evalapi.InterruptTarget{RequestID: "obs"}); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("observer interrupt = %v", err)
	}

	// Revocation takes effect immediately.
	if err := h.m.Revoke(ctx, session.ID, "owner-a", "executor-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := h.m.Results(ctx, session.ID, "executor-b"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("revoked executor can still read results: %v", err)
	}
	if _, err := h.m.Execute(ctx, evalapi.Request{ID: "revoked", SessionID: session.ID, CellID: "c", Code: "1", AgentID: "executor-b"}, nil); !errors.Is(err, evalapi.ErrAuthorityDomain) {
		t.Fatalf("revoked executor = %v", err)
	}
}

func TestGrantExpiry(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	session := h.create("fixture", "owner-a", "owner-a")
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "short-lived", Role: evalapi.RoleObserver, ExpiresAt: time.Now().Add(200 * time.Millisecond),
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := h.m.Results(ctx, session.ID, "short-lived"); err != nil {
		t.Fatalf("grant before expiry failed: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := h.m.Results(ctx, session.ID, "short-lived"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("expired grant = %v", err)
	}
	if _, err := h.m.Execute(ctx, evalapi.Request{ID: "late", SessionID: session.ID, CellID: "c", Code: "1", AgentID: "short-lived"}, nil); !errors.Is(err, evalapi.ErrAuthorityDomain) {
		t.Fatalf("expired grant execute = %v", err)
	}
}

func TestExecutionIsSerialized(t *testing.T) {
	h, _ := newFixtureHarness(t)
	session := h.create("fixture", "owner-a", "owner-a")
	h.expectCompleted(session, "owner-a", "let n = 0")

	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := h.m.Execute(context.Background(), evalapi.Request{
				ID:        fmt.Sprintf("concurrent-%d", index),
				SessionID: session.ID,
				CellID:    fmt.Sprintf("cell-%d", index),
				Code:      "let n = n + 1",
				AgentID:   "owner-a",
			}, nil)
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent execute: %v", err)
		}
	}
	final := h.expectCompleted(session, "owner-a", "n")
	if len(final.Displays) != 1 || final.Displays[0].Text != fmt.Sprint(workers) {
		t.Fatalf("serialized counter = %+v, want %d", final.Displays, workers)
	}
}

func TestCloseOwnerCancelsInFlightExecution(t *testing.T) {
	h, _ := newFixtureHarness(t)
	session := h.create("fixture", "owner-a", "owner-a")
	done := make(chan evalapi.Result, 1)
	go func() {
		result, err := h.m.Execute(context.Background(), evalapi.Request{
			ID: "long", SessionID: session.ID, CellID: "c", Code: "sleep 20000", AgentID: "owner-a",
		}, nil)
		if err != nil {
			done <- evalapi.Result{Status: evalapi.ResultStatusCrashed, Error: err.Error()}
			return
		}
		done <- result
	}()
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	if err := h.m.CloseOwner(context.Background(), "owner-a"); err != nil {
		t.Fatalf("CloseOwner: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("CloseOwner took %s", elapsed)
	}
	select {
	case result := <-done:
		if result.Succeeded() {
			t.Fatalf("in-flight execution completed after owner close: %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("in-flight execution did not return after owner close")
	}
	state, err := h.m.Session(context.Background(), session.ID, "owner-a")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if state.Alive() {
		t.Fatalf("session still alive after owner close: %q", state.State)
	}
}

func TestForgetOwnerRemovesPrivateState(t *testing.T) {
	h, _ := newFixtureHarness(t)
	session := h.create("fixture", "owner-a", "owner-a")
	kernel := session.Workspace
	if _, err := os.Stat(kernel); err != nil {
		t.Fatalf("kernel dir: %v", err)
	}
	if err := h.m.ForgetOwner(context.Background(), "owner-a"); err != nil {
		t.Fatalf("ForgetOwner: %v", err)
	}
	if _, err := os.Stat(kernel); !os.IsNotExist(err) {
		t.Fatalf("kernel dir survived ForgetOwner: %v", err)
	}
	if _, err := h.m.Session(context.Background(), session.ID, "owner-a"); !errors.Is(err, evalapi.ErrNotFound) {
		t.Fatalf("session record survived ForgetOwner: %v", err)
	}
}

func TestResetRequiresOwnerAndBumpsGeneration(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	session := h.create("fixture", "owner-a", "owner-a")
	before, err := h.m.Session(ctx, session.ID, "owner-a")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.m.Reset(ctx, session.ID, "owner-a"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	after, err := h.m.Session(ctx, session.ID, "owner-a")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if after.Generation <= before.Generation {
		t.Fatalf("generation did not advance: %d -> %d", before.Generation, after.Generation)
	}
	if err := h.m.Reset(ctx, session.ID, "stranger"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("stranger reset = %v", err)
	}
}

func TestSessionListOnlyShowsAccessibleKernels(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	mine := h.create("fixture", "owner-a", "owner-a")
	theirs := h.create("fixture", "owner-b", "owner-b")
	visible, err := h.m.Sessions(ctx, "owner-a")
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	ids := make(map[string]bool, len(visible))
	for _, item := range visible {
		ids[item.ID] = true
	}
	if !ids[mine.ID] || ids[theirs.ID] {
		t.Fatalf("owner-a sees %v (mine=%s theirs=%s)", ids, mine.ID, theirs.ID)
	}
	if err := h.m.Grant(ctx, theirs.ID, "owner-b", evalapi.Grant{AgentID: "owner-a", Role: evalapi.RoleObserver}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	visible, err = h.m.Sessions(ctx, "owner-a")
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	shared := false
	for _, item := range visible {
		if item.ID == theirs.ID {
			shared = true
			if item.OwnerID != "owner-b" {
				t.Fatalf("shared session owner = %q", item.OwnerID)
			}
		}
	}
	if !shared {
		t.Fatalf("observer grant did not appear in the agent's session list")
	}
	if !strings.HasPrefix(mine.Workspace, resolvePath(h.workspace)+string(os.PathSeparator)) {
		t.Fatalf("kernel workspace %q outside namespace", mine.Workspace)
	}
}

// TestRevokeCancelsQueuedAndRunningExecutions: a grantee never runs a cell in the
// owner's kernel at all. A cross-domain cell is refused at admission, and the
// owner's running cell is unaffected by the revocation.
func TestRevokeCancelsQueuedAndRunningExecutions(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	session := h.create("fixture", "owner-a", "owner-a")
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "executor-b", Role: evalapi.RoleObserver,
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	// Hold the kernel with a long owner cell, then attempt a grantee cell behind it.
	ownerDone := make(chan evalapi.Result, 1)
	go func() {
		result, err := h.m.Execute(ctx, evalapi.Request{
			ID: "owner-long", SessionID: session.ID, CellID: "c", AgentID: "owner-a", Code: "sleep 600",
		}, nil)
		if err != nil {
			ownerDone <- evalapi.Result{Status: evalapi.ResultStatusCrashed, Error: err.Error()}
			return
		}
		ownerDone <- result
	}()
	time.Sleep(200 * time.Millisecond)

	queuedDone := make(chan error, 1)
	go func() {
		_, err := h.m.Execute(ctx, evalapi.Request{
			ID: "executor-queued", SessionID: session.ID, CellID: "c", AgentID: "executor-b", Code: "1 + 1",
		}, nil)
		queuedDone <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := h.m.Revoke(ctx, session.ID, "owner-a", "executor-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	select {
	case err := <-queuedDone:
		if !errors.Is(err, evalapi.ErrAuthorityDomain) {
			t.Fatalf("cross-domain execution = %v, want ErrAuthorityDomain", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("cross-domain execution did not return")
	}
	select {
	case result := <-ownerDone:
		if !result.Succeeded() {
			t.Fatalf("owner cell was affected by revoke: %+v", result)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("owner execution did not finish")
	}
}

// TestRevokeDropsReadAccessImmediately: revoking a read grant removes the
// grantee's ability to read the owner's results and artifacts.
func TestRevokeDropsReadAccessImmediately(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	session := h.create("fixture", "owner-a", "owner-a")
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "observer-b", Role: evalapi.RoleObserver,
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := h.m.Results(ctx, session.ID, "observer-b"); err != nil {
		t.Fatalf("grantee read before revoke: %v", err)
	}
	start := time.Now()
	if err := h.m.Revoke(ctx, session.ID, "owner-a", "observer-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := h.m.Results(ctx, session.ID, "observer-b"); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("revoked grantee can still read: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("revocation took %s", elapsed)
	}
}

// TestGranteeCannotStartACellInTheOwnersKernel: the removed shared-heap path is
// refused at admission, immediately and without touching the owner's process.
func TestGranteeCannotStartACellInTheOwnersKernel(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	session := h.create("fixture", "owner-a", "owner-a")
	if err := h.m.Grant(ctx, session.ID, "owner-a", evalapi.Grant{
		AgentID: "executor-b", Role: evalapi.RoleObserver,
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	start := time.Now()
	if _, err := h.m.Execute(ctx, evalapi.Request{
		ID: "cross-domain", SessionID: session.ID, CellID: "c", AgentID: "executor-b", Code: "sleep 20000",
	}, nil); !errors.Is(err, evalapi.ErrAuthorityDomain) {
		t.Fatalf("cross-domain execute = %v, want ErrAuthorityDomain", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cross-domain admission took %s", elapsed)
	}
	if err := h.m.Revoke(ctx, session.ID, "owner-a", "executor-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// The owner's kernel and its capability are untouched.
	h.expectCompleted(session, "owner-a", "let n = 1")
}
