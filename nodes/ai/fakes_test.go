package ainodes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelp/vidyom/connectors/ai/openai"
	"github.com/rafaelp/vidyom/connectors/ai/openai/mockllm"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/retrieval/retrievaltest"
	"github.com/rafaelp/vidyom/schemas"
)

const (
	testTenant = domain.TenantID("acme")
	testJob    = id.ULID("01J00000000000000000000000")
)

// baseEnv implements node.Env.
type baseEnv struct {
	t        *testing.T
	logs     *bytes.Buffer
	scratch  string
	inputs   map[string]node.LocalInput
	forbid   [][]domain.StorageRole
	progress []float64
	mu       sync.Mutex
}

func newBaseEnv(t *testing.T) *baseEnv {
	t.Helper()
	return &baseEnv{t: t, logs: &bytes.Buffer{}, scratch: t.TempDir(), inputs: map[string]node.LocalInput{}}
}

func (e *baseEnv) Tenant() domain.TenantID { return testTenant }
func (e *baseEnv) JobID() id.Friendly      { return "JOB-001" }
func (e *baseEnv) JobULID() id.ULID        { return testJob }
func (e *baseEnv) NodeID() string          { return "node" }
func (e *baseEnv) TraceID() string         { return "" }
func (e *baseEnv) Logger() *slog.Logger    { return slog.New(slog.NewTextHandler(e.logs, nil)) }
func (e *baseEnv) Progress(percent float64, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.progress = append(e.progress, percent)
}
func (e *baseEnv) Scratch() node.Scratch { return fakeScratch{e.scratch} }
func (e *baseEnv) Storage(context.Context, domain.StorageRole) (connector.Storage, error) {
	return nil, errs.New(errs.CodeInternal, "ai nodes do not open storage")
}
func (e *baseEnv) OpenInput(_ context.Context, ref string, forbid ...domain.StorageRole) (node.LocalInput, error) {
	e.forbid = append(e.forbid, forbid)
	input, ok := e.inputs[ref]
	if !ok {
		return node.LocalInput{}, errs.NotFound("input")
	}
	for _, role := range forbid {
		if input.Role == role {
			return node.LocalInput{}, errs.New(errs.CodeForbiddenStorageRole, "forbidden role")
		}
	}
	return input, nil
}
func (e *baseEnv) HasPermission(domain.Permission) bool { return false }

type fakeScratch struct{ dir string }

func (s fakeScratch) Path(name string) (string, error) { return filepath.Join(s.dir, name), nil }
func (s fakeScratch) URI(name string) string {
	return "scratch://" + string(testJob) + "/node/" + name
}
func (s fakeScratch) Resolve(uri domain.InputURI) (string, error) {
	return filepath.Join(s.dir, uri.File), nil
}

// aiEnv implements node.AIEnv.
type aiEnv struct {
	*baseEnv
	transcriber connector.Transcriber
	embedder    connector.Embedder
}

func (e *aiEnv) Transcriber(context.Context) (connector.Transcriber, error) {
	if e.transcriber == nil {
		return nil, errs.New(errs.CodeUnavailable, "no transcriber")
	}
	return e.transcriber, nil
}
func (e *aiEnv) Embedder(context.Context) (connector.Embedder, error) {
	if e.embedder == nil {
		return nil, errs.New(errs.CodeUnavailable, "no embedder")
	}
	return e.embedder, nil
}

// daemonEnv implements node.DaemonEnv.
type daemonEnv struct {
	*baseEnv
	store    *retrievaltest.Store
	embedder connector.Embedder
	llm      func(domain.Asset) (connector.LLM, error)
	config   config.Tenant
	events   []event.Draft
	asked    []domain.AIProcessing
}

func newDaemonEnv(t *testing.T) *daemonEnv {
	t.Helper()
	return &daemonEnv{baseEnv: newBaseEnv(t), store: retrievaltest.New(testTenant)}
}

