// Package workertest starts the real Python AI worker for Go tests, with the fake
// engine: no ML dependency and no download. Only test code may import it.
package workertest

import (
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Root returns the repository root.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

// Python returns an interpreter able to run the worker, or skips the test when the
// host has none: the worker needs Python 3.12 or newer (I37).
func Python(t testing.TB) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH")
	}
	check := exec.Command(python, "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 12) else 1)")
	if err := check.Run(); err != nil {
		t.Skip("python3 is older than 3.12")
	}
	return python
}

// Env is the environment for a worker process.
func Env(extra ...string) []string {
	env := append(os.Environ(), "PYTHONPATH="+filepath.Join(Root(), "aiworker"))
	return append(env, extra...)
}

// Command builds a worker command line.
func Command(t testing.TB, args []string, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(Python(t), append([]string{"-m", "vidyom"}, args...)...)
	cmd.Env = Env(extraEnv...)
	return cmd
}

// ShortTempDir returns a directory whose path fits in a Unix socket address, which
// t.TempDir often does not on macOS.
func ShortTempDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vdm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// Worker is a running worker process on a Unix socket.
type Worker struct {
	Cmd    *exec.Cmd
	Socket string
}

// Endpoint is the configured form of the worker's address.
func (w *Worker) Endpoint() string { return "unix://" + w.Socket }

// Stop kills the process and waits for it.
func (w *Worker) Stop() {
	if w.Cmd.Process != nil {
		_ = w.Cmd.Process.Kill()
		_ = w.Cmd.Wait()
		w.Cmd.Process = nil
	}
}

// Start runs the fake engine on a Unix socket inside dir and waits until it accepts.
func Start(t testing.TB, dir string, extraEnv ...string) *Worker {
	t.Helper()
	socket := filepath.Join(dir, "ai.sock")
	cmd := Command(t, []string{"--engine", "fake", "--socket", socket}, extraEnv...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the AI worker: %v", err)
	}
	w := &Worker{Cmd: cmd, Socket: socket}
	t.Cleanup(w.Stop)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		// Connect rather than stat: a killed worker leaves a stale socket file behind.
		if conn, err := net.Dial("unix", socket); err == nil {
			_ = conn.Close()
			return w
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the AI worker did not start listening")
	return nil
}

// WriteWAV writes a silent mono WAV of the given length with the stdlib only, so the
// fake engine reads its duration with no ffprobe.
func WriteWAV(t testing.TB, path string, seconds float64) {
	t.Helper()
	const rate = 8000
	samples := uint32(seconds * rate)
	data := make([]byte, 44+2*samples)
	copy(data[0:], "RIFF")
	binary.LittleEndian.PutUint32(data[4:], 36+2*samples)
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], rate)
	binary.LittleEndian.PutUint32(data[28:], rate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], 2*samples)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
