//go:build e2e && realmodels

package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// The real-model proof (deliverable 5 of the integration stage, I291): faster-whisper
// large-v3 and BGE-M3 on CPU, through the AI worker image, on real speech, searched with
// a paraphrase that shares no word with what was said. This is what §15.2's "no literal
// match" means, and what the fake engine cannot show. It downloads gigabytes, needs macOS
// for `say`, and reports what happened instead of asserting a quality it cannot promise:
// the assertions are only that the path ran on real models.
//
//	make real-model-proof
//
// Models are cached in the Docker volume vidyom-dev-integration-models, outside the
// repository, and survive the run. Remove it with `docker volume rm` when done.

const modelsVolume = "vidyom-dev-integration-models"

// story is one passage of the spoken fixture, with a query saying the same thing in other
// words. Each story is long enough for its own embedding windows (D15).
type story struct {
	Name  string
	Text  string
	Query string
}

var stories = []story{
	{
		Name: "lost dog",
		Text: "When the storm came, Tomas could not find his dog anywhere in the village. " +
			"He walked along the river for three days, calling her name until his voice broke. " +
			"Neighbors told him to stop looking, that no animal could survive such floods. " +
			"On the fourth morning he heard scratching at the kitchen door. " +
			"There she stood, thin and muddy, her tail wagging as though nothing had happened. " +
			"Tomas sat down on the floor and cried into her wet fur for a long time. " +
			"Since that day he has never once closed that door at night.",
		Query: "beloved pet reappears safely following severe weather",
	},
	{
		Name: "baker",
		Text: "Every morning before sunrise, old Marta lit the oven in her little bakery by the harbor. " +
			"Fishermen came for bread while the sky was still black, and she never charged them for the first loaf. " +
			"Her daughter told her again and again that this generosity would ruin the shop. " +
			"Marta only laughed and kept kneading the dough with her strong hands. " +
			"When she turned ninety, the whole town lined up outside with flowers and coins, " +
			"and every boat in the harbor rang its bell at dawn to thank her.",
		Query: "elderly woman giving free loaves honored by grateful community",
	},
	{
		Name: "violin",
		Text: "My grandfather kept a broken violin on top of the wardrobe for fifty years. " +
			"He had carried it across the border as a young man, wrapped in a blanket, and he refused to talk about it. " +
			"Last winter, I took it to a repair shop in the city, and the craftsman found a letter hidden inside the body. " +
			"It was from his sister, who stayed behind, promising to meet him again after the war. " +
			"He read it twice, folded it carefully, and asked me to teach him how to play again.",
		Query: "hidden correspondence discovered within old musical instrument reveals family secret",
	},
}

var wordPattern = regexp.MustCompile(`[\p{L}\p{N}']+`)

func words(text string) []string {
	return wordPattern.FindAllString(strings.ToLower(text), -1)
}

func wordSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range words(text) {
		out[w] = true
	}
	return out
}

func sharedWords(query string, text string) []string {
	have := wordSet(text)
	var shared []string
	for _, w := range words(query) {
		if have[w] {
			shared = append(shared, w)
		}
	}
	return shared
}

