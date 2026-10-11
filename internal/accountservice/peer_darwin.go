package accountservice

import (
	"errors"
	"net"
)

// Production financial transport is Linux-only. macOS may inspect/test the
// ledger but cannot qualify a financial peer using a claimed executable hash.
func authenticatePeer(net.Conn, string) error {
	return errors.New("Linux peer credentials are required")
}
