// Package nodeutil holds what the media node packages share.
package nodeutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
	"github.com/rafaelp/vidyom/schemas"
)

// Base gives a node its catalog descriptor, so it declares exactly what the catalog
// says. A type missing from the catalog is a programming error found at registration.
type Base struct {
	Desc node.Descriptor
}

// Descriptor returns the catalog's descriptor.
func (b Base) Descriptor() node.Descriptor { return b.Desc }

// NewBase loads a node type's descriptor, panicking if the catalog lacks it.
func NewBase(nodeType string) Base {
	desc, err := schemas.DescriptorFor(nodeType, 1)
	if err != nil {
		panic(err)
	}
	return Base{Desc: desc}
}

// Classify marks what a retry can fix (D11): storage or network unavailability is
// transient. Context errors pass untouched, so the runner sees a cancellation.
func Classify(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch errs.CodeOf(err) {
	case errs.CodeUnavailable, errs.CodeRateLimited, errs.CodeTimeout:
		return node.Transient(err)
	}
	return err
}

// FFmpegError wraps a tool failure behind a generic message. The cause, with the
// tool's output, stays for internal logs and never reaches a client (I190).
func FFmpegError(err error, what string) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var run *ffmpeg.RunError
	if errors.As(err, &run) {
		// A killed process or a full disk can heal; anything else is about the file.
		if run.ExitCode < 0 || strings.Contains(run.Stderr, "No space left on device") {
			return node.Transient(errs.Wrap(errs.CodeUnavailable, err, what+": the media tool was interrupted"))
		}
		return errs.Wrap(errs.CodeFailedPrecondition, err, what+": the media tool could not process this file")
	}
	return errs.Wrap(errs.CodeInternal, err, what+" failed")
}

// ProbeError wraps a failed probe the same way.
func ProbeError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errs.Wrap(errs.CodeFailedPrecondition, err, "this file could not be read as media")
}

// Encode runs ffmpeg, turning its progress into the node's.
func Encode(ctx context.Context, env node.Env, tool *ffmpeg.Tool, args []string, durationSeconds float64, what string) error {
	err := tool.Run(ctx, args, ffmpeg.RunOptions{
		DurationSeconds: durationSeconds,
		OnProgress:      func(percent float64) { env.Progress(percent, what) },
	})
	return FFmpegError(err, what)
}

// Tools is the provenance entry of a node that ran FFmpeg (R21).
func Tools(ctx context.Context, tool *ffmpeg.Tool) (map[string]string, string) {
	version, err := tool.Version(ctx)
	if err != nil {
		return nil, ""
	}
	return map[string]string{"ffmpeg": version}, version
}

// TempFor returns a new temp path for an encoder, in the same directory as the final
// path, so finishing is an atomic rename and a failed run leaves nothing a later node
// could mistake for output. The name is unique to this call: a stale attempt that
// overlaps its successor must never write into a file the other one owns, which is
// what -y would do on a shared name. Callers remove it when they do not publish it.
func TempFor(path string) string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err) // crypto/rand does not fail on a supported platform
	}
	return filepath.Join(filepath.Dir(path), ".tmp-"+hex.EncodeToString(random[:])+"-"+filepath.Base(path))
}

// Publish moves a finished temp file into place.
func Publish(temp, path string) error {
	if err := os.Rename(temp, path); err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot publish an output file")
	}
	return nil
}

// ContentType names a file by its extension, for what stored objects record.
func ContentType(key string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(key), ".")) {
	case "mp4", "m4v":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "mkv":
		return "video/x-matroska"
	case "webm":
		return "video/webm"
	case "ts":
		return "video/mp2t"
	case "avi":
		return "video/x-msvideo"
	case "mxf":
		return "application/mxf"
	case "opus", "ogg":
		return "audio/ogg"
	case "m4a":
		return "audio/mp4"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "json":
		return "application/json"
	case "vtt":
		return "text/vtt"
	case "srt":
		return "application/x-subrip"
	case "md":
		return "text/markdown"
	case "txt":
		return "text/plain"
	}
	return "application/octet-stream"
}
