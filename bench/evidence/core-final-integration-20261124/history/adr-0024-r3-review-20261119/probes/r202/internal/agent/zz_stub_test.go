package agent

// Review-only stub so the copied R2-04 test file compiles on 1a68226 (pre-R2-04). Not used by the
// profile probe; the parity test that calls it is not run on this snapshot.

import (
	"context"
	"errors"
)

func StreamRepairContextFromStore(context.Context, Store, string, int64) (RepairContext, error) {
	return RepairContext{}, errors.New("not available before R2-04")
}
