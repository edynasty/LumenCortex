package evalruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

func TestOutputIsBoundedWhileStreaming(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	result := h.exec(session, "owner", `print("x" * 400000)`, evalapi.Budget{OutputBytes: 8 << 10})
	if !result.Succeeded() {
		t.Fatalf("cell failed: %+v", result)
	}
	if len(result.Stdout) > 16<<10 {
		t.Fatalf("stdout was not bounded: %d bytes", len(result.Stdout))
	}
	if !result.Usage.OutputTruncated {
		t.Fatalf("truncation was not reported: %+v", result.Usage)
	}
	if result.Usage.OutputBytes <= 0 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestMemoryBoundKillsRunawayWorker(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) {
		opts.Budget.MemoryBytes = 128 << 20
	})
	session := h.create("python", "owner", "owner")
	start := time.Now()
	result := h.exec(session, "owner", `
import time
chunks = []
for _ in range(80):
    chunks.append(bytearray(8 * 1024 * 1024))
    time.sleep(0.02)
print("finished")
`, evalapi.Budget{WallSeconds: 30})
	elapsed := time.Since(start)
	if result.Succeeded() {
		t.Fatalf("runaway allocation completed: %+v", result)
	}
	if result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("state = %q", result.StateStatus)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("memory bound did not fire quickly: %s", elapsed)
	}
	// The kernel is restarted for the next cell.
	h.expectCompleted(session, "owner", "1 + 1")
}

func TestCPUBoundIsEnforcedByTheKernel(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) {
		opts.Budget.CPUSeconds = 1
	})
	session := h.create("python", "owner", "owner")
	start := time.Now()
	result := h.exec(session, "owner", `
x = 0
while True:
    x += 1
`, evalapi.Budget{WallSeconds: 40})
	elapsed := time.Since(start)
	if result.Succeeded() {
		t.Fatalf("CPU-bound loop completed: %+v", result)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("CPU limit did not fire: %s", elapsed)
	}
}

func TestDiskBoundLimitsOneFile(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) {
		opts.Budget.DiskBytes = 1 << 20
	})
	session := h.create("python", "owner", "owner")
	result := h.exec(session, "owner", `
try:
    with open("big.bin", "wb") as handle:
        handle.write(b"x" * (4 * 1024 * 1024))
    print("wrote")
except OSError as exc:
    print("limited", exc.errno)
`, evalapi.Budget{WallSeconds: 20})
	if result.Succeeded() && strings.Contains(result.Stdout, "wrote") {
		// If the write reported success, the file must still be bounded.
		info, err := os.Stat(filepath.Join(session.Workspace, "work", "big.bin"))
		if err == nil && info.Size() > 2<<20 {
			t.Fatalf("disk limit did not bound the file: %d bytes", info.Size())
		}
		return
	}
	if result.Succeeded() && !strings.Contains(result.Stdout, "limited") {
		t.Fatalf("unexpected output: %q", result.Stdout)
	}
}

func TestKernelCapCountsIdleWorkers(t *testing.T) {
	h, _ := newFixtureHarness(t)
	first := h.create("fixture", "owner-a", "owner-a")
	_ = h.create("fixture", "owner-a", "owner-a")
	if err := h.m.Close(testContext(t), first.ID, "owner-a"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A closed kernel no longer counts, so a new one fits.
	h.create("fixture", "owner-a", "owner-a")
}
