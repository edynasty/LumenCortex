package toolset

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactTransferUsesWorkspaceAndProtectedStateBoundary(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	state := filepath.Join(root, ".lumencortex")
	if err := os.MkdirAll(filepath.Join(state, "eval-workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "eval-workspaces", "private"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, Protected: Protected{Legacy: state}})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{0, 1, 128, 255, 10}
	if err := set.WriteArtifact(context.Background(), "data.bin", raw); err != nil {
		t.Fatal(err)
	}
	got, err := set.ReadArtifact(context.Background(), "data.bin", int64(len(raw)))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("binary bytes changed: %v %v", got, err)
	}
	if _, err := set.ReadArtifact(context.Background(), "data.bin", int64(len(raw)-1)); err == nil {
		t.Fatal("oversized import accepted")
	}
	if _, err := set.ReadArtifact(context.Background(), filepath.Join(outside, "secret"), 100); err == nil {
		t.Fatal("outside workspace was read")
	}
	if _, err := set.ReadArtifact(context.Background(), filepath.Join(state, "eval-workspaces", "private"), 100); !errors.Is(err, ErrProtectedPath) {
		t.Fatalf("private execution state exposed: %v", err)
	}
	if err := set.WriteArtifact(context.Background(), filepath.Join(outside, "new"), raw); err == nil {
		t.Fatal("outside workspace was written")
	}
	readonly, err := New(Options{Workspace: root, Policy: PolicyReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	if err := readonly.WriteArtifact(context.Background(), "data.bin", []byte("overwrite")); err == nil {
		t.Fatal("read-only grant wrote an artifact")
	}
	got, err = os.ReadFile(filepath.Join(root, "data.bin"))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("denied write changed data: %v %v", got, err)
	}
}
