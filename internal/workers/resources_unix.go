//go:build unix

package workers

import "golang.org/x/sys/unix"

// freeDiskMB reports the space free under a directory, zero when unknown.
func freeDiskMB(dir string) int {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) >> 20)
}
