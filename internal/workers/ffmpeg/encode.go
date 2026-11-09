package ffmpeg

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rafaelp/vidyom/profiles"
)

// Standard rates a VFR or broken source is normalized to (I191).
var standardRates = []Rational{
	{24000, 1001}, {24, 1}, {25, 1}, {30000, 1001}, {30, 1}, {50, 1}, {60000, 1001}, {60, 1},
}

const maxNormalizedRate = 120

// TargetRate returns the constant rate an output is written at. A CFR source keeps
// its exact rate (R17). A VFR or broken source takes the nearest standard rate
// within 1.5% of its average, else the next standard rate above it, else its
// rounded average when above 60, capped (I191, D21).
func TargetRate(info Info) Rational {
	if !info.VFR && !info.Broken && info.FrameRate.Valid() {
		return info.FrameRate
	}
	avg := info.FPS
	if avg <= 0 {
		return Rational{30, 1}
	}
	var nearest Rational
	for _, r := range standardRates {
		if math.Abs(r.Float()-avg) <= avg*0.015 && (!nearest.Valid() || math.Abs(r.Float()-avg) < math.Abs(nearest.Float()-avg)) {
			nearest = r
		}
	}
	if nearest.Valid() {
		return nearest
	}
	for _, r := range standardRates {
		if r.Float() > avg {
			return r
		}
	}
	return Rational{int64(math.Min(math.Round(avg), maxNormalizedRate)), 1}
}

// TargetDimensions scales so the shorter side does not exceed ceiling, never
// upward (R17, I184), and rounds down to even, as 4:2:0 needs.
func TargetDimensions(width, height, ceiling int) (int, int) {
	short := min(width, height)
	w, h := width, height
	if ceiling > 0 && short > ceiling {
		scale := float64(ceiling) / float64(short)
		w = int(math.Round(float64(width) * scale))
		h = int(math.Round(float64(height) * scale))
	}
	return max(w&^1, 2), max(h&^1, 2)
}

// baseOutputArgs are the options every encode shares (I190).
func baseOutputArgs() []string {
	return []string{"-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1"}
}

// pixelFormat picks the output format: the profile's when set, otherwise 8-bit
// 4:2:0 unless the source has more depth, which HEVC can keep.
func pixelFormat(v profiles.Video, src Info) string {
	if v.PixelFormat != "" {
		return v.PixelFormat
	}
	if src.BitDepth > 8 {
		return "yuv420p10le"
	}
	return "yuv420p"
}

func videoCodecArgs(v profiles.Video, src Info) []string {
	args := []string{"-c:v", v.Codec, "-preset", v.Preset, "-crf", strconv.Itoa(v.CRF), "-pix_fmt", pixelFormat(v, src)}
	if v.Profile != "" {
		args = append(args, "-profile:v", v.Profile)
	}
	if v.Codec == "libx265" {
		args = append(args, "-tag:v", "hvc1", "-x265-params", "log-level=error")
	}
	return args
}

func audioCodecArgs(a profiles.Audio) []string {
	args := []string{"-c:a", a.Codec, "-b:a", a.Bitrate}
	if a.Channels > 0 {
		args = append(args, "-ac", strconv.Itoa(a.Channels))
	}
	if a.SampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(a.SampleRate))
	}
	return args
}

// muxerArgs names the muxer, because the temp file's name hides the extension.
func muxerArgs(p profiles.Profile, faststart bool) []string {
	var args []string
	if p.Container == "mp4" && faststart {
		args = append(args, "-movflags", "+faststart")
	}
	return append(args, "-f", muxerFor(p.Container))
}

func muxerFor(container string) string {
	switch container {
	case "m4a":
		return "ipod"
	default:
		return container
	}
}

// timelineAudioFilter keeps audio on the source's timeline: gaps filled, the same
// zero instant (R10, D21).
const timelineAudioFilter = "aresample=async=1:first_pts=0"