// wordErrorRate is the edit distance over words, against the words of the script.
func wordErrorRate(reference, hypothesis []string) float64 {
	if len(reference) == 0 {
		return 0
	}
	previous := make([]int, len(hypothesis)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(reference); i++ {
		current := make([]int, len(hypothesis)+1)
		current[0] = i
		for j := 1; j <= len(hypothesis); j++ {
			cost := 1
			if reference[i-1] == hypothesis[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return float64(previous[len(hypothesis)]) / float64(len(reference))
}

// speak renders the stories with macOS `say`, then a test pattern as the picture. It
// returns the fixture and where each story starts and ends.
func speak(t *testing.T, dir string) (string, [][2]float64) {
	t.Helper()
	var list strings.Builder
	var spans [][2]float64
	position := 0.0
	silence := filepath.Join(dir, "silence.wav")
	must(t, os.Environ(), dir, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "anullsrc=r=22050:cl=mono", "-t", "1.5", "-c:a", "pcm_s16le", silence)
	for i, s := range stories {
		aiff := filepath.Join(dir, fmt.Sprintf("story%d.aiff", i))
		must(t, os.Environ(), dir, "say", "-v", "Samantha", "-o", aiff, s.Text)
		wav := filepath.Join(dir, fmt.Sprintf("story%d.wav", i))
		must(t, os.Environ(), dir, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", aiff, "-ar", "22050", "-ac", "1", "-c:a", "pcm_s16le", wav)
		length := ffprobeDuration(t, wav)
		spans = append(spans, [2]float64{position, position + length})
		position += length + 1.5
		fmt.Fprintf(&list, "file '%s'\nfile '%s'\n", wav, silence)
	}
	listFile := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(listFile, []byte(list.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	speech := filepath.Join(dir, "speech.wav")
	must(t, os.Environ(), dir, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "concat", "-safe", "0", "-i", listFile, "-c", "copy", speech)
	fixture := filepath.Join(dir, "stories.mp4")
	total := ffprobeDuration(t, speech)
	must(t, os.Environ(), dir, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=1280x720:rate=25:duration=%.3f", total),
		"-i", speech, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", fixture)
	return fixture, spans
}

func TestRealModels(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the fixture's speech comes from macOS say")
	}
	if _, err := exec.LookPath("say"); err != nil {
		t.Skip("no say on this machine")
	}
	for _, s := range stories {
		for _, other := range stories {
			if shared := sharedWords(s.Query, other.Text); len(shared) > 0 {
				t.Fatalf("the query %q shares %v with the speech: not a paraphrase", s.Query, shared)
			}
		}
	}

	s := newCompose(t).(*composeStack)
	s.project = composeProject + "-real"
	s.extraEnv = []string{"VIDYOM_AI_ENGINE=real", "VIDYOM_MODELS_VOLUME=" + modelsVolume}
	s.composeFiles = []string{filepath.Join(s.repo, "deploy", "docker-compose.realmodels.yml")}
	s.configOverride = func(t *testing.T) {
		t.Helper()
		path := filepath.Join(s.config, "daemon.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		replaced := strings.Replace(string(data), `"engine": "fake"`, `"engine": "real"`, 1)
		if replaced == string(data) {
			t.Fatal("daemon.json does not name the fake engine")
		}
		if err := os.WriteFile(path, []byte(replaced), 0o644); err != nil {
			t.Fatal(err)
		}
		// No LLM at all: retrieval and vectors alone have to find the story.
		if err := os.WriteFile(filepath.Join(s.config, "tenants", "acme", "ai.json"), []byte(`{"ai": {"defaultProcessing": "local_only"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			s.Dump(t)
		}
		s.Down(t)
	})

	report := map[string]any{"host": runtime.GOOS + "/" + runtime.GOARCH, "date": time.Now().UTC().Format(time.RFC3339)}
	save := func() {
		out, _ := json.MarshalIndent(report, "", "  ")
		if path := os.Getenv("VIDYOM_REAL_REPORT"); path != "" {
			_ = os.WriteFile(path, out, 0o644)
		}
		t.Logf("report:\n%s", out)
	}
	defer save()

	must(t, os.Environ(), "", "docker", "volume", "create", modelsVolume)
	s.Up(t)

	// Download first, so its time is not mistaken for inference.
	started := time.Now()
	s.compose(t, "run", "--rm", "-T", "--no-deps", "--entrypoint", "python", "ai-engine", "-c", `
import time
t = time.time()
from faster_whisper import WhisperModel
WhisperModel("large-v3", device="cpu", compute_type="int8")
print("whisper ready", round(time.time() - t, 1))
t = time.time()
from sentence_transformers import SentenceTransformer
SentenceTransformer("BAAI/bge-m3", device="cpu")
print("bge-m3 ready", round(time.time() - t, 1))
`)
	report["model_download_and_load_seconds"] = math.Round(time.Since(started).Seconds())

	init := s.Init(t)
	dir, err := os.MkdirTemp("", "vdmreal")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fixture, spans := speak(t, dir)
	report["fixture_seconds"] = ffprobeDuration(t, fixture)

	s.StartDaemon(t)
	s.StartAIWorker(t)
	s.StartWorkers(t)

	bin := filepath.Join(dir, "vidyom")
	must(t, append(os.Environ(), "CGO_ENABLED=0"), repoRoot(t), "go", "build", "-tags", "vidyom_test_keyring", "-o", bin, "./cmd/vidyom")
	admin := newClient(t, "admin", dir, bin, s.HTTPSURL(), init.TLSFingerprint)
	waitFor(t, time.Minute, "the daemon", func() (bool, string) {
		_, stderr, code := admin.try(t, "server", "add", "main", "--url", s.HTTPSURL(), "--fingerprint", init.TLSFingerprint, "--force")
		return code == 0, stderr
	})
	waitFor(t, time.Minute, "pairing", func() (bool, string) {
		_, stderr, code := admin.try(t, "server", "pair", "main", "--code", init.PairingCode)
		return code == 0, stderr
	})
	admin.ok(t, "tenant", "use", tenant)

	var imported struct {
		Job *api.RunWorkflowResponse `json:"job"`
	}
	admin.jsonOut(t, &imported, "original", "import", fixture, "--title", "Three stories", "--type", "other")
	jobID := string(imported.Job.JobID)
	var job api.JobDetail
	ingestStart := time.Now()
	deadline := time.Now().Add(2 * time.Hour)
	for time.Now().Before(deadline) {
		admin.jsonOut(t, &job, "job", "status", jobID)
		failOn(t, job)
		if job.Status == domain.JobCompleted {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if job.Status != domain.JobCompleted {
		t.Fatalf("ingest did not finish: %s", nodeSummary(job))
	}
	report["ingest_seconds"] = math.Round(time.Since(ingestStart).Seconds())
	timing := map[string]float64{}
	for _, n := range job.Nodes {
		if n.StartedAt != nil && n.FinishedAt != nil {
			timing[n.NodeID] = math.Round(n.FinishedAt.Sub(*n.StartedAt).Seconds()*10) / 10
		}
	}
	report["node_seconds"] = timing

	var transcript domain.Transcript
	admin.jsonOut(t, &transcript, "asset", "transcript", "ORI-001")
	if !strings.Contains(transcript.Engine, "faster-whisper") {
		t.Fatalf("the transcript's engine is %q, so this proved nothing", transcript.Engine)
	}
	var text []string
	for _, seg := range transcript.Segments {
		text = append(text, strings.TrimSpace(seg.Text))
	}
	spoken := strings.Join(text, " ")
	var script strings.Builder
	for _, story := range stories {
		script.WriteString(story.Text + " ")
	}
	report["engine"] = transcript.Engine
	report["language"] = transcript.Language
	report["transcript"] = spoken
	report["segments"] = len(transcript.Segments)
	report["word_error_rate"] = math.Round(wordErrorRate(words(script.String()), words(spoken))*1000) / 1000

	var asset domain.Asset
	admin.jsonOut(t, &asset, "asset", "show", "ORI-001")
	if asset.Transcript == nil || asset.Transcript.EmbeddingModel == "fake-hash-embed@1" || asset.Status != domain.AssetReadyForClipping {
		t.Fatalf("asset after ingest: %+v", asset)
	}
	report["embedding_model"] = asset.Transcript.EmbeddingModel

	// Search with the paraphrases. Rank is the position of the first hit that overlaps the
	// story's own interval.
	queries := []map[string]any{}
	for i, s := range stories {
		entry := map[string]any{
			"story": s.Name, "query": s.Query, "story_seconds": spans[i],
			"shared_with_speech":     sharedWords(s.Query, script.String()),
			"shared_with_transcript": sharedWords(s.Query, spoken),
		}
		for _, mode := range []string{"hybrid", "semantic", "lexical"} {
			var result api.SearchResponse
			start := time.Now()
			admin.jsonOut(t, &result, "original", "search", s.Query, "--asset", "ORI-001", "--mode", mode, "--limit", "20")
			rank := 0
			hits := []map[string]any{}
			for j, hit := range result.Items {
				inStory := hit.StartSeconds < spans[i][1] && spans[i][0] < hit.EndSeconds
				if rank == 0 && inStory {
					rank = j + 1
				}
				hits = append(hits, map[string]any{
					"start": hit.StartSeconds, "end": hit.EndSeconds, "score": hit.Score, "source": hit.Source,
					"in_story": inStory, "text": truncate(hit.Text, 90),
				})
			}
			entry[mode] = map[string]any{
				"mode": result.Mode, "model": result.EmbeddingModel, "hits": hits, "rank_of_story": rank,
				"query_ms": time.Since(start).Milliseconds(),
			}
		}
		queries = append(queries, entry)
	}
	report["queries"] = queries

	// §3.2: the clip flow with a paraphrase and no LLM: a proposal, marked ambiguous (I28).
	var clip api.RunWorkflowResponse
	admin.jsonOut(t, &clip, "clip", "create", "--source", "ORI-001", "--prompt", stories[1].Query)
	clipDeadline := time.Now().Add(30 * time.Minute)
	var clipJob api.JobDetail
	for time.Now().Before(clipDeadline) {
		admin.jsonOut(t, &clipJob, "job", "status", string(clip.JobID))
		failOn(t, clipJob)
		if clipJob.Status == domain.JobWaitingApproval {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if clipJob.Status == domain.JobWaitingApproval {
		var approval domain.Approval
		admin.jsonOut(t, &approval, "approval", "show", "APR-001")
		var evidence map[string]any
		_ = json.Unmarshal(approval.Evidence, &evidence)
		report["clip_proposal"] = map[string]any{
			"query": stories[1].Query, "story_seconds": spans[1],
			"segments": evidence["segments"], "confidence": evidence["confidence"],
			"ambiguous": evidence["ambiguous"], "excerpt": evidence["excerpt"],
		}
	}
}
