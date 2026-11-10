package evalruntime

import (
	"fmt"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/procrt"
)

// The V8 heap cap is a fraction of the kernel's memory budget, not the whole
// of it: the budget bounds the resident set, while --max-old-space-size bounds
// only the JS heap. Node's own baseline (isolate, libuv, compiled code) and
// external memory (Buffer, ArrayBuffer) are outside that heap, so a cap equal to
// MemoryBytes would let the process exceed the RSS budget before V8 noticed.
const (
	javascriptHeapNumerator   = 3
	javascriptHeapDenominator = 5
	javascriptHeapFloorBytes  = 32 << 20
	javascriptHeapCeilBytes   = 4 << 30
)

// javascriptHeapCap is the V8 old-space cap for a kernel with memoryBytes of
// RSS budget, or 0 when the runtime should keep its own default (no budget).
func javascriptHeapCap(memoryBytes int64) int64 {
	if memoryBytes <= 0 {
		return 0
	}
	heap := memoryBytes * javascriptHeapNumerator / javascriptHeapDenominator
	if heap < javascriptHeapFloorBytes {
		heap = javascriptHeapFloorBytes
	}
	if heap > javascriptHeapCeilBytes {
		heap = javascriptHeapCeilBytes
	}
	return heap &^ (1<<20 - 1) // whole MiB
}

// javascriptRuntimeArgs returns the node flags that make V8 enforce the heap
// cap. They must precede the worker script.
func javascriptRuntimeArgs(memoryBytes int64) []string {
	heap := javascriptHeapCap(memoryBytes)
	if heap <= 0 {
		return nil
	}
	mib := heap >> 20
	semi := mib / 4
	if semi > 16 {
		semi = 16
	}
	if semi < 1 {
		semi = 1
	}
	return []string{
		fmt.Sprintf("--max-old-space-size=%d", mib),
		fmt.Sprintf("--max-semi-space-size=%d", semi),
	}
}

// enforcementView reports how one kernel's bounds are actually applied. The
// language matters for memory: Python has no interpreter-enforced heap cap,
// while Node's V8 does.
func enforcementView(backend, language string, budget evalapi.Budget, memory string) evalapi.EnforcementView {
	view := evalapi.EnforcementView{
		Memory:           memory,
		CPU:              evalapi.EnforcementHard,
		Disk:             evalapi.EnforcementHard,
		MemoryLimitBytes: budget.MemoryBytes,
	}
	if backend == "" {
		view.Memory, view.CPU, view.Disk = evalapi.EnforcementNone, evalapi.EnforcementNone, evalapi.EnforcementNone
	}
	if memory == evalapi.EnforcementSupervised {
		view.MemoryPollMS = memoryPoll.Milliseconds()
	}
	if language == "javascript" {
		view.MemoryRuntimeCapBytes = javascriptHeapCap(budget.MemoryBytes)
	}
	view.Notes = []string{
		"cpu: RLIMIT_CPU via ulimit -t, enforced by the kernel (SIGXCPU)",
		memoryNote(view.Memory),
		"disk: RLIMIT_FSIZE via ulimit -f bounds one file; the workspace total is supervised and the artifacts have their own quota",
	}
	if view.MemoryRuntimeCapBytes > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("memory: V8 heap cap --max-old-space-size=%d MiB is interpreter-enforced (exit 134 on exhaustion); external allocations such as Buffer stay under the RSS bound", view.MemoryRuntimeCapBytes>>20))
	}
	return view
}

// procrtEnforcement maps a kernel's enforcement report onto the process census.
func procrtEnforcement(view evalapi.EnforcementView) procrt.Enforcement {
	return procrt.Enforcement{
		Memory:                view.Memory,
		CPU:                   view.CPU,
		Disk:                  view.Disk,
		MemoryLimitBytes:      view.MemoryLimitBytes,
		MemoryRuntimeCapBytes: view.MemoryRuntimeCapBytes,
		MemoryPollMS:          view.MemoryPollMS,
		Notes:                 append([]string(nil), view.Notes...),
	}
}

// sandboxBackend is the backend the launcher actually runs, which is the host's
// answer when Options.Sandbox.Backend left the choice to the platform.
func (m *Manager) sandboxBackend() string {
	if m == nil || m.launcher == nil {
		return ""
	}
	return m.launcher.backend
}
