//go:build unix

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
)

const (
	// readyFDEnv tells a detached child which descriptor to report readiness on.
	readyFDEnv = "VIDYOMD_READY_FD"
	// inherited descriptors start after stdin, stdout and stderr.
	readyFD      = 3
	readyMessage = "ready\n"
	detachWait   = 2 * time.Minute
)

// startDetached re-executes this binary in a new session and waits for the child to
// report that every listener is bound (I139). A child that fails closed closes the
// pipe, so a refused start is never reported as started.
func startDetached(cfg config.Daemon, configDir string, stdout io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find this binary: %w", err)
	}
	logDir := filepath.Join(cfg.DataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return fmt.Errorf("create the log directory: %w", err)
	}
	logPath := filepath.Join(logDir, "vidyomd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the daemon's log: %w", err)
	}
	defer logFile.Close()

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create the readiness pipe: %w", err)
	}
	defer readEnd.Close()

	cmd := exec.Command(executable, "start", "--config-dir", configDir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{writeEnd}
	cmd.Env = append(os.Environ(), readyFDEnv+"="+strconv.Itoa(readyFD))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = writeEnd.Close()
		return fmt.Errorf("start the daemon: %w", err)
	}
	_ = writeEnd.Close()
	pid := cmd.Process.Pid
	// The child outlives this process; nothing here should wait on it.
	_ = cmd.Process.Release()

	result := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(readEnd).ReadString('\n')
		result <- line
	}()
	select {
	case line := <-result:
		if line != readyMessage {
			return fmt.Errorf("the daemon did not start; see %s", logPath)
		}
	case <-time.After(detachWait):
		return fmt.Errorf("the daemon (pid %d) is not listening after %s; see %s", pid, detachWait, logPath)
	}
	fmt.Fprintf(stdout, "vidyomd started, pid %d, log %s\n", pid, logPath)
	return nil
}

// readyNotifier returns what the detached child calls once listening. It does
// nothing in a foreground start.
func readyNotifier() (func(), error) {
	raw := os.Getenv(readyFDEnv)
	if raw == "" {
		return func() {}, nil
	}
	_ = os.Unsetenv(readyFDEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil {
		return nil, errors.New("invalid " + readyFDEnv)
	}
	pipe := os.NewFile(uintptr(fd), "ready")
	if pipe == nil {
		return nil, errors.New("invalid " + readyFDEnv)
	}
	syscall.CloseOnExec(fd)
	return func() {
		_, _ = pipe.WriteString(readyMessage)
		_ = pipe.Close()
	}, nil
}
