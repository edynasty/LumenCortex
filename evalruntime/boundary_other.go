//go:build !linux

package evalruntime

import (
	"context"
	"github.com/edynasty/LumenCortex/evalapi"
)

func (m *Manager) inspectBoundary(ctx context.Context, w *worker, binding evalapi.BoundaryContext) evalapi.BoundaryReport {
	r := newBoundaryReport(binding)
	for name := range r.Checks {
		r.Checks[name] = evalapi.BoundaryCheck{Status: evalapi.BoundaryUnknown, Evidence: "Linux host boundary assertion is unavailable on this platform"}
	}
	r.Decide()
	return r
}
