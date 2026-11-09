package publicexport

import (
	"context"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// Interval is a span on an asset's own timeline.
type Interval struct {
	Start float64
	End   float64
}

// SensitiveIntervals returns what must not reach anything public for an asset
// version (D30): its own marks, plus each lineage source's marks mapped onto its
// timeline. ai.index copies marks onto a derivative when it is indexed; reading the
// sources here too means a mark added later still counts (I207, I231).
//
// An error means the answer is unknown, so callers release nothing timed.
func SensitiveIntervals(ctx context.Context, store repo.TenantRepos, asset domain.Asset, version string) ([]Interval, error) {
	own, err := store.Transcripts().Sensitive(ctx, id.Ref(asset.ID), version)
	if err != nil {
		return nil, err
	}
	out := make([]Interval, 0, len(own))
	for _, m := range own {
		out = append(out, Interval{m.StartSeconds, m.EndSeconds})
	}
	if asset.Lineage == nil {
		return out, nil
	}

	// The clip's timeline is its lineage segments concatenated in order. A version
	// other than the current one may have been cut from other segments.
	sameCut := version == asset.CurrentVersion
	offset := 0.0
	for _, seg := range asset.Lineage.Segments {
		length := seg.EndSeconds - seg.StartSeconds
		sourceMarks, err := store.Transcripts().Sensitive(ctx, id.Ref(seg.SourceAssetID), seg.SourceVersion)
		if err != nil {
			return nil, err
		}
		for _, m := range sourceMarks {
			start := max(m.StartSeconds, seg.StartSeconds)
			end := min(m.EndSeconds, seg.EndSeconds)
			if start >= end {
				continue
			}
			if !sameCut {
				return nil, errs.New(errs.CodeFailedPrecondition, "a source mark cannot be mapped onto an older version's timeline")
			}
			out = append(out, Interval{offset + start - seg.StartSeconds, offset + end - seg.StartSeconds})
		}
		offset += length
	}
	return out, nil
}

// Overlaps reports whether a span touches any interval.
func Overlaps(intervals []Interval, start, end float64) bool {
	for _, iv := range intervals {
		if start < iv.End && iv.Start < end {
			return true
		}
	}
	return false
}

// ReleasedSegments returns the transcript segments that touch no sensitive interval.
// A segment that overlaps is dropped whole: no partial segments (I231).
func ReleasedSegments(t domain.Transcript, marks []Interval) []domain.TranscriptSegment {
	var out []domain.TranscriptSegment
	for _, seg := range t.Segments {
		if strings.TrimSpace(seg.Text) == "" || Overlaps(marks, seg.Start, seg.End) {
			continue
		}
		seg.Words = nil
		out = append(out, seg)
	}
	return out
}

// Windows splits released segments into D15's speech windows: 30 to 60 s at segment
// boundaries with about 15% overlap. Used only when vectors must be computed at
// export time (I232). A sensitive interval between two segments ends a window, so
// no window's time span covers one.
func Windows(segments []domain.TranscriptSegment, marks []Interval) []domain.SearchChunk {
	const (
		minSeconds = 30.0
		maxSeconds = 60.0
		overlap    = 0.15
	)
	var out []domain.SearchChunk
	for i := 0; i < len(segments); {
		start := segments[i].Start
		j := i
		var text []string
		for j < len(segments) {
			if j > i && segments[j].End-start > maxSeconds {
				break
			}
			if j > i && segments[j-1].End-start >= minSeconds {
				break
			}
			if j > i && Overlaps(marks, segments[j-1].Start, segments[j].End) {
				break
			}
			text = append(text, strings.TrimSpace(segments[j].Text))
			j++
		}
		end := segments[j-1].End
		out = append(out, domain.SearchChunk{
			Level: domain.ChunkWindow, StartSeconds: start, EndSeconds: end, Text: strings.Join(text, " "),
		})
		if j >= len(segments) {
			break
		}
		// Start the next window about 15% before this one ended, always moving on,
		// and never back across a sensitive interval.
		next := j
		for next-1 > i && !Overlaps(marks, segments[next-1].Start, segments[j].End) {
			back := end - segments[next-1].Start
			if back > (end-start)/3 || (next != j && back > (end-start)*overlap) {
				break
			}
			next--
		}
		i = next
	}
	return out
}
