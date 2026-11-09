package workers

import "golang.org/x/sys/unix"

// totalMemoryMB reports physical memory, zero when unknown.
func totalMemoryMB() int {
	bytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int(bytes >> 20)
}
