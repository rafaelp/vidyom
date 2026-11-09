package plugins

import (
	"net/url"
	"os"
	"syscall"
)

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }

// signalZero reports whether a process exists.
func signalZero(p *os.Process) error { return p.Signal(syscall.Signal(0)) }

// isolatorCheck reports whether this host's default sandbox can run.
func (o *Options) isolatorCheck() error { return defaultIsolator().check(policy{}) }
