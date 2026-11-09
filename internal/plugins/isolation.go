package plugins

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Isolation levels reported in domain.PluginRecord.Isolation (I260).
const (
	IsolationLandlock    = "landlock"
	IsolationSeatbelt    = "seatbelt"
	IsolationNone        = "none"
	IsolationUnavailable = "unavailable"
)

// policy is what one plugin process may touch. Every path is absolute and resolved, so
// /var and /private/var do not disagree on macOS.
type policy struct {
	entry      string   // the executable
	pluginDir  string   // read and execute
	readPaths  []string // read only: system files needed to run, and ro: grants
	writePaths []string // read and write: the private temp directory and rw grants
	execPaths  []string // read and execute: operator-declared runtimes (I265)
	proxyPort  int      // the only TCP port reachable; 0 means no network at all
}

// isolator confines a child process. check reports whether this host can enforce a
// policy; start confines and starts cmd.
type isolator interface {
	name() string
	check(p policy) error
	start(cmd *exec.Cmd, p policy) error
}

// noIsolation is what plugins.allowUnsandboxed selects. Only the scrubbed environment,
// the stdio-only channel and daemon-side checks hold (I260).
type noIsolation struct{}

func (noIsolation) name() string                        { return IsolationNone }
func (noIsolation) check(policy) error                  { return nil }
func (noIsolation) start(cmd *exec.Cmd, _ policy) error { return cmd.Start() }

// existing keeps the paths that exist, resolved. A sandbox rule on a missing path is an
// error on some systems and pointless on all.
func existing(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		out = append(out, resolved)
	}
	return out
}

// scrubbedEnv is the whole environment of a plugin process: none of the daemon's is
// inherited, so a master key in VIDYOM_MASTER_KEY_B64 or a cloud credential never
// reaches it (I260).
func scrubbedEnv(pluginID, tenant, tempDir, proxyURL string) []string {
	env := []string{
		"PATH=/usr/bin:/bin",
		"LANG=C.UTF-8",
		"VIDYOM_PLUGIN_ID=" + pluginID,
		"VIDYOM_TENANT=" + tenant,
	}
	if runtime.GOOS == "windows" {
		env = append(env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	if tempDir != "" {
		env = append(env, "TMPDIR="+tempDir, "TEMP="+tempDir, "TMP="+tempDir, "HOME="+tempDir)
	} else {
		env = append(env, "HOME=/nonexistent")
	}
	if proxyURL != "" {
		env = append(env,
			"HTTP_PROXY="+proxyURL, "HTTPS_PROXY="+proxyURL,
			"http_proxy="+proxyURL, "https_proxy="+proxyURL,
			"NO_PROXY=", "no_proxy=",
		)
	}
	return env
}

// splitFilesystem turns effective filesystem permissions into policy paths.
func splitFilesystem(entries []string, tempDir string) (read, write []string, err error) {
	for _, entry := range entries {
		path, readOnly, parseErr := parseFilesystemEntry(entry)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		switch {
		case path == "":
			if tempDir != "" {
				write = append(write, tempDir)
			}
		case readOnly:
			read = append(read, path)
		default:
			write = append(write, path)
		}
	}
	return read, write, nil
}

func hasPermission(entries []string, want string) bool {
	for _, entry := range entries {
		if strings.EqualFold(entry, want) {
			return true
		}
	}
	return false
}
