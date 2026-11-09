//go:build linux

package evalruntime

import (
	"errors"
	"net"
)

func verifyBrokerPeer(conn *net.UnixConn) error {
	peer, err := brokerPeer(conn)
	if err != nil {
		return err
	}
	if peer.Uid != 0 {
		return errors.New("broker peer is not privileged service")
	}
	return nil
}