// TranscodeArgs builds a whole-file encode to a video profile. The output has
// constant frame rate, the source's zero instant, and no upscaling.
func TranscodeArgs(src Info, out string, p profiles.Profile) ([]string, error) {
	if p.Video == nil {
		return nil, fmt.Errorf("profile %s has no video", p.Ref())
	}
	if !src.HasVideo {
		return nil, fmt.Errorf("source has no video stream")
	}
	var input []string
	if src.Broken {
		input = append(input, "-fflags", "+genpts+igndts")
	}
	args := InputArgs(src.Path, input...)
	args = append(args, "-map", fmt.Sprintf("0:%d", src.VideoIndex))
	if src.HasAudio {
		args = append(args, "-map", fmt.Sprintf("0:%d", src.AudioIndex))
	}
	args = append(args, baseOutputArgs()...)
	w, h := TargetDimensions(src.Width, src.Height, p.Video.MaxHeight)
	if w != src.Width || h != src.Height {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", w, h))
	}
	args = append(args, "-fps_mode", "cfr", "-r", TargetRate(src).String())
	args = append(args, videoCodecArgs(*p.Video, src)...)
	if src.HasAudio {
		args = append(args, "-af", timelineAudioFilter)
		args = append(args, audioCodecArgs(p.Audio)...)
	}
	args = append(args, muxerArgs(p, p.Faststart)...)
	return append(args, out), nil
}

// opusRates are the sample rates Opus encodes natively.
var opusRates = []int{8000, 12000, 16000, 24000, 48000}

// ExtractSampleRate is the largest supported rate not above both the profile's and
// the source's, so audio is never upsampled (D40, I193).
func ExtractSampleRate(a profiles.Audio, sourceRate int) int {
	target := a.SampleRate
	if sourceRate > 0 && (target == 0 || sourceRate < target) {
		target = sourceRate
	}
	if a.Codec != "libopus" {
		return target
	}
	best := opusRates[0]
	for _, r := range opusRates {
		if r <= target {
			best = r
		}
	}
	return best
}

// ExtractBitrate caps the profile's bitrate at the source's when known (D40).
func ExtractBitrate(a profiles.Audio, sourceBitrate int) string {
	kilo, err := strconv.Atoi(strings.TrimSuffix(a.Bitrate, "k"))
	if err != nil || sourceBitrate <= 0 {
		return a.Bitrate
	}
	if source := sourceBitrate / 1000; source >= 6 && source < kilo {
		return strconv.Itoa(source) + "k"
	}
	return a.Bitrate
}

// AudioExtractArgs builds the ASR audio extraction (§4.2, D56). A track shorter
// than the file is padded, so the output has the file's timeline (I193).
func AudioExtractArgs(src Info, out string, p profiles.Profile) ([]string, error) {
	if !p.AudioOnly() {
		return nil, fmt.Errorf("profile %s is not audio-only", p.Ref())
	}
	if !src.HasAudio {
		return nil, fmt.Errorf("source has no audio stream")
	}
	audio := p.Audio
	audio.SampleRate = ExtractSampleRate(p.Audio, src.AudioSampleRate)
	audio.Bitrate = ExtractBitrate(p.Audio, src.AudioBitrate)
	if audio.Channels == 0 || (src.AudioChannels > 0 && src.AudioChannels < audio.Channels) {
		audio.Channels = src.AudioChannels
	}
	args := InputArgs(src.Path)
	args = append(args, "-map", fmt.Sprintf("0:%d", src.AudioIndex), "-vn")
	args = append(args, baseOutputArgs()...)
	filter := timelineAudioFilter
	if src.DurationSeconds > 0 {
		filter += fmt.Sprintf(",apad=whole_dur=%.6f", src.DurationSeconds)
	}
	args = append(args, "-af", filter)
	args = append(args, audioCodecArgs(audio)...)
	args = append(args, muxerArgs(p, false)...)
	return append(args, out), nil
}

// Segment is one interval to render, in the source's timeline.
type Segment struct {
	Start float64
	End   float64
}

