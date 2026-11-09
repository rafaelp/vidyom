//go:build !unix

package plugins

import (
	"os"
	"os/exec"
)

func setProcessGroup(*exec.Cmd) {}

func terminateGroup(pid int) { killGroup(pid) }

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
