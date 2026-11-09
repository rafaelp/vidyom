package retrieval

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

func exportTranscript() domain.Transcript {
	return domain.Transcript{
		Language:        "en",
		DurationSeconds: 3700,
		Engine:          "fake/canned@1",
		TimestampBasis:  "working_master",
		Segments: []domain.TranscriptSegment{
			{Start: 0, End: 1.5, Text: "First  line\n\nwith a blank line."},
			{Start: 3661.2345, End: 3662.9996, Text: "A <tag> & --> arrow.", Speaker: "A"},
			{Start: 3700, End: 3700, Text: "   "},
		},
	}
}

func TestRenderSRT(t *testing.T) {
	data, err := RenderTranscript(exportTranscript(), FormatSRT)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:00:00,000 --> 00:00:01,500\nFirst line with a blank line.\n\n" +
		"2\n01:01:01,235 --> 01:01:03,000\nA <tag> & --> arrow.\n\n"
	if string(data) != want {
		t.Fatalf("got:\n%s", data)
	}
}

func TestRenderVTTEscapes(t *testing.T) {
	data, err := RenderTranscript(exportTranscript(), FormatVTT)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "WEBVTT\n\n00:00:00.000 --> 00:00:01.500\n") {
		t.Fatalf("got:\n%s", text)
	}
	if !strings.Contains(text, "A &lt;tag&gt; &amp; --&gt; arrow.") {
		t.Fatalf("got:\n%s", text)
	}
}

func TestRenderMarkdown(t *testing.T) {
	data, err := RenderTranscript(exportTranscript(), FormatMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "`fake/canned@1`") || !strings.Contains(text, "**[01:01:01.235]** A: A <tag>") {
		t.Fatalf("got:\n%s", text)
	}
}

func TestRenderJSON(t *testing.T) {
	data, err := RenderTranscript(exportTranscript(), FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.Transcript
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Engine != "fake/canned@1" || len(decoded.Segments) != 3 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if !strings.Contains(string(data), "<tag>") {
		t.Fatalf("an open format should stay readable: %s", data)
	}
	empty, err := RenderTranscript(domain.Transcript{}, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"segments": []`) {
		t.Fatalf("got %s", empty)
	}
}

func TestRenderUnknownFormat(t *testing.T) {
	if _, err := RenderTranscript(domain.Transcript{}, "docx"); err == nil {
		t.Fatal("must refuse")
	}
}

func TestTimecodeCarriesIntoSeconds(t *testing.T) {
	if got := timecode(1.9996, ","); got != "00:00:02,000" {
		t.Fatalf("got %s", got)
	}
	if got := timecode(-1, "."); got != "00:00:00.000" {
		t.Fatalf("got %s", got)
	}
}
