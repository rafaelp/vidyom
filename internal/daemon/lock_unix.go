//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockDataDir holds <data-dir>/vidyomd.lock for the life of the process (I137). The
// kernel releases it if the process dies, so a stale file never blocks a restart.
func lockDataDir(dataDir string) (func(), error) {
	path := filepath.Join(dataDir, "vidyomd.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the data directory lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another vidyomd holds the data directory %s", dataDir)
		}
		return nil, fmt.Errorf("lock the data directory: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
