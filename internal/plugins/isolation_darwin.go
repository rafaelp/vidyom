package plugins

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

const sandboxExec = "/usr/bin/sandbox-exec"

func defaultIsolator() isolator { return &seatbeltIsolator{} }

// seatbeltIsolator runs the child through sandbox-exec with a generated profile. It is
// deprecated by Apple and still the only built-in option on macOS (I260).
type seatbeltIsolator struct {
	once sync.Once
	err  error
}

func (s *seatbeltIsolator) name() string { return IsolationSeatbelt }

func (s *seatbeltIsolator) probe() {
	s.once.Do(func() {
		out, err := exec.Command(sandboxExec, "-p", "(version 1)(allow default)", "/usr/bin/true").CombinedOutput()
		if err != nil {
			s.err = fmt.Errorf("sandbox-exec is not usable on this host: %v: %s", err, strings.TrimSpace(string(out)))
		}
	})
}

func (s *seatbeltIsolator) check(policy) error {
	s.probe()
	return s.err
}

func (s *seatbeltIsolator) start(cmd *exec.Cmd, p policy) error {
	if err := s.check(p); err != nil {
		return err
	}
	if cmd.Path == "" {
		return errors.New("plugin command has no path")
	}
	profile := seatbeltProfile(p)
	args := append([]string{sandboxExec, "-p", profile, cmd.Path}, cmd.Args[1:]...)
	cmd.Path = sandboxExec
	cmd.Args = args
	return cmd.Start()
}

func quoteSBPL(path string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
}

func subpaths(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(" (subpath ")
		b.WriteString(quoteSBPL(path))
		b.WriteString(")")
	}
	return b.String()
}

// seatbeltProfile denies everything and allows the policy. The system paths are what
// dyld and the Go and Python runtimes need before any plugin code runs.
func seatbeltProfile(p policy) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-fork)\n(allow signal (target same-sandbox))\n(allow sysctl-read)\n(allow file-read-metadata)\n")
	b.WriteString("(allow process-exec* (literal " + quoteSBPL(p.entry) + ")" + subpaths([]string{p.pluginDir}) + subpaths(p.execPaths) + ")\n")

	read := []string{p.pluginDir}
	read = append(read, p.execPaths...)
	read = append(read, p.readPaths...)
	b.WriteString("(allow file-read*" + subpaths(read) + ")\n")
	b.WriteString("(allow file-read* file-write-data (literal \"/dev/null\"))\n")
	// dyld lists the root directory while starting a process.
	b.WriteString("(allow file-read-data (literal \"/\"))\n")
	if len(p.writePaths) > 0 {
		b.WriteString("(allow file-read* file-write*" + subpaths(p.writePaths) + ")\n")
	}
	if p.proxyPort != 0 {
		b.WriteString("(allow network-outbound (remote ip \"localhost:" + strconv.Itoa(p.proxyPort) + "\"))\n")
	}
	return b.String()
}

// systemReadPaths are readable by every plugin. /etc as a whole is not, because the
// daemon's configuration could live there (I264). These are resolved paths: /etc is a
// link to /private/etc.
func systemReadPaths() []string {
	return []string{
		"/usr/lib", "/System/Library", "/System/Volumes/Preboot/Cryptexes", "/usr/share/zoneinfo",
		"/private/var/db/dyld", "/private/var/db/timezone", "/usr/share/icu",
		"/private/etc/ssl", "/private/etc/hosts", "/private/etc/resolv.conf", "/private/etc/localtime",
		"/dev/urandom", "/dev/random", "/dev/zero",
	}
}
