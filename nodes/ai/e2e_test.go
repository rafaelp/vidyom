package ainodes

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fasterwhisper "github.com/rafaelp/vidyom/connectors/transcription/faster-whisper"

	"github.com/rafaelp/vidyom/connectors/ai/bgem3"
	"github.com/rafaelp/vidyom/connectors/ai/workerclient/workertest"
	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/retrieval"
	"github.com/rafaelp/vidyom/internal/retrieval/retrievaltest"
)

type oneStore struct{ store *retrievaltest.Store }

func (s oneStore) Create(context.Context, domain.TenantID) (repo.TenantStore, error) {
	return nil, errors.New("not used")
}
func (s oneStore) ForTenant(context.Context, domain.TenantID) (repo.TenantStore, error) {
	return s.store, nil
}
func (s oneStore) Tenants(context.Context) ([]domain.TenantID, error) { return nil, nil }
func (s oneStore) Close() error                                       { return nil }

// realConnectors opens the real connectors against a running fake-engine worker.
func realConnectors(t *testing.T, endpoint string) (connector.Transcriber, connector.Embedder) {
	t.Helper()
	registry := connector.NewRegistry()
	fasterwhisper.Register(registry)
	bgem3.Register(registry)
	params := connector.AIParams{Tenant: testTenant, Daemon: config.AIWorker{Endpoint: endpoint, Engine: aiworker.EngineFake}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	transcribers, err := registry.Transcriber(fasterwhisper.Driver)
	if err != nil {
		t.Fatal(err)
	}
	transcriber, err := transcribers(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	embedders, err := registry.Embedder(bgem3.Driver)
	if err != nil {
		t.Fatal(err)
	}
	embedder, err := embedders(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	return transcriber, embedder
}

// The whole AI path of §3.1 and §3.2 with the real Python process and the real
// connectors: only the LLM endpoint and the tenant store are stand-ins. With the fake
// engine this proves the path, never transcription or semantic quality (I27).
func TestWholeAIPathWithThePythonWorker(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir)
	transcriber, embedder := realConnectors(t, worker.Endpoint())
	audio := filepath.Join(dir, "audio.wav")
	workertest.WriteWAV(t, audio, 300)

	workerEnv := &aiEnv{baseEnv: newBaseEnv(t), transcriber: transcriber, embedder: embedder}
	workerEnv.inputs["scratch://audio"] = node.LocalInput{Path: audio}
	daemon := newDaemonEnv(t)
	daemon.embedder = embedder
	daemon.store.PutAsset(original())
	server, llm := mockEndpoint(t, config.EndpointLocal)
	daemon.llm = llm

	// ai.transcribe
	result, err := run(t, findNode(t, "ai.transcribe"), workerEnv, 1, map[string]any{"source": "scratch://audio", "assetId": "ORI-001", "wordTimestamps": true})
	if err != nil {
		t.Fatal(err)
	}
	transcribed := decode[transcribeOutput](t, result)
	if !strings.HasPrefix(transcribed.Engine, "fake/") || result.Tools["asr"] != transcribed.Engine {
		t.Fatalf("the fake must be labeled: %q, %v", transcribed.Engine, result.Tools)
	}
	if transcribed.DurationSeconds != 300 {
		t.Fatalf("duration = %f", transcribed.DurationSeconds)
	}
	if !strings.Contains(transcribed.Transcript.Segments[0].Text, "FAKE ENGINE") {
		t.Fatalf("first segment = %q", transcribed.Transcript.Segments[0].Text)
	}

	// ai.analyze
	result, err = run(t, findNode(t, "ai.analyze"), daemon, 1, map[string]any{"assetId": "ORI-001", "transcript": transcribed.Transcript})
	if err != nil {
		t.Fatal(err)
	}
	analysis := decode[domain.Analysis](t, result)
	if !analysis.Available || len(analysis.Chapters) == 0 {
		t.Fatalf("analysis = %+v", analysis)
	}

	// ai.embed
	result, err = run(t, findNode(t, "ai.embed"), workerEnv, 1, map[string]any{"assetId": "ORI-001", "transcript": transcribed.Transcript, "analysis": analysis})
	if err != nil {
		t.Fatal(err)
	}
	embeddings := decode[embedOutput](t, result)
	if embeddings.Model != aiworker.FakeEmbeddingModel || len(embeddings.Chunks) < 5 {
		t.Fatalf("model %q, %d chunks", embeddings.Model, len(embeddings.Chunks))
	}

	// ai.index
	result, err = run(t, findNode(t, "ai.index"), daemon, 1, map[string]any{
		"assetId": "ORI-001", "version": "v1", "transcript": transcribed.Transcript, "analysis": analysis, "embeddings": embeddings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[indexOutput](t, result); !out.Indexed {
		t.Fatalf("out = %+v", out)
	}
	if len(daemon.events) != 2 || daemon.events[0].Type != event.AssetIndexed {
		t.Fatalf("events = %+v", daemon.events)
	}

	// GET /search: hybrid, with a query vector from the same worker.
	search := retrieval.New(retrieval.Options{
		Stores:   oneStore{daemon.store},
		Embedder: func(context.Context, domain.TenantID) (connector.Embedder, error) { return embedder, nil },
	})
	response, err := search.Search(context.Background(), service.Scope{Tenant: testTenant}, api.SearchQuery{Q: "conscious breathing", AssetID: "ORI-001"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Mode != retrieval.ModeHybrid || response.EmbeddingModel != aiworker.FakeEmbeddingModel || len(response.Items) == 0 {
		t.Fatalf("response = %+v", response)
	}
	if !strings.Contains(strings.ToLower(response.Items[0].Text), "conscious breathing") || response.Items[0].Source != "hybrid" {
		t.Fatalf("top hit = %+v", response.Items[0])
	}

	// ai.find_segment, with the example of §15.2.
	prompt := "Story of the 90-year-old woman who had no remorse"
	result, err = run(t, findNode(t, "ai.find_segment"), daemon, 1, map[string]any{"asset": "ORI-001", "query": prompt, "mode": "complete_story"})
	if err != nil {
		t.Fatal(err)
	}
	found := decode[findOutput](t, result)
	if found.Method != retrieval.MethodLLM || len(found.Segments) != 1 || !strings.Contains(found.Excerpt, "woman") {
		t.Fatalf("found = %+v", found)
	}

	// The same with no LLM stays useful, and says it is ambiguous (I28).
	withoutLLM := *daemon
	withoutLLM.llm = nil
	result, err = run(t, findNode(t, "ai.find_segment"), &withoutLLM, 1, map[string]any{"asset": "ORI-001", "query": prompt})
	if err != nil {
		t.Fatal(err)
	}
	if out := decode[findOutput](t, result); !out.Ambiguous || len(out.Candidates) == 0 || !strings.Contains(out.Candidates[0].Excerpt, "woman") {
		t.Fatalf("out = %+v", out)
	}

	// A sensitive mark on the source, then the clip of §3.2.
	segment := found.Segments[0]
	if err := daemon.store.Transcripts().MarkSensitive(context.Background(), domain.SensitiveSegment{
		AssetID: "ORI-001", Version: "v1", StartSeconds: segment.StartSeconds, EndSeconds: segment.StartSeconds + 2,
	}); err != nil {
		t.Fatal(err)
	}
	result, err = run(t, findNode(t, "ai.derive_transcript"), daemon, 1, map[string]any{"sourceId": "ORI-001", "segments": found.Segments})
	if err != nil {
		t.Fatal(err)
	}
	derived := decode[struct {
		Transcript domain.Transcript `json:"transcript"`
	}](t, result).Transcript
	if !strings.HasPrefix(derived.Engine, "derived:fake/") || len(derived.Segments) == 0 || derived.Segments[0].Start > 1 {
		t.Fatalf("derived = %+v", derived)
	}

	clip := original()
	clip.ID, clip.ULID, clip.Kind, clip.Title = "CLIP-001", "01J00000000000000000000002", domain.KindClip, prompt
	clip.Lineage = &domain.Lineage{Method: domain.LineageComputed, SourceAssetID: "ORI-001", RootAssetID: "ORI-001", Segments: found.Segments}
	daemon.store.PutAsset(clip)
	result, err = run(t, findNode(t, "ai.embed"), workerEnv, 1, map[string]any{"assetId": "CLIP-001", "transcript": derived})
	if err != nil {
		t.Fatal(err)
	}
	clipEmbeddings := decode[embedOutput](t, result)
	if _, err := run(t, findNode(t, "ai.index"), daemon, 1, map[string]any{"assetId": "CLIP-001", "transcript": derived, "embeddings": clipEmbeddings}); err != nil {
		t.Fatal(err)
	}
	marks, err := daemon.store.Transcripts().Sensitive(context.Background(), id.Ref("CLIP-001"), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 1 || marks[0].StartSeconds != 0 || marks[0].EndSeconds != 2 {
		t.Fatalf("the clip must inherit the mark on its own timeline: %+v", marks)
	}

	// ai.export_transcript in the worker pool, with the marks for anything public.
	result, err = run(t, findNode(t, "ai.export_transcript"), workerEnv, 1, map[string]any{"transcript": derived, "sensitive": marks, "excludeSensitive": true, "formats": []string{"vtt"}})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[struct {
		Files map[string]exportedFile `json:"files"`
	}](t, result)
	if !strings.HasPrefix(out.Files["vtt"].File, "scratch://") {
		t.Fatalf("files = %+v", out.Files)
	}
	vtt := readFile(t, filepath.Join(workerEnv.scratch, "transcript.vtt"))
	if !strings.HasPrefix(vtt, "WEBVTT") {
		t.Fatalf("vtt = %s", vtt)
	}

	// ai.editorial
	result, err = run(t, findNode(t, "ai.editorial"), daemon, 1, map[string]any{"assetId": "CLIP-001", "prompt": prompt})
	if err != nil {
		t.Fatal(err)
	}
	if draft := decode[retrieval.Editorial](t, result); !draft.Generated || !strings.HasPrefix(draft.Title, "[mock]") {
		t.Fatalf("draft = %+v", draft)
	}

	tasks := map[string]int{}
	for _, request := range server.Requests() {
		tasks[request.Task]++
	}
	if tasks["analyze"] == 0 || tasks["find_segment"] == 0 || tasks["editorial"] == 0 {
		t.Fatalf("tasks = %v", tasks)
	}
}

// A job's cancel reaches the Python worker and ends the node as cancelled (D11).
func TestCancelReachesThePythonWorker(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir, "VIDYOM_AI_FAKE_DELAY_SECONDS=60")
	transcriber, _ := realConnectors(t, worker.Endpoint())
	audio := filepath.Join(dir, "audio.wav")
	workertest.WriteWAV(t, audio, 5)
	env := &aiEnv{baseEnv: newBaseEnv(t), transcriber: transcriber}
	env.inputs["scratch://audio"] = node.LocalInput{Path: audio}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := findNode(t, "ai.transcribe").Execute(ctx, env, node.Input{Parameters: mustJSON(t, map[string]any{"source": "scratch://audio"}), Attempt: 1})
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || node.IsTransient(err) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not return")
	}
}

// The worker is down: a retry can fix that, so the failure is transient.
func TestWorkerDownIsTransient(t *testing.T) {
	dir := workertest.ShortTempDir(t)
	worker := workertest.Start(t, dir)
	transcriber, _ := realConnectors(t, worker.Endpoint())
	worker.Stop()
	env := &aiEnv{baseEnv: newBaseEnv(t), transcriber: transcriber}
	env.inputs["scratch://audio"] = node.LocalInput{Path: filepath.Join(dir, "missing.wav")}
	_, err := run(t, findNode(t, "ai.transcribe"), env, 1, map[string]any{"source": "scratch://audio"})
	if !node.IsTransient(err) || !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
