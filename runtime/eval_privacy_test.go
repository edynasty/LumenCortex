package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEvalPrivacyDeletionPurgesOnlyOwnerArtifacts(t *testing.T) {
	e, owner := executionTestEngine(t)
	ctx := context.Background()
	runtimeID := executionRuntimeID(t, e, "python")
	first, err := e.CreateEvalSession(ctx, owner, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.NewSession(ctx, SessionOptions{Goal: "retained owner"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.CreateEvalSession(ctx, other.ID, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	executeHostCell(t, e, owner, first.ID, "private-one", "private_value=31\nopen('private.txt','w').write('synthetic-private-one')")
	executeHostCell(t, e, other.ID, second.ID, "private-two", "private_value=47\nopen('private.txt','w').write('synthetic-private-two')")
	if _, err := e.DeleteSession(ctx, owner, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.repoDir, "eval-workspaces", owner)); !os.IsNotExist(err) {
		t.Fatalf("deleted owner's private directory survived: %v", err)
	}
	retained := executeHostCell(t, e, other.ID, second.ID, "retained", "print(private_value)")
	if retained.Stdout != "47\n" {
		t.Fatalf("another owner's state was removed: %#v", retained)
	}
	if _, err := e.CreateEvalSession(ctx, owner, runtimeID); err == nil {
		t.Fatal("deleted owner recreated kernel")
	}
}
