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
	RRate       string            `json:"r_frame_rate"`
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
	if info.HasVideo {
		scan, err := t.scanPackets(ctx, path, info.VideoIndex)
		if err != nil {
			return Info{}, err
		}
		info.VFR, info.Broken = scan.classify()
		if info.ContainerDuration <= 0 || info.Broken {
			if d := scan.duration(); d > 0 {
				info.DurationSeconds = d
			}
		}
	}
	return info, nil
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
			if !info.FrameRate.Valid() {
				info.FrameRate = parseRational(s.RRate)
			}
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

// packetScan is what the timestamps of a video stream say.
type packetScan struct {
	pts        []float64
	missingPTS int
	badDTS     int
	total      int
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
		scan.total++
		if v, err := strconv.ParseFloat(pts, 64); err == nil {
			scan.pts = append(scan.pts, v)
		} else {
			scan.missingPTS++
		}
		if v, err := strconv.ParseFloat(dts, 64); err == nil {
			if v <= lastDTS {
				scan.badDTS++
			}
			lastDTS = v
		}
	}
	return scan
}

// classify applies the thresholds of I191.
func (s packetScan) classify() (vfr, broken bool) {
	if s.missingPTS > 0 || s.badDTS > 0 {
		broken = true
	}
	deltas := s.sortedDeltas()
	if len(deltas) == 0 {
		return false, broken
	}
	sorted := append([]float64(nil), deltas...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	if median <= 0 {
		return false, true
	}
	allowed := math.Max(0.10*median, 0.001)
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

func (s packetScan) sortedDeltas() []float64 {
	if len(s.pts) < 2 {
		return nil
	}
	pts := append([]float64(nil), s.pts...)
	sort.Float64s(pts)
	deltas := make([]float64, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		deltas = append(deltas, pts[i]-pts[i-1])
	}
	return deltas
}

// duration estimates the stream's length from its timestamps.
func (s packetScan) duration() float64 {
	if len(s.pts) == 0 {
		return 0
	}
	lo, hi := s.pts[0], s.pts[0]
	for _, v := range s.pts {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	last := 0.0
	if deltas := s.sortedDeltas(); len(deltas) > 0 {
		sorted := append([]float64(nil), deltas...)
		sort.Float64s(sorted)
		last = sorted[len(sorted)/2]
	}
	return hi - lo + last
}
