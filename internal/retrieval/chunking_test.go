package retrieval

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// transcriptOf builds a transcript of contiguous sentences of the given durations.
func transcriptOf(durations []float64, withWords bool) domain.Transcript {
	var t domain.Transcript
	position := 0.0
	for i, duration := range durations {
		text := fmt.Sprintf("Sentence number %d ends here.", i)
		segment := domain.TranscriptSegment{Start: position, End: position + duration, Text: text}
		if withWords {
			fields := strings.Fields(text)
			step := duration / float64(len(fields))
			for j, field := range fields {
				segment.Words = append(segment.Words, domain.Word{Start: position + float64(j)*step, End: position + float64(j+1)*step, Text: field})
			}
		}
		t.Segments = append(t.Segments, segment)
		position += duration
	}
	t.DurationSeconds = position
	return t
}

func TestChunkingProperties(t *testing.T) {
	overlapTotal, overlapCount := 0.0, 0
	for seed := uint64(1); seed <= 40; seed++ {
		random := rand.New(rand.NewPCG(seed, seed*7))
		durations := make([]float64, 40+random.IntN(400))
		for i := range durations {
			durations[i] = 1 + random.Float64()*14
		}
		transcript := transcriptOf(durations, seed%2 == 0)
		sentences := SplitSentences(transcript)
		if len(sentences) != len(durations) {
			t.Fatalf("seed %d: %d sentences from %d", seed, len(sentences), len(durations))
		}
		boundaryStarts, boundaryEnds := map[float64]bool{}, map[float64]bool{}
		for _, s := range sentences {
			boundaryStarts[s.Start], boundaryEnds[s.End] = true, true
		}
		chunks := ChunkTranscript(transcript, nil)
		for i, chunk := range chunks {
			span := chunk.EndSeconds - chunk.StartSeconds
			if chunk.Level != domain.ChunkWindow {
				t.Fatalf("level %q", chunk.Level)
			}
			if span > windowMaxSeconds+1e-9 {
				t.Errorf("seed %d chunk %d: %.2f s is over 60", seed, i, span)
			}
			if span < windowMinSeconds-1e-9 {
				t.Errorf("seed %d chunk %d of %d: %.2f s is under 30", seed, i, len(chunks), span)
			}
			if !boundaryStarts[chunk.StartSeconds] || !boundaryEnds[chunk.EndSeconds] {
				t.Errorf("seed %d chunk %d is not at sentence boundaries", seed, i)
			}
			if chunk.Text == "" {
				t.Errorf("seed %d chunk %d is empty", seed, i)
			}
			if i > 0 && chunk.StartSeconds <= chunks[i-1].StartSeconds {
				t.Errorf("seed %d chunk %d does not move forward", seed, i)
			}
		}
		// Every sentence is inside some window, so nothing said is unsearchable.
		for _, s := range sentences {
			covered := false
			for _, chunk := range chunks {
				if chunk.StartSeconds <= s.Start && s.End <= chunk.EndSeconds {
					covered = true
					break
				}
			}
			if !covered {
				t.Errorf("seed %d: sentence at %.2f is in no window", seed, s.Start)
			}
		}
		// Overlap is about 15% of the window, measured between neighbors. The tail is
		// excluded: it may reach further back to hold 30 s.
		for i := 1; i < len(chunks)-1; i++ {
			overlap := chunks[i-1].EndSeconds - chunks[i].StartSeconds
			if overlap < 0 {
				t.Errorf("seed %d: gap between chunks %d and %d", seed, i-1, i)
			}
			overlapTotal += overlap / (chunks[i-1].EndSeconds - chunks[i-1].StartSeconds)
			overlapCount++
		}
	}
	// Sentence boundaries win over the exact number, so this holds on average, not per
	// window: with sentences up to 15 s one window can land far from 15%.
	if mean := overlapTotal / float64(overlapCount); mean < 0.12 || mean > 0.20 {
		t.Errorf("mean overlap %.3f is not about 15%% (%d windows)", mean, overlapCount)
	}
}

// With short sentences there are boundaries near 15%, so each window is close.
func TestChunkingOverlapWithShortSentences(t *testing.T) {
	durations := make([]float64, 600)
	for i := range durations {
		durations[i] = 2 + float64(i%3)*0.5
	}
	chunks := ChunkTranscript(transcriptOf(durations, false), nil)
	for i := 1; i < len(chunks)-1; i++ {
		previous := chunks[i-1]
		ratio := (previous.EndSeconds - chunks[i].StartSeconds) / (previous.EndSeconds - previous.StartSeconds)
		if ratio < 0.10 || ratio > 0.20 {
			t.Errorf("chunk %d: overlap %.3f", i, ratio)
		}
	}
}

