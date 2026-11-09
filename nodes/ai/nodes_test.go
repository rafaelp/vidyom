package ainodes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/connectors/ai/openai/mockllm"
	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/retrieval"
	"github.com/rafaelp/vidyom/schemas"
)

func silence() domain.Transcript {
	return domain.Transcript{Segments: []domain.TranscriptSegment{}}
}

func TestEveryCatalogAINodeIsRegisteredAsDeclared(t *testing.T) {
	registry := node.NewRegistry()
	Register(registry)
	catalog, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, entry := range catalog.Nodes {
		if entry.Owner != "ai" {
			continue
		}
		listed++
		n, ok := registry.Lookup(entry.Type, entry.TypeVersion)
		if !ok {
			t.Errorf("%s is not registered", entry.Type)
			continue
		}
		if err := schemas.CheckDescriptor(n.Descriptor()); err != nil {
			t.Error(err)
		}
	}
	if listed != 8 || len(registry.List()) != listed {
		t.Fatalf("catalog lists %d, registered %d", listed, len(registry.List()))
	}
}

// A node of the wrong pool fails clearly instead of reaching for what it must not have.
func TestNodesRequireTheirPool(t *testing.T) {
	plain := newBaseEnv(t)
	for _, nodeType := range []string{"ai.transcribe", "ai.embed", "ai.analyze", "ai.index", "ai.find_segment", "ai.derive_transcript", "ai.editorial"} {
		_, err := findNode(t, nodeType).Execute(context.Background(), plain, node.Input{Parameters: json.RawMessage(`{}`)})
		if !errs.IsCode(err, errs.CodeInternal) {
			t.Errorf("%s: err = %v", nodeType, err)
		}
	}
}

// ---- ai.transcribe ----

type stubTranscriber struct {
	external bool
	engine   string
	err      error
	got      connector.TranscribeRequest
}

func (s *stubTranscriber) Info() connector.Info {
	return connector.Info{Capabilities: map[string]bool{"external": s.external}}
}
func (s *stubTranscriber) Transcribe(_ context.Context, req connector.TranscribeRequest) (domain.Transcript, error) {
	s.got = req
	if s.err != nil {
		return domain.Transcript{}, s.err
	}
	if req.Progress != nil {
		req.Progress(50, "half")
	}
	engine := s.engine
	if engine == "" {
		engine = "faster-whisper/large-v3"
	}
	return domain.Transcript{Language: "en", DurationSeconds: 12, TimestampBasis: "working_master", Engine: engine,
		Segments: []domain.TranscriptSegment{{Start: 0, End: 12, Text: "Hello."}}}, nil
}

func transcribeEnv(t *testing.T, s *stubTranscriber) *aiEnv {
	t.Helper()
	env := &aiEnv{baseEnv: newBaseEnv(t), transcriber: s}
	env.inputs["scratch://audio"] = node.LocalInput{Path: "/tmp/audio.opus"}
	env.inputs["storage://proxy/a"] = node.LocalInput{Path: "/tmp/proxy.mp4", Role: domain.RoleProxy}
	env.inputs["storage://archive/a"] = node.LocalInput{Path: "/tmp/raw.mov", Role: domain.RoleArchive}
	return env
}