func (e *daemonEnv) Store() repo.TenantStore { return e.store }
func (e *daemonEnv) Emit(_ context.Context, _ repo.TenantRepos, d event.Draft) error {
	e.events = append(e.events, d)
	return nil
}
func (e *daemonEnv) Publishing(context.Context, string) (connector.Publishing, error) {
	return nil, errs.New(errs.CodeInternal, "ai nodes do not publish")
}
func (e *daemonEnv) TenantConfig(context.Context) (config.Tenant, error) { return e.config, nil }
func (e *daemonEnv) Embedder(context.Context) (connector.Embedder, error) {
	if e.embedder == nil {
		return nil, errs.New(errs.CodeUnavailable, "no embedder")
	}
	return e.embedder, nil
}
func (e *daemonEnv) LLM(_ context.Context, asset domain.Asset) (connector.LLM, error) {
	e.asked = append(e.asked, asset.AIProcessing)
	if e.llm == nil {
		return nil, errs.New(errs.CodeFailedPrecondition, "no endpoint allowed")
	}
	return e.llm(asset)
}
func (e *daemonEnv) Secrets() connector.SecretResolver { return nil }

// useAllowedEndpoints selects endpoints the way env.LLM must (D8).
func (e *daemonEnv) useAllowedEndpoints(t *testing.T, endpoints ...config.LLMEndpoint) {
	t.Helper()
	e.llm = func(asset domain.Asset) (connector.LLM, error) {
		allowed := config.AllowedEndpoints(endpoints, asset.AIProcessing)
		if len(allowed) == 0 {
			return nil, errs.New(errs.CodeFailedPrecondition, "no endpoint allowed")
		}
		return openai.New(allowed[0], nil, openai.Options{Backoff: 1})
	}
}

// mockEndpoint serves the mock LLM and returns an endpoint for it in the given class.
// The driver requires https for external, so that class gets a stand-in LLM over the
// same mock.
func mockEndpoint(t *testing.T, class config.EndpointClass) (*mockllm.Server, func(domain.Asset) (connector.LLM, error)) {
	t.Helper()
	server := &mockllm.Server{}
	web := httptest.NewServer(server)
	t.Cleanup(web.Close)
	local, err := openai.New(config.LLMEndpoint{Name: "mock-endpoint", BaseURL: web.URL + "/v1", Model: "mock", Class: config.EndpointLocal}, nil, openai.Options{Backoff: 1})
	if err != nil {
		t.Fatal(err)
	}
	return server, func(domain.Asset) (connector.LLM, error) { return reclassified{local, class}, nil }
}

type reclassified struct {
	connector.LLM
	class config.EndpointClass
}

func (r reclassified) Class() config.EndpointClass { return r.class }

// run executes a node after checking its parameters and output against the catalog,
// which is what the engine and workflows rely on.
func run(t *testing.T, n node.Node, env node.Env, attempt int, params any) (node.Result, error) {
	t.Helper()
	descriptor := n.Descriptor()
	catalog, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := catalog.Lookup(descriptor.Type, descriptor.TypeVersion)
	if !ok {
		t.Fatalf("%s is not in the catalog", descriptor.Type)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate(entry.ParametersRef(), raw); err != nil {
		t.Fatalf("%s: parameters do not match the catalog: %v\n%s", descriptor.Type, err, raw)
	}
	result, execErr := n.Execute(context.Background(), env, node.Input{Parameters: raw, Attempt: attempt})
	if execErr == nil {
		if err := schemas.Validate(entry.OutputRef(), result.Output); err != nil {
			t.Fatalf("%s: output does not match the catalog: %v\n%s", descriptor.Type, err, result.Output)
		}
	}
	return result, execErr
}

func decode[T any](t *testing.T, result node.Result) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("decode output: %v\n%s", err, result.Output)
	}
	return out
}

func findNode(t *testing.T, nodeType string) node.Node {
	t.Helper()
	registry := node.NewRegistry()
	Register(registry)
	n, ok := registry.Lookup(nodeType, 1)
	if !ok {
		t.Fatalf("%s is not registered", nodeType)
	}
	return n
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// storyTranscript has 10 s sentences with a story at 100-160 s and a sensitive name at
// 120-130 s.
func storyTranscript() domain.Transcript {
	t := domain.Transcript{Language: "en", Engine: "fake/canned@1", TimestampBasis: "working_master"}
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

func original() domain.Asset {
	return domain.Asset{
		ID:             "ORI-001",
		ULID:           "01J00000000000000000000001",
		TenantID:       testTenant,
		Kind:           domain.KindOriginal,
		Title:          "Hatha Yoga Class",
		AIProcessing:   domain.AILocalOnly,
		CurrentVersion: "v1",
		Status:         domain.AssetReadyForClipping,
	}
}

func allowExternal(a domain.Asset) domain.Asset {
	a.AIProcessing = domain.AIExternalAllowed
	a.AIConsent = &domain.AIConsent{AuthorizedBy: "admin@example.com", Scope: "asset"}
	return a
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
