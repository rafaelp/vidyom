//go:build !linux && !darwin

package plugins

import (
	"errors"
	"os/exec"
)

func defaultIsolator() isolator { return unsupportedIsolator{} }

// unsupportedIsolator is every platform with no sandbox here, such as native Windows.
// Plugins are refused unless plugins.allowUnsandboxed is set (I260, I44). Docker Compose
// is the supported path on Windows, and Linux containers get Landlock.
type unsupportedIsolator struct{}

func (unsupportedIsolator) name() string { return IsolationNone }

func (unsupportedIsolator) check(policy) error {
	return errors.New("no plugin sandbox on this platform")
}

func (unsupportedIsolator) start(*exec.Cmd, policy) error {
	return errors.New("no plugin sandbox on this platform")
}

func systemReadPaths() []string { return nil }
