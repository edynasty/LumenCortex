package runtime

import (
	"context"

	"github.com/edynasty/LumenCortex/internal/procrt"
)

// ProcessInfo is one supervised child process owned by a Core session: an eval
// kernel, a debug adapter, a job, a hook or a command. It is the Hub's census
// row: who owns the process, what it is, whether it runs, what it used, and how
// its bounds are really applied.
type ProcessInfo struct {
	Owner     procrt.Owner `json:"owner"`
	PID       int          `json:"pid"`
	TreePIDs  []int        `json:"treePids,omitempty"`
	State     string       `json:"state"`
	StartedAt string       `json:"startedAt,omitempty"`
	// Usage is what the host measured; an unmeasured field is zero, never an
	// estimate.
	CPUMS           int64 `json:"cpuMs,omitempty"`
	RSSBytes        int64 `json:"rssBytes,omitempty"`
	MemoryPeakBytes int64 `json:"memoryPeakBytes,omitempty"`
	// Enforcement says how each bound is applied (hard, runtime, supervised,
	// none), so a supervised kill is never displayed as a kernel limit.
	MemoryEnforcement     string   `json:"memoryEnforcement"`
	CPUEnforcement        string   `json:"cpuEnforcement"`
	DiskEnforcement       string   `json:"diskEnforcement"`
	MemoryLimitBytes      int64    `json:"memoryLimitBytes,omitempty"`
	MemoryRuntimeCapBytes int64    `json:"memoryRuntimeCapBytes,omitempty"`
	MemoryPollMS          int64    `json:"memoryPollMs,omitempty"`
	Notes                 []string `json:"notes,omitempty"`
}

// Processes returns the supervised process census for one Core session. It is
// additive host API for the Hub: eval kernels report their worker, and any other
// subsystem that registers with procrt appears in the same list.
//
// The census is per host process. When the eval host is the Linux UID broker, its
// privileged processes are not in this process's registry, so the list is empty
// there; that limitation is reported rather than fabricated.
func (e *Engine) Processes(ctx context.Context, sessionID string) ([]ProcessInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		return nil, err
	}
	rows := procrt.Default().List(procrt.Owner{SessionID: sessionID})
	out := make([]ProcessInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, ProcessInfo{
			Owner:                 row.Owner,
			PID:                   row.PID,
			TreePIDs:              append([]int(nil), row.TreePIDs...),
			State:                 row.State,
			StartedAt:             row.StartedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			CPUMS:                 row.Usage.CPUMs,
			RSSBytes:              row.Usage.RSSBytes,
			MemoryPeakBytes:       row.Usage.MemoryPeakBytes,
			MemoryEnforcement:     row.Enforcement.MemoryLimitLabel(),
			CPUEnforcement:        row.Enforcement.CPU,
			DiskEnforcement:       row.Enforcement.Disk,
			MemoryLimitBytes:      row.Enforcement.MemoryLimitBytes,
			MemoryRuntimeCapBytes: row.Enforcement.MemoryRuntimeCapBytes,
			MemoryPollMS:          row.Enforcement.MemoryPollMS,
			Notes:                 append([]string(nil), row.Enforcement.Notes...),
		})
	}
	return out, nil
}
