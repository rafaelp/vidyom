package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// scriptedLLM answers from a queue and records everything it receives.
type scriptedLLM struct {
	class    config.EndpointClass
	answers  []string
	fail     error
	requests []connector.ChatRequest
}

func (l *scriptedLLM) Info() connector.Info        { return connector.Info{Driver: "scripted"} }
func (l *scriptedLLM) Name() string                { return "scripted-endpoint" }
func (l *scriptedLLM) Class() config.EndpointClass { return l.class }
func (l *scriptedLLM) Complete(_ context.Context, req connector.ChatRequest) (connector.ChatResponse, error) {
	l.requests = append(l.requests, req)
	if l.fail != nil {
		return connector.ChatResponse{}, l.fail
	}
	content := `{}`
	if len(l.answers) > 0 {
		content, l.answers = l.answers[0], l.answers[1:]
	}
	return connector.ChatResponse{Content: content, Model: "scripted@1", InputTokens: 100, OutputTokens: 10}, nil
}

func (l *scriptedLLM) sent() string {
	var parts []string
	for _, r := range l.requests {
		for _, m := range r.Messages {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// storyTranscript has 10 s sentences: an unrelated start, the story at 100-160 s, an
// unrelated end, with a sensitive name inside the story.
func storyTranscript() domain.Transcript {
	var t domain.Transcript
	t.Language = "en"
	add := func(text string) {
		start := float64(len(t.Segments)) * 10
		t.Segments = append(t.Segments, domain.TranscriptSegment{Start: start, End: start + 10, Text: text})
	}
	for i := range 10 {
		add(fmt.Sprintf("Opening sentence %d about the practice.", i))
	}
	add("I want to tell you about a woman of ninety years.")
	add("She came to class every week for a long time.")
	add("Her neighbor Maria Silva told me about her illness.")
	add("A student asked about her regrets one day.")
	add("She said she had no remorse at all.")
	add("That is the end of the story of the woman.")
	for i := range 10 {
		add(fmt.Sprintf("Closing sentence %d about the posture.", i))
	}
	t.DurationSeconds = float64(len(t.Segments)) * 10
	return t
}

func storyRequest() FindRequest {
	return FindRequest{
		Asset:      domain.Asset{ID: "ORI-001", CurrentVersion: "v1", Title: "Class"},
		Version:    "v1",
		Transcript: storyTranscript(),
		Query:      "the woman who had no remorse",
		Hits: []domain.SearchHit{
			{AssetID: "ORI-001", Version: "v1", Level: domain.ChunkWindow, StartSeconds: 130, EndSeconds: 160, Score: 2.0 / 61, Source: "hybrid", Text: "She said she had no remorse"},
			{AssetID: "ORI-001", Version: "v1", Level: domain.ChunkWindow, StartSeconds: 0, EndSeconds: 40, Score: 1.0 / 70, Source: "vector", Text: "Opening"},
		},
	}
}

func TestFindWithLLMUsesItsBoundaries(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{
		`{"candidates":[{"start_seconds":101.2,"end_seconds":158.0,"confidence":0.9,"rationale":"The whole story"}]}`,
	}}
	req := storyRequest()
	req.LLM = llm
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Method != MethodLLM || result.Reason != "" {
		t.Fatalf("method = %q reason = %q", result.Method, result.Reason)
	}
	if result.Ambiguous {
		t.Fatal("one confident candidate is not ambiguous")
	}
	if len(result.Segments) != 1 || result.Segments[0].StartSeconds != 100 || result.Segments[0].EndSeconds != 160 {
		t.Fatalf("snapped to sentences: %+v", result.Segments)
	}
	if result.Segments[0].SourceAssetID != "ORI-001" || result.Segments[0].SourceVersion != "v1" {
		t.Fatalf("segment = %+v", result.Segments[0])
	}
	if result.Confidence != 0.9 || result.Candidates[0].Rationale != "The whole story" {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage.InputTokens != 100 || result.Usage.Provider != "scripted-endpoint" {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if !llm.requests[0].JSONMode {
		t.Fatal("must ask for JSON")
	}
}

// D51: the excerpt is what was said, never what the LLM wrote.
func TestExcerptComesFromTheTranscript(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{
		`{"candidates":[{"start_seconds":130,"end_seconds":150,"confidence":0.9,"excerpt":"INVENTED","rationale":"r"}]}`,
	}}
	req := storyRequest()
	req.LLM = llm
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Excerpt, "INVENTED") || !strings.Contains(result.Excerpt, "no remorse") {
		t.Fatalf("excerpt = %q", result.Excerpt)
	}
}

func TestFindWithoutLLMIsAlwaysAmbiguous(t *testing.T) {
	result, err := Find(context.Background(), storyRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ambiguous || result.Method != MethodRetrieval || result.Reason != ReasonNoEndpoint {
		t.Fatalf("result = %+v", result)
	}
	if result.Confidence >= confidentAt {
		t.Fatalf("retrieval alone cannot look confident: %f", result.Confidence)
	}
	top := result.Candidates[0]
	if top.Segments[0].StartSeconds != 120 || top.Segments[0].EndSeconds != 170 {
		t.Fatalf("a sentence of context around the hit: %+v", top.Segments[0])
	}
	if !strings.Contains(top.Excerpt, "no remorse") || top.Rationale == "" {
		t.Fatalf("candidate = %+v", top)
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("alternatives side by side: %+v", result.Candidates)
	}
}

func TestFindNothingRetrievedFails(t *testing.T) {
	req := storyRequest()
	req.Hits = nil
	llm := &scriptedLLM{class: config.EndpointLocal}
	req.LLM = llm
	_, err := Find(context.Background(), req)
	if !errors.Is(err, ErrNoMatch) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
	if len(llm.requests) != 0 {
		t.Fatal("nothing to ask about")
	}
}

func TestFindEmptyTranscriptFails(t *testing.T) {
	req := storyRequest()
	req.Transcript = domain.Transcript{}
	if _, err := Find(context.Background(), req); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindUnknownMode(t *testing.T) {
	req := storyRequest()
	req.Mode = "random"
	if _, err := Find(context.Background(), req); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestLLMFailureDegrades(t *testing.T) {
	req := storyRequest()
	req.LLM = &scriptedLLM{class: config.EndpointLocal, fail: errs.New(errs.CodeUnavailable, "down")}
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ambiguous || result.Method != MethodRetrieval || result.Reason != ReasonEndpointFailed {
		t.Fatalf("result = %+v", result)
	}
}

func TestLLMGarbageDegradesAfterOneMoreTry(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{"I cannot do that", "still no JSON"}}
	req := storyRequest()
	req.LLM = llm
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != ReasonEndpointFailed || len(llm.requests) != 2 {
		t.Fatalf("reason %q after %d requests", result.Reason, len(llm.requests))
	}
}

func TestLLMAnswersAreToleratedInFences(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{
		"Sure!\n```json\n{\"candidates\":[{\"start_seconds\":100,\"end_seconds\":160,\"confidence\":0.8,\"rationale\":\"a {brace} inside\"}]}\n```\nDone.",
	}}
	req := storyRequest()
	req.LLM = llm
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Method != MethodLLM || result.Candidates[0].Rationale != "a {brace} inside" {
		t.Fatalf("result = %+v", result)
	}
}

func TestLLMNoProposalFallsBack(t *testing.T) {
	req := storyRequest()
	req.LLM = &scriptedLLM{class: config.EndpointLocal, answers: []string{`{"candidates":[]}`}}
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != ReasonNoProposal || !result.Ambiguous || len(result.Candidates) == 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestCancellationDuringLLMIsReturned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := storyRequest()
	req.LLM = &scriptedLLM{class: config.EndpointLocal, fail: context.Canceled}
	if _, err := Find(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// LLM output is untrusted: intervals outside the recording or malformed never pass.
func TestLLMIntervalsAreValidated(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{`{"candidates":[
		{"start_seconds":9000,"end_seconds":9100,"confidence":0.99,"rationale":"past the end"},
		{"start_seconds":150,"end_seconds":100,"confidence":0.99,"rationale":"backwards"},
		{"end_seconds":100,"confidence":0.99,"rationale":"no start"},
		{"start_seconds":100,"end_seconds":100,"confidence":0.99,"rationale":"empty"},
		{"start_seconds":100,"end_seconds":160,"confidence":7,"rationale":"valid, confidence out of range"}
	]}`}}
	req := storyRequest()
	req.LLM = llm
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Confidence != 1 {
		t.Fatalf("result = %+v", result)
	}
}

// An interval no retrieved hit supports cannot look certain (I205).
func TestUnsupportedIntervalIsCapped(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{
		`{"candidates":[{"start_seconds":200,"end_seconds":230,"confidence":0.99,"rationale":"somewhere else"}]}`,
	}}
	req := storyRequest()
	req.LLM = llm
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Confidence != unsupportedConfidenceCap || !result.Ambiguous {
		t.Fatalf("result = %+v", result)
	}
}

func TestAmbiguityRule(t *testing.T) {
	two := func(first, second string) []string {
		return []string{`{"candidates":[` + first + `,` + second + `]}`}
	}
	cases := map[string]struct {
		answers []string
		want    bool
	}{
		"low confidence": {[]string{`{"candidates":[{"start_seconds":130,"end_seconds":160,"confidence":0.55,"rationale":"r"}]}`}, true},
		"at the bar":     {[]string{`{"candidates":[{"start_seconds":130,"end_seconds":160,"confidence":0.6,"rationale":"r"}]}`}, false},
		"close competitor": {two(`{"start_seconds":130,"end_seconds":160,"confidence":0.9,"rationale":"a"}`,
			`{"start_seconds":0,"end_seconds":40,"confidence":0.8,"rationale":"b"}`), true},
		"distant competitor": {two(`{"start_seconds":130,"end_seconds":160,"confidence":0.9,"rationale":"a"}`,
			`{"start_seconds":0,"end_seconds":40,"confidence":0.5,"rationale":"b"}`), false},
		"the same passage twice": {two(`{"start_seconds":130,"end_seconds":160,"confidence":0.9,"rationale":"a"}`,
			`{"start_seconds":131,"end_seconds":161,"confidence":0.88,"rationale":"b"}`), false},
	}
	for name, c := range cases {
		req := storyRequest()
		req.LLM = &scriptedLLM{class: config.EndpointLocal, answers: c.answers}
		result, err := Find(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.Ambiguous != c.want {
			t.Errorf("%s: ambiguous = %v, want %v, candidates = %+v", name, result.Ambiguous, c.want, result.Candidates)
		}
	}
}

func TestShortModeFitsTheTarget(t *testing.T) {
	req := storyRequest()
	req.Mode = FindShort
	req.TargetDuration = 30
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	top := result.Segments[0]
	if span := top.EndSeconds - top.StartSeconds; span > 30 || span < 20 {
		t.Fatalf("span = %f", span)
	}
	if top.StartSeconds > 145 || top.EndSeconds < 145 {
		t.Fatalf("not around the hit: %+v", top)
	}

	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{
		`{"candidates":[{"start_seconds":100,"end_seconds":160,"confidence":0.9,"rationale":"too long"}]}`,
	}}
	req.LLM = llm
	result, err = Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if span := result.Segments[0].EndSeconds - result.Segments[0].StartSeconds; span > 30 {
		t.Fatalf("the LLM's interval must be trimmed: %f", span)
	}
	if !strings.Contains(llm.sent(), "at most 30 seconds") {
		t.Fatal("the mode must reach the prompt")
	}
}

func TestBestMomentsAssembleInTimelineOrder(t *testing.T) {
	req := storyRequest()
	req.Mode = FindBestMoments
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Segments) != 2 {
		t.Fatalf("segments = %+v", result.Segments)
	}
	if result.Segments[0].StartSeconds >= result.Segments[1].StartSeconds {
		t.Fatalf("not in order: %+v", result.Segments)
	}
	if !strings.Contains(result.Excerpt, " … ") {
		t.Fatalf("excerpt = %q", result.Excerpt)
	}

	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{`{"candidates":[
		{"start_seconds":130,"end_seconds":160,"confidence":0.9,"rationale":"second"},
		{"start_seconds":0,"end_seconds":40,"confidence":0.8,"rationale":"first"},
		{"start_seconds":135,"end_seconds":155,"confidence":0.7,"rationale":"overlaps"}
	]}`}}
	req.LLM = llm
	result, err = Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || len(result.Segments) != 2 || result.Segments[0].StartSeconds != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.Ambiguous {
		t.Fatal("assembled moments have no competitor")
	}
}

func TestBestMomentsRespectTheTarget(t *testing.T) {
	req := storyRequest()
	req.Mode = FindBestMoments
	req.TargetDuration = 35
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Segments) != 1 {
		t.Fatalf("only the first fits: %+v", result.Segments)
	}
	if len(result.Candidates) < 2 {
		t.Fatalf("the other stays as an alternative: %+v", result.Candidates)
	}
}

