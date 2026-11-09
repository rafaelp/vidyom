//go:build !unix

package daemon

import "os"

func checkOwner(string, os.FileInfo) error { return nil }
