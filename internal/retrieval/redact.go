package retrieval

import (
	"sort"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// RedactedText replaces sensitive speech in exports (D30).
const RedactedText = "[redacted]"

// Interval is a span of a timeline in seconds.
type Interval struct {
	Start float64
	End   float64
}

func normalizeIntervals(marks []domain.SensitiveSegment) []Interval {
	intervals := make([]Interval, 0, len(marks))
	for _, mark := range marks {
		if mark.EndSeconds > mark.StartSeconds {
			intervals = append(intervals, Interval{mark.StartSeconds, mark.EndSeconds})
		}
	}
	sort.Slice(intervals, func(a, b int) bool { return intervals[a].Start < intervals[b].Start })
	merged := intervals[:0]
	for _, interval := range intervals {
		if n := len(merged); n > 0 && interval.Start <= merged[n-1].End {
			if interval.End > merged[n-1].End {
				merged[n-1].End = interval.End
			}
			continue
		}
		merged = append(merged, interval)
	}
	return merged
}

func overlapsAny(intervals []Interval, start, end float64) bool {
	for _, interval := range intervals {
		if start == end {
			if start >= interval.Start && start <= interval.End {
				return true
			}
			continue
		}
		if start < interval.End && end > interval.Start {
			return true
		}
	}
	return false
}

// RemoveSensitive returns the transcript with marked speech gone, for external AI and
// anything public (D30). With word timings only the overlapping words go; a segment
// with none goes whole if it touches a mark, because its words' times are unknown.
func RemoveSensitive(t domain.Transcript, marks []domain.SensitiveSegment) domain.Transcript {
	intervals := normalizeIntervals(marks)
	if len(intervals) == 0 {
		return t
	}
	out := t
	out.Segments = make([]domain.TranscriptSegment, 0, len(t.Segments))
	for _, segment := range t.Segments {
		if !overlapsAny(intervals, segment.Start, segment.End) {
			out.Segments = append(out.Segments, segment)
			continue
		}
		if len(segment.Words) == 0 {
			continue
		}
		var kept []domain.Word
		for _, word := range segment.Words {
			if !overlapsAny(intervals, word.Start, word.End) {
				kept = append(kept, word)
			}
		}
		if len(kept) == 0 {
			continue
		}
		trimmed := segment
		trimmed.Words = kept
		trimmed.Start, trimmed.End = kept[0].Start, kept[len(kept)-1].End
		trimmed.Text = joinWords(kept)
		out.Segments = append(out.Segments, trimmed)
	}
	return out
}

// RedactSensitive returns the transcript with marked speech replaced by RedactedText
// and its timing kept, for exports and review excerpts (D30, I210).
func RedactSensitive(t domain.Transcript, marks []domain.SensitiveSegment) domain.Transcript {
	intervals := normalizeIntervals(marks)
	if len(intervals) == 0 {
		return t
	}
	out := t
	out.Segments = make([]domain.TranscriptSegment, 0, len(t.Segments))
	for _, segment := range t.Segments {
		if !overlapsAny(intervals, segment.Start, segment.End) {
			out.Segments = append(out.Segments, segment)
			continue
		}
		redacted := segment
		if len(segment.Words) == 0 {
			redacted.Text = RedactedText
			out.Segments = append(out.Segments, redacted)
			continue
		}
		words := make([]domain.Word, 0, len(segment.Words))
		for _, word := range segment.Words {
			if !overlapsAny(intervals, word.Start, word.End) {
				words = append(words, word)
				continue
			}
			if n := len(words); n > 0 && words[n-1].Text == RedactedText {
				words[n-1].End = word.End
				continue
			}
			words = append(words, domain.Word{Start: word.Start, End: word.End, Text: RedactedText})
		}
		redacted.Words = words
		redacted.Text = joinWords(words)
		out.Segments = append(out.Segments, redacted)
	}
	return out
}

// MapToDerived maps intervals on a source's timeline onto a derivative's, given the
// source intervals the derivative was cut from, in timeline order (I207). Clips of
// clips work because a clip's marks are themselves mapped.
func MapToDerived(source []Interval, cuts []domain.Segment) []Interval {
	var out []Interval
	offset := 0.0
	for _, cut := range cuts {
		for _, interval := range source {
			start := max(interval.Start, cut.StartSeconds)
			end := min(interval.End, cut.EndSeconds)
			if end > start {
				out = append(out, Interval{offset + start - cut.StartSeconds, offset + end - cut.StartSeconds})
			}
		}
		offset += cut.EndSeconds - cut.StartSeconds
	}
	return out
}

// MarksToIntervals returns merged intervals of sensitive marks.
func MarksToIntervals(marks []domain.SensitiveSegment) []Interval {
	return normalizeIntervals(marks)
}
