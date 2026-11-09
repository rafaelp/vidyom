//go:build unix

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwner refuses a directory another user owns, who could replace what is in it.
func checkOwner(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Geteuid(); int(stat.Uid) != uid && stat.Uid != 0 {
		return fmt.Errorf("%s is owned by uid %d, not by this user (%d) or root", path, stat.Uid, uid)
	}
	return nil
}