func TestChunkingShortRecordingGetsOneWindow(t *testing.T) {
	chunks := ChunkTranscript(transcriptOf([]float64{3, 4, 2}, false), nil)
	if len(chunks) != 1 || chunks[0].StartSeconds != 0 || chunks[0].EndSeconds != 9 {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestChunkingEmptyTranscript(t *testing.T) {
	if chunks := ChunkTranscript(domain.Transcript{}, nil); len(chunks) != 0 {
		t.Fatalf("chunks = %+v", chunks)
	}
	blank := domain.Transcript{Segments: []domain.TranscriptSegment{{Start: 0, End: 5, Text: "   "}}}
	if chunks := ChunkTranscript(blank, nil); len(chunks) != 0 {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestChunkingTailReachesBack(t *testing.T) {
	durations := make([]float64, 15)
	for i := range durations {
		durations[i] = 8
	}
	chunks := ChunkTranscript(transcriptOf(durations, false), nil)
	last := chunks[len(chunks)-1]
	if last.EndSeconds != 120 {
		t.Fatalf("does not reach the end: %+v", last)
	}
	if span := last.EndSeconds - last.StartSeconds; span < windowMinSeconds {
		t.Fatalf("the tail is %.1f s", span)
	}
}

// Unpunctuated ASR output: one "sentence" for minutes. Word boundaries split it.
func TestChunkingSplitsLongUnpunctuatedSpeech(t *testing.T) {
	var segment domain.TranscriptSegment
	var fields []string
	for i := range 600 {
		word := fmt.Sprintf("word%d", i)
		fields = append(fields, word)
		segment.Words = append(segment.Words, domain.Word{Start: float64(i), End: float64(i) + 0.9, Text: word})
	}
	segment.Start, segment.End, segment.Text = 0, 600, strings.Join(fields, " ")
	transcript := domain.Transcript{Segments: []domain.TranscriptSegment{segment}}

	if n := len(SplitSentences(transcript)); n != 1 {
		t.Fatalf("expected one sentence, got %d", n)
	}
	chunks := ChunkTranscript(transcript, nil)
	if len(chunks) < 10 {
		t.Fatalf("only %d chunks", len(chunks))
	}
	for i, chunk := range chunks {
		if span := chunk.EndSeconds - chunk.StartSeconds; span > windowMaxSeconds {
			t.Errorf("chunk %d: %.1f s", i, span)
		}
	}
	if !strings.Contains(chunks[len(chunks)-1].Text, "word599") {
		t.Fatal("the end of the speech is missing")
	}
}

// With no words and no punctuation, timing is proportional and the split still holds.
func TestChunkingSplitsLongSegmentWithoutWords(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("palavra ", 400))
	transcript := domain.Transcript{Segments: []domain.TranscriptSegment{{Start: 10, End: 410, Text: text}}}
	chunks := ChunkTranscript(transcript, nil)
	if len(chunks) < 5 {
		t.Fatalf("only %d chunks", len(chunks))
	}
	if chunks[0].StartSeconds != 10 || math.Abs(chunks[len(chunks)-1].EndSeconds-410) > 1e-6 {
		t.Fatalf("range %.1f-%.1f", chunks[0].StartSeconds, chunks[len(chunks)-1].EndSeconds)
	}
	for _, chunk := range chunks {
		if chunk.EndSeconds-chunk.StartSeconds > windowMaxSeconds {
			t.Fatalf("%.1f s", chunk.EndSeconds-chunk.StartSeconds)
		}
	}
}

// A long silence makes 30 s impossible, but spans must never pass 60 s.
func TestChunkingAroundSilence(t *testing.T) {
	transcript := domain.Transcript{Segments: []domain.TranscriptSegment{
		{Start: 0, End: 5, Text: "Before the silence."},
		{Start: 200, End: 205, Text: "After the silence."},
	}}
	chunks := ChunkTranscript(transcript, nil)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v", chunks)
	}
	for _, chunk := range chunks {
		if chunk.EndSeconds-chunk.StartSeconds > windowMaxSeconds {
			t.Fatalf("%.1f s", chunk.EndSeconds-chunk.StartSeconds)
		}
	}
}

// A long sentence before a silence must not be repeated by the next window.
func TestChunkingDoesNotOverlapAcrossSilence(t *testing.T) {
	transcript := domain.Transcript{Segments: []domain.TranscriptSegment{
		{Start: 0, End: 2, Text: "Short opening."},
		{Start: 2, End: 46, Text: "A long sentence that nearly fills the window by itself."},
		{Start: 250, End: 260, Text: "Much later, after a silence."},
	}}
	chunks := ChunkTranscript(transcript, nil)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if chunks[1].StartSeconds != 250 {
		t.Fatalf("second window = %+v", chunks[1])
	}
}

func TestChapterLevel(t *testing.T) {
	analysis := &domain.Analysis{
		Available: true,
		Chapters: []domain.Chapter{
			{Title: "Breathing", StartSeconds: 0, EndSeconds: 120, Summary: "How to breathe."},
			{Title: "No summary", StartSeconds: 120, EndSeconds: 200},
			{Title: "Backwards", StartSeconds: 300, EndSeconds: 200, Summary: "Invalid interval."},
		},
	}
	chunks := ChunkTranscript(transcriptOf([]float64{5, 5}, false), analysis)
	var chapters []domain.SearchChunk
	for _, chunk := range chunks {
		if chunk.Level == domain.ChunkChapter {
			chapters = append(chapters, chunk)
		}
	}
	if len(chapters) != 1 || chapters[0].Text != "Breathing. How to breathe." || chapters[0].EndSeconds != 120 {
		t.Fatalf("chapters = %+v", chapters)
	}
	analysis.Available = false
	for _, chunk := range ChunkTranscript(transcriptOf([]float64{5}, false), analysis) {
		if chunk.Level == domain.ChunkChapter {
			t.Fatal("an unavailable analysis has no chapters to use")
		}
	}
}

func TestSentencesAcrossSegmentsAndPauses(t *testing.T) {
	sentences := SplitSentences(domain.Transcript{Segments: []domain.TranscriptSegment{
		{Start: 0, End: 2, Text: "This sentence continues"},
		{Start: 2, End: 4, Text: "into the next segment. Short one! Really?"},
		{Start: 4, End: 5, Text: "No punctuation here"},
		{Start: 9, End: 10, Text: "and a pause came first"},
	}})
	want := []string{
		"This sentence continues into the next segment.",
		"Short one!",
		"Really?",
		"No punctuation here",
		"and a pause came first",
	}
	if len(sentences) != len(want) {
		t.Fatalf("%d sentences: %+v", len(sentences), sentences)
	}
	for i, text := range want {
		if sentences[i].Text != text {
			t.Errorf("sentence %d = %q, want %q", i, sentences[i].Text, text)
		}
	}
	if sentences[0].Start != 0 {
		t.Errorf("start = %f", sentences[0].Start)
	}
	if sentences[len(sentences)-1].End != 10 {
		t.Errorf("end = %f", sentences[len(sentences)-1].End)
	}
}

func TestSentencesUseWordTimings(t *testing.T) {
	sentences := SplitSentences(domain.Transcript{Segments: []domain.TranscriptSegment{{
		Start: 0, End: 10, Text: "One two. Three four.",
		Words: []domain.Word{
			{Start: 1, End: 1.5, Text: "One"},
			{Start: 1.5, End: 2, Text: "two."},
			{Start: 8, End: 8.5, Text: "Three"},
			{Start: 8.5, End: 9, Text: "four."},
		},
	}}})
	if len(sentences) != 2 || sentences[0].Start != 1 || sentences[0].End != 2 || sentences[1].Start != 8 || sentences[1].End != 9 {
		t.Fatalf("sentences = %+v", sentences)
	}
}

func TestSentencesSpeakerChangeEndsASentence(t *testing.T) {
	sentences := SplitSentences(domain.Transcript{Segments: []domain.TranscriptSegment{
		{Start: 0, End: 2, Text: "no punctuation", Speaker: "A"},
		{Start: 2, End: 4, Text: "other voice", Speaker: "B"},
	}})
	if len(sentences) != 2 {
		t.Fatalf("sentences = %+v", sentences)
	}
}

func TestSentencesInScriptsWithoutSpaces(t *testing.T) {
	sentences := SplitSentences(domain.Transcript{Segments: []domain.TranscriptSegment{
		{Start: 0, End: 6, Text: "呼吸を意識します。次に進みます。"},
	}})
	if len(sentences) != 2 || sentences[0].Text != "呼吸を意識します。" || sentences[1].Text != "次に進みます。" {
		t.Fatalf("sentences = %+v", sentences)
	}
	joined := windowChunk(sentences).Text
	if joined != "呼吸を意識します。次に進みます。" {
		t.Fatalf("joined = %q", joined)
	}
}

func TestSentencesClosingQuotesAndTerminators(t *testing.T) {
	for _, word := range []string{"end.", `end."`, "end?)", "end!", "end…", "fim؟", "終わり。", "end.”"} {
		if !endsSentence(word) {
			t.Errorf("%q must end a sentence", word)
		}
	}
	for _, word := range []string{"word", "3.5", "e.g", `"`, ""} {
		if endsSentence(word) {
			t.Errorf("%q must not end a sentence", word)
		}
	}
}
