package editing

import (
	"runtime"
	"testing"
)

// requireExistingFileUpdate skips a test whose subject needs LC to replace an existing file.
// Existing files are only replaced by an atomic exchange (fail-closed policy, OPUS-V-01); on a
// platform without one every update is refused with unsupported_filesystem and nothing is written.
// TestOpusV01ExistingFileUpdateRefusedWithoutExchange asserts that refusal with the real primitive.
func requireExistingFileUpdate(t *testing.T) {
	t.Helper()
	if !platformExchangesExistingFiles {
		t.Skipf("GOOS=%s has no atomic exchange primitive, so LC refuses existing-file updates (OPUS-V-01)", runtime.GOOS)
	}
}
