//go:build unix

package evaluator

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A schema path that names a FIFO inside the workspace is bound as absent and never opened: reading it
// would block every gate, SessionChecks and CheckApprovalState until a writer appeared.
func TestSchemaFIFOIsNotRead(t *testing.T) {
	f := newFixture(t, false)
	if err := syscall.Mkfifo(filepath.Join(f.dir, "schema.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan []Input, 1)
	go func() {
		done <- ResolveInputs(f.dir, []Spec{{ID: "s", Kind: KindSchema, Schema: "schema.json"}})[0].Inputs
	}()
	select {
	case inputs := <-done:
		if len(inputs) != 1 || inputs[0].Path != "schema.json" || inputs[0].Digest != AbsentInput {
			t.Fatalf("inputs %v, want schema.json absent", inputs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResolveInputs blocked on a FIFO schema")
	}
}
