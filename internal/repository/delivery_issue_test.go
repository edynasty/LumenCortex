package repository

import (
	"context"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func TestDeliveryIssueDedupPreservesDistinctCauses(t *testing.T) {
	repo, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []taskstate.DeliveryIssue{
		{Phase: "baseline", Path: "a", Reason: "denied"},
		{Phase: "baseline", Path: "a", Reason: "timeout"},
		{Phase: "baseline", Path: "b", Reason: "denied"},
		{Phase: "workspace", Path: "b", Reason: "denied"},
	}
	d := &taskstate.DeliveryContract{
		Files:          []taskstate.DeliveryFile{{Path: "a", Changed: true}, {Path: "b", Changed: true}},
		BaselineIssues: []taskstate.DeliveryIssue{want[0], want[0], want[1], want[2], want[3], want[2]},
	}
	r := repo.VerifyDelivery(context.Background(), d)
	if r.Status != taskstate.DeliveryUnavailable || !slices.Equal(r.Issues, want) || r.Files[0].Change != "unknown" || r.Files[1].Change != "unknown" {
		t.Fatalf("distinct phase/path/cause lost: %+v", r)
	}
}
