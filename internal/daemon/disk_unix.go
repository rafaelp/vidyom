//go:build unix

package daemon

import "golang.org/x/sys/unix"

func freeBytes(path string) (uint64, bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}
