package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

const goodAnalysis = `{
  "summary": "A class about breathing.",
  "chapters": [
    {"title":"Second","start_seconds":100,"end_seconds":160,"summary":"The story"},
    {"title":"First","start_seconds":0,"end_seconds":100,"summary":"Opening"},
    {"title":"Past the end","start_seconds":9000,"end_seconds":9100},
    {"title":"Backwards","start_seconds":50,"end_seconds":10},
    {"title":"No bounds"}
  ],
  "tags": ["yoga","breathing","yoga"," "],
  "stories": [{"title":"The woman","start_seconds":101,"end_seconds":159,"summary":"s","confidence":2.5}]
}`

func TestAnalyze(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{goodAnalysis}}
	out, usage, err := Analyze(context.Background(), llm, storyTranscript(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Available || out.Summary != "A class about breathing." || out.Model != "scripted@1" || out.Endpoint != "local" {
		t.Fatalf("analysis = %+v", out)
	}
	if len(out.Chapters) != 2 || out.Chapters[0].Title != "First" || out.Chapters[1].StartSeconds != 100 {
		t.Fatalf("chapters must be valid and sorted: %+v", out.Chapters)
	}
	if len(out.Tags) != 2 || out.Tags[0] != "breathing" {
		t.Fatalf("tags = %+v", out.Tags)
	}
	if len(out.Stories) != 1 || out.Stories[0].StartSeconds != 100 || out.Stories[0].EndSeconds != 160 || out.Stories[0].Confidence != 1 {
		t.Fatalf("stories snapped and clamped: %+v", out.Stories)
	}
	if usage.InputTokens != 100 || usage.Provider != "scripted-endpoint" {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestAnalyzeRemovesSensitiveForExternalEndpoints(t *testing.T) {
	marks := []domain.SensitiveSegment{{StartSeconds: 120, EndSeconds: 130}}
	external := &scriptedLLM{class: config.EndpointExternal, answers: []string{goodAnalysis}}
	if _, _, err := Analyze(context.Background(), external, storyTranscript(), marks); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(external.sent(), "Maria") {
		t.Fatal("sensitive text reached an external endpoint")
	}
	if !strings.Contains(external.sent(), "no remorse") {
		t.Fatal("the rest must still be sent")
	}

	local := &scriptedLLM{class: config.EndpointLocal, answers: []string{goodAnalysis}}
	if _, _, err := Analyze(context.Background(), local, storyTranscript(), marks); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(local.sent(), "Maria") {
		t.Fatal("D30 scopes the exclusion to external AI")
	}
}

func TestAnalyzeAllSensitiveSendsNothing(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointExternal}
	out, _, err := Analyze(context.Background(), llm, storyTranscript(), []domain.SensitiveSegment{{StartSeconds: 0, EndSeconds: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Available || out.UnavailableReason != ReasonNoSpeech || len(llm.requests) != 0 {
		t.Fatalf("analysis = %+v after %d requests", out, len(llm.requests))
	}
}

func TestAnalyzeFailureIsReturned(t *testing.T) {
	failing := &scriptedLLM{class: config.EndpointLocal, fail: errs.New(errs.CodeUnavailable, "down")}
	if _, _, err := Analyze(context.Background(), failing, storyTranscript(), nil); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
	garbage := &scriptedLLM{class: config.EndpointLocal, answers: []string{"no", "no"}}
	if _, _, err := Analyze(context.Background(), garbage, storyTranscript(), nil); err == nil || len(garbage.requests) != 2 {
		t.Fatalf("err = %v after %d requests", err, len(garbage.requests))
	}
}

func longTranscript(sentences int) domain.Transcript {
	var t domain.Transcript
	for i := range sentences {
		start := float64(i) * 5
		t.Segments = append(t.Segments, domain.TranscriptSegment{Start: start, End: start + 5, Text: fmt.Sprintf("Sentence number %d %s.", i, strings.Repeat("filler ", 30))})
	}
	return t
}

func TestAnalyzeInBlocksThenCondenses(t *testing.T) {
	transcript := longTranscript(400)
	blocks := analysisBlocks(SplitSentences(transcript))
	if len(blocks) < 2 {
		t.Fatalf("%d blocks", len(blocks))
	}
	var answers []string
	for i := range blocks {
		answers = append(answers, fmt.Sprintf(`{"summary":"Part %d.","chapters":[],"tags":["t%d"],"stories":[]}`, i, i))
	}
	answers = append(answers, `{"summary":"The whole recording."}`)
	llm := &scriptedLLM{class: config.EndpointLocal, answers: answers}
	out, usage, err := Analyze(context.Background(), llm, transcript, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) != len(blocks)+1 {
		t.Fatalf("%d requests for %d blocks", len(llm.requests), len(blocks))
	}
	if out.Summary != "The whole recording." || len(out.Tags) != len(blocks) {
		t.Fatalf("analysis = %+v", out)
	}
	if usage.InputTokens != 100*(len(blocks)+1) {
		t.Fatalf("usage = %+v", usage)
	}
	for _, r := range llm.requests[:len(blocks)] {
		if len(r.Messages[1].Content) > analysisBlockChars {
			t.Fatalf("a block of %d characters", len(r.Messages[1].Content))
		}
	}
}

func TestAnalyzeFailedCondenseJoinsSummaries(t *testing.T) {
	transcript := longTranscript(400)
	blocks := analysisBlocks(SplitSentences(transcript))
	var answers []string
	for i := range blocks {
		answers = append(answers, fmt.Sprintf(`{"summary":"Part %d."}`, i))
	}
	llm := &scriptedLLM{class: config.EndpointLocal, answers: append(answers, "bad", "bad")}
	out, _, err := Analyze(context.Background(), llm, transcript, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Available || !strings.Contains(out.Summary, "Part 0.") || !strings.Contains(out.Summary, "Part 1.") {
		t.Fatalf("summary = %q", out.Summary)
	}
}

func TestAnalyzeCapsLongStrings(t *testing.T) {
	long := strings.Repeat("x", 10000)
	data, _ := json.Marshal(map[string]any{
		"summary":  long,
		"chapters": []map[string]any{{"title": long, "start_seconds": 0, "end_seconds": 10, "summary": long}},
		"tags":     []string{long},
	})
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{string(data)}}
	out, _, err := Analyze(context.Background(), llm, storyTranscript(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(out.Summary)); n > maxSummaryRunes+1 {
		t.Errorf("summary: %d", n)
	}
	if n := len([]rune(out.Chapters[0].Title)); n > maxTitleRunes+1 {
		t.Errorf("title: %d", n)
	}
	if n := len([]rune(out.Tags[0])); n > maxTagRunes+1 {
		t.Errorf("tag: %d", n)
	}
}

func TestDecodeLLMJSON(t *testing.T) {
	var v map[string]any
	for _, bad := range []string{"", "no json", "{", `{"a":`, "[1,2]"} {
		if err := decodeLLMJSON(bad, &v); !errors.Is(err, ErrInvalidOutput) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	for _, good := range []string{`{"a":1}`, "```json\n{\"a\":1}\n```", `text {"a":"}"} trailing`} {
		if err := decodeLLMJSON(good, &v); err != nil {
			t.Errorf("%q: err = %v", good, err)
		}
	}
	// A decoder error can quote the answer, which can quote the transcript.
	err := decodeLLMJSON(`{"a":"private text"x}`, &v)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Errorf("err = %v", err)
	}
}

func TestDraftEditorial(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{
		`{"title":"A title\nwith a newline","description":"Desc","tags":["a","a","b"]}`,
	}}
	out, usage, err := DraftEditorial(context.Background(), llm, "Working", "A prompt", storyTranscript(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Generated || out.Title != "A title with a newline" || out.Description != "Desc" || len(out.Tags) != 2 || out.Endpoint != "local" {
		t.Fatalf("draft = %+v", out)
	}
	if usage.OutputTokens != 10 {
		t.Fatalf("usage = %+v", usage)
	}
	if !strings.Contains(llm.sent(), "A prompt") || !strings.Contains(llm.sent(), "no remorse") {
		t.Fatal("prompt and transcript must be sent")
	}
}

func TestDraftEditorialExcludesSensitiveForExternal(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointExternal, answers: []string{`{"title":"T"}`}}
	_, _, err := DraftEditorial(context.Background(), llm, "W", "P", storyTranscript(), []domain.SensitiveSegment{{StartSeconds: 120, EndSeconds: 130}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(llm.sent(), "Maria") {
		t.Fatal("sensitive text reached an external endpoint")
	}
}

func TestDraftEditorialNeedsATitle(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{`{"description":"only"}`}}
	if _, _, err := DraftEditorial(context.Background(), llm, "W", "P", storyTranscript(), nil); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("err = %v", err)
	}
}

func TestDraftEditorialBoundsItsInput(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{`{"title":"T"}`}}
	if _, _, err := DraftEditorial(context.Background(), llm, "W", "P", longTranscript(1000), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(llm.requests[0].Messages[1].Content); n > editorialInputChars+200 {
		t.Fatalf("%d characters", n)
	}
}

func TestFallbackEditorial(t *testing.T) {
	out := FallbackEditorial("Clip title", "Hatha Yoga Class", "Story of the woman", []string{"yoga"})
	if out.Generated || out.Title != "Story of the woman" || out.Description != "From: Hatha Yoga Class" || out.Tags[0] != "yoga" {
		t.Fatalf("draft = %+v", out)
	}
	out = FallbackEditorial("Clip title", "Source", "", nil)
	if out.Title != "Clip title" || out.Tags == nil {
		t.Fatalf("draft = %+v", out)
	}
	out = FallbackEditorial("", "Source", "", nil)
	if out.Title != "Source" {
		t.Fatalf("draft = %+v", out)
	}
}
