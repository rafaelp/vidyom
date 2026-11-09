package observability

import "golang.org/x/sys/unix"

func totalMemory() (uint64, bool) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, false
	}
	return total, true
}

func diskFree(path string) (uint64, bool) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, false
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), true
}
