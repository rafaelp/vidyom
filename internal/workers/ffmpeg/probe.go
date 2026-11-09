package ffmpeg

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Rational is an exact frame rate, kept exact so a CFR source keeps its rate (R17).
type Rational struct {
	Num int64
	Den int64
}

// Valid reports whether the rational is usable as a rate.
func (r Rational) Valid() bool { return r.Num > 0 && r.Den > 0 }

// Float returns the value, zero when invalid.
func (r Rational) Float() float64 {
	if !r.Valid() {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// String renders the form FFmpeg takes.
func (r Rational) String() string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

func parseRational(s string) Rational {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return Rational{}
	}
	n, err1 := strconv.ParseInt(num, 10, 64)
	d, err2 := strconv.ParseInt(den, 10, 64)
	if err1 != nil || err2 != nil {
		return Rational{}
	}
	return Rational{n, d}
}

// Info is what the worker knows about a media file.
type Info struct {
	Path string

	// DurationSeconds is the file's timeline length: the container's, or a packet
	// scan's when that is missing or the timestamps are broken (I191).
	DurationSeconds   float64
	ContainerDuration float64
	VideoDuration     float64
	AudioDuration     float64
	StartTime         float64

	HasVideo    bool
	VideoIndex  int
	Width       int // display width, after rotation
	Height      int // display height, after rotation
	Rotation    int
	FrameRate   Rational // average
	FPS         float64
	VFR         bool
	Broken      bool
	VideoCodec  string
	PixelFormat string
	BitDepth    int

	HasAudio        bool
	AudioIndex      int
	AudioCodec      string
	AudioChannels   int
	AudioSampleRate int
	AudioBitrate    int // bits per second, zero when unknown

	Container string // ffprobe's format name
	Ext       string // extension without the dot (R26)
}

// EffectiveHeight is the shorter display side, which selects a profile and is what
// the ceiling limits (I184).
func (i Info) EffectiveHeight() int {
	if i.Width > 0 && i.Width < i.Height {
		return i.Width
	}
	return i.Height
}

// Tolerance is max(100 ms, 1 frame), the timeline tolerance of R10 and D21. A file
// without video uses 100 ms.
func Tolerance(fps float64) float64 {
	if fps > 0 && 1/fps > 0.1 {
		return 1 / fps
	}
	return 0.1
}

type probeStream struct {
	Index       int               `json:"index"`
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	PixFmt      string            `json:"pix_fmt"`
	BitsPerRaw  string            `json:"bits_per_raw_sample"`
	AvgRate     string            `json:"avg_frame_rate"`
	Duration    string            `json:"duration"`
	Channels    int               `json:"channels"`
	SampleRate  string            `json:"sample_rate"`
	BitRate     string            `json:"bit_rate"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
	SideData    []struct {
		Rotation *float64 `json:"rotation"`
	} `json:"side_data_list"`
}

type probeDoc struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		StartTime  string            `json:"start_time"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

func parseSeconds(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// Probe inspects a file. It reads the container's description, then scans the
// video packets' timestamps to detect VFR and broken timestamps (D21).
func (t *Tool) Probe(ctx context.Context, path string) (Info, error) {
	args := append([]string{"-hide_banner", "-v", "error", "-print_format", "json", "-show_format", "-show_streams"}, InputArgs(path)...)
	out, err := t.output(ctx, t.FFprobe, args...)
	if err != nil {
		return Info{}, err
	}
	var doc probeDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return Info{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	info := fromDoc(path, doc)
	var scanned float64
	if info.HasVideo {
		scan, err := t.scanPackets(ctx, path, info.VideoIndex)
		if err != nil {
			return Info{}, err
		}
		info.VFR, info.Broken = scan.classify()
		if !info.FrameRate.Valid() {
			info.applyMeasuredRate(scan.fps())
		}
		scanned = scan.duration()
	}
	// The container's word is enough unless it has none or the timestamps cannot be
	// trusted. Then the timeline is the longer of both streams: an output keeps the
	// longer one, so a video-only figure would reject a healthy file (R10).
	if info.ContainerDuration <= 0 || info.Broken {
		if info.HasAudio {
			audio, err := t.scanPackets(ctx, path, info.AudioIndex)
			if err != nil {
				return Info{}, err
			}
			scanned = math.Max(scanned, audio.duration())
		}
		if scanned > 0 {
			info.DurationSeconds = scanned
		}
	}
	return info, nil
}

// applyMeasuredRate fills the rate of a stream that declares no average, such as a
// WebM without a default duration, from its timestamps. r_frame_rate is no substitute:
// it names the stream's smallest tick, not how many frames it holds (I191). A constant
// stream gets a rate it can keep, while an irregular one is left to TargetRate.
func (i *Info) applyMeasuredRate(fps float64) {
	if fps <= 0 {
		return
	}
	i.FPS = fps
	if i.VFR || i.Broken {
		return
	}
	for _, r := range standardRates {
		if math.Abs(r.Float()-fps) <= fps*0.001 {
			i.FrameRate = r
			return
		}
	}
	i.FrameRate = Rational{int64(math.Round(fps * 1000)), 1000}
}

func fromDoc(path string, doc probeDoc) Info {
	info := Info{
		Path:              path,
		Container:         doc.Format.FormatName,
		ContainerDuration: parseSeconds(doc.Format.Duration),
		VideoIndex:        -1,
		AudioIndex:        -1,
	}
	info.StartTime, _ = strconv.ParseFloat(doc.Format.StartTime, 64)
	info.DurationSeconds = info.ContainerDuration
	for _, s := range doc.Streams {
		switch s.CodecType {
		case "video":
			if info.HasVideo || s.Disposition["attached_pic"] == 1 {
				continue
			}
			info.HasVideo = true
			info.VideoIndex = s.Index
			info.VideoCodec = s.CodecName
			info.PixelFormat = s.PixFmt
			info.BitDepth = bitDepth(s)
			info.Rotation = rotation(s)
			info.Width, info.Height = s.Width, s.Height
			if abs := info.Rotation % 180; abs != 0 {
				info.Width, info.Height = s.Height, s.Width
			}
			info.FrameRate = parseRational(s.AvgRate)
			info.FPS = info.FrameRate.Float()
			info.VideoDuration = parseSeconds(s.Duration)
		case "audio":
			if info.HasAudio {
				continue
			}
			info.HasAudio = true
			info.AudioIndex = s.Index
			info.AudioCodec = s.CodecName
			info.AudioChannels = s.Channels
			info.AudioSampleRate, _ = strconv.Atoi(s.SampleRate)
			info.AudioBitrate, _ = strconv.Atoi(s.BitRate)
			info.AudioDuration = parseSeconds(s.Duration)
		}
	}
	if info.DurationSeconds <= 0 {
		info.DurationSeconds = math.Max(info.VideoDuration, info.AudioDuration)
	}
	info.Ext = ExtensionFor(path, doc.Format.FormatName, doc.Format.Tags["major_brand"], info.VideoCodec)
	return info
}

func rotation(s probeStream) int {
	for _, side := range s.SideData {
		if side.Rotation != nil {
			r := int(math.Round(*side.Rotation)) % 360
			if r < 0 {
				r += 360
			}
			return r
		}
	}
	if v, err := strconv.Atoi(s.Tags["rotate"]); err == nil {
		r := v % 360
		if r < 0 {
			r += 360
		}
		return r
	}
	return 0
}

var depthInFormat = regexp.MustCompile(`p(\d{1,2})(le|be)$`)

func bitDepth(s probeStream) int {
	if v, err := strconv.Atoi(s.BitsPerRaw); err == nil && v > 0 {
		return v
	}
	if m := depthInFormat.FindStringSubmatch(s.PixFmt); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v
	}
	return 8
}

var safeExt = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// ExtensionFor returns the extension that names an original (R26): the path's when
// it has one, else derived from the container, because a spooled upload is named by
// ULID and has none.
func ExtensionFor(path, formatName, majorBrand, videoCodec string) string {
	if ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")); safeExt.MatchString(ext) {
		return ext
	}
	switch {
	case strings.HasPrefix(formatName, "mov"):
		if strings.HasPrefix(majorBrand, "qt") {
			return "mov"
		}
		return "mp4"
	case strings.HasPrefix(formatName, "matroska"):
		if videoCodec == "vp8" || videoCodec == "vp9" || videoCodec == "av1" {
			return "webm"
		}
		return "mkv"
	case formatName == "mpegts":
		return "ts"
	case formatName == "mpeg" || formatName == "mpegvideo":
		return "mpg"
	case formatName == "asf":
		return "wmv"
	case formatName == "ogg":
		return "ogv"
	case formatName == "":
		return "bin"
	}
	name, _, _ := strings.Cut(formatName, ",")
	if safeExt.MatchString(name) {
		return name
	}
	return "bin"
}

// packetScan is what the timestamps of a stream say.
type packetScan struct {
	pts         []float64
	dts         []float64
	missingPTS  int
	missingBoth int
	badDTS      int
}

// times returns the timeline to measure. A packet with only a DTS is normal with
// B-frames in AVI and MPEG-PS, and the remaining PTS values would look irregular, so
// a stream missing any PTS is measured by its DTS.
func (s packetScan) times() []float64 {
	if s.missingPTS > 0 && len(s.dts) > 0 {
		return s.dts
	}
	return s.pts
}

func (t *Tool) scanPackets(ctx context.Context, path string, streamIndex int) (packetScan, error) {
	args := append([]string{
		"-hide_banner", "-v", "error",
		"-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,dts_time",
		"-of", "compact=p=0",
	}, InputArgs(path)...)
	cmd := command(ctx, t.FFprobe, args...)
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return packetScan{}, err
	}
	if err := cmd.Start(); err != nil {
		return packetScan{}, &RunError{Tool: "ffprobe", ExitCode: -1, Err: err}
	}
	scan := readPackets(stdout)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return packetScan{}, ctx.Err()
		}
		return packetScan{}, runError("ffprobe", err, stderr.String())
	}
	return scan, nil
}

