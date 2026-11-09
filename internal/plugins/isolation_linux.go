package plugins

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

func defaultIsolator() isolator { return &landlockIsolator{} }

// The Landlock ABI versions this code cares about.
const (
	abiNetwork = 4
	abiScoping = 6
	abiIoctl   = 5
	abiTrunc   = 3
	abiRefer   = 2

	landlockRuleNetPort = 2
)

// landlockRuleset is struct landlock_ruleset_attr up to the fields used here. The kernel
// accepts a shorter or longer struct as long as what it does not know is zero.
type landlockRuleset struct {
	handledFS  uint64
	handledNet uint64
	scoped     uint64
}

type landlockNetPort struct {
	allowed uint64
	port    uint64
}

// landlockIsolator confines a child with Landlock plus a seccomp filter.
type landlockIsolator struct {
	once sync.Once
	abi  int
	err  error
}

func (l *landlockIsolator) name() string { return IsolationLandlock }

func (l *landlockIsolator) probe() {
	l.once.Do(func() {
		abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
		if errno != 0 {
			l.err = fmt.Errorf("Landlock is not available on this kernel: %w", errno)
			return
		}
		l.abi = int(abi)
	})
}

func (l *landlockIsolator) check(p policy) error {
	l.probe()
	if l.err != nil {
		return l.err
	}
	if _, ok := auditArch(); !ok {
		return fmt.Errorf("no seccomp filter for %s", runtime.GOARCH)
	}
	if p.proxyPort != 0 && l.abi < abiNetwork {
		return fmt.Errorf("Landlock ABI %d cannot restrict TCP, needed for a network grant (needs %d, Linux 6.7)", l.abi, abiNetwork)
	}
	return nil
}

// start confines this goroutine's OS thread, starts the child on it and lets the thread
// die with the goroutine. Landlock and seccomp apply per thread and are inherited over
// clone and exec, so only the child is affected; the daemon's other threads are not.
func (l *landlockIsolator) start(cmd *exec.Cmd, p policy) error {
	if err := l.check(p); err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		// Never unlocked on purpose: Go destroys a thread whose goroutine exits while
		// locked, which is what must happen to a thread carrying a sandbox.
		runtime.LockOSThread()
		if err := l.restrictThread(p); err != nil {
			errc <- err
			return
		}
		errc <- cmd.Start()
	}()
	return <-errc
}

