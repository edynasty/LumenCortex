package evalruntime

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

// fixtureManifest declares the third-party runtime exactly as a plugin would.
func fixtureManifest(t *testing.T) evalapi.Manifest {
	exe := requireFixture(t)
	return evalapi.Manifest{
		ID:             "fixture",
		Version:        "1.0.0",
		SPIVersion:     evalapi.SPIVersion,
		Language:       "fixture",
		RuntimeVersion: "fixture-1.0",
		Executable:     exe,
		Capabilities: []evalapi.Capability{
			{Name: evalapi.CapPersistent, Supported: true},
			{Name: evalapi.CapArtifact, Supported: true},
			{Name: evalapi.CapDisplay, Supported: true},
			{Name: evalapi.CapToolCallback, Supported: true},
			{Name: evalapi.CapSubprocess, Supported: true},
			{Name: evalapi.CapInterrupt, Supported: false, Notes: "runs to completion; the host terminates the process group"},
		},
		Platforms:     []string{runtime.GOOS},
		Sandbox:       evalapi.SandboxRequirement{Required: true},
		ArtifactTypes: []string{"text/plain"},
	}
}

func newFixtureHarness(t *testing.T) (*harness, evalapi.Manifest) {
	t.Helper()
	h := newHarness(t, nil)
	manifest := fixtureManifest(t)
	if err := h.m.Register(manifest); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return h, manifest
}

// TestPluginLifecycle is the §10 acceptance path for a new language: register,
// discover, capability query, create, execute, reset, close, and dispose without
// touching the Core agent loop.
func TestPluginLifecycle(t *testing.T) {
	h, manifest := newFixtureHarness(t)
	ctx := context.Background()

	discovered, err := h.m.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	found := false
	for _, item := range discovered {
		if item.ID != manifest.ID {
			continue
		}
		found = true
		if item.Language != "fixture" {
			t.Fatalf("language = %q", item.Language)
		}
		if !item.Supports(evalapi.CapPersistent) {
			t.Fatalf("persistent capability not declared")
		}
		if item.Supports(evalapi.CapInterrupt) {
			t.Fatalf("fixture must not claim interrupt support")
		}
	}
	if !found {
		t.Fatalf("fixture runtime is not discoverable")
	}

	session := h.create("fixture", "owner-a", "owner-a")
	if session.State != evalapi.StateReady {
		t.Fatalf("state after create = %q", session.State)
	}
	if !strings.HasPrefix(session.Workspace, h.workspace) {
		t.Fatalf("kernel workspace %q is not inside the host namespace %q", session.Workspace, h.workspace)
	}
	if session.Generation != 1 {
		t.Fatalf("generation = %d", session.Generation)
	}

	first := h.expectCompleted(session, "owner-a", "let x = 2 + 3\nprint x")
	if strings.TrimSpace(first.Stdout) != "5" {
		t.Fatalf("stdout = %q", first.Stdout)
	}
	second := h.expectCompleted(session, "owner-a", "x * 10")
	if len(second.Displays) != 1 || second.Displays[0].Text != "50" {
		t.Fatalf("displays = %+v", second.Displays)
	}
	if second.Generation != first.Generation {
		t.Fatalf("generation changed across a completed cell: %d -> %d", first.Generation, second.Generation)
	}

	if err := h.m.Reset(ctx, session.ID, "owner-a"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	afterReset := h.exec(session, "owner-a", "print x", evalapi.Budget{})
	if afterReset.Succeeded() {
		t.Fatalf("variable survived reset: %q", afterReset.Stdout)
	}
	if !strings.Contains(afterReset.Error, "undefined variable") {
		t.Fatalf("error = %q", afterReset.Error)
	}

	health, err := h.m.Health(ctx, session.ID, "owner-a")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.WorkerAlive || health.Generation < 2 {
		t.Fatalf("health = %+v", health)
	}

	if err := h.m.Close(ctx, session.ID, "owner-a"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := h.m.Session(ctx, session.ID, "owner-a")
	if err != nil {
		t.Fatalf("Session after close: %v", err)
	}
	if closed.State != evalapi.StateStopped {
		t.Fatalf("state after close = %q", closed.State)
	}
	if _, err := h.m.Execute(ctx, evalapi.Request{ID: "late", SessionID: session.ID, CellID: "c", Code: "1", AgentID: "owner-a"}, nil); err == nil {
		t.Fatalf("executing a closed session must fail")
	}

	if err := h.m.Dispose(ctx); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if _, err := h.m.Discover(ctx); !errors.Is(err, evalapi.ErrClosed) {
		t.Fatalf("Discover after Dispose = %v", err)
	}
}

func TestVersionNegotiationRejectsIncompatibleSPI(t *testing.T) {
	h := newHarness(t, nil)
	manifest := fixtureManifest(t)
	manifest.SPIVersion = "2.0"
	if err := h.m.Register(manifest); err == nil {
		t.Fatalf("registering an incompatible SPI must fail")
	}
	manifest.SPIVersion = evalapi.SPIVersion
	manifest.ID = "bad-language"
	manifest.Language = ""
	if err := h.m.Register(manifest); err == nil {
		t.Fatalf("registering a manifest without a language must fail")
	}
}

func TestUnavailableRuntimeIsNotDiscovered(t *testing.T) {
	h := newHarness(t, func(opts *Options) {
		opts.Python = "/nonexistent/python3"
	})
	discovered, err := h.m.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, item := range discovered {
		if item.ID == "python" {
			t.Fatalf("an unavailable interpreter must not be offered")
		}
	}
	if _, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "python", OwnerID: "o", AgentID: "o", Workspace: h.workspace, Permission: "workspace",
	}); !errors.Is(err, evalapi.ErrNotFound) {
		t.Fatalf("Create with a missing interpreter = %v", err)
	}
}

