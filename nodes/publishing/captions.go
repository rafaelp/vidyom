package publishingnodes

import (
	"fmt"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// renderCaptions writes released segments as WebVTT or SRT. The segments must
// already have passed the sensitive filter (D30, I228).
func renderCaptions(format string, segments []domain.TranscriptSegment) (string, error) {
	var b strings.Builder
	switch format {
	case "vtt":
		b.WriteString("WEBVTT\n\n")
	case "srt":
	default:
		return "", errs.Newf(errs.CodeInvalidArgument, "caption format %q is not supported: use vtt or srt", format)
	}
	cue := 0
	for _, s := range segments {
		text := cueText(s.Text)
		if text == "" {
			continue
		}
		cue++
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", cue, cueTime(s.Start, format), cueTime(s.End, format), text)
	}
	if cue == 0 {
		return "", nil
	}
	return b.String(), nil
}

// cueText keeps one cue on one line and takes out what would break the format: a
// blank line ends a cue, and "-->" is reserved in WebVTT.
func cueText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "-->", "->")
}

func cueTime(seconds float64, format string) string {
	if seconds < 0 {
		seconds = 0
	}
	ms := int64(seconds*1000 + 0.5)
	h := ms / 3_600_000
	m := ms / 60_000 % 60
	s := ms / 1000 % 60
	sep := "."
	if format == "srt" {
		sep = ","
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", h, m, s, sep, ms%1000)
}
