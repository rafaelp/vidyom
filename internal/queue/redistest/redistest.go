// Package redistest starts a real redis-server for crash and data-loss tests (plan
// section 8, rule 10). It uses a non-default port and a temporary directory, and
// removes both. Tests skip, saying why, when no server binary exists.
package redistest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Server is a redis-server process under test control.
type Server struct {
	t       *testing.T
	Addr    string
	Dir     string
	port    int
	mu      sync.Mutex
	cmd     *exec.Cmd
	stderr  bytes.Buffer
	command []string
}

func command() ([]string, error) {
	if path, err := exec.LookPath("mise"); err == nil {
		probe := exec.Command(path, "exec", "redis@8.8.0", "--", "redis-server", "--version")
		if err := probe.Run(); err == nil {
			return []string{path, "exec", "redis@8.8.0", "--", "redis-server"}, nil
		}
	}
	if path, err := exec.LookPath("redis-server"); err == nil {
		probe := exec.Command(path, "--version")
		if err := probe.Run(); err == nil {
			return []string{path}, nil
		}
	}
	return nil, fmt.Errorf("no usable redis-server: tried mise exec redis@8.8.0 and PATH")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Start runs a server with AOF on (D4), or skips the test.
func Start(t *testing.T) *Server {
	t.Helper()
	cmdline, err := command()
	if err != nil {
		t.Skipf("real Redis test skipped: %v", err)
	}
	dir, err := os.MkdirTemp("", "vidyom-dev-engine-redis-")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{t: t, Dir: dir, port: freePort(t), command: cmdline}
	s.Addr = fmt.Sprintf("127.0.0.1:%d", s.port)
	t.Cleanup(func() {
		s.Kill()
		_ = os.RemoveAll(dir)
	})
	s.Restart()
	return s
}

// Restart starts the process, keeping whatever is in Dir.
func (s *Server) Restart() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		s.t.Fatal("redis-server is already running")
	}
	args := append(append([]string{}, s.command[1:]...),
		"--port", fmt.Sprint(s.port),
		"--bind", "127.0.0.1",
		"--dir", s.Dir,
		"--appendonly", "yes",
		"--appendfsync", "always",
		"--save", "",
		"--daemonize", "no",
	)
	cmd := exec.Command(s.command[0], args...)
	cmd.Stderr = &s.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start redis-server: %v", err)
	}
	s.cmd = cmd
	c := redis.NewClient(&redis.Options{Addr: s.Addr})
	defer c.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := c.Ping(ctx).Err()
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("redis-server did not come up: %v; stderr: %s", err, s.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Kill stops the process with SIGKILL, as a crash would.
func (s *Server) Kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	_ = s.cmd.Wait()
	s.cmd = nil
}

// WipeData removes everything on disk, so the next Restart comes up empty: a Redis
// that lost its data.
func (s *Server) WipeData() {
	s.t.Helper()
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.Dir, e.Name())); err != nil {
			s.t.Fatal(err)
		}
	}
}

// Client returns a new client; the caller closes it.
func (s *Server) Client() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: s.Addr})
}
