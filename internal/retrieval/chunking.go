package retrieval

import (
	"math"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// ChunkingStrategy names the segmentation of D15, stored with every vector (R18). A
// change to the rules below needs a new name and a reindex.
const ChunkingStrategy = "sentence-window-30-60s-overlap15@1"

const (
	windowMinSeconds    = 30.0
	windowTargetSeconds = 45.0
	windowMaxSeconds    = 60.0
	overlapRatio        = 0.15
	longPieceSeconds    = 45.0
)

// ChunkTranscript produces the speech windows and, when analysis has them, the chapter
// summaries of D15. See I204 for the edge cases.
func ChunkTranscript(t domain.Transcript, analysis *domain.Analysis) []domain.SearchChunk {
	chunks := chunkWindows(splitLongSentences(SplitSentences(t)))
	if analysis != nil && analysis.Available {
		for _, chapter := range analysis.Chapters {
			summary := strings.TrimSpace(chapter.Summary)
			if summary == "" || chapter.EndSeconds <= chapter.StartSeconds {
				continue
			}
			text := summary
			if title := strings.TrimSpace(chapter.Title); title != "" {
				text = title + ". " + summary
			}
			chunks = append(chunks, domain.SearchChunk{
				Level:        domain.ChunkChapter,
				StartSeconds: chapter.StartSeconds,
				EndSeconds:   chapter.EndSeconds,
				Text:         text,
			})
		}
	}
	return chunks
}

func chunkWindows(sentences []Sentence) []domain.SearchChunk {
	var chunks []domain.SearchChunk
	first := 0
	for first < len(sentences) {
		last := first
		for last+1 < len(sentences) {
			start := sentences[first].Start
			if sentences[last].End-start >= windowTargetSeconds {
				break
			}
			if sentences[last+1].End-start > windowMaxSeconds {
				break
			}
			last++
		}
		final := last == len(sentences)-1
		// A tail too short for a window reaches back, when earlier sentences exist.
		if final {
			for first > 0 &&
				sentences[last].End-sentences[first].Start < windowMinSeconds &&
				sentences[last].End-sentences[first-1].Start <= windowMaxSeconds {
				first--
			}
		}
		chunks = append(chunks, windowChunk(sentences[first:last+1]))
		if final {
			break
		}
		first = nextWindowStart(sentences, first, last)
	}
	return chunks
}

// nextWindowStart picks the sentence whose start is closest to 15% of the closed
// window's span before its end, always moving forward.
func nextWindowStart(sentences []Sentence, first, last int) int {
	span := sentences[last].End - sentences[first].Start
	want := sentences[last].End - span*overlapRatio
	// Overlap keeps context across a cut. Across a silence longer than the overlap
	// there is none to keep, and reaching back could repeat a whole window.
	if sentences[last+1].Start-sentences[last].End >= span*overlapRatio {
		return last + 1
	}
	best, bestDistance := last+1, math.Abs(sentences[last+1].Start-want)
	for i := first + 1; i <= last; i++ {
		if distance := math.Abs(sentences[i].Start - want); distance < bestDistance {
			best, bestDistance = i, distance
		}
	}
	return best
}

func windowChunk(sentences []Sentence) domain.SearchChunk {
	var builder strings.Builder
	for i, s := range sentences {
		if i > 0 && needsSpace(sentences[i-1].Text, s.Text) {
			builder.WriteByte(' ')
		}
		builder.WriteString(s.Text)
	}
	return domain.SearchChunk{
		Level:        domain.ChunkWindow,
		StartSeconds: sentences[0].Start,
		EndSeconds:   sentences[len(sentences)-1].End,
		Text:         builder.String(),
	}
}

// splitLongSentences cuts sentences no window can hold, common in unpunctuated ASR
// output, at word boundaries. A single word longer than the limit stays whole, the one
// case where a window passes 60 s.
func splitLongSentences(sentences []Sentence) []Sentence {
	out := make([]Sentence, 0, len(sentences))
	for _, s := range sentences {
		if s.End-s.Start <= windowMaxSeconds || len(s.pieces) < 2 {
			out = append(out, s)
			continue
		}
		var current []domain.Word
		for _, piece := range s.pieces {
			if len(current) > 0 && piece.End-current[0].Start > longPieceSeconds {
				out = append(out, newSentence(current))
				current = nil
			}
			current = append(current, piece)
		}
		if len(current) > 0 {
			out = append(out, newSentence(current))
		}
	}
	return out
}
