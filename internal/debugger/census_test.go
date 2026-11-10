package debugger

import (
	"context"
	"testing"

	"github.com/edynasty/LumenCortex/internal/debugger/dapfake"
	"github.com/edynasty/LumenCortex/internal/procrt"
)

// TestAdapterIsInProcessCensus: a running adapter is visible in the shared
// process census under its debug session, and stopping the session withdraws it.
func TestAdapterIsInProcessCensus(t *testing.T) {
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5})
	owner := procrt.Owner{SessionID: snap.SessionID, Component: procrt.ComponentDAP, ObjectID: snap.ID}
	rows := procrt.Default().List(owner)
	if len(rows) != 1 || rows[0].PID != snap.AdapterPID || rows[0].PID == 0 {
		t.Fatalf("census rows %+v, adapter pid %d", rows, snap.AdapterPID)
	}
	if rows[0].Enforcement.Memory != procrt.None {
		t.Fatalf("adapter census claims an unverified memory bound: %+v", rows[0].Enforcement)
	}
	c := control(snap)
	c.Action = "stop"
	if _, err := m.Control(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if rows := procrt.Default().List(owner); len(rows) != 0 {
		t.Fatalf("census after stop %+v", rows)
	}
}