func (l *landlockIsolator) fsMasks() (handled, fileRead, dirRead, fileWrite, dirWrite uint64) {
	fileRead = unix.LANDLOCK_ACCESS_FS_READ_FILE
	dirRead = fileRead | unix.LANDLOCK_ACCESS_FS_READ_DIR
	fileWrite = unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	dirWrite = fileWrite | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	handled = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	if l.abi >= abiRefer {
		handled |= unix.LANDLOCK_ACCESS_FS_REFER
		dirWrite |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if l.abi >= abiTrunc {
		handled |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
		fileWrite |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
		dirWrite |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if l.abi >= abiIoctl {
		handled |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return handled, fileRead, dirRead, fileWrite, dirWrite
}

// restrictThread applies everything to the calling thread. It runs on a locked thread.
func (l *landlockIsolator) restrictThread(p policy) error {
	handled, fileRead, dirRead, fileWrite, dirWrite := l.fsMasks()
	attr := landlockRuleset{handledFS: handled}
	if l.abi >= abiNetwork {
		attr.handledNet = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	}
	if l.abi >= abiScoping {
		attr.scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(fd))

	add := func(path string, fileAccess, dirAccess uint64) error {
		info, err := os.Stat(path)
		if err != nil {
			return nil // gone since the policy was built: nothing to allow
		}
		access := fileAccess
		if info.IsDir() {
			access = dirAccess
		}
		pathFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s for Landlock: %w", path, err)
		}
		defer unix.Close(pathFD)
		rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(pathFD)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		if errno != 0 {
			return fmt.Errorf("Landlock rule for %s: %w", path, errno)
		}
		return nil
	}
	const exec = unix.LANDLOCK_ACCESS_FS_EXECUTE
	for _, path := range p.readPaths {
		if err := add(path, fileRead, dirRead); err != nil {
			return err
		}
	}
	for _, path := range p.execPaths {
		if err := add(path, fileRead|exec, dirRead|exec); err != nil {
			return err
		}
	}
	for _, path := range p.writePaths {
		if err := add(path, fileRead|fileWrite, dirRead|dirWrite); err != nil {
			return err
		}
	}
	if err := add(p.pluginDir, fileRead|exec, dirRead|exec); err != nil {
		return err
	}
	if err := add(p.entry, fileRead|exec, dirRead|exec); err != nil {
		return err
	}
	for _, path := range existing([]string{"/dev/null"}) {
		if err := add(path, fileRead|fileWrite, dirRead); err != nil {
			return err
		}
	}
	// The dynamic loader is opened for execution: a dynamically linked plugin or
	// interpreter needs its libraries, which are not commands (I265).
	for _, path := range existing([]string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"}) {
		if err := add(path, fileRead|exec, dirRead|exec); err != nil {
			return err
		}
	}
	if p.proxyPort != 0 {
		rule := landlockNetPort{allowed: unix.LANDLOCK_ACCESS_NET_CONNECT_TCP, port: uint64(p.proxyPort)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, landlockRuleNetPort, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		if errno != 0 {
			return fmt.Errorf("Landlock network rule: %w", errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if err := installSeccomp(p.proxyPort != 0); err != nil {
		return err
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("apply Landlock: %w", errno)
	}
	return nil
}

func auditArch() (uint32, bool) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, true
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, true
	}
	return 0, false
}

// Classic BPF opcodes.
const (
	bpfLoadW = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeq   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJge   = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfAnd   = 0x54 // BPF_ALU | BPF_AND | BPF_K
	bpfRet   = 0x06 // BPF_RET | BPF_K

	seccompOffsetNr   = 0
	seccompOffsetArch = 4
	seccompOffsetArg0 = 16
	seccompOffsetArg1 = 24

	x32SyscallBit = 0x40000000
	sockTypeMask  = 0xf
)

// deniedSyscalls would reach past the sandbox, or into the daemon: io_uring can create
// sockets without the socket syscall this filter watches.
var deniedSyscalls = []uint32{
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE, unix.SYS_KEXEC_LOAD,
}

// bpfProgram is a tiny assembler: jumps name labels, resolved at the end.
type bpfProgram struct {
	code   []unix.SockFilter
	labels map[string]int
	fixups []bpfFixup
}

type bpfFixup struct {
	at     int
	jt, jf string
}

func (b *bpfProgram) stmt(code uint16, k uint32) {
	b.code = append(b.code, unix.SockFilter{Code: code, K: k})
}

func (b *bpfProgram) jump(code uint16, k uint32, jt, jf string) {
	b.fixups = append(b.fixups, bpfFixup{at: len(b.code), jt: jt, jf: jf})
	b.code = append(b.code, unix.SockFilter{Code: code, K: k})
}

func (b *bpfProgram) label(name string) { b.labels[name] = len(b.code) }

func (b *bpfProgram) resolve() ([]unix.SockFilter, error) {
	offset := func(from int, name string) (uint8, error) {
		if name == "" {
			return 0, nil
		}
		to, ok := b.labels[name]
		if !ok {
			return 0, fmt.Errorf("unknown label %s", name)
		}
		distance := to - from - 1
		if distance < 0 || distance > 255 {
			return 0, errors.New("jump out of range")
		}
		return uint8(distance), nil
	}
	for _, f := range b.fixups {
		jt, err := offset(f.at, f.jt)
		if err != nil {
			return nil, err
		}
		jf, err := offset(f.at, f.jf)
		if err != nil {
			return nil, err
		}
		b.code[f.at].Jt, b.code[f.at].Jf = jt, jf
	}
	return b.code, nil
}

// seccompFilter denies deniedSyscalls and limits socket: with a network grant only TCP
// over IP, which Landlock then limits to the proxy's port, and with none, no socket at
// all. Landlock cannot restrict UDP or connecting to a Unix socket, which is why this
// exists. A denied call fails with EPERM.
func seccompFilter(network bool) ([]unix.SockFilter, error) {
	arch, ok := auditArch()
	if !ok {
		return nil, fmt.Errorf("no seccomp filter for %s", runtime.GOARCH)
	}
	allow := uint32(unix.SECCOMP_RET_ALLOW)
	deny := uint32(unix.SECCOMP_RET_ERRNO) | uint32(unix.EPERM)
	b := &bpfProgram{labels: map[string]int{}}
	b.stmt(bpfLoadW, seccompOffsetArch)
	b.jump(bpfJeq, arch, "", "deny")
	b.stmt(bpfLoadW, seccompOffsetNr)
	b.jump(bpfJge, x32SyscallBit, "deny", "")
	for _, nr := range deniedSyscalls {
		b.jump(bpfJeq, nr, "deny", "")
	}
	b.jump(bpfJeq, uint32(unix.SYS_SOCKET), "socket", "allow")
	b.label("socket")
	if network {
		b.stmt(bpfLoadW, seccompOffsetArg0)
		b.jump(bpfJeq, unix.AF_INET, "tcp", "")
		b.jump(bpfJeq, unix.AF_INET6, "tcp", "deny")
		b.label("tcp")
		b.stmt(bpfLoadW, seccompOffsetArg1)
		b.stmt(bpfAnd, sockTypeMask)
		b.jump(bpfJeq, unix.SOCK_STREAM, "allow", "deny")
	} else {
		b.stmt(bpfRet, deny)
	}
	b.label("deny")
	b.stmt(bpfRet, deny)
	b.label("allow")
	b.stmt(bpfRet, allow)
	return b.resolve()
}

func installSeccomp(network bool) error {
	filter, err := seccompFilter(network)
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	runtime.KeepAlive(filter)
	return nil
}

// systemReadPaths are readable by every plugin. /etc as a whole is not, because the
// daemon's configuration could live there (I264).
func systemReadPaths() []string {
	return []string{
		"/usr/share/zoneinfo", "/usr/share/ca-certificates",
		"/etc/ssl", "/etc/ca-certificates", "/etc/pki",
		"/etc/hosts", "/etc/resolv.conf", "/etc/nsswitch.conf", "/etc/localtime", "/etc/ld.so.cache",
		"/dev/urandom", "/dev/random", "/dev/zero",
	}
}