// Stored stories give retrieval a better boundary, still marked ambiguous.
func TestRetrievalReachesAStoredStory(t *testing.T) {
	req := storyRequest()
	req.Analysis = &domain.Analysis{Available: true, Stories: []domain.Story{{Title: "Woman", StartSeconds: 100, EndSeconds: 160, Confidence: 0.9}}}
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	top := result.Segments[0]
	if top.StartSeconds > 100 || top.EndSeconds < 160 {
		t.Fatalf("story bounds ignored: %+v", top)
	}
	if !result.Ambiguous {
		t.Fatal("still no LLM confirmed it")
	}
}

func TestChapterHitsOnlyWhenNoWindowMatched(t *testing.T) {
	req := storyRequest()
	req.Hits = append([]domain.SearchHit{{AssetID: "ORI-001", Version: "v1", Level: domain.ChunkChapter, StartSeconds: 0, EndSeconds: 260, Score: 1, Source: "lexical"}}, req.Hits...)
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if span := result.Segments[0].EndSeconds - result.Segments[0].StartSeconds; span > 100 {
		t.Fatalf("a chapter must not swallow the clip: %+v", result.Segments)
	}
	req.Hits = req.Hits[:1]
	result, err = Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) == 0 {
		t.Fatal("a chapter alone is still evidence")
	}
}

