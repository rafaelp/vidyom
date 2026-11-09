//go:build !linux && !darwin

package observability

func totalMemory() (uint64, bool) { return 0, false }

func diskFree(string) (uint64, bool) { return 0, false }
