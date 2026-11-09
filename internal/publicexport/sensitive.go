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

// maxLineageDepth bounds a walk along a lineage; real chains are short (D36).
const maxLineageDepth = 16

// SensitiveIntervals returns what must not reach anything public for an asset
// version: its own marks plus every lineage source's, mapped onto its timeline through
// the whole chain (D30, I231, I284). A mark added to a grandparent after a clip of a
// clip was cut therefore still counts. A source that cannot be read is an error, which
// the caller turns into withholding the text.
func SensitiveIntervals(ctx context.Context, store repo.TenantRepos, asset domain.Asset, version string) ([]Interval, error) {
	return sensitiveIntervals(ctx, store, asset, version, 0)
}

func sensitiveIntervals(ctx context.Context, store repo.TenantRepos, asset domain.Asset, version string, depth int) ([]Interval, error) {
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
	if depth >= maxLineageDepth {
		return nil, errs.New(errs.CodeFailedPrecondition, "a lineage is too deep to read its sensitive marks")
	}

	// The clip's timeline is its lineage segments concatenated in order. A version
	// other than the current one may have been cut from other segments.
	sameCut := version == asset.CurrentVersion
	offset := 0.0
	for _, seg := range asset.Lineage.Segments {
		length := seg.EndSeconds - seg.StartSeconds
		if length <= 0 {
			continue
		}
		start := offset
		offset += length
		sourceID := seg.SourceAssetID
		if sourceID == "" {
			sourceID = asset.Lineage.SourceAssetID
		}
		if sourceID == "" {
			continue
		}
		source, err := store.Assets().Get(ctx, id.Ref(sourceID))
		if err != nil {
			return nil, err
		}
		sourceVersion := seg.SourceVersion
		if sourceVersion == "" {
			sourceVersion = source.CurrentVersion
		}
		sourceMarks, err := sensitiveIntervals(ctx, store, source, sourceVersion, depth+1)
		if err != nil {
			return nil, err
		}
		for _, m := range sourceMarks {
			from := max(m.Start, seg.StartSeconds)
			to := min(m.End, seg.EndSeconds)
			if from >= to {
				continue
			}
			if !sameCut {
				return nil, errs.New(errs.CodeFailedPrecondition, "a source mark cannot be mapped onto an older version's timeline")
			}
			out = append(out, Interval{start + from - seg.StartSeconds, start + to - seg.StartSeconds})
		}
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
