package runtime

import (
	"context"

	"github.com/edynasty/LumenCortex/evalapi"
)

// EvalHealth probes only a kernel the calling LC session may observe.
func (e *Engine) EvalHealth(ctx context.Context, sessionID, evalID string) (evalapi.Health, error) {
	if err := e.evalSessionAllowed(ctx, sessionID); err != nil {
		return evalapi.Health{}, err
	}
	manager, err := e.evalHost(ctx)
	if err != nil {
		return evalapi.Health{}, err
	}
	return manager.Health(ctx, evalID, sessionID)
}
