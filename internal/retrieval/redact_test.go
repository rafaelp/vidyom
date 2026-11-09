package retrieval

import (
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

func wordsTranscript() domain.Transcript {
	return domain.Transcript{
		Language: "en",
		Segments: []domain.TranscriptSegment{
			{Start: 0, End: 4, Text: "Maria Silva has diabetes", Words: []domain.Word{
				{Start: 0, End: 1, Text: "Maria"},
				{Start: 1, End: 2, Text: "Silva"},
				{Start: 2, End: 3, Text: "has"},
				{Start: 3, End: 4, Text: "diabetes"},
			}},
			{Start: 4, End: 6, Text: "No words on this one", Speaker: "A"},
			{Start: 6, End: 8, Text: "Safe sentence."},
		},
	}
}

func mark(start, end float64) []domain.SensitiveSegment {
	return []domain.SensitiveSegment{{StartSeconds: start, EndSeconds: end}}
}

func allText(t domain.Transcript) string {
	var parts []string
	for _, s := range t.Segments {
		parts = append(parts, s.Text)
		for _, w := range s.Words {
			parts = append(parts, w.Text)
		}
	}
	return strings.Join(parts, " ")
}

func TestRemoveDropsOnlyOverlappingWords(t *testing.T) {
	out := RemoveSensitive(wordsTranscript(), mark(0, 2))
	if out.Segments[0].Text != "has diabetes" || len(out.Segments[0].Words) != 2 {
		t.Fatalf("segment = %+v", out.Segments[0])
	}
	if out.Segments[0].Start != 2 {
		t.Fatalf("start = %f", out.Segments[0].Start)
	}
	if strings.Contains(allText(out), "Maria") || strings.Contains(allText(out), "Silva") {
		t.Fatal("marked words survived")
	}
}

// Timing inside a segment with no word timestamps is unknown, so all of it goes.
func TestRemoveDropsWholeSegmentWithoutWords(t *testing.T) {
	out := RemoveSensitive(wordsTranscript(), mark(5, 5.5))
	if strings.Contains(allText(out), "No words") {
		t.Fatal("a segment with no word timings must go whole")
	}
	if len(out.Segments) != 2 {
		t.Fatalf("segments = %+v", out.Segments)
	}
}

func TestRemoveDropsSegmentWhenAllWordsGo(t *testing.T) {
	out := RemoveSensitive(wordsTranscript(), mark(0, 4))
	if strings.Contains(allText(out), "diabetes") {
		t.Fatal("marked words survived")
	}
	if len(out.Segments) != 2 {
		t.Fatalf("segments = %+v", out.Segments)
	}
}

func TestRemoveWithNoMarksChangesNothing(t *testing.T) {
	original := wordsTranscript()
	out := RemoveSensitive(original, nil)
	if allText(out) != allText(original) {
		t.Fatal("changed")
	}
}

func TestRemoveDoesNotChangeTheOriginal(t *testing.T) {
	original := wordsTranscript()
	before := allText(original)
	_ = RemoveSensitive(original, mark(0, 8))
	_ = RedactSensitive(original, mark(0, 8))
	if allText(original) != before {
		t.Fatal("the original was modified")
	}
}

func TestRemoveIgnoresInvalidMarksAndMerges(t *testing.T) {
	marks := []domain.SensitiveSegment{
		{StartSeconds: 3, EndSeconds: 3},
		{StartSeconds: 7, EndSeconds: 6},
		{StartSeconds: 1, EndSeconds: 2},
		{StartSeconds: 1.5, EndSeconds: 3},
	}
	intervals := MarksToIntervals(marks)
	if len(intervals) != 1 || intervals[0].Start != 1 || intervals[0].End != 3 {
		t.Fatalf("intervals = %+v", intervals)
	}
}

func TestBoundaryTouchingIsNotOverlap(t *testing.T) {
	// The 4-6 segment only touches a mark starting at 6, and the 6-8 segment overlaps it.
	out := RemoveSensitive(wordsTranscript(), mark(6, 7))
	if !strings.Contains(allText(out), "No words") {
		t.Fatal("a segment ending exactly where a mark starts does not overlap it")
	}
	if strings.Contains(allText(out), "Safe") {
		t.Fatal("the overlapping segment must go")
	}
}

func TestRedactKeepsTimingAndHidesText(t *testing.T) {
	out := RedactSensitive(wordsTranscript(), mark(0, 2))
	if out.Segments[0].Text != "[redacted] has diabetes" {
		t.Fatalf("text = %q", out.Segments[0].Text)
	}
	if len(out.Segments[0].Words) != 3 || out.Segments[0].Words[0].End != 2 {
		t.Fatalf("words = %+v", out.Segments[0].Words)
	}
	if out.Segments[0].Start != 0 || out.Segments[0].End != 4 {
		t.Fatal("timing must stay")
	}
	if strings.Contains(allText(out), "Maria") {
		t.Fatal("marked words survived")
	}
}

func TestRedactSegmentWithoutWords(t *testing.T) {
	out := RedactSensitive(wordsTranscript(), mark(5, 5.1))
	if out.Segments[1].Text != RedactedText || out.Segments[1].Speaker != "A" {
		t.Fatalf("segment = %+v", out.Segments[1])
	}
}

func TestMapToDerived(t *testing.T) {
	cuts := []domain.Segment{{StartSeconds: 100, EndSeconds: 110}, {StartSeconds: 200, EndSeconds: 205}}
	source := []Interval{{95, 102}, {108, 201}, {300, 400}}
	out := MapToDerived(source, cuts)
	want := []Interval{{0, 2}, {8, 10}, {10, 11}}
	if len(out) != len(want) {
		t.Fatalf("out = %+v", out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, out[i], want[i])
		}
	}
}