func readPackets(r io.Reader) packetScan {
	var scan packetScan
	lastDTS := math.Inf(-1)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		var pts, dts string
		for field := range strings.SplitSeq(scanner.Text(), "|") {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "pts_time":
				pts = value
			case "dts_time":
				dts = value
			}
		}
		if pts == "" && dts == "" {
			continue
		}
		ptsValue, ptsErr := strconv.ParseFloat(pts, 64)
		dtsValue, dtsErr := strconv.ParseFloat(dts, 64)
		switch {
		case ptsErr != nil && dtsErr != nil:
			scan.missingBoth++
		case ptsErr != nil:
			scan.missingPTS++
		}
		if ptsErr == nil {
			scan.pts = append(scan.pts, ptsValue)
		}
		if dtsErr == nil {
			if dtsValue <= lastDTS {
				scan.badDTS++
			}
			lastDTS = dtsValue
			scan.dts = append(scan.dts, dtsValue)
		}
	}
	return scan
}

// classify applies the thresholds of I191.
func (s packetScan) classify() (vfr, broken bool) {
	if s.missingBoth > 0 || s.badDTS > 0 {
		broken = true
	}
	deltas := s.sortedDeltas()
	if len(deltas) == 0 {
		return false, broken
	}
	median := medianOf(deltas)
	if median <= 0 {
		return false, true
	}
	// 1.5 ms, not 1: Matroska rounds timestamps to the millisecond, so a constant 120 fps
	// stream alternates 8 and 9 ms and must not read as variable.
	allowed := math.Max(0.10*median, 0.0015)
	gap := math.Max(5, 100*median)
	deviating := 0
	for _, d := range deltas {
		if math.Abs(d-median) > allowed {
			deviating++
		}
		if d > gap {
			broken = true
		}
	}
	threshold := int(math.Ceil(0.005 * float64(len(deltas))))
	if threshold < 2 {
		threshold = 2
	}
	return deviating >= threshold, broken
}

func medianOf(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

func (s packetScan) sortedDeltas() []float64 {
	times := s.times()
	if len(times) < 2 {
		return nil
	}
	sorted := append([]float64(nil), times...)
	sort.Float64s(sorted)
	deltas := make([]float64, 0, len(sorted)-1)
	for i := 1; i < len(sorted); i++ {
		deltas = append(deltas, sorted[i]-sorted[i-1])
	}
	return deltas
}

func (s packetScan) span() (lo, hi float64, ok bool) {
	times := s.times()
	if len(times) == 0 {
		return 0, 0, false
	}
	lo, hi = times[0], times[0]
	for _, v := range times {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	return lo, hi, true
}

// duration estimates the stream's length from its timestamps.
func (s packetScan) duration() float64 {
	lo, hi, ok := s.span()
	if !ok {
		return 0
	}
	last := 0.0
	if deltas := s.sortedDeltas(); len(deltas) > 0 {
		last = medianOf(deltas)
	}
	return hi - lo + last
}

// fps measures the frames per second, zero when there are too few packets.
func (s packetScan) fps() float64 {
	lo, hi, ok := s.span()
	if !ok || hi <= lo {
		return 0
	}
	return float64(len(s.times())-1) / (hi - lo)
}
