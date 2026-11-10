package evalruntime

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/procrt"
)

// TestEnforcementReportIsTruthfulPerPlatform pins the label a host may publish:
// a supervised RSS monitor is never reported as hard, and only Linux cgroup
// memory.max is hard.
func TestEnforcementReportIsTruthfulPerPlatform(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	view := session.Enforcement
	if view.Memory == "" || view.CPU == "" || view.Disk == "" {
		t.Fatalf("enforcement is not reported: %+v", view)
	}
	if view.CPU != evalapi.EnforcementHard {
		t.Fatalf("cpu = %q; RLIMIT_CPU is kernel-enforced", view.CPU)
	}
	switch runtime.GOOS {
	case "darwin":
		if view.Memory != evalapi.EnforcementSupervised {
			t.Fatalf("darwin memory = %q; RLIMIT_AS/RLIMIT_DATA are rejected (EINVAL), so only a supervised poll exists", view.Memory)
		}
		if view.MemoryPollMS <= 0 {
			t.Fatalf("a supervised bound must report its poll interval: %+v", view)
		}
	case "linux":
		if view.Memory != evalapi.EnforcementHard {
			t.Fatalf("linux memory = %q, want hard (cgroup memory.max)", view.Memory)
		}
	}
	if view.MemoryLimitBytes != session.Budget.MemoryBytes {
		t.Fatalf("memory limit = %d, budget %d", view.MemoryLimitBytes, session.Budget.MemoryBytes)
	}
	// The census row repeats the same report, so a Hub cannot show a different
	// story than the session.
	rows := procrt.Default().List(procrt.Owner{SessionID: session.OwnerID})
	if len(rows) == 0 {
		// The kernel starts lazily; run a cell and re-read the census.
		h.expectCompleted(session, "owner", "1 + 1")
		rows = procrt.Default().List(procrt.Owner{SessionID: session.OwnerID})
	}
	if len(rows) != 1 {
		t.Fatalf("census = %+v", rows)
	}
	if rows[0].Enforcement.MemoryLimitLabel() != view.Memory {
		t.Fatalf("census says %q, session says %q", rows[0].Enforcement.MemoryLimitLabel(), view.Memory)
	}
}

// TestSupervisedMemoryBreachIsMeasured proves the supervised bound fires, the
// kernel is lost, the owner is told which bound and how it was applied, and the
// measured overshoot stays inside the poll-and-allocate window this host can
// actually promise.
func TestSupervisedMemoryBreachIsMeasured(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux enforces memory through cgroup memory.max; see TestCgroupMemoryBreachIsHard")
	}
	const limit = 128 << 20
	h, _ := newPythonHarness(t, func(opts *Options) {
		opts.Budget.MemoryBytes = limit
	})
	session := h.create("python", "owner", "owner")
	started := time.Now()
	result := h.exec(session, "owner", `
import time
chunks = []
for _ in range(80):
    chunks.append(bytearray(8 * 1024 * 1024))
    time.sleep(0.02)
print("finished")
`, evalapi.Budget{WallSeconds: 30})
	elapsed := time.Since(started)

	if result.Succeeded() {
		t.Fatalf("a runaway allocation completed: %+v", result)
	}
	if result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("state = %q, want lost", result.StateStatus)
	}
	if result.Limit != "memory" {
		t.Fatalf("limit = %q, want memory (error %q)", result.Limit, result.Error)
	}
	if result.Enforcement != evalapi.EnforcementSupervised {
		t.Fatalf("enforcement = %q; an RSS poll must never be reported as hard", result.Enforcement)
	}
	if !strings.Contains(result.Error, "memory limit exceeded") {
		t.Fatalf("error = %q", result.Error)
	}
	// Overshoot is measured, not assumed: the peak the host sampled is above the
	// limit by at most what the worker allocates within one poll interval plus
	// one sampling read of the group.
	peak := result.Usage.MemoryPeakBytes
	if peak <= limit {
		t.Fatalf("sampled peak %d is not above the %d byte limit; the measurement is missing", peak, limit)
	}
	overshoot := peak - limit
	t.Logf("supervised memory breach: limit=%d peak=%d overshoot=%d poll=%s wall=%s", limit, peak, overshoot, memoryPoll, elapsed)
	// The cell allocates up to ~400 MiB/s; one 200ms interval plus the ps read
	// cannot hide more than that. This bound is the documented overshoot window.
	if maxOvershoot := int64(96 << 20); overshoot > maxOvershoot {
		t.Fatalf("overshoot %d exceeds the measured window %d for pole %s", overshoot, maxOvershoot, memoryPoll)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("the bound did not fire quickly: %s", elapsed)
	}
	// The whole process tree is gone and the census is empty.
	deadline := time.Now().Add(10 * time.Second)
	for procrt.Default().Count() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("census still has %d rows after the breach", procrt.Default().Count())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The kernel is restarted for the next cell, in a new generation.
	after := h.expectCompleted(session, "owner", "1 + 1")
	if after.Generation <= result.Generation {
		t.Fatalf("generation did not advance after the loss: %d -> %d", result.Generation, after.Generation)
	}
}

