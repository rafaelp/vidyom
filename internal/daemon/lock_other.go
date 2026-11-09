//go:build !unix

package daemon

// lockDataDir is not enforced on this platform in this version (I44, I137).
func lockDataDir(string) (func(), error) { return func() {}, nil }