func TestUnknownRuntimeAndSession(t *testing.T) {
	h, _ := newFixtureHarness(t)
	ctx := context.Background()
	if _, err := h.m.Create(ctx, evalapi.CreateOptions{
		RuntimeID: "nope", OwnerID: "o", AgentID: "o", Workspace: h.workspace, Permission: "workspace",
	}); !errors.Is(err, evalapi.ErrNotFound) {
		t.Fatalf("Create unknown runtime = %v", err)
	}
	if _, err := h.m.Session(ctx, "missing", "o"); !errors.Is(err, evalapi.ErrNotFound) {
		t.Fatalf("Session missing = %v", err)
	}
	if err := h.m.Interrupt(ctx, "missing", "o"); !errors.Is(err, evalapi.ErrNotFound) {
		t.Fatalf("Interrupt missing = %v", err)
	}
}

func TestKernelCapIsEnforced(t *testing.T) {
	h, _ := newFixtureHarness(t)
	// The default cap is 2: a mixed-language host must be able to hold Python and
	// JavaScript kernels at once, but not an unbounded number.
	first := h.create("fixture", "owner-a", "owner-a")
	second := h.create("fixture", "owner-a", "owner-a")
	if first.ID == second.ID {
		t.Fatalf("two kernels share an id")
	}
	if _, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "fixture", OwnerID: "owner-a", AgentID: "owner-a", Workspace: h.workspace, Permission: "workspace",
	}); !errors.Is(err, evalapi.ErrLimit) {
		t.Fatalf("third kernel = %v, want ErrLimit", err)
	}
	if err := h.m.Close(context.Background(), first.ID, "owner-a"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "fixture", OwnerID: "owner-a", AgentID: "owner-a", Workspace: h.workspace, Permission: "workspace",
	}); err != nil {
		t.Fatalf("create after close: %v", err)
	}
}

func TestLeaseExpiry(t *testing.T) {
	h, _ := newFixtureHarness(t)
	session, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "fixture", OwnerID: "owner-a", AgentID: "owner-a",
		Workspace: h.workspace, Permission: "workspace",
		LeaseUntil: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = h.m.Execute(context.Background(), evalapi.Request{
		ID: "r", SessionID: session.ID, CellID: "c", Code: "1", AgentID: "owner-a",
	}, nil)
	if !errors.Is(err, evalapi.ErrExpired) {
		t.Fatalf("Execute after lease expiry = %v", err)
	}
}

func TestWorkspaceMustExist(t *testing.T) {
	h, _ := newFixtureHarness(t)
	missing := h.workspace + "/does-not-exist"
	if _, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "fixture", OwnerID: "o", AgentID: "o", Workspace: missing, Permission: "workspace",
	}); err == nil {
		t.Fatalf("creating a kernel in a missing workspace must fail")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatalf("the manager must not create the host namespace")
	}
}
