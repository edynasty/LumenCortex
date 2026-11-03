package runtime

import (
	"context"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// DeliveryContract and DeliveryFile declare mechanically verifiable task requirements.
// Checks are selected through the existing approved checks configuration, not arbitrary commands.
type DeliveryContract = taskstate.DeliveryContract
type DeliveryFile = taskstate.DeliveryFile

func deliveryIn(workspace string) func(context.Context, *taskstate.DeliveryContract) ([]string, error) {
	return func(ctx context.Context, d *taskstate.DeliveryContract) ([]string, error) {
		repo, err := repository.New(workspace)
		if err != nil {
			return nil, err
		}
		return repo.DeliveryMissing(ctx, d)
	}
}
