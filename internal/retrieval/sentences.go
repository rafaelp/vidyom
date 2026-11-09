package retrieval

import (
	"strings"
	"unicode"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// Sentence is a timed sentence of a transcript. Pieces are its timed words, real or
// estimated, kept so a sentence too long for one window can be split.
type Sentence struct {
	Start  float64
	End    float64
	Text   string
	pieces []domain.Word
}

// A pause this long between words ends a sentence even with no punctuation: ASR output
// does not always punctuate.
const sentencePauseSeconds = 2.0

// SplitSentences cuts a transcript into timed sentences. Word timestamps are used when
// a segment has them; otherwise words get a time proportional to their length inside
// the segment. A sentence may span segments, and a change of speaker ends it.
func SplitSentences(t domain.Transcript) []Sentence {
	var (
		out     []Sentence
		current []domain.Word
		speaker string
	)
	flush := func() {
		if len(current) == 0 {
			return
		}
		out = append(out, newSentence(current))
		current = nil
	}
	for _, segment := range t.Segments {
		pieces := segmentPieces(segment)
		if len(pieces) == 0 {
			continue
		}
		if segment.Speaker != speaker {
			flush()
			speaker = segment.Speaker
		}
		for _, piece := range pieces {
			if len(current) > 0 && piece.Start-current[len(current)-1].End >= sentencePauseSeconds {
				flush()
			}
			current = append(current, piece)
			if endsSentence(piece.Text) {
				flush()
			}
		}
	}
	flush()
	return out
}

func newSentence(pieces []domain.Word) Sentence {
	return Sentence{
		Start:  pieces[0].Start,
		End:    pieces[len(pieces)-1].End,
		Text:   joinWords(pieces),
		pieces: pieces,
	}
}

// segmentPieces returns the segment's timed words, estimating them when absent.
func segmentPieces(segment domain.TranscriptSegment) []domain.Word {
	if len(segment.Words) > 0 {
		out := make([]domain.Word, 0, len(segment.Words))
		for _, w := range segment.Words {
			if text := strings.TrimSpace(w.Text); text != "" {
				out = append(out, domain.Word{Start: w.Start, End: w.End, Text: text})
			}
		}
		return out
	}
	tokens := tokenizeForTiming(segment.Text)
	if len(tokens) == 0 {
		return nil
	}
	total := 0
	for _, token := range tokens {
		total += len([]rune(token))
	}
	span := segment.End - segment.Start
	position := segment.Start
	out := make([]domain.Word, len(tokens))
	for i, token := range tokens {
		step := span * float64(len([]rune(token))) / float64(total)
		end := position + step
		if i == len(tokens)-1 {
			end = segment.End
		}
		out[i] = domain.Word{Start: position, End: end, Text: token}
		position = end
	}
	return out
}

// tokenizeForTiming splits on whitespace and also after CJK sentence marks, because
// those languages use no spaces.
func tokenizeForTiming(text string) []string {
	var out []string
	for _, field := range strings.Fields(text) {
		start := 0
		runes := []rune(field)
		for i, r := range runes {
			if isUnspacedTerminator(r) && i+1 < len(runes) {
				out = append(out, string(runes[start:i+1]))
				start = i + 1
			}
		}
		out = append(out, string(runes[start:]))
	}
	return out
}

func isUnspacedTerminator(r rune) bool {
	switch r {
	case '。', '！', '？', '｡':
		return true
	}
	return false
}

// endsSentence reports whether a word ends in sentence punctuation, looking past
// closing quotes and brackets.
func endsSentence(word string) bool {
	runes := []rune(word)
	for i := len(runes) - 1; i >= 0; i-- {
		r := runes[i]
		switch r {
		case '.', '!', '?', '…', '؟', '。', '！', '？', '｡', '۔', '।':
			return true
		case '"', '\'', ')', ']', '}', '»', '”', '’', '」', '』', '）':
			continue
		}
		return false
	}
	return false
}

// joinWords joins words with spaces, except around scripts written without them.
func joinWords(words []domain.Word) string {
	var builder strings.Builder
	for i, word := range words {
		if i > 0 && needsSpace(words[i-1].Text, word.Text) {
			builder.WriteByte(' ')
		}
		builder.WriteString(word.Text)
	}
	return builder.String()
}

func needsSpace(previous, next string) bool {
	last, first := lastRune(previous), firstRune(next)
	return !isUnspacedScript(last) && !isUnspacedScript(first)
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	return runes[len(runes)-1]
}

func isUnspacedScript(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) || isUnspacedTerminator(r)
}

// sentenceIndexAt returns the sentence containing t, or the nearest one.
func sentenceIndexAt(sentences []Sentence, t float64) int {
	best, bestDistance := 0, -1.0
	for i, s := range sentences {
		var distance float64
		switch {
		case t < s.Start:
			distance = s.Start - t
		case t > s.End:
			distance = t - s.End
		default:
			return i
		}
		if bestDistance < 0 || distance < bestDistance {
			best, bestDistance = i, distance
		}
	}
	return best
}

// snapToSentences moves an interval to the sentence boundaries it touches (§4.4). It
// returns false when no sentence overlaps the interval and none is near it.
func snapToSentences(sentences []Sentence, start, end float64) (float64, float64, bool) {
	if len(sentences) == 0 || end <= start {
		return 0, 0, false
	}
	first, last := -1, -1
	for i, s := range sentences {
		if s.End > start && s.Start < end {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return 0, 0, false
	}
	return sentences[first].Start, sentences[last].End, true
}

// textBetween joins the sentences overlapping an interval.
func textBetween(sentences []Sentence, start, end float64) string {
	var parts []string
	for _, s := range sentences {
		if s.End > start && s.Start < end {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, " ")
}
