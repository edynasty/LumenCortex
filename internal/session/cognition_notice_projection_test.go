package session

import (
	"context"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// A revoked notice that no answered request saw can be projected away instead
// of permanently isolating an otherwise unexposed reader.
func TestCognitionNoticeProjectionBeforeModelExposure(t *testing.T) {
	s, _ := maintenanceStore(t)
	createSession(t, s, "maintainer", `{"purpose":"maintenance"}`)
	sourceThread(t, s, "maintainer")
	createSession(t, s, "reader", "")
	_, seqs, err := s.BeginRun(context.Background(), "reader", RunStart{
		Notices:       []protocol.Message{{Role: "user", Origin: protocol.OriginKnowledge, Content: "private/path.txt"}},
		NoticeSources: []string{"maintainer"},
	})
	if err != nil || len(seqs) != 1 {
		t.Fatalf("Start: %v %v", seqs, err)
	}
	if _, err := s.RevokeSession(context.Background(), "maintainer", "test"); err != nil {
		t.Fatal(err)
	}
	projection, err := s.Projection(context.Background(), "reader")
	if err != nil || projection.Isolated != "" || !slices.Equal(projection.Withheld, seqs) {
		t.Fatalf("unexposed notice not projectable: %+v %v", projection, err)
	}
}
