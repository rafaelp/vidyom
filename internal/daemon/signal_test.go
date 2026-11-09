//go:build unix

package daemon

import (
	"os"
	"syscall"
)

func sendHangup() error { return syscall.Kill(os.Getpid(), syscall.SIGHUP) }
