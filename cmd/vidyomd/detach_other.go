//go:build !unix

package main

import (
	"errors"
	"io"

	"github.com/rafaelp/vidyom/internal/contract/config"
)

// startDetached is refused here in this version (I44, I139); Compose is the path.
func startDetached(config.Daemon, string, io.Writer) error {
	return errors.New("--detach is not available on this platform; run in the foreground or through Docker Compose")
}

func readyNotifier() (func(), error) { return func() {}, nil }
