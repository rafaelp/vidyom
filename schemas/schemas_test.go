package schemas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEveryDocumentCompiles(t *testing.T) {
	for _, name := range []string{Workflow, PluginManifest, ConnectorConfig, MediaProfile, NodeCatalogFile, AIWorkerRPC} {
		if _, err := Compile(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSpecExamplesValidate(t *testing.T) {
	cases := map[string]string{
		"workflow-spec-6.3.json":        Workflow,
		"workflow-spec-6.4.json":        Workflow,
		"workflow-spec-8.2.json":        Workflow,
		"workflow-hooks-8.1.json":       Workflow,
		"config-storages-7.2.json":      ConnectorConfig,
		"config-publishing-7.3.json":    ConnectorConfig,
		"config-observability-7.4.json": ConnectorConfig,
		"plugin-7.6.json":               PluginManifest,
		"profile-master-1080p.json":     MediaProfile,
		"profile-speech-opus.json":      MediaProfile,
	}
	for file, schema := range cases {
		if err := Validate(schema, readTestdata(t, file)); err != nil {
			t.Errorf("%s against %s: %v", file, schema, err)
		}
	}
}

func TestNegativeDocuments(t *testing.T) {
	cases := []struct {
		name, schema, doc string
	}{
		{"workflow without nodes", Workflow, `{"id":"x","version":"1.0.0"}`},
		{"workflow bad version", Workflow, `{"id":"x","version":"1","nodes":[{"id":"n","type":"a.b","typeVersion":1}]}`},
		{"workflow unknown field", Workflow, `{"id":"x","version":"1.0.0","nodes":[{"id":"n","type":"a.b","typeVersion":1}],"script":"rm -rf /"}`},
		{"event trigger without event", Workflow, `{"id":"x","version":"1.0.0","trigger":{"type":"event"},"nodes":[{"id":"n","type":"a.b","typeVersion":1}]}`},
		{"node hook start", Workflow, `{"id":"x","version":"1.0.0","nodes":[{"id":"n","type":"a.b","typeVersion":1,"hooks":{"start":[]}}]}`},
		{"inline secret in storage", ConnectorConfig, `{"storages":{"archive":{"driver":"gcs","token":"abc"},"master":{"driver":"gcs"},"proxy":{"driver":"gcs"}}}`},
		{"inline secret in parameters", ConnectorConfig, `{"observability":{"logs":{"sinks":[{"id":"x","driver":"loki","enabled":true,"parameters":{"api_key":"abc"}}]}}}`},
		{"missing storage role", ConnectorConfig, `{"storages":{"master":{"driver":"gcs"}}}`},
		{"plugin without permissions", PluginManifest, `{"id":"a","version":"1.0.0","runtime":"process","provides":{}}`},
		{"plugin native runtime", PluginManifest, `{"id":"a","version":"1.0.0","runtime":"so","provides":{},"permissions":{}}`},
		{"profile bad codec", MediaProfile, `{"id":"p","version":1,"audio":{"codec":"mp3","bitrate":"96k"},"container":"mp4"}`},
	}
	for _, c := range cases {
		if err := Validate(c.schema, []byte(c.doc)); err == nil {
			t.Errorf("%s: expected a validation error", c.name)
		}
	}
}

func TestStorageExampleIsDisjoint(t *testing.T) {
	var s config.Storages
	if err := json.Unmarshal(readTestdata(t, "config-storages-7.2.json"), &s); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStorages(s); err != nil {
		t.Fatal(err)
	}
	if s.Roles[domain.RoleMaster].Prefix != "masters/" {
		t.Fatalf("config types do not match the file: %+v", s)
	}
}

func TestPublishingConfigShape(t *testing.T) {
	var f config.PublishingFile
	if err := json.Unmarshal(readTestdata(t, "config-publishing-7.3.json"), &f); err != nil {
		t.Fatal(err)
	}
	if f.Publishing.Destinations["youtube-main"].Driver != "youtube" || !f.Publishing.Defaults.RequireApproval {
		t.Fatalf("config types do not match the file: %+v", f)
	}
}

func TestWorkflowModelMatchesSpecExample(t *testing.T) {
	def, err := wfdef.Parse(readTestdata(t, "workflow-spec-6.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if def.ID != "ingest-original" || len(def.Nodes) != 7 || len(def.Connections["register"]) != 2 {
		t.Fatalf("model mismatch: %+v", def)
	}
	if !def.Inputs["file"].Required || def.Inputs["file"].Type != wfdef.InputFile {
		t.Fatalf("inputs mismatch: %+v", def.Inputs)
	}
}

func TestCatalogIsConsistent(t *testing.T) {
	cat, err := NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Nodes) == 0 {
		t.Fatal("empty catalog")
	}
	owners := map[string]bool{"engine": true, "media": true, "ai": true, "publishing": true}
	seen := map[string]bool{}
	for _, e := range cat.Nodes {
		key := e.Type
		if seen[key] {
			t.Errorf("%s: duplicate", key)
		}
		seen[key] = true
		if !owners[e.Owner] {
			t.Errorf("%s: unknown owner %q", e.Type, e.Owner)
		}
		switch e.Pool {
		case node.PoolEngine, node.PoolDaemon, node.PoolMedia, node.PoolAI:
		default:
			t.Errorf("%s: bad pool %q", e.Type, e.Pool)
		}
		switch e.SideEffect {
		case node.EffectNone, node.EffectIdempotent, node.EffectReconcilable, node.EffectUnsafe:
		default:
			t.Errorf("%s: bad side effect %q", e.Type, e.SideEffect)
		}
		if e.Title == "" {
			t.Errorf("%s: no title", e.Type)
		}
		if _, err := Compile(e.ParametersRef()); err != nil {
			t.Errorf("%s parameters: %v", e.Type, err)
		}
		if _, err := Compile(e.OutputRef()); err != nil {
			t.Errorf("%s output: %v", e.Type, err)
		}
	}
}

// The spec's examples and §3 require these types to exist.
func TestCatalogHasSpecNodes(t *testing.T) {
	cat, err := NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"workflow-spec-6.3.json", "workflow-spec-6.4.json", "workflow-spec-8.2.json"} {
		def, err := wfdef.Parse(readTestdata(t, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range def.Nodes {
			if _, ok := cat.Lookup(n.Type, n.TypeVersion); !ok {
				t.Errorf("%s uses %s@%d, which the catalog lacks", file, n.Type, n.TypeVersion)
			}
		}
	}
	for _, typ := range []string{
		"flow.if", "flow.foreach", "flow.wait", "flow.merge", "workflow.execute", "approval.request",
		"storage.put", "storage.get", "storage.verify", "video.transcode", "video.cut", "audio.extract",
		"ai.transcribe", "ai.index", "catalog.mark_ready", "publish.withdraw",
	} {
		if _, ok := cat.Lookup(typ, 1); !ok {
			t.Errorf("catalog lacks %s", typ)
		}
	}
}

func TestCatalogSafetyDeclarations(t *testing.T) {
	cat, err := NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	upload, _ := cat.Lookup("publish.upload", 1)
	if upload.SideEffect != node.EffectReconcilable {
		t.Errorf("publish.upload must be reconcilable (D11), got %s", upload.SideEffect)
	}
	// Daemon-only nodes must never land on a worker pool (D5).
	for _, typ := range []string{"catalog.get", "catalog.register", "catalog.mark_ready", "ai.index", "ai.find_segment", "publish.upload", "catalog.public.sync"} {
		e, ok := cat.Lookup(typ, 1)
		if !ok {
			t.Errorf("catalog lacks %s", typ)
			continue
		}
		if e.Pool != node.PoolDaemon {
			t.Errorf("%s must run in the daemon pool, got %s", typ, e.Pool)
		}
	}
	// Scratch files are only reachable from worker pools (I5).
	for _, typ := range []string{"video.cut", "storage.put", "audio.extract"} {
		if e, _ := cat.Lookup(typ, 1); e.Pool != node.PoolMedia {
			t.Errorf("%s must run in the media pool, got %s", typ, e.Pool)
		}
	}
}

func TestParameterSchemasApply(t *testing.T) {
	cat, err := NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	put, _ := cat.Lookup("storage.put", 1)
	good := `{"storage":"archive","source":"upload:UPL-001","key":"originals/ORI-001/v1/original.mov","verifyChecksum":true}`
	if err := Validate(put.ParametersRef(), []byte(good)); err != nil {
		t.Errorf("valid parameters rejected: %v", err)
	}
	for _, bad := range []string{
		`{"storage":"cold","source":"x","key":"k"}`,
		`{"storage":"master","source":"x"}`,
		`{"storage":"master","source":"x","key":"k","extra":1}`,
	} {
		if err := Validate(put.ParametersRef(), []byte(bad)); err == nil {
			t.Errorf("invalid parameters accepted: %s", bad)
		}
	}
	exec, _ := cat.Lookup("workflow.execute", 1)
	if err := Validate(exec.ParametersRef(), []byte(`{"workflow":"ingest-derived","file":"scratch://x","kind":"clip"}`)); err != nil {
		t.Errorf("workflow.execute passes extra parameters as child inputs (§6.4): %v", err)
	}
}

func TestDescriptorHelpers(t *testing.T) {
	d, err := DescriptorFor("video.cut", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDescriptor(d); err != nil {
		t.Fatal(err)
	}
	d.Pool = node.PoolDaemon
	if err := CheckDescriptor(d); err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("pool mismatch not reported: %v", err)
	}
	d, _ = DescriptorFor("video.cut", 1)
	d.SideEffect = node.EffectNone
	if err := CheckDescriptor(d); err == nil {
		t.Fatal("side-effect mismatch not reported")
	}
	if _, err := DescriptorFor("nope.nope", 1); err == nil {
		t.Fatal("unknown type must fail")
	}
}

func TestFakeEmbedTestVector(t *testing.T) {
	data, err := Read(AIWorkerRPC)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs struct {
			Fake struct {
				Dimensions  int `json:"dimensions"`
				TestVectors []struct {
					Text    string             `json:"text"`
					Buckets map[string]float64 `json:"nonZeroBuckets"`
				} `json:"testVectors"`
			} `json:"fakeHashEmbed"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Defs.Fake.Dimensions != aiworker.FakeEmbeddingDimensions {
		t.Fatalf("dimensions = %d", doc.Defs.Fake.Dimensions)
	}
	if len(doc.Defs.Fake.TestVectors) == 0 {
		t.Fatal("no test vectors")
	}
	for _, tv := range doc.Defs.Fake.TestVectors {
		got := map[string]float64{}
		for i, v := range aiworker.FakeHashEmbed(tv.Text) {
			if v != 0 {
				raw, _ := json.Marshal(v)
				var f float64
				_ = json.Unmarshal(raw, &f)
				got[itoa(i)] = f
			}
		}
		if len(got) != len(tv.Buckets) {
			t.Errorf("%q: got %d buckets, want %d: %v", tv.Text, len(got), len(tv.Buckets), got)
			continue
		}
		for k, want := range tv.Buckets {
			if abs(got[k]-want) > 1e-6 {
				t.Errorf("%q bucket %s = %f, want %f", tv.Text, k, got[k], want)
			}
		}
	}
}

func itoa(i int) string {
	raw, _ := json.Marshal(i)
	return string(raw)
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func TestPublicDBSchemaEmbedded(t *testing.T) {
	sql, err := PublicDBSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE TABLE videos", "search_fts", "embeddings", "meta"} {
		if !strings.Contains(sql, want) {
			t.Errorf("schema lacks %q", want)
		}
	}
}
