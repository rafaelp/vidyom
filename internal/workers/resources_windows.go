//go:build windows

package workers

import "golang.org/x/sys/windows"

// freeDiskMB reports the space free under a directory, zero when unknown.
func freeDiskMB(dir string) int {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(path, &free, nil, nil); err != nil {
		return 0
	}
	return int(free >> 20)
}
