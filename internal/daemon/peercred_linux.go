//go:build linux

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

const (
	peerCredentialsSupported = true
	maxSocketPath            = 108 // sun_path on Linux
)

// peerUID reads SO_PEERCRED from a Unix connection (R19).
func peerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		uid    int
		sysErr error
	)
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			sysErr = e
			return
		}
		uid = int(cred.Uid)
	})
	if err != nil {
		return 0, err
	}
	if sysErr != nil {
		return 0, fmt.Errorf("SO_PEERCRED: %w", sysErr)
	}
	return uid, nil
}