// SegmentEncode says how segments are rendered.
type SegmentEncode struct {
	Video      profiles.Video
	Audio      profiles.Audio
	Container  string
	Faststart  bool
	MaxShort   int // ceiling for the shorter side
	ExtraVideo []string
}

// SegmentArgs renders segments from one source, frame-accurately, by decoding each
// from an input seek and joining with the concat filter (I192). Stream copy is
// never used, since it can only cut at keyframes.
func SegmentArgs(src Info, segments []Segment, out string, enc SegmentEncode) ([]string, error) {
	if !src.HasVideo {
		return nil, fmt.Errorf("source has no video stream")
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("no segments")
	}
	var args []string
	var filter strings.Builder
	for i, s := range segments {
		length := s.End - s.Start
		args = append(args, InputArgs(src.Path,
			"-ss", fmt.Sprintf("%.6f", s.Start),
			"-t", fmt.Sprintf("%.6f", length),
		)...)
		fmt.Fprintf(&filter, "[%d:%d]setpts=PTS-STARTPTS[v%d];", i, src.VideoIndex, i)
		if src.HasAudio {
			fmt.Fprintf(&filter, "[%d:%d]%s,apad=whole_dur=%.6f,atrim=0:%.6f,asetpts=PTS-STARTPTS[a%d];",
				i, src.AudioIndex, timelineAudioFilter, length, length, i)
		}
	}
	for i := range segments {
		fmt.Fprintf(&filter, "[v%d]", i)
		if src.HasAudio {
			fmt.Fprintf(&filter, "[a%d]", i)
		}
	}
	audioStreams := 0
	if src.HasAudio {
		audioStreams = 1
	}
	fmt.Fprintf(&filter, "concat=n=%d:v=1:a=%d", len(segments), audioStreams)
	if src.HasAudio {
		filter.WriteString("[vjoined][ajoined];")
	} else {
		filter.WriteString("[vjoined];")
	}
	w, h := TargetDimensions(src.Width, src.Height, enc.MaxShort)
	fmt.Fprintf(&filter, "[vjoined]scale=%d:%d:flags=lanczos[vout]", w, h)
	args = append(args, "-filter_complex", filter.String(), "-map", "[vout]")
	if src.HasAudio {
		args = append(args, "-map", "[ajoined]")
	}
	args = append(args, baseOutputArgs()...)
	args = append(args, "-fps_mode", "cfr", "-r", TargetRate(src).String())
	args = append(args, videoCodecArgs(enc.Video, src)...)
	args = append(args, enc.ExtraVideo...)
	if src.HasAudio {
		args = append(args, audioCodecArgs(enc.Audio)...)
	}
	if enc.Container == "mp4" && enc.Faststart {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-f", muxerFor(enc.Container), out)
	return args, nil
}

// thumbnailMaxWidth bounds a thumbnail's longer side (I192).
const thumbnailMaxWidth = 1280

// ThumbnailArgs extracts one JPEG at a position, never upscaled.
func ThumbnailArgs(src Info, at float64, out string) ([]string, error) {
	if !src.HasVideo {
		return nil, fmt.Errorf("source has no video stream")
	}
	args := InputArgs(src.Path, "-ss", fmt.Sprintf("%.6f", at))
	args = append(args, "-map", fmt.Sprintf("0:%d", src.VideoIndex), "-an")
	args = append(args, baseOutputArgs()...)
	w, h := src.Width, src.Height
	if longer := max(w, h); longer > thumbnailMaxWidth {
		scale := float64(thumbnailMaxWidth) / float64(longer)
		w = int(math.Round(float64(w) * scale))
		h = int(math.Round(float64(h) * scale))
	}
	args = append(args,
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", max(w&^1, 2), max(h&^1, 2)),
		"-frames:v", "1", "-q:v", "3", "-pix_fmt", "yuvj420p", "-update", "1", "-f", "image2", out,
	)
	return args, nil
}
