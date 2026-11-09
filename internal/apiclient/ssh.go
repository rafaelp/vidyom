package apiclient

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// SSHTargetPattern is what server add --ssh accepts. It cannot start with "-", so
// the target can never be read as an ssh option (I165).
var SSHTargetPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.:_-]*$|^[A-Za-z0-9][A-Za-z0-9.:_-]*$`)

const sshTunnelWait = 20 * time.Second

// sshTunnel forwards a local socket to the daemon's socket on a host operator's
// machine through the system ssh client (D12, I165). It starts on first use, so a
// command that never connects never spawns ssh.
type sshTunnel struct {
	endpoint Endpoint

	mu      sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	dir     string
	socket  string
	stderr  *boundedBuffer
	waitErr error
}

func newSSHTunnel(e Endpoint) (*sshTunnel, error) {
	if !SSHTargetPattern.MatchString(e.SSHTarget) {
		return nil, errs.Newf(errs.CodeInvalidArgument, "invalid SSH target %q: expected user@host", e.SSHTarget)
	}
	if !strings.HasPrefix(e.RemoteSocket, "/") {
		return nil, errs.New(errs.CodeInvalidArgument, "the daemon socket on the SSH host must be an absolute path")
	}
	return &sshTunnel{endpoint: e}, nil
}

func (t *sshTunnel) ensure(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil {
		select {
		case <-t.exited:
			return "", t.exitError()
		default:
			return t.socket, nil
		}
	}

	binary := t.endpoint.SSHBinary
	if binary == "" {
		binary = "ssh"
	}
	dir, err := os.MkdirTemp("", "vdmssh")
	if err != nil {
		return "", errs.Wrap(errs.CodeInternal, err, "cannot create a private directory for the SSH tunnel")
	}
	socket := filepath.Join(dir, "s")
	args := []string{
		"-N", "-T",
		"-o", "ExitOnForwardFailure=yes",
		"-L", socket + ":" + t.endpoint.RemoteSocket,
		"--", t.endpoint.SSHTarget,
	}
	stderr := &boundedBuffer{limit: 4096}
	cmd := exec.Command(binary, args...)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return "", errs.Wrap(errs.CodeUnavailable, err, "cannot start the ssh client for "+t.endpoint.SSHTarget)
	}
	t.cmd, t.dir, t.socket, t.stderr = cmd, dir, socket, stderr
	t.exited = make(chan struct{})
	go func() {
		t.waitErr = cmd.Wait()
		close(t.exited)
	}()

	deadline := time.NewTimer(sshTunnelWait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(socket); err == nil {
			return socket, nil
		}
		select {
		case <-t.exited:
			return "", t.exitError()
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", errs.Newf(errs.CodeUnavailable, "the SSH tunnel to %s did not come up within %s", t.endpoint.SSHTarget, sshTunnelWait)
		case <-tick.C:
		}
	}
}

func (t *sshTunnel) exitError() error {
	detail := strings.TrimSpace(t.stderr.String())
	if detail == "" {
		return errs.Newf(errs.CodeUnavailable, "the ssh client for %s exited", t.endpoint.SSHTarget)
	}
	return errs.Newf(errs.CodeUnavailable, "the ssh client for %s exited: %s", t.endpoint.SSHTarget, detail)
}

func (t *sshTunnel) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd == nil {
		return nil
	}
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	<-t.exited
	if t.dir != "" {
		_ = os.RemoveAll(t.dir)
	}
	t.cmd = nil
	return nil
}

// boundedBuffer keeps the first bytes of a program's stderr for error messages.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ fmt.Stringer = (*boundedBuffer)(nil)
