package toolset

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewArtifactExportRespectsLease(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, []byte("owner B"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.edit.Leases().Acquire(root, path, "b", false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = s.WriteArtifact(context.Background(), "a.bin", []byte{0, 128, 255}); err == nil {
		t.Error("artifact bypassed competing write lease")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "owner B" {
		t.Fatalf("lease owner's data lost: %q", got)
	}
}

func TestReviewArtifactExportRejectsStaleRead(t *testing.T) {
	root := t.TempDir()
	s, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(context.Background(), "a.bin", []byte{0, 128, 255}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a.bin")
	if err := os.WriteFile(path, []byte("new user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(context.Background(), "a.bin", []byte("stale overwrite")); err == nil {
		t.Fatal("stale artifact replaced newer user data")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new user data" {
		t.Fatalf("new user data lost: %q", got)
	}
}
