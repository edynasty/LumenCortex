package debugger

import (
	"os"
	"testing"

	"github.com/edynasty/LumenCortex/internal/debugger/dapfake"
)

// TestFakeDAPAdapterHelper is the deterministic fake adapter process: tests
// start this test binary with dapfake.Args and -test.run selecting this test.
func TestFakeDAPAdapterHelper(t *testing.T) {
	served, err := dapfake.ServeIfSelected()
	if err != nil {
		t.Fatal(err)
	}
	if !served {
		t.Skip("only runs as a fake DAP adapter child")
	}
	os.Exit(0)
}
