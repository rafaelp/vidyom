// Package ffmpeg runs ffmpeg and ffprobe for the media worker. Commands are argv
// arrays handed directly to the operating system, never a shell, and no value is
// ever concatenated into a command string (§6.5, I190). Inputs are opened with a
// protocol and demuxer whitelist, so an uploaded HLS playlist or concat script
// cannot make FFmpeg read local files or reach the network.
package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DemuxerWhitelist lists the containers recordings arrive in (I190). The names are
// FFmpeg's demuxer names.
const DemuxerWhitelist = "mov,mp4,m4a,3gp,3g2,mj2,matroska,webm,avi,mxf,mpegts,mpeg,mpegvideo,asf,flv,ogg,wav,flac,mp3,aac,dv"

// ProtocolWhitelist allows only local files on input.
const ProtocolWhitelist = "file"

// Minimum version: -fps_mode exists from 5.1 (I190).
const (
	minMajor = 5
	minMinor = 1
)

// stderrTail is how much of ffmpeg's stderr a RunError keeps.
const stderrTail = 8 << 10

// killDelay is how long FFmpeg gets to finish after SIGTERM.
const killDelay = 5 * time.Second

// RunError is a failed ffmpeg or ffprobe run. Its text holds tool output, so it
// goes to internal logs only, wrapped behind a generic coded error.
type RunError struct {
	Tool     string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *RunError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("%s failed with exit code %d: %s", e.Tool, e.ExitCode, e.Stderr)
	}
	return fmt.Sprintf("%s failed: %v", e.Tool, e.Err)
}

func (e *RunError) Unwrap() error { return e.Err }

// Tool locates the binaries and runs them.
type Tool struct {
	FFmpeg  string
	FFprobe string

	versionOnce sync.Once
	version     string
	versionErr  error
}

// New returns a Tool for these binaries. Empty names mean ffmpeg and ffprobe on PATH.
func New(ffmpegPath, ffprobePath string) *Tool {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if ffprobePath == "" {
		ffprobePath = "ffprobe"
	}
	return &Tool{FFmpeg: ffmpegPath, FFprobe: ffprobePath}
}

var (
	defaultMu   sync.RWMutex
	defaultTool = New("", "")
)

// Default returns the process-wide Tool the nodes use. Node packages register with
// no options (master plan section 9), so the worker sets it once at startup.
func Default() *Tool {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultTool
}

// SetDefault replaces the process-wide Tool.
func SetDefault(t *Tool) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultTool = t
}

var versionPattern = regexp.MustCompile(`^ffmpeg version n?(\d+)\.(\d+)`)

// Version returns the first line's version, such as "9.0.2", for provenance (R21).
func (t *Tool) Version(ctx context.Context) (string, error) {
	t.versionOnce.Do(func() {
		out, err := t.output(ctx, t.FFmpeg, "-hide_banner", "-version")
		if err != nil {
			t.versionErr = err
			return
		}
		line, _, _ := strings.Cut(string(out), "\n")
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "ffmpeg" && fields[1] == "version" {
			t.version = fields[2]
			return
		}
		t.version = strings.TrimSpace(line)
	})
	return t.version, t.versionErr
}

// Check verifies both binaries run, the version is new enough and the required
// encoders exist. Warnings are for what only some inputs need.
func (t *Tool) Check(ctx context.Context) (warnings []string, err error) {
	out, err := t.output(ctx, t.FFmpeg, "-hide_banner", "-version")
	if err != nil {
		return nil, err
	}
	if _, err := t.output(ctx, t.FFprobe, "-hide_banner", "-version"); err != nil {
		return nil, err
	}
	if m := versionPattern.FindSubmatch(out); m != nil {
		major, _ := strconv.Atoi(string(m[1]))
		minor, _ := strconv.Atoi(string(m[2]))
		if major < minMajor || (major == minMajor && minor < minMinor) {
			return nil, fmt.Errorf("ffmpeg %d.%d is too old: %d.%d or newer is required", major, minor, minMajor, minMinor)
		}
	} else {
		line, _, _ := strings.Cut(string(out), "\n")
		warnings = append(warnings, fmt.Sprintf("cannot parse the ffmpeg version from %q; assuming it is new enough", strings.TrimSpace(line)))
	}
	encoders, err := t.output(ctx, t.FFmpeg, "-hide_banner", "-encoders")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"libx264", "aac", "libopus"} {
		if !hasEncoder(encoders, name) {
			return nil, fmt.Errorf("ffmpeg lacks the required encoder %s", name)
		}
	}
	if !hasEncoder(encoders, "libx265") {
		warnings = append(warnings, "ffmpeg lacks libx265: master-2160p-hevc will fail")
	}
	return warnings, nil
}

