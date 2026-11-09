//go:build !linux

package evalruntime

import (
	"errors"
	"os"
)

type brokerIdentity struct{ uid, gid int }

func prepareBrokerLaunch(identity *brokerIdentity) ([]*os.File, func(), error) {
	if identity != nil {
		return nil, nil, errors.New("UID broker requires Linux")
	}
	return nil, func() {}, nil
}
