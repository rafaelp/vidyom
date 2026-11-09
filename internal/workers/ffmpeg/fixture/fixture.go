// Package fixture generates small test media with FFmpeg. Everything is written
// under a temporary directory the caller supplies, and no media is ever committed.
package fixture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Require skips the test when FFmpeg is absent, unless VIDYOM_REQUIRE_FFMPEG=1
// demands that it be present.
func Require(t testing.TB) {
	t.Helper()
	_, errF := exec.LookPath("ffmpeg")
	_, errP := exec.LookPath("ffprobe")
	if errF == nil && errP == nil {
		return
	}
	if os.Getenv("VIDYOM_REQUIRE_FFMPEG") == "1" {
		t.Fatalf("ffmpeg and ffprobe are required: %v %v", errF, errP)
	}
	t.Skip("ffmpeg and ffprobe not found")
}

// Spec describes a generated clip.
type Spec struct {
	Name    string
	Width   int
	Height  int
	Rate    string // "30", "30000/1001"
	Seconds float64
	NoAudio bool
	// Extra output options, placed before the file name.
	Extra []string
	// Codec defaults to libx264 with a fast preset.
	Codec []string
	// Filter is a video filter chain.
	Filter string
}

func run(t testing.TB, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "ffmpeg", append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// Generate runs ffmpeg with explicit arguments to produce dir/name, for formats the
// other helpers do not cover, and returns its path.
func Generate(t testing.TB, dir, name string, args ...string) string {
	t.Helper()
	Require(t)
	path := filepath.Join(dir, name)
	run(t, append(append([]string(nil), args...), path)...)
	return path
}

// Video generates a clip in dir and returns its path.
func Video(t testing.TB, dir string, s Spec) string {
	t.Helper()
	Require(t)
	if s.Name == "" {
		s.Name = "fixture.mp4"
	}
	if s.Width == 0 {
		s.Width, s.Height = 320, 240
	}
	if s.Rate == "" {
		s.Rate = "30"
	}
	if s.Seconds == 0 {
		s.Seconds = 2
	}
	duration := fmt.Sprintf("%.3f", s.Seconds)
	args := []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%s", s.Width, s.Height, s.Rate)}
	if !s.NoAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	}
	args = append(args, "-t", duration)
	if s.Filter != "" {
		args = append(args, "-vf", s.Filter)
	}
	codec := s.Codec
	if codec == nil {
		codec = []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"}
	}
	args = append(args, codec...)
	if !s.NoAudio {
		args = append(args, "-c:a", "aac", "-b:a", "64k")
	}
	args = append(args, s.Extra...)
	path := filepath.Join(dir, s.Name)
	run(t, append(args, path)...)
	return path
}

// VFR generates a clip whose frame rate drops from 30 to 10 fps halfway through.
func VFR(t testing.TB, dir string, seconds float64) string {
	t.Helper()
	return Video(t, dir, Spec{
		Name:    "vfr.mp4",
		Seconds: seconds,
		Filter:  fmt.Sprintf("select='if(lt(t,%.3f),1,not(mod(n,3)))'", seconds/2),
		Extra:   []string{"-fps_mode", "vfr"},
	})
}

// OffsetStart generates an MPEG-TS whose timestamps start at offset seconds, like a
// recording cut from a broadcast.
func OffsetStart(t testing.TB, dir string, seconds, offset float64) string {
	t.Helper()
	return Video(t, dir, Spec{
		Name:    "offset.ts",
		Seconds: seconds,
		Extra:   []string{"-output_ts_offset", fmt.Sprintf("%.3f", offset)},
	})
}

// BrokenTimestamps generates a Matroska file with a large jump in the video
// timestamps partway through.
func BrokenTimestamps(t testing.TB, dir string, seconds float64) string {
	t.Helper()
	Require(t)
	path := filepath.Join(dir, "broken.mkv")
	run(t,
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", fmt.Sprintf("%.3f", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-bf", "0",
		"-bsf:v", "setts=ts=if(gt(N\\,45)\\,TS+7000\\,TS)",
		"-c:a", "aac", "-b:a", "64k",
		path,
	)
	return path
}

// Rotated generates a clip with a 90 degree display rotation, like a phone
// recording held upright: coded landscape, displayed portrait.
func Rotated(t testing.TB, dir string, width, height int, seconds float64) string {
	t.Helper()
	coded := Video(t, dir, Spec{Name: "coded.mp4", Width: width, Height: height, Seconds: seconds})
	path := filepath.Join(dir, "rotated.mp4")
	run(t, "-display_rotation", "90", "-i", coded, "-c", "copy", path)
	return path
}

// TwoColors generates a clip that is solid red for the first half and solid blue
// for the second, so a test can tell which side of a cut a frame came from.
func TwoColors(t testing.TB, dir string, seconds float64) string {
	t.Helper()
	Require(t)
	half := fmt.Sprintf("%.3f", seconds/2)
	path := filepath.Join(dir, "twocolors.mp4")
	run(t,
		"-f", "lavfi", "-i", "color=c=red:size=320x240:rate=30:d="+half,
		"-f", "lavfi", "-i", "color=c=blue:size=320x240:rate=30:d="+half,
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]",
		"-map", "[v]", "-map", "2:a", "-t", fmt.Sprintf("%.3f", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "300",
		"-c:a", "aac", "-b:a", "64k",
		path,
	)
	return path
}

// ShortAudio generates a clip whose audio stops at audioSeconds, before the video.
func ShortAudio(t testing.TB, dir string, seconds, audioSeconds float64) string {
	t.Helper()
	Require(t)
	path := filepath.Join(dir, "shortaudio.mp4")
	run(t,
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x240:rate=30:d=%.3f", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:d=%.3f", audioSeconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "64k",
		path,
	)
	return path
}

// Hostile writes an HLS playlist with a video extension: the shape of a file that
// tries to make FFmpeg read another local file.
func Hostile(t testing.TB, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "hostile.mp4")
	playlist := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:10.0,\nfile:///etc/passwd\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(path, []byte(playlist), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
