package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/toolset"
)

// TestHubHandoffThroughASymlinkedWorkspaceReleasesTheSourcesWriteLeases pins FFINAL-HUB-01: a workspace
// opened through a symlink stores a write lease with Root as the workspace was spelled and Target with its
// symlinks resolved. The handoff must release the stopped source's leases by that stored identity, so the
// handoff succeeds, the target owns the task and can write, and a lease of an unrelated holder survives.
func TestHubHandoffThroughASymlinkedWorkspaceReleasesTheSourcesWriteLeases(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	realWorkspace, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Workspace: alias})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	source := newParentSession(t, e, p)
	target := newParentSession(t, e, p)
	const observer = "observer-agent"
	ctx := context.Background()

	// Acquire, list and release through the symlinked workspace: the stored lease names the real path
	// while its root keeps the alias, which is the identity the handoff has to release.
	scratch, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "scratch.txt", Holder: source})
	if err != nil {
		t.Fatal(err)
	}
	if scratch.Root != alias || scratch.Target != filepath.Join(realWorkspace, "scratch.txt") {
		t.Fatalf("lease identity through the symlinked workspace: root %q target %q (want root %q, target under %q)",
			scratch.Root, scratch.Target, alias, realWorkspace)
	}
	if !holdsWriteLease(leaseList(t, e), source, scratch.Target) {
		t.Fatalf("the acquired lease is not listed: %+v", leaseList(t, e))
	}
	if err := e.ReleaseWriteLease(ctx, WriteLeaseRequest{Path: "scratch.txt", Holder: source}); err != nil {
		t.Fatal(err)
	}
	if holdsWriteLease(leaseList(t, e), source, scratch.Target) {
		t.Fatalf("the released lease is still listed: %+v", leaseList(t, e))
	}

	if err := e.ConfigureHub(ctx, source, []HubTaskSpec{{ID: "u1", Goal: "HUB:unit"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx, source, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	p.waitRequest(t)
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: source, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "shared.txt", Holder: source}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "pkg", Holder: source, Prefix: true}); err != nil {
		t.Fatal(err)
	}
	unrelated, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "other.txt", Holder: observer})
	if err != nil {
		t.Fatal(err)
	}

	got, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target, Note: "own the workspace"})
	if err != nil || got != target {
		t.Fatalf("handoff through a symlinked workspace: %q, %v (want %s)", got, err, target)
	}
	p.waitCount(t, 2)
	if sup.Active(source) {
		t.Error("the handoff source still runs")
	}
	if !sup.Active(target) {
		t.Error("the handoff target does not run")
	}

	// Task Lease: the target owns the unit with the receiver's lifetime; the source holds nothing.
	leases, err := e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != target || leases[0].Expired ||
		!leases[0].ExpiresAt.After(time.Now().Add(DefaultHubTaskLeaseTTL-2*time.Minute)) {
		t.Fatalf("task leases after the handoff: %+v (want u1 held by %s for the receiver's lifetime)", leases, target)
	}
	held, err := e.store.TaskLeasesHeldBy(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Fatalf("the stopped source still holds task leases: %+v", held)
	}

	// Write leases: none of the source's remain; the unrelated holder's lease is untouched.
	remaining := leaseList(t, e)
	for _, lease := range remaining {
		if lease.Holder == source {
			t.Fatalf("the stopped source still holds write lease %+v", lease)
		}
	}
	if len(remaining) != 1 || remaining[0].Holder != observer || remaining[0].Target != unrelated.Target ||
		remaining[0].Prefix != unrelated.Prefix || !remaining[0].Expires.Equal(unrelated.Expires) {
		t.Fatalf("write leases after the handoff: %+v (want only the unchanged lease %+v)", remaining, unrelated)
	}

	// The durable event projects metadata: counts survive, while target is intentionally omitted.
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: source, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var handoffs []map[string]any
	for _, event := range page.Events {
		if event.Type != "agent.handoff" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		handoffs = append(handoffs, data)
	}
	if len(handoffs) != 1 || handoffs[0]["leases"] != float64(1) || handoffs[0]["writeLeases"] != float64(2) {
		t.Fatalf("agent.handoff events: %+v (want one with leases 1, writeLeases 2)", handoffs)
	}

	// The target writes the path the source leased, and is still refused the unrelated holder's path.
	files, err := toolset.New(toolset.Options{Workspace: alias, Policy: toolset.PolicyFull, SessionID: target, Editing: e.editing})
	if err != nil {
		t.Fatal(err)
	}
	result, err := files.Execute(ctx, "write_file", map[string]any{"path": "shared.txt", "content": "handed over\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("the handoff target cannot write the path the source leased: %s", result.Content)
	}
	result, err = files.Execute(ctx, "write_file", map[string]any{"path": "other.txt", "content": "not mine\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Content, "lease_conflict") {
		t.Fatalf("the target wrote a path another holder leases: %s", result.Content)
	}
	if _, err := os.Stat(filepath.Join(realDir, "other.txt")); !os.IsNotExist(err) {
		t.Fatalf("a refused write created the leased file: %v", err)
	}

	sup.Cancel(target)
	waitInactive(t, sup, source, target)
}

func holdsWriteLease(leases []WriteLease, holder, target string) bool {
	for _, lease := range leases {
		if lease.Holder == holder && lease.Target == target {
			return true
		}
	}
	return false
}
