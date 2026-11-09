//go:build !unix

package daemon

func freeBytes(string) (uint64, bool) { return 0, false }
