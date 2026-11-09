package retrieval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Open transcript formats (§4.1).
const (
	FormatJSON     = "json"
	FormatSRT      = "srt"
	FormatVTT      = "vtt"
	FormatMarkdown = "md"
)

// ExportFormats are all the formats, in the order they are written.
var ExportFormats = []string{FormatJSON, FormatSRT, FormatVTT, FormatMarkdown}

// RenderTranscript writes a transcript in an open format.
func RenderTranscript(t domain.Transcript, format string) ([]byte, error) {
	switch format {
	case FormatJSON:
		return renderJSON(t)
	case FormatSRT:
		return renderSRT(t), nil
	case FormatVTT:
		return renderVTT(t), nil
	case FormatMarkdown:
		return renderMarkdown(t), nil
	default:
		return nil, errs.Newf(errs.CodeInvalidArgument, "unknown transcript format %q", format)
	}
}

func renderJSON(t domain.Transcript) ([]byte, error) {
	if t.Segments == nil {
		t.Segments = []domain.TranscriptSegment{}
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(t); err != nil {
		return nil, fmt.Errorf("encode transcript: %w", err)
	}
	return buffer.Bytes(), nil
}

// timecode formats milliseconds as HH:MM:SS<sep>mmm. Timestamps are rounded once here,
// from the total, so a value just under a second boundary cannot print 1000 ms.
func timecode(seconds float64, separator string) string {
	total := int64(math.Round(max(seconds, 0) * 1000))
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", total/3600000, total/60000%60, total/1000%60, separator, total%1000)
}

// cueText keeps a cue on its own lines: a blank line would end a cue early.
func cueText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func renderSRT(t domain.Transcript) []byte {
	var buffer bytes.Buffer
	index := 0
	for _, segment := range t.Segments {
		text := cueText(segment.Text)
		if text == "" {
			continue
		}
		index++
		fmt.Fprintf(&buffer, "%d\n%s --> %s\n%s\n\n", index, timecode(segment.Start, ","), timecode(segment.End, ","), text)
	}
	return buffer.Bytes()
}

var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func renderVTT(t domain.Transcript) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("WEBVTT\n\n")
	for _, segment := range t.Segments {
		text := cueText(segment.Text)
		if text == "" {
			continue
		}
		fmt.Fprintf(&buffer, "%s --> %s\n%s\n\n", timecode(segment.Start, "."), timecode(segment.End, "."), vttEscaper.Replace(text))
	}
	return buffer.Bytes()
}

func renderMarkdown(t domain.Transcript) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("# Transcript\n\n")
	if t.Engine != "" {
		fmt.Fprintf(&buffer, "Engine: `%s`\n\n", t.Engine)
	}
	for _, segment := range t.Segments {
		text := cueText(segment.Text)
		if text == "" {
			continue
		}
		speaker := ""
		if segment.Speaker != "" {
			speaker = " " + cueText(segment.Speaker) + ":"
		}
		fmt.Fprintf(&buffer, "**[%s]**%s %s\n\n", timecode(segment.Start, "."), speaker, text)
	}
	return buffer.Bytes()
}
