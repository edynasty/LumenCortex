package runtime

import (
	"context"
	"github.com/edynasty/LumenCortex/evalapi"
)

// AssertEvalBoundary obtains new OS evidence for one owned kernel. Its report
// is not an admission token and no report supplied by a client is accepted.
func (e *Engine) AssertEvalBoundary(ctx context.Context, sessionID, evalID string, binding evalapi.BoundaryContext) (evalapi.BoundaryReport, error) {
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		return evalapi.BoundaryReport{}, err
	}
	m, err := e.evalHost(ctx)
	if err != nil {
		return evalapi.BoundaryReport{}, err
	}
	return m.AssertBoundary(ctx, evalID, sessionID, binding)
}
