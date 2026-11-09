//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

// No peer credentials here, so the socket listener is refused at validation (I136).
const (
	peerCredentialsSupported = false
	maxSocketPath            = 0
)

func peerUID(*net.UnixConn) (int, error) {
	return 0, errors.New("peer credentials are not available on this platform")
}