func TestDeriveMapsWordsToTheClipTimeline(t *testing.T) {
	source := domain.Transcript{
		Language: "pt",
		Engine:   "faster-whisper/large-v3",
		Segments: []domain.TranscriptSegment{
			{Start: 10, End: 14, Text: "um dois tres quatro", Speaker: "A", Words: []domain.Word{
				{Start: 10, End: 11, Text: "um"}, {Start: 11, End: 12, Text: "dois"}, {Start: 12, End: 13, Text: "tres"}, {Start: 13, End: 14, Text: "quatro"},
			}},
			{Start: 20, End: 22, Text: "cinco seis"},
			{Start: 30, End: 32, Text: "sete oito"},
		},
	}
	cuts := []domain.Segment{
		{SourceVersion: "v1", StartSeconds: 11, EndSeconds: 13},
		{SourceVersion: "v1", StartSeconds: 19, EndSeconds: 23},
	}
	out, err := DeriveTranscript(cuts, func(version string) (domain.Transcript, error) {
		if version != "v1" {
			t.Errorf("version = %q", version)
		}
		return source, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Engine != "derived:faster-whisper/large-v3" || out.Language != "pt" || out.TimestampBasis != "working_master" {
		t.Fatalf("header = %+v", out)
	}
	if out.DurationSeconds != 6 {
		t.Fatalf("duration = %f", out.DurationSeconds)
	}
	if len(out.Segments) != 2 {
		t.Fatalf("segments = %+v", out.Segments)
	}
	first := out.Segments[0]
	if first.Text != "dois tres" || first.Start != 0 || first.End != 2 || first.Speaker != "A" || len(first.Words) != 2 {
		t.Fatalf("first = %+v", first)
	}
	second := out.Segments[1]
	if second.Text != "cinco seis" || second.Start != 3 || second.End != 5 {
		t.Fatalf("second = %+v", second)
	}
}

func TestDeriveKeepsSegmentsByMidpointWhenNoWords(t *testing.T) {
	source := domain.Transcript{Engine: "e", Segments: []domain.TranscriptSegment{
		{Start: 0, End: 10, Text: "mostly outside"},
		{Start: 10, End: 20, Text: "mostly inside"},
	}}
	out, err := DeriveTranscript([]domain.Segment{{StartSeconds: 8, EndSeconds: 18}}, func(string) (domain.Transcript, error) { return source, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Segments) != 1 || out.Segments[0].Text != "mostly inside" {
		t.Fatalf("segments = %+v", out.Segments)
	}
	if out.Segments[0].Start != 2 || out.Segments[0].End != 10 {
		t.Fatalf("clamped to the cut: %+v", out.Segments[0])
	}
}

func TestDeriveRejectsBadInput(t *testing.T) {
	lookup := func(string) (domain.Transcript, error) { return domain.Transcript{}, nil }
	if _, err := DeriveTranscript(nil, lookup); err == nil {
		t.Fatal("no segments")
	}
	if _, err := DeriveTranscript([]domain.Segment{{StartSeconds: 5, EndSeconds: 5}}, lookup); err == nil {
		t.Fatal("empty interval")
	}
	if _, err := DeriveTranscript([]domain.Segment{{StartSeconds: -1, EndSeconds: 5}}, lookup); err == nil {
		t.Fatal("negative start")
	}
}

// Deriving a clip from a clip must not stack the prefix.
func TestDerivedPrefixDoesNotStack(t *testing.T) {
	out, err := DeriveTranscript([]domain.Segment{{StartSeconds: 0, EndSeconds: 1}}, func(string) (domain.Transcript, error) {
		return domain.Transcript{Engine: "derived:fake/canned@1"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Engine != "derived:fake/canned@1" {
		t.Fatalf("engine = %q", out.Engine)
	}
	if out.Segments == nil {
		t.Fatal("segments must encode as an array, never null")
	}
}