// TestJavaScriptHeapCapIsRuntimeEnforced proves the V8 heap cap really bounds an
// allocation-heavy cell: the interpreter aborts the process, the kernel is lost,
// and the result says runtime, not supervised and not hard.
func TestJavaScriptHeapCapIsRuntimeEnforced(t *testing.T) {
	// The budget is deliberately larger than the heap cap plus Node's baseline
	// (about 50 MiB), so the interpreter-enforced cap fires before the RSS
	// monitor would: the two mechanisms are distinguishable.
	const limit = 512 << 20
	h := newJavaScriptHarness(t, func(opts *Options) {
		opts.Budget.MemoryBytes = limit
	})
	session := h.create("javascript", "owner", "owner")
	if session.Enforcement.MemoryRuntimeCapBytes <= 0 {
		t.Fatalf("the JS kernel does not report a V8 heap cap: %+v", session.Enforcement)
	}
	started := time.Now()
	result := h.exec(session, "owner", `
const keep = [];
for (;;) {
  const batch = [];
  for (let i = 0; i < 20000; i++) batch.push({ i, s: 'value-' + i });
  keep.push(batch);
}
`, evalapi.Budget{WallSeconds: 60})
	elapsed := time.Since(started)

	if result.Succeeded() {
		t.Fatalf("a heap-exhausting cell completed: %+v", result)
	}
	if result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("state = %q, want lost", result.StateStatus)
	}
	if result.Limit != "memory" || result.Enforcement != evalapi.EnforcementRuntime {
		t.Fatalf("limit=%q enforcement=%q; a V8 heap cap is interpreter-enforced", result.Limit, result.Enforcement)
	}
	if !strings.Contains(result.Error, "heap cap") && !strings.Contains(result.Error, "heap") {
		t.Fatalf("error = %q", result.Error)
	}
	if elapsed > 60*time.Second {
		t.Fatalf("the heap cap did not fire quickly: %s", elapsed)
	}
	h.expectCompleted(session, "owner", "1 + 1")
}

// TestJavaScriptExternalMemoryStaysSupervised proves the V8 cap is not the whole
// memory bound: Buffer allocations live outside the JS heap, so the supervised
// RSS monitor is still what stops them. Without it the cap alone would be a false
// claim of a memory bound.
func TestJavaScriptExternalMemoryStaysSupervised(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux enforces the RSS bound through cgroup memory.max")
	}
	const limit = 192 << 20
	h := newJavaScriptHarness(t, func(opts *Options) {
		opts.Budget.MemoryBytes = limit
	})
	session := h.create("javascript", "owner", "owner")
	result := h.exec(session, "owner", `
const chunks = [];
for (;;) { chunks.push(Buffer.alloc(16 * 1024 * 1024, 1)); }
`, evalapi.Budget{WallSeconds: 45})
	if result.Succeeded() {
		t.Fatalf("an external-memory runaway completed: %+v", result)
	}
	if result.StateStatus != evalapi.StateStatusLost || result.Limit != "memory" {
		t.Fatalf("result = %+v", result)
	}
	if result.Enforcement != evalapi.EnforcementSupervised {
		t.Fatalf("enforcement = %q; Buffer memory is outside the V8 heap cap and only the RSS monitor bounds it", result.Enforcement)
	}
	t.Logf("external memory breach: %q", result.Error)
}

// TestCgroupMemoryBreachIsHard is the Linux half of the memory proof: cgroup v2
// memory.max kills the worker, so the label is hard and the kernel is lost.
func TestCgroupMemoryBreachIsHard(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroup memory.max only exists on linux")
	}
	const limit = 96 << 20
	h, _ := newPythonHarness(t, func(opts *Options) {
		opts.Budget.MemoryBytes = limit
	})
	session := h.create("python", "owner", "owner")
	if session.Enforcement.Memory != evalapi.EnforcementHard {
		t.Fatalf("linux memory enforcement = %q", session.Enforcement.Memory)
	}
	result := h.exec(session, "owner", `
import time
chunks = []
for _ in range(80):
    chunks.append(bytearray(8 * 1024 * 1024))
    time.sleep(0.01)
`, evalapi.Budget{WallSeconds: 30})
	if result.Succeeded() || result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("result = %+v", result)
	}
	if result.Limit != "memory" || result.Enforcement != evalapi.EnforcementHard {
		t.Fatalf("limit=%q enforcement=%q", result.Limit, result.Enforcement)
	}
	if result.Usage.MemoryPeakBytes > 0 && result.Usage.MemoryPeakBytes > int64(limit)*2 {
		t.Fatalf("cgroup peak %d is implausible for memory.max=%d", result.Usage.MemoryPeakBytes, limit)
	}
	_ = fmt.Sprint()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.m.Close(ctx, session.ID, "owner"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := procrt.Default().List(procrt.Owner{SessionID: session.OwnerID}); len(got) != 0 {
		t.Fatalf("census after close = %+v", got)
	}
}
