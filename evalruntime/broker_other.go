//go:build !linux

package evalruntime

import (
	"context"
	"errors"
	"net"
)

func verifyBrokerPeer(*net.UnixConn) error            { return errors.New("UID broker requires Linux") }
func ServeBroker(context.Context, BrokerPolicy) error { return errors.New("UID broker requires Linux") }
