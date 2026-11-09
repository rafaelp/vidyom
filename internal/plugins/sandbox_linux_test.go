package plugins

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/rafaelp/vidyom/internal/contract/plugin"
)

func TestSandboxDeniesTheKernelKeyring(t *testing.T) {
	requireSandbox(t, 0)
	f := newFixture(t)
	f.installProbe("p", plugin.Permissions{})
	result := f.mustCall("p", "keyctl", nil)
	if result.Allow || result.Reason != "denied" {
		t.Errorf("keyctl: %+v", result)
	}
}

func TestSeccompFilterAssembles(t *testing.T) {
	for _, network := range []bool{false, true} {
		filter, err := seccompFilter(network)
		if err != nil {
			t.Fatal(err)
		}
		if len(filter) == 0 || len(filter) > 255 {
			t.Errorf("network=%v: %d instructions", network, len(filter))
		}
	}
}

// runBPF interprets the instructions the filter uses, so its decisions can be read as a
// table, on any architecture, including ones no container here can run it on.
func runBPF(t *testing.T, program []unix.SockFilter, data map[uint32]uint32) uint32 {
	t.Helper()
	var acc uint32
	for pc := 0; pc < len(program); pc++ {
		in := program[pc]
		switch in.Code {
		case bpfLoadW:
			acc = data[in.K]
		case bpfAnd:
			acc &= in.K
		case bpfJeq:
			if acc == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfJge:
			if acc >= in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfRet:
			return in.K
		default:
			t.Fatalf("unexpected instruction %#x", in.Code)
		}
	}
	t.Fatal("the program ran off its end")
	return 0
}

func TestSeccompFilterDecisions(t *testing.T) {
	arch, ok := auditArch()
	if !ok {
		t.Skip("no filter for this architecture")
	}
	const (
		allow = uint32(unix.SECCOMP_RET_ALLOW)
		deny  = uint32(unix.SECCOMP_RET_ERRNO) | uint32(unix.EPERM)
	)
	call := func(nr, a0, a1 uint32) map[uint32]uint32 {
		return map[uint32]uint32{seccompOffsetArch: arch, seccompOffsetNr: nr, seccompOffsetArg0: a0, seccompOffsetArg1: a1}
	}
	tcp := uint32(unix.SOCK_STREAM | unix.SOCK_CLOEXEC | unix.SOCK_NONBLOCK)
	cases := []struct {
		name     string
		data     map[uint32]uint32
		network  uint32 // with a network grant
		isolated uint32 // with none
	}{
		{"read", call(unix.SYS_READ, 0, 0), allow, allow},
		{"execve", call(unix.SYS_EXECVE, 0, 0), allow, allow}, // Landlock decides this one
		{"TCP over IPv4", call(unix.SYS_SOCKET, unix.AF_INET, tcp), allow, deny},
		{"TCP over IPv6", call(unix.SYS_SOCKET, unix.AF_INET6, tcp), allow, deny},
		{"UDP", call(unix.SYS_SOCKET, unix.AF_INET, unix.SOCK_DGRAM), deny, deny},
		{"raw IP", call(unix.SYS_SOCKET, unix.AF_INET, unix.SOCK_RAW), deny, deny},
		{"a Unix socket", call(unix.SYS_SOCKET, unix.AF_UNIX, tcp), deny, deny},
		{"netlink", call(unix.SYS_SOCKET, unix.AF_NETLINK, unix.SOCK_RAW), deny, deny},
		{"packet", call(unix.SYS_SOCKET, unix.AF_PACKET, unix.SOCK_RAW), deny, deny},
		{"io_uring", call(unix.SYS_IO_URING_SETUP, 0, 0), deny, deny},
		{"ptrace", call(unix.SYS_PTRACE, 0, 0), deny, deny},
		{"process_vm_readv", call(unix.SYS_PROCESS_VM_READV, 0, 0), deny, deny},
		{"keyctl", call(unix.SYS_KEYCTL, 0, 0), deny, deny},
		{"unshare", call(unix.SYS_UNSHARE, 0, 0), deny, deny},
		{"mount", call(unix.SYS_MOUNT, 0, 0), deny, deny},
		{"bpf", call(unix.SYS_BPF, 0, 0), deny, deny},
		{"the x32 ABI", call(x32SyscallBit|unix.SYS_SOCKET, unix.AF_INET, tcp), deny, deny},
		{"another architecture", map[uint32]uint32{seccompOffsetArch: arch + 1, seccompOffsetNr: unix.SYS_READ}, deny, deny},
	}
	withNetwork, err := seccompFilter(true)
	if err != nil {
		t.Fatal(err)
	}
	withoutNetwork, err := seccompFilter(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := runBPF(t, withNetwork, c.data); got != c.network {
			t.Errorf("%s with a network grant: %#x, want %#x", c.name, got, c.network)
		}
		if got := runBPF(t, withoutNetwork, c.data); got != c.isolated {
			t.Errorf("%s with no network grant: %#x, want %#x", c.name, got, c.isolated)
		}
	}
}

func TestLandlockRefusesWhatTheKernelCannotEnforce(t *testing.T) {
	old := &landlockIsolator{abi: abiNetwork - 1}
	old.once.Do(func() {}) // as if probed
	if err := old.check(policy{proxyPort: 1234}); err == nil {
		t.Error("a network grant needs ABI 4 to restrict TCP")
	}
	if err := old.check(policy{}); err != nil {
		t.Errorf("with no network grant seccomp is enough: %v", err)
	}
	missing := &landlockIsolator{err: errors.New("not available")}
	missing.once.Do(func() {})
	if err := missing.check(policy{}); err == nil {
		t.Error("no Landlock, no sandbox")
	}
}
