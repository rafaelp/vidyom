package retrieval

import (
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// DerivedEnginePrefix marks a transcript mapped from its source instead of transcribed.
const DerivedEnginePrefix = "derived:"

// DeriveTranscript maps a source transcript onto a clip's timeline (§4.3): for each
// interval in order, the speech inside it is moved to where that interval lands in the
// clip. transcriptFor returns the source transcript of a version. With word timings a
// word stays if its midpoint is inside the interval; without them a segment stays if
// its own midpoint is, clamped to the interval, because a clip must not caption speech
// it does not contain.
func DeriveTranscript(cuts []domain.Segment, transcriptFor func(version string) (domain.Transcript, error)) (domain.Transcript, error) {
	if len(cuts) == 0 {
		return domain.Transcript{}, errs.New(errs.CodeInvalidArgument, "no segments to derive a transcript from")
	}
	out := domain.Transcript{TimestampBasis: "working_master", Segments: []domain.TranscriptSegment{}}
	offset := 0.0
	for _, cut := range cuts {
		if cut.EndSeconds <= cut.StartSeconds || cut.StartSeconds < 0 {
			return domain.Transcript{}, errs.New(errs.CodeInvalidArgument, "a segment needs 0 <= start < end")
		}
		source, err := transcriptFor(cut.SourceVersion)
		if err != nil {
			return domain.Transcript{}, err
		}
		if out.Language == "" {
			out.Language = source.Language
		}
		if out.Engine == "" {
			out.Engine = DerivedEnginePrefix + strings.TrimPrefix(source.Engine, DerivedEnginePrefix)
		}
		shift := offset - cut.StartSeconds
		for _, segment := range source.Segments {
			if mapped, ok := mapSegment(segment, cut, shift); ok {
				out.Segments = append(out.Segments, mapped)
			}
		}
		offset += cut.EndSeconds - cut.StartSeconds
	}
	out.DurationSeconds = offset
	return out, nil
}

func midpoint(start, end float64) float64 { return start + (end-start)/2 }

func insideCut(cut domain.Segment, start, end float64) bool {
	middle := midpoint(start, end)
	return middle >= cut.StartSeconds && middle < cut.EndSeconds
}

func mapSegment(segment domain.TranscriptSegment, cut domain.Segment, shift float64) (domain.TranscriptSegment, bool) {
	clamp := func(t float64) float64 { return min(max(t, cut.StartSeconds), cut.EndSeconds) + shift }
	if len(segment.Words) > 0 {
		var words []domain.Word
		for _, word := range segment.Words {
			if insideCut(cut, word.Start, word.End) {
				words = append(words, domain.Word{Start: clamp(word.Start), End: clamp(word.End), Text: word.Text})
			}
		}
		if len(words) == 0 {
			return domain.TranscriptSegment{}, false
		}
		text := joinWords(words)
		if len(words) == len(segment.Words) {
			text = segment.Text
		}
		return domain.TranscriptSegment{
			Start:   words[0].Start,
			End:     words[len(words)-1].End,
			Text:    text,
			Words:   words,
			Speaker: segment.Speaker,
		}, true
	}
	if !insideCut(cut, segment.Start, segment.End) {
		return domain.TranscriptSegment{}, false
	}
	return domain.TranscriptSegment{
		Start:   clamp(segment.Start),
		End:     clamp(segment.End),
		Text:    segment.Text,
		Speaker: segment.Speaker,
	}, true
}
