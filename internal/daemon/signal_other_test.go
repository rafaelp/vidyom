//go:build !unix

package daemon

import "errors"

func sendHangup() error { return errors.New("no SIGHUP on this platform") }