func TestHitsOfAnotherVersionAreIgnored(t *testing.T) {
	req := storyRequest()
	for i := range req.Hits {
		req.Hits[i].Version = "v2"
	}
	llm := &scriptedLLM{class: config.EndpointLocal}
	req.LLM = llm
	if _, err := Find(context.Background(), req); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v", err)
	}
	if len(llm.requests) != 0 {
		t.Fatal("another version's timeline is no context")
	}
}

// D30: nothing marked sensitive reaches a non-local endpoint, in any field.
func TestSensitiveTextNeverReachesAnExternalEndpoint(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointExternal, answers: []string{
		`{"candidates":[{"start_seconds":100,"end_seconds":160,"confidence":0.9,"rationale":"r"}]}`,
	}}
	req := storyRequest()
	req.LLM = llm
	req.Sensitive = []domain.SensitiveSegment{{StartSeconds: 120, EndSeconds: 130}}
	// The hit's own text comes from the index and is not redacted.
	req.Hits[0].Text = "Her neighbor Maria Silva told me about her illness."
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	sent := llm.sent()
	if strings.Contains(sent, "Maria") || strings.Contains(sent, "illness") {
		t.Fatalf("sensitive text reached the endpoint:\n%s", sent)
	}
	if !strings.Contains(sent, "no remorse") {
		t.Fatal("the rest of the context must still be sent")
	}
	if strings.Contains(result.Excerpt, "Maria") || !strings.Contains(result.Excerpt, RedactedText) {
		t.Fatalf("excerpt = %q", result.Excerpt)
	}
}