func TestTranscribe(t *testing.T) {
	stub := &stubTranscriber{}
	env := transcribeEnv(t, stub)
	result, err := run(t, findNode(t, "ai.transcribe"), env, 1, map[string]any{
		"source": "scratch://audio", "assetId": "ORI-001", "language": "en", "wordTimestamps": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[transcribeOutput](t, result)
	if out.Engine != "faster-whisper/large-v3" || out.DurationSeconds != 12 || out.Language != "en" {
		t.Fatalf("out = %+v", out)
	}
	if out.Transcript.AssetID != "ORI-001" || out.Transcript.TimestampBasis != "working_master" {
		t.Fatalf("transcript = %+v", out.Transcript)
	}
	if stub.got.AudioPath != "/tmp/audio.opus" || stub.got.Language != "en" || stub.got.WordTimestamps {
		t.Fatalf("request = %+v", stub.got)
	}
	if len(env.progress) == 0 {
		t.Fatal("progress must reach the environment")
	}
	if result.Tools["asr"] != "faster-whisper/large-v3" {
		t.Fatalf("tools = %v", result.Tools)
	}
	if len(result.Usage) != 1 || result.Usage[0].Operation != "ai.transcription" || result.Usage[0].Quantity != 12 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestTranscribeDefaultsToWordTimestamps(t *testing.T) {
	stub := &stubTranscriber{}
	if _, err := run(t, findNode(t, "ai.transcribe"), transcribeEnv(t, stub), 1, map[string]any{"source": "scratch://audio"}); err != nil {
		t.Fatal(err)
	}
	if !stub.got.WordTimestamps {
		t.Fatal("§4.4 needs sentence boundaries, so word timestamps are the default")
	}
}

// D56: never from the proxy; clipping's rule about archive holds here too.
func TestTranscribeRefusesProxyAndArchive(t *testing.T) {
	for _, source := range []string{"storage://proxy/a", "storage://archive/a"} {
		stub := &stubTranscriber{}
		_, err := run(t, findNode(t, "ai.transcribe"), transcribeEnv(t, stub), 1, map[string]any{"source": source})
		if !errs.IsCode(err, errs.CodeForbiddenStorageRole) || node.IsTransient(err) {
			t.Fatalf("%s: err = %v", source, err)
		}
		if stub.got.AudioPath != "" {
			t.Fatalf("%s reached the transcriber", source)
		}
	}
}

// D8 covers transcription too.
func TestTranscribeRefusesAnExternalEngineOnLocalOnly(t *testing.T) {
	cases := map[string]string{"absent": "", "local_only": "local_only"}
	for name, policy := range cases {
		stub := &stubTranscriber{external: true}
		params := map[string]any{"source": "scratch://audio"}
		if policy != "" {
			params["aiProcessing"] = policy
		}
		_, err := run(t, findNode(t, "ai.transcribe"), transcribeEnv(t, stub), 1, params)
		if !errs.IsCode(err, errs.CodeFailedPrecondition) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if stub.got.AudioPath != "" {
			t.Fatalf("%s: the audio reached an external engine", name)
		}
	}
	stub := &stubTranscriber{external: true}
	if _, err := run(t, findNode(t, "ai.transcribe"), transcribeEnv(t, stub), 1, map[string]any{"source": "scratch://audio", "aiProcessing": "external_allowed"}); err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeClassifiesFailures(t *testing.T) {
	transient := &stubTranscriber{err: errs.New(errs.CodeUnavailable, "worker down")}
	if _, err := run(t, findNode(t, "ai.transcribe"), transcribeEnv(t, transient), 1, map[string]any{"source": "scratch://audio"}); !node.IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
	permanent := &stubTranscriber{err: errs.New(errs.CodeInvalidArgument, "bad audio")}
	if _, err := run(t, findNode(t, "ai.transcribe"), transcribeEnv(t, permanent), 1, map[string]any{"source": "scratch://audio"}); err == nil || node.IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
	cancelled := &stubTranscriber{err: context.Canceled}
	_, err := run(t, findNode(t, "ai.transcribe"), transcribeEnv(t, cancelled), 1, map[string]any{"source": "scratch://audio"})
	if !errors.Is(err, context.Canceled) || node.IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
}

// ---- ai.embed ----

type stubEmbedder struct {
	external bool
	model    string
	calls    int
	wrong    bool
}

func (s *stubEmbedder) Info() connector.Info {
	return connector.Info{Capabilities: map[string]bool{"external": s.external}}
}
func (s *stubEmbedder) Embed(_ context.Context, req connector.EmbedRequest) (connector.EmbedResult, error) {
	s.calls++
	model := s.model
	if model == "" {
		model = aiworker.FakeEmbeddingModel
	}
	out := connector.EmbedResult{Model: model, Dimensions: aiworker.FakeEmbeddingDimensions}
	for _, text := range req.Texts {
		out.Vectors = append(out.Vectors, aiworker.FakeHashEmbed(text))
	}
	if s.wrong {
		out.Vectors = out.Vectors[1:]
	}
	return out, nil
}

func TestEmbed(t *testing.T) {
	embedder := &stubEmbedder{}
	env := &aiEnv{baseEnv: newBaseEnv(t), embedder: embedder}
	analysis := domain.Analysis{Available: true, Chapters: []domain.Chapter{{Title: "Story", StartSeconds: 100, EndSeconds: 160, Summary: "A woman."}}}
	result, err := run(t, findNode(t, "ai.embed"), env, 1, map[string]any{"transcript": storyTranscript(), "analysis": analysis, "assetId": "ORI-001"})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[embedOutput](t, result)
	if out.Model != aiworker.FakeEmbeddingModel || out.Dimensions != aiworker.FakeEmbeddingDimensions || out.Chunking != retrieval.ChunkingStrategy {
		t.Fatalf("out: %q %d %q", out.Model, out.Dimensions, out.Chunking)
	}
	windows, chapters := 0, 0
	for _, chunk := range out.Chunks {
		if len(chunk.Vector) != out.Dimensions || chunk.Text == "" {
			t.Fatalf("chunk = %+v", chunk.SearchChunk)
		}
		if chunk.Level == domain.ChunkChapter {
			chapters++
		} else {
			windows++
		}
	}
	if windows < 3 || chapters != 1 {
		t.Fatalf("windows %d, chapters %d", windows, chapters)
	}
	if result.Tools["embedding"] != aiworker.FakeEmbeddingModel || result.Tools["chunking"] != retrieval.ChunkingStrategy {
		t.Fatalf("tools = %v", result.Tools)
	}
}

func TestEmbedNoSpeechCallsNothing(t *testing.T) {
	embedder := &stubEmbedder{}
	env := &aiEnv{baseEnv: newBaseEnv(t), embedder: embedder}
	result, err := run(t, findNode(t, "ai.embed"), env, 1, map[string]any{"transcript": silence()})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[embedOutput](t, result)
	if embedder.calls != 0 || len(out.Chunks) != 0 || out.Chunks == nil {
		t.Fatalf("calls %d, out %+v", embedder.calls, out)
	}
}

func TestEmbedRefusesAnExternalEmbedder(t *testing.T) {
	embedder := &stubEmbedder{external: true}
	env := &aiEnv{baseEnv: newBaseEnv(t), embedder: embedder}
	_, err := run(t, findNode(t, "ai.embed"), env, 1, map[string]any{"transcript": storyTranscript()})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) || embedder.calls != 0 {
		t.Fatalf("err = %v, calls = %d", err, embedder.calls)
	}
}

func TestEmbedRefusesMissingVectors(t *testing.T) {
	env := &aiEnv{baseEnv: newBaseEnv(t), embedder: &stubEmbedder{wrong: true}}
	_, err := run(t, findNode(t, "ai.embed"), env, 1, map[string]any{"transcript": storyTranscript()})
	if !errs.IsCode(err, errs.CodeInternal) {
		t.Fatalf("err = %v", err)
	}
}

// ---- ai.export_transcript ----

func TestExportWritesAllFormatsToScratch(t *testing.T) {
	env := newBaseEnv(t)
	result, err := run(t, findNode(t, "ai.export_transcript"), env, 1, map[string]any{"transcript": storyTranscript()})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[struct {
		Files map[string]exportedFile `json:"files"`
	}](t, result)
	if len(out.Files) != 4 {
		t.Fatalf("files = %+v", out.Files)
	}
	for format, file := range out.Files {
		if !strings.HasPrefix(file.File, "scratch://") {
			t.Errorf("%s: %s", format, file.File)
		}
		data, err := os.ReadFile(filepath.Join(env.scratch, "transcript."+format))
		if err != nil {
			t.Fatal(err)
		}
		if file.SizeBytes != int64(len(data)) || file.SHA256 != sha(data) {
			t.Errorf("%s: wrong size or checksum", format)
		}
	}
}

func TestExportOnlyWhatWasAsked(t *testing.T) {
	result, err := run(t, findNode(t, "ai.export_transcript"), newBaseEnv(t), 1, map[string]any{"transcript": storyTranscript(), "formats": []string{"srt"}})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[struct {
		Files map[string]exportedFile `json:"files"`
	}](t, result)
	if _, ok := out.Files["srt"]; !ok || len(out.Files) != 1 {
		t.Fatalf("files = %+v", out.Files)
	}
}

// D30: redacted in exports, excluded from anything public.
func TestExportRedactsOrExcludesSensitiveSpeech(t *testing.T) {
	marks := []map[string]any{{"start_seconds": 120, "end_seconds": 130, "reason": "health"}}
	env := newBaseEnv(t)
	if _, err := run(t, findNode(t, "ai.export_transcript"), env, 1, map[string]any{"transcript": storyTranscript(), "sensitive": marks}); err != nil {
		t.Fatal(err)
	}
	for _, format := range retrieval.ExportFormats {
		text := readFile(t, filepath.Join(env.scratch, "transcript."+format))
		if strings.Contains(text, "Maria") {
			t.Errorf("%s leaks marked speech", format)
		}
		if !strings.Contains(text, retrieval.RedactedText) {
			t.Errorf("%s must show the redaction", format)
		}
	}

	env = newBaseEnv(t)
	if _, err := run(t, findNode(t, "ai.export_transcript"), env, 1, map[string]any{"transcript": storyTranscript(), "sensitive": marks, "excludeSensitive": true}); err != nil {
		t.Fatal(err)
	}
	for _, format := range retrieval.ExportFormats {
		text := readFile(t, filepath.Join(env.scratch, "transcript."+format))
		if strings.Contains(text, "Maria") || strings.Contains(text, retrieval.RedactedText) {
			t.Errorf("%s must drop the span entirely", format)
		}
		if !strings.Contains(text, "no remorse") {
			t.Errorf("%s lost the rest of the speech", format)
		}
	}
}

// ---- ai.analyze ----

func analyzeResult(t *testing.T, env *daemonEnv) (domain.Analysis, node.Result) {
	t.Helper()
	result, err := run(t, findNode(t, "ai.analyze"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript()})
	if err != nil {
		t.Fatal(err)
	}
	return decode[domain.Analysis](t, result), result
}

func TestAnalyzeWithALocalEndpoint(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	env.config.Budget.PerUnitMinor = map[string]float64{"ai.local": 2}
	analysis, result := analyzeResult(t, env)
	if !analysis.Available || analysis.AssetID != "ORI-001" || analysis.Version != "v1" || analysis.Endpoint != "local" {
		t.Fatalf("analysis = %+v", analysis)
	}
	if len(analysis.Chapters) == 0 || !strings.HasPrefix(analysis.Summary, "[mock]") {
		t.Fatalf("analysis = %+v", analysis)
	}
	if len(server.Requests()) != 1 || server.Requests()[0].Task != "analyze" {
		t.Fatalf("requests = %+v", server.Requests())
	}
	if !strings.Contains(server.Requests()[0].Text(), "Maria") {
		t.Fatal("no marks here, and a local endpoint keeps the text anyway")
	}
	if len(result.Usage) != 1 || result.Usage[0].Operation != "ai.local" || result.Usage[0].Provider != "mock-endpoint" || result.Usage[0].Unit != "tokens" {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if result.Usage[0].EstimatedMinor == 0 {
		t.Fatal("priced from the tenant's table")
	}
	if result.Tools["llm"] != "mock-llm@1" {
		t.Fatalf("tools = %v", result.Tools)
	}
}

// D8: nothing is sent on local_only, even when env.LLM hands over an external endpoint.
func TestAnalyzeLocalOnlyRefusesAnExternalEndpoint(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	analysis, result := analyzeResult(t, env)
	if analysis.Available || analysis.UnavailableReason != retrieval.ReasonNoEndpoint {
		t.Fatalf("analysis = %+v", analysis)
	}
	if len(server.Requests()) != 0 {
		t.Fatal("content reached an external endpoint on local_only")
	}
	if len(result.Usage) != 0 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

// I208: external_allowed with no consent record counts as not given.
func TestAnalyzeExternalAllowedWithoutConsentIsLocalOnly(t *testing.T) {
	env := newDaemonEnv(t)
	asset := original()
	asset.AIProcessing = domain.AIExternalAllowed
	env.store.PutAsset(asset)
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	analysis, _ := analyzeResult(t, env)
	if analysis.Available || len(server.Requests()) != 0 {
		t.Fatalf("analysis = %+v, requests = %d", analysis, len(server.Requests()))
	}
	if env.asked[0] != domain.AILocalOnly {
		t.Fatalf("env.LLM was asked for %q", env.asked[0])
	}
}

// I208: parameters cannot widen the policy; only the stored record counts.
func TestAnalyzeReadsThePolicyFromTheStore(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original()) // stored as local_only
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	result, err := findNode(t, "ai.analyze").Execute(context.Background(), env, node.Input{
		Parameters: mustJSON(t, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript(), "ai_processing": "external_allowed"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decode[domain.Analysis](t, result).Available || len(server.Requests()) != 0 {
		t.Fatal("a parameter widened the policy")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAnalyzeExternalAllowedSendsWithoutSensitiveSpeech(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(allowExternal(original()))
	if err := env.store.Transcripts().MarkSensitive(context.Background(), domain.SensitiveSegment{AssetID: "ORI-001", Version: "v1", StartSeconds: 120, EndSeconds: 130}); err != nil {
		t.Fatal(err)
	}
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	analysis, result := analyzeResult(t, env)
	if !analysis.Available || analysis.Endpoint != "external" {
		t.Fatalf("analysis = %+v", analysis)
	}
	requests := server.Requests()
	if len(requests) == 0 {
		t.Fatal("nothing was sent")
	}
	for _, request := range requests {
		if strings.Contains(request.Text(), "Maria") || strings.Contains(request.Text(), "illness") {
			t.Fatalf("sensitive speech reached an external endpoint:\n%s", request.Text())
		}
	}
	if !strings.Contains(requests[0].Text(), "no remorse") {
		t.Fatal("the rest must still be sent")
	}
	if result.Usage[0].Operation != "ai.external" {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestAnalyzeWithNoEndpoint(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	analysis, _ := analyzeResult(t, env)
	if analysis.Available || analysis.UnavailableReason != retrieval.ReasonNoEndpoint {
		t.Fatalf("analysis = %+v", analysis)
	}
}

// I206: an LLM outage must not hold back an ingest.
func TestAnalyzeDegradesWhenTheEndpointFails(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	server.FailNext(http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	analysis, _ := analyzeResult(t, env)
	if analysis.Available || analysis.UnavailableReason != retrieval.ReasonEndpointFailed {
		t.Fatalf("analysis = %+v", analysis)
	}
	if strings.Contains(env.logs.String(), "Maria") {
		t.Fatal("transcript text reached the logs")
	}
}

func TestAnalyzeInvalidOutputDegrades(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	server.Reply(func(r mockllm.Request) (string, bool) { return "not json, Maria Silva", true })
	analysis, _ := analyzeResult(t, env)
	if analysis.Available || analysis.UnavailableReason != retrieval.ReasonEndpointFailed {
		t.Fatalf("analysis = %+v", analysis)
	}
	if strings.Contains(env.logs.String(), "Maria") {
		t.Fatal("LLM output reached the logs")
	}
}

func TestAnalyzeNoSpeechAsksNobody(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	result, err := run(t, findNode(t, "ai.analyze"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": silence()})
	if err != nil {
		t.Fatal(err)
	}
	if analysis := decode[domain.Analysis](t, result); analysis.Available || analysis.UnavailableReason != retrieval.ReasonNoSpeech || len(server.Requests()) != 0 {
		t.Fatalf("analysis = %+v", analysis)
	}
}

func TestUnknownAssetIsNotFound(t *testing.T) {
	env := newDaemonEnv(t)
	for nodeType, params := range map[string]map[string]any{
		"ai.analyze":           {"assetId": "ORI-404", "transcript": silence()},
		"ai.editorial":         {"assetId": "ORI-404"},
		"ai.index":             {"assetId": "ORI-404", "transcript": silence()},
		"ai.find_segment":      {"asset": "ORI-404", "query": "x"},
		"ai.derive_transcript": {"sourceId": "ORI-404", "segments": []map[string]any{{"start_seconds": 0, "end_seconds": 1}}},
	} {
		if _, err := run(t, findNode(t, nodeType), env, 1, params); !errs.IsCode(err, errs.CodeNotFound) || node.IsTransient(err) {
			t.Errorf("%s: err = %v", nodeType, err)
		}
	}
}

// ---- ai.index ----

func embedded(t *testing.T, model string) embedOutput {
	t.Helper()
	env := &aiEnv{baseEnv: newBaseEnv(t), embedder: &stubEmbedder{model: model}}
	result, err := run(t, findNode(t, "ai.embed"), env, 1, map[string]any{"transcript": storyTranscript()})
	if err != nil {
		t.Fatal(err)
	}
	return decode[embedOutput](t, result)
}

func TestIndexStoresEverythingAndEmitsBothEvents(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	embeddings := embedded(t, "")
	analysis := domain.Analysis{Available: true, Summary: "S"}
	result, err := run(t, findNode(t, "ai.index"), env, 1, map[string]any{
		"assetId": "ORI-001", "version": "v1", "transcript": storyTranscript(), "analysis": analysis, "embeddings": embeddings,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[indexOutput](t, result)
	if !out.Indexed || out.Vectors != len(embeddings.Chunks) || out.LexicalDocuments != len(embeddings.Chunks) || out.EmbeddingModel != aiworker.FakeEmbeddingModel {
		t.Fatalf("out = %+v", out)
	}
	ctx := context.Background()
	if _, err := env.store.Transcripts().Get(ctx, "ORI-001", "v1"); err != nil {
		t.Fatal(err)
	}
	stored, err := env.store.Transcripts().Analysis(ctx, "ORI-001", "v1")
	if err != nil || stored.Summary != "S" || stored.AssetID != "ORI-001" || stored.Version != "v1" {
		t.Fatalf("analysis = %+v, err = %v", stored, err)
	}
	chunks, chunking := env.store.VectorChunks(aiworker.FakeEmbeddingModel, "ORI-001", "v1")
	if len(chunks) == 0 || chunking != retrieval.ChunkingStrategy {
		t.Fatalf("%d chunks, chunking %q", len(chunks), chunking)
	}
	if active, _ := env.store.Search().ActiveModel(ctx); active != aiworker.FakeEmbeddingModel {
		t.Fatalf("active = %q", active)
	}
	if len(env.events) != 2 || env.events[0].Type != event.AssetIndexed || env.events[1].Type != event.OriginalIndexed {
		t.Fatalf("events = %+v", env.events)
	}
	// Events never carry transcript content (§8.2).
	payload, _ := json.Marshal(env.events[0].Payload)
	if strings.Contains(string(payload), "remorse") || strings.Contains(string(payload), "Maria") {
		t.Fatalf("payload = %s", payload)
	}
	// The window ranges of both indexes are equal, so their hits fuse.
	lexical := env.store.LexicalChunks("ORI-001", "v1")
	if lexical[0].StartSeconds != chunks[0].StartSeconds || lexical[0].EndSeconds != chunks[0].EndSeconds {
		t.Fatalf("lexical %+v, vector %+v", lexical[0], chunks[0].SearchChunk)
	}
	current, _ := env.store.Assets().Get(ctx, "ORI-001")
	if current.Language != "en" {
		t.Fatalf("language = %q", current.Language)
	}
}

func TestIndexDerivativeEmitsOnlyAssetIndexed(t *testing.T) {
	env := newDaemonEnv(t)
	clip := original()
	clip.ID, clip.Kind = "CLIP-001", domain.KindClip
	env.store.PutAsset(clip)
	if _, err := run(t, findNode(t, "ai.index"), env, 1, map[string]any{"assetId": "CLIP-001", "transcript": storyTranscript(), "embeddings": embedded(t, "")}); err != nil {
		t.Fatal(err)
	}
	if len(env.events) != 1 || env.events[0].Type != event.AssetIndexed {
		t.Fatalf("events = %+v", env.events)
	}
}

func TestIndexRetryOfACommittedAttemptDoesNotEmitAgain(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	params := map[string]any{"assetId": "ORI-001", "transcript": storyTranscript(), "embeddings": embedded(t, "")}
	if _, err := run(t, findNode(t, "ai.index"), env, 1, params); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, findNode(t, "ai.index"), env, 2, params); err != nil {
		t.Fatal(err)
	}
	if len(env.events) != 2 {
		t.Fatalf("events = %+v", env.events)
	}
}

func TestIndexNoSpeechIsNotIndexed(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	result, err := run(t, findNode(t, "ai.index"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": silence(), "embeddings": embedOutput{Chunking: retrieval.ChunkingStrategy, Chunks: []domain.EmbeddedChunk{}}})
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[indexOutput](t, result); out.Indexed {
		t.Fatalf("out = %+v", out)
	}
	if len(env.events) != 0 {
		t.Fatal("no valid index, no event")
	}
}

func TestIndexWithoutEmbeddingsIsLexicalOnlyAndNotIndexed(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	result, err := run(t, findNode(t, "ai.index"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript()})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[indexOutput](t, result)
	if out.Indexed || out.LexicalDocuments == 0 || out.Vectors != 0 {
		t.Fatalf("out = %+v", out)
	}
}

func TestIndexValidatesItsInput(t *testing.T) {
	good := embedded(t, "")
	noModel := good
	noModel.Model = ""
	noChunking := good
	noChunking.Chunking = ""
	wrongDims := good
	wrongDims.Dimensions = 5
	badTranscript := storyTranscript()
	badTranscript.Segments[0].End = -1
	cases := map[string]map[string]any{
		"no model":       {"assetId": "ORI-001", "transcript": storyTranscript(), "embeddings": noModel},
		"no chunking":    {"assetId": "ORI-001", "transcript": storyTranscript(), "embeddings": noChunking},
		"wrong dims":     {"assetId": "ORI-001", "transcript": storyTranscript(), "embeddings": wrongDims},
		"bad transcript": {"assetId": "ORI-001", "transcript": badTranscript, "embeddings": good},
	}
	for name, params := range cases {
		env := newDaemonEnv(t)
		env.store.PutAsset(original())
		_, err := run(t, findNode(t, "ai.index"), env, 1, params)
		if !errs.IsCode(err, errs.CodeInvalidArgument) || node.IsTransient(err) {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(env.events) != 0 {
			t.Errorf("%s: emitted", name)
		}
	}
}

// One transaction: a failed event leaves nothing behind (§8.2).
func TestIndexRollsBackTogether(t *testing.T) {
	env := &failingEmit{daemonEnv: newDaemonEnv(t)}
	env.store.PutAsset(original())
	_, err := run(t, findNode(t, "ai.index"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript(), "embeddings": embedded(t, "")})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if env.store.Rollbacks != 1 {
		t.Fatalf("rollbacks = %d", env.store.Rollbacks)
	}
	if chunks := env.store.LexicalChunks("ORI-001", "v1"); len(chunks) != 0 {
		t.Fatalf("%d chunks survived", len(chunks))
	}
	if _, err := env.store.Transcripts().Get(context.Background(), "ORI-001", "v1"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("the transcript survived: %v", err)
	}
}

type failingEmit struct{ *daemonEnv }

func (e *failingEmit) Emit(context.Context, repo.TenantRepos, event.Draft) error {
	return errs.New(errs.CodeInternal, "outbox down")
}

// I207: a clip inherits its source's marks, mapped onto its own timeline.
func TestIndexCarriesSensitiveMarksToAClip(t *testing.T) {
	env := newDaemonEnv(t)
	ctx := context.Background()
	env.store.PutAsset(original())
	if err := env.store.Transcripts().MarkSensitive(ctx, domain.SensitiveSegment{AssetID: "ORI-001", Version: "v1", StartSeconds: 120, EndSeconds: 130, Reason: "health"}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Transcripts().MarkSensitive(ctx, domain.SensitiveSegment{AssetID: "ORI-001", Version: "v1", StartSeconds: 500, EndSeconds: 510}); err != nil {
		t.Fatal(err)
	}
	clip := original()
	clip.ID, clip.ULID, clip.Kind = "CLIP-001", "01J00000000000000000000002", domain.KindClip
	clip.Lineage = &domain.Lineage{
		Method:        domain.LineageComputed,
		SourceAssetID: "ORI-001",
		RootAssetID:   "ORI-001",
		Segments: []domain.Segment{
			{SourceAssetID: "ORI-001", SourceVersion: "v1", StartSeconds: 10, EndSeconds: 20},
			{SourceAssetID: "ORI-001", SourceVersion: "v1", StartSeconds: 100, EndSeconds: 125},
		},
	}
	env.store.PutAsset(clip)
	params := map[string]any{"assetId": "CLIP-001", "version": "v1", "transcript": storyTranscript(), "embeddings": embedded(t, "")}
	if _, err := run(t, findNode(t, "ai.index"), env, 1, params); err != nil {
		t.Fatal(err)
	}
	marks, err := env.store.Transcripts().Sensitive(ctx, "CLIP-001", "v1")
	if err != nil {
		t.Fatal(err)
	}
	// 120-125 of the source is 20 s into the second cut, after 10 s of the first.
	if len(marks) != 1 || marks[0].StartSeconds != 30 || marks[0].EndSeconds != 35 || !strings.Contains(marks[0].Reason, "ORI-001") {
		t.Fatalf("marks = %+v", marks)
	}
	// A retry does not duplicate them.
	if _, err := run(t, findNode(t, "ai.index"), env, 2, params); err != nil {
		t.Fatal(err)
	}
	if marks, _ := env.store.Transcripts().Sensitive(ctx, "CLIP-001", "v1"); len(marks) != 1 {
		t.Fatalf("marks = %+v", marks)
	}
}

// D15: reindexing runs side by side and the new model takes over only when complete.
func TestIndexSwapsTheActiveModelOnlyWhenComplete(t *testing.T) {
	env := newDaemonEnv(t)
	ctx := context.Background()
	second := original()
	second.ID, second.ULID = "ORI-002", "01J00000000000000000000003"
	env.store.PutAsset(original())
	env.store.PutAsset(second)
	index := func(asset id.Friendly, model string) {
		t.Helper()
		if _, err := run(t, findNode(t, "ai.index"), env, 1, map[string]any{"assetId": string(asset), "transcript": storyTranscript(), "embeddings": embedded(t, model)}); err != nil {
			t.Fatal(err)
		}
	}
	index("ORI-001", "old@1")
	index("ORI-002", "old@1")
	if active, _ := env.store.Search().ActiveModel(ctx); active != "old@1" {
		t.Fatalf("active = %q", active)
	}

	index("ORI-001", "new@1")
	if active, _ := env.store.Search().ActiveModel(ctx); active != "old@1" {
		t.Fatalf("swapped before completion: active = %q", active)
	}
	if !env.store.HasModel("old@1") || !env.store.HasModel("new@1") {
		t.Fatal("both models must exist side by side")
	}

	index("ORI-002", "new@1")
	if active, _ := env.store.Search().ActiveModel(ctx); active != "new@1" {
		t.Fatalf("not swapped after completion: active = %q", active)
	}
	if env.store.HasModel("old@1") {
		t.Fatal("the old index must be replaced")
	}
}

// ---- ai.find_segment ----

func findEnv(t *testing.T) *daemonEnv {
	t.Helper()
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	env.embedder = &stubEmbedder{}
	if _, err := run(t, findNode(t, "ai.index"), env, 1, map[string]any{"assetId": "ORI-001", "version": "v1", "transcript": storyTranscript(), "embeddings": embedded(t, "")}); err != nil {
		t.Fatal(err)
	}
	return env
}

type findOutput struct {
	Segments   []domain.Segment          `json:"segments"`
	Candidates []domain.CandidateSegment `json:"candidates"`
	Ambiguous  bool                      `json:"ambiguous"`
	Confidence float64                   `json:"confidence"`
	Excerpt    string                    `json:"excerpt"`
	Method     string                    `json:"method"`
	Reason     string                    `json:"reason"`
}

// D51 and I28: no LLM still gives candidates, ambiguous, with evidence.
func TestFindSegmentWithoutLLM(t *testing.T) {
	env := findEnv(t)
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "the woman who had no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[findOutput](t, result)
	if !out.Ambiguous || out.Method != retrieval.MethodRetrieval || out.Reason != retrieval.ReasonNoEndpoint {
		t.Fatalf("out = %+v", out)
	}
	if len(out.Segments) == 0 || len(out.Candidates) == 0 || out.Excerpt == "" || out.Confidence <= 0 {
		t.Fatalf("out = %+v", out)
	}
	if out.Segments[0].SourceAssetID != "ORI-001" || out.Segments[0].SourceVersion != "v1" {
		t.Fatalf("segment = %+v", out.Segments[0])
	}
	if !strings.Contains(out.Excerpt, "remorse") {
		t.Fatalf("excerpt = %q", out.Excerpt)
	}
	if len(result.Usage) != 0 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestFindSegmentAcceptsCatalogOutput(t *testing.T) {
	env := findEnv(t)
	// catalog.get's output, claiming a policy the asset does not have.
	asset := map[string]any{"id": "ORI-001", "kind": "original", "status": "ready_for_clipping", "ai_processing": "external_allowed"}
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": asset, "query": "no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	if decode[findOutput](t, result).Method != retrieval.MethodRetrieval || len(server.Requests()) != 0 {
		t.Fatal("the policy must come from the store, not from the parameter")
	}
}

func TestFindSegmentRejectsABadAssetParameter(t *testing.T) {
	env := findEnv(t)
	for _, asset := range []any{map[string]any{"title": "no id"}, 7, ""} {
		if _, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": asset, "query": "x"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%v: err = %v", asset, err)
		}
	}
}

func TestFindSegmentWithALocalLLM(t *testing.T) {
	env := findEnv(t)
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "who had no remorse", "mode": "complete_story"})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[findOutput](t, result)
	if out.Method != retrieval.MethodLLM || out.Reason != "" {
		t.Fatalf("out = %+v", out)
	}
	if len(server.Requests()) != 1 || server.Requests()[0].Task != "find_segment" {
		t.Fatalf("requests = %+v", server.Requests())
	}
	if !strings.Contains(out.Excerpt, "remorse") || out.Confidence == 0 {
		t.Fatalf("out = %+v", out)
	}
	if len(result.Usage) != 1 || result.Usage[0].Operation != "ai.local" {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestFindSegmentLocalOnlyNeverContactsAnExternalEndpoint(t *testing.T) {
	env := findEnv(t)
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[findOutput](t, result)
	if !out.Ambiguous || out.Method != retrieval.MethodRetrieval || out.Reason != retrieval.ReasonNoEndpoint {
		t.Fatalf("out = %+v", out)
	}
	if len(server.Requests()) != 0 {
		t.Fatal("content reached an external endpoint on local_only")
	}
}

func TestFindSegmentExternalAllowedExcludesSensitiveSpeech(t *testing.T) {
	env := findEnv(t)
	env.store.PutAsset(allowExternal(original()))
	if err := env.store.Transcripts().MarkSensitive(context.Background(), domain.SensitiveSegment{AssetID: "ORI-001", Version: "v1", StartSeconds: 120, EndSeconds: 130}); err != nil {
		t.Fatal(err)
	}
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "the woman who had no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	if len(server.Requests()) == 0 {
		t.Fatal("nothing was sent")
	}
	for _, request := range server.Requests() {
		if strings.Contains(request.Text(), "Maria") {
			t.Fatalf("sensitive speech reached an external endpoint:\n%s", request.Text())
		}
	}
	if out := decode[findOutput](t, result); strings.Contains(out.Excerpt, "Maria") {
		t.Fatalf("excerpt = %q", out.Excerpt)
	}
}

func TestFindSegmentEndpointFailureDegrades(t *testing.T) {
	env := findEnv(t)
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	server.FailNext(http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError)
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[findOutput](t, result); !out.Ambiguous || out.Reason != retrieval.ReasonEndpointFailed {
		t.Fatalf("out = %+v", out)
	}
}

// I211: nothing retrieved must not become a random cut.
func TestFindSegmentNoMatchFails(t *testing.T) {
	env := findEnv(t)
	_, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "quarterly invoice"})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) || node.IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindSegmentNeedsATranscript(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	_, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "x"})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindSegmentWithoutEmbedderIsLexical(t *testing.T) {
	env := findEnv(t)
	env.embedder = nil
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[findOutput](t, result); len(out.Segments) == 0 {
		t.Fatalf("out = %+v", out)
	}
}

func TestFindSegmentReadsAStoredStory(t *testing.T) {
	env := findEnv(t)
	if err := env.store.Transcripts().SaveAnalysis(context.Background(), domain.Analysis{
		AssetID: "ORI-001", Version: "v1", Available: true,
		Stories: []domain.Story{{Title: "The woman", StartSeconds: 40, EndSeconds: 200, Confidence: 0.9}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[findOutput](t, result); out.Segments[0].StartSeconds > 40 || out.Segments[0].EndSeconds < 200 {
		t.Fatalf("segments = %+v", out.Segments)
	}
}

// ---- ai.derive_transcript ----

func TestDeriveTranscript(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	stored := storyTranscript()
	stored.AssetID, stored.Version = "ORI-001", "v1"
	if err := env.store.Transcripts().Save(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	result, err := run(t, findNode(t, "ai.derive_transcript"), env, 1, map[string]any{
		"sourceId": "ORI-001",
		"segments": []map[string]any{
			{"source_asset_id": "ORI-001", "source_version": "v1", "start_seconds": 100, "end_seconds": 120},
			{"source_asset_id": "ORI-001", "start_seconds": 140, "end_seconds": 150},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	transcript := decode[struct {
		Transcript domain.Transcript `json:"transcript"`
	}](t, result).Transcript
	if transcript.Engine != "derived:fake/canned@1" || transcript.DurationSeconds != 30 || transcript.TimestampBasis != "working_master" {
		t.Fatalf("transcript = %+v", transcript)
	}
	if len(transcript.Segments) != 3 || transcript.Segments[2].Start != 20 || !strings.Contains(transcript.Segments[2].Text, "no remorse") {
		t.Fatalf("segments = %+v", transcript.Segments)
	}
}

// R12: one source per clip in the MVP.
func TestDeriveRefusesAnotherSource(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	_, err := run(t, findNode(t, "ai.derive_transcript"), env, 1, map[string]any{
		"sourceId": "ORI-001",
		"segments": []map[string]any{{"source_asset_id": "ORI-002", "start_seconds": 0, "end_seconds": 1}},
	})
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeriveNeedsTheSourceTranscript(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	_, err := run(t, findNode(t, "ai.derive_transcript"), env, 1, map[string]any{
		"sourceId": "ORI-001",
		"segments": []map[string]any{{"start_seconds": 0, "end_seconds": 1}},
	})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

// ---- ai.editorial ----

func TestEditorialFallsBackWithoutAnLLM(t *testing.T) {
	env := newDaemonEnv(t)
	clip := original()
	clip.ID, clip.ULID, clip.Kind, clip.Title = "CLIP-001", "01J00000000000000000000002", domain.KindClip, "Clip"
	clip.Lineage = &domain.Lineage{SourceAssetID: "ORI-001"}
	clip.Tags = []string{"yoga"}
	env.store.PutAsset(original())
	env.store.PutAsset(clip)
	result, err := run(t, findNode(t, "ai.editorial"), env, 1, map[string]any{"assetId": "CLIP-001", "prompt": "The woman who had no remorse"})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[retrieval.Editorial](t, result)
	if out.Generated || out.Title != "The woman who had no remorse" || out.Description != "From: Hatha Yoga Class" || out.Tags[0] != "yoga" {
		t.Fatalf("out = %+v", out)
	}
}

func TestEditorialWithAnLLMReadsTheStoredTranscript(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	stored := storyTranscript()
	stored.AssetID, stored.Version = "ORI-001", "v1"
	if err := env.store.Transcripts().Save(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	result, err := run(t, findNode(t, "ai.editorial"), env, 1, map[string]any{"assetId": "ORI-001", "prompt": "A prompt"})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[retrieval.Editorial](t, result)
	if !out.Generated || !strings.HasPrefix(out.Title, "[mock]") {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(server.Requests()[0].Text(), "no remorse") {
		t.Fatal("the stored transcript must be used")
	}
	if len(result.Usage) != 1 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestEditorialLocalOnlyNeverContactsAnExternalEndpoint(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	result, err := run(t, findNode(t, "ai.editorial"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript(), "prompt": "P"})
	if err != nil {
		t.Fatal(err)
	}
	if decode[retrieval.Editorial](t, result).Generated || len(server.Requests()) != 0 {
		t.Fatal("content reached an external endpoint on local_only")
	}
}

func TestEditorialExternalAllowedExcludesSensitiveSpeech(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(allowExternal(original()))
	if err := env.store.Transcripts().MarkSensitive(context.Background(), domain.SensitiveSegment{AssetID: "ORI-001", Version: "v1", StartSeconds: 120, EndSeconds: 130}); err != nil {
		t.Fatal(err)
	}
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	if _, err := run(t, findNode(t, "ai.editorial"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript(), "prompt": "P"}); err != nil {
		t.Fatal(err)
	}
	if len(server.Requests()) == 0 || strings.Contains(server.Requests()[0].Text(), "Maria") {
		t.Fatalf("requests = %+v", server.Requests())
	}
}

func TestEditorialEndpointFailureFallsBack(t *testing.T) {
	env := newDaemonEnv(t)
	env.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointLocal)
	env.llm = llm
	server.FailNext(http.StatusBadGateway, http.StatusBadGateway, http.StatusBadGateway)
	result, err := run(t, findNode(t, "ai.editorial"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript(), "prompt": "P"})
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[retrieval.Editorial](t, result); out.Generated || out.Title != "P" {
		t.Fatalf("out = %+v", out)
	}
}

// Nothing here may log what was said.
func TestLogsCarryNoTranscriptText(t *testing.T) {
	env := findEnv(t)
	env.store.PutAsset(allowExternal(original()))
	server, llm := mockEndpoint(t, config.EndpointExternal)
	env.llm = llm
	server.FailNext(http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError)
	if _, err := run(t, findNode(t, "ai.find_segment"), env, 1, map[string]any{"asset": "ORI-001", "query": "no remorse"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, findNode(t, "ai.analyze"), env, 1, map[string]any{"assetId": "ORI-001", "transcript": storyTranscript()}); err != nil {
		t.Fatal(err)
	}
	logs := env.logs.String()
	for _, secret := range []string{"Maria", "remorse", "illness"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs carry %q:\n%s", secret, logs)
		}
	}
}
