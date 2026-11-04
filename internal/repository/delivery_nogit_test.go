package repository

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func TestDeliveryNativeFileChangeWithoutGit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	repo, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := &taskstate.DeliveryContract{Files: []taskstate.DeliveryFile{{Path: "result.txt", Changed: true}}}
	if err := repo.CaptureDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	result := repo.VerifyDelivery(context.Background(), d)
	if result.Status != taskstate.DeliveryVerified {
		t.Fatalf("no-Git delivery: %+v", result)
	}
}