// D30 scopes the exclusion to external AI; excerpts are redacted for every class (I210).
func TestLocalEndpointSeesSensitiveTextButExcerptIsRedacted(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{
		`{"candidates":[{"start_seconds":100,"end_seconds":160,"confidence":0.9,"rationale":"r"}]}`,
	}}
	req := storyRequest()
	req.LLM = llm
	req.Sensitive = []domain.SensitiveSegment{{StartSeconds: 120, EndSeconds: 130}}
	result, err := Find(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.sent(), "Maria") {
		t.Fatal("a local endpoint keeps the context")
	}
	if strings.Contains(result.Excerpt, "Maria") {
		t.Fatalf("excerpt = %q", result.Excerpt)
	}
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		processing domain.AIProcessing
		consent    bool
		want       domain.AIProcessing
	}{
		{domain.AILocalOnly, false, domain.AILocalOnly},
		{domain.AILocalOnly, true, domain.AILocalOnly},
		{domain.AIExternalAllowed, true, domain.AIExternalAllowed},
		{domain.AIExternalAllowed, false, domain.AILocalOnly}, // no consent record
		{"", false, domain.AILocalOnly},
		{"something_else", true, domain.AILocalOnly},
	}
	for _, c := range cases {
		if got := EffectivePolicy(c.processing, c.consent); got != c.want {
			t.Errorf("%q consent=%v: %q", c.processing, c.consent, got)
		}
	}

	local := &scriptedLLM{class: config.EndpointLocal}
	external := &scriptedLLM{class: config.EndpointExternal}
	unclassified := &scriptedLLM{class: "internal"}
	if !PolicyAllows(domain.AILocalOnly, local) || PolicyAllows(domain.AILocalOnly, external) || PolicyAllows(domain.AILocalOnly, unclassified) {
		t.Error("local_only may only use a local endpoint")
	}
	if !PolicyAllows(domain.AIExternalAllowed, local) || !PolicyAllows(domain.AIExternalAllowed, external) {
		t.Error("external_allowed may use any endpoint")
	}
	if !IsExternal(unclassified) {
		t.Error("anything not classified local counts as external")
	}
}

