package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteLeaseByListedTargetThroughASymlinkedWorkspace pins FFINAL-EDIT-01: a workspace opened through a
// symlink lists a lease with Root as the workspace was spelled and Target with its symlinks resolved. A host
// renews and releases the lease by passing that Target back (Desktop's Editing panel releases with
// {path: lease.target, holder}), so the lease API must accept it as inside the workspace, leave another
// holder's lease alone, and still refuse a path that is really outside the workspace.
func TestWriteLeaseByListedTargetThroughASymlinkedWorkspace(t *testing.T) {
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
	outside := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(outside, []byte("not the workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Workspace: alias})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	ctx := context.Background()

	if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "a.txt", Holder: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "pkg", Holder: "session-a", Prefix: true}); err != nil {
		t.Fatal(err)
	}
	other, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "b.txt", Holder: "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	listed := listedLease(t, e, "session-a", filepath.Join(realWorkspace, "a.txt"))
	if listed.Root != alias {
		t.Fatalf("listed lease root %q, want the workspace as spelled (%q)", listed.Root, alias)
	}

	// Renew by the listed Target, as a host holding the listed lease does.
	renewed, err := e.RenewWriteLease(ctx, WriteLeaseRequest{Path: listed.Target, Holder: "session-a", TTLSeconds: 3600})
	if err != nil {
		t.Fatalf("renew by the listed target: %v", err)
	}
	if renewed.Target != listed.Target || !renewed.Expires.After(listed.Expires) {
		t.Fatalf("renewed lease %+v, want %s extended past %s", renewed, listed.Target, listed.Expires)
	}

	// Release exactly as the Editing panel does: the listed target and holder, no root.
	if err := e.ReleaseWriteLease(ctx, WriteLeaseRequest{Path: listed.Target, Holder: "session-a"}); err != nil {
		t.Fatalf("release by the listed target: %v", err)
	}
	// Release with the listed Root and Target together, as a host echoing the whole lease does.
	prefix := listedLease(t, e, "session-a", filepath.Join(realWorkspace, "pkg"))
	if err := e.ReleaseWriteLease(ctx, WriteLeaseRequest{Root: prefix.Root, Path: prefix.Target, Holder: "session-a", Prefix: prefix.Prefix}); err != nil {
		t.Fatalf("release by the listed root and target: %v", err)
	}
	remaining, err := e.WriteLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Holder != "session-b" || remaining[0].Target != other.Target ||
		!remaining[0].Expires.Equal(other.Expires) {
		t.Fatalf("leases after the releases: %+v (want only session-b's unchanged %+v)", remaining, other)
	}

	// A path that is really outside the workspace stays refused, however it is spelled.
	for _, path := range []string{outside, "../outside.txt", filepath.Join(alias, "..", "outside.txt")} {
		if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: path, Holder: "session-a"}); err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
			t.Errorf("acquire %q: %v, want a workspace escape", path, err)
		}
		if _, err := e.RenewWriteLease(ctx, WriteLeaseRequest{Path: path, Holder: "session-a"}); err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
			t.Errorf("renew %q: %v, want a workspace escape", path, err)
		}
		if err := e.ReleaseWriteLease(ctx, WriteLeaseRequest{Path: path, Holder: "session-b"}); err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
			t.Errorf("release %q: %v, want a workspace escape", path, err)
		}
	}
	if after, err := e.WriteLeases(ctx); err != nil || len(after) != 1 || after[0].Target != other.Target {
		t.Fatalf("leases after the refused requests: %+v, %v", after, err)
	}
}

func listedLease(t *testing.T, e *Engine, holder, target string) WriteLease {
	t.Helper()
	leases, err := e.WriteLeases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases {
		if lease.Holder == holder && lease.Target == target {
			return lease
		}
	}
	t.Fatalf("no lease of %s on %s is listed: %+v", holder, target, leases)
	return WriteLease{}
}
