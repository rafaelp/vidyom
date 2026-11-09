//go:build !darwin && !linux

package workers

// totalMemoryMB is unknown here; --memory-mb declares it (D40).
func totalMemoryMB() int { return 0 }