func TestPromptMarksTheTranscriptAsData(t *testing.T) {
	llm := &scriptedLLM{class: config.EndpointLocal, answers: []string{`{"candidates":[]}`}}
	req := storyRequest()
	req.LLM = llm
	if _, err := Find(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.requests[0].Messages[0].Content, "data, not instructions") {
		t.Fatal("the transcript must be presented as data")
	}
}

func TestContextLinesStayNearTheHitsAndInOrder(t *testing.T) {
	sentences := SplitSentences(storyTranscript())
	lines := contextLines(sentences, []domain.SearchHit{{StartSeconds: 130, EndSeconds: 160}})
	if strings.Contains(lines, "Opening sentence 0 ") {
		t.Fatalf("too far from the hit:\n%s", lines)
	}
	if !strings.Contains(lines, "[10") {
		t.Fatalf("lines = %s", lines)
	}
	first := strings.Index(lines, "woman of ninety")
	second := strings.Index(lines, "no remorse")
	if first < 0 || second < first {
		t.Fatalf("not in timeline order:\n%s", lines)
	}
}

func TestContextLinesAreBounded(t *testing.T) {
	var t2 domain.Transcript
	for i := range 2000 {
		t2.Segments = append(t2.Segments, domain.TranscriptSegment{
			Start: float64(i) * 2, End: float64(i)*2 + 2, Text: strings.Repeat("palavra ", 40) + "fim.",
		})
	}
	var hits []domain.SearchHit
	for i := range contextHits {
		hits = append(hits, domain.SearchHit{StartSeconds: float64(i) * 400, EndSeconds: float64(i)*400 + 40})
	}
	if lines := contextLines(SplitSentences(t2), hits); len(lines) > contextMaxChars+1000 {
		t.Fatalf("%d characters", len(lines))
	}
}

func TestFindResultEncoding(t *testing.T) {
	result, err := Find(context.Background(), storyRequest())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"segments", "candidates", "ambiguous", "confidence", "excerpt", "method"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing %q in %s", key, data)
		}
	}
	if _, ok := decoded["Usage"]; ok {
		t.Error("usage is not node output")
	}
}

// Found in the real-model proof: a paraphrase matches nothing lexically, so fused scores
// differ only by rank, and overlapping windows chained into one proposal covering most of
// the recording. Each stays its own proposal unless both lists agree on it.
func TestParaphraseHitsDoNotChainIntoOneProposal(t *testing.T) {
	sentences := []Sentence{
		{Start: 0, End: 50, Text: "First story."},
		{Start: 50, End: 95, Text: "Second story."},
		{Start: 95, End: 140, Text: "Third story."},
	}
	hit := func(start, end, score float64, source string) domain.SearchHit {
		return domain.SearchHit{AssetID: "ORI-001", Version: "v1", Level: domain.ChunkWindow, StartSeconds: start, EndSeconds: end, Score: score, Source: source}
	}
	paraphrase := storyProposals(FindRequest{}, sentences, []domain.SearchHit{
		hit(45, 93, 1.0/61, "vector"), hit(0, 50, 1.0/62, "vector"), hit(85, 133, 1.0/63, "vector"),
	})
	if len(paraphrase) != 3 {
		t.Fatalf("%d proposals from three uncorroborated hits: %+v", len(paraphrase), paraphrase)
	}
	corroboratedHits := storyProposals(FindRequest{}, sentences, []domain.SearchHit{
		hit(45, 93, 2.0/61, "hybrid"), hit(85, 133, 2.0/62, "hybrid"),
	})
	if len(corroboratedHits) != 1 {
		t.Fatalf("two touching hits found by both lists should be one story: %+v", corroboratedHits)
	}
}