func hasEncoder(list []byte, name string) bool {
	for line := range strings.SplitSeq(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			return true
		}
	}
	return false
}

// HasEncoder reports whether ffmpeg has an encoder.
func (t *Tool) HasEncoder(ctx context.Context, name string) bool {
	list, err := t.output(ctx, t.FFmpeg, "-hide_banner", "-encoders")
	return err == nil && hasEncoder(list, name)
}

// RunOptions controls one ffmpeg run.
type RunOptions struct {
	// DurationSeconds lets progress become a percentage; zero means no progress.
	DurationSeconds float64
	// OnProgress receives a percentage in [0, 100].
	OnProgress func(percent float64)
}

// Run executes ffmpeg with argv. Common flags are added here, so builders only
// describe the job. On cancellation it sends SIGTERM, then kills after a delay.
func (t *Tool) Run(ctx context.Context, args []string, opts RunOptions) error {
	argv := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:1", "-y"}, args...)
	cmd := command(ctx, t.FFmpeg, argv...)
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return &RunError{Tool: "ffmpeg", ExitCode: -1, Err: err}
	}
	progress := make(chan struct{})
	go func() {
		defer close(progress)
		readProgress(stdout, opts)
	}()
	<-progress
	err = cmd.Wait()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return runError("ffmpeg", err, stderr.String())
}

func readProgress(r io.Reader, opts RunOptions) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if opts.OnProgress == nil || opts.DurationSeconds <= 0 {
			continue
		}
		value, ok := strings.CutPrefix(scanner.Text(), "out_time_us=")
		if !ok {
			continue
		}
		micros, err := strconv.ParseInt(value, 10, 64)
		if err != nil || micros < 0 {
			continue
		}
		percent := float64(micros) / 1e6 / opts.DurationSeconds * 100
		if percent > 100 {
			percent = 100
		}
		opts.OnProgress(percent)
	}
	_, _ = io.Copy(io.Discard, r)
}

func runError(tool string, err error, stderr string) error {
	re := &RunError{Tool: tool, ExitCode: -1, Stderr: strings.TrimSpace(stderr), Err: err}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		re.ExitCode = exit.ExitCode()
	}
	return re
}

// output runs a short command and returns stdout.
func (t *Tool) output(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := command(ctx, bin, args...)
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, runError(bin, err, stderr.String())
	}
	return out, nil
}

// command builds an exec.Cmd with a minimal environment, so nothing the worker
// holds in its own environment reaches the child.
func command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = killDelay
	cmd.Env = minimalEnv()
	return cmd
}

func minimalEnv() []string {
	var env []string
	for _, name := range []string{
		"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH",
		"DYLD_FALLBACK_LIBRARY_PATH", "SystemRoot", "SYSTEMROOT", "PATHEXT",
	} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env, "AV_LOG_FORCE_NOCOLOR=1", "LC_ALL=C")
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	max int
	buf bytes.Buffer
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf.Write(p)
	if extra := b.buf.Len() - b.max; extra > 0 {
		b.buf.Next(extra)
	}
	return n, nil
}

func (b *tailBuffer) String() string { return b.buf.String() }

// InputArgs returns the hardened options that open one input, ending in -i path.
// extra are input options such as -ss, placed before -i.
func InputArgs(path string, extra ...string) []string {
	args := make([]string, 0, len(extra)+6)
	args = append(args, "-protocol_whitelist", ProtocolWhitelist, "-format_whitelist", DemuxerWhitelist)
	args = append(args, extra...)
	return append(args, "-i", path)
}
