// Package connector defines the provider-driver contracts (§7): storage,
// publishing, catalog, public catalog, transcription, embeddings, LLM, and the
// observability exporters. A connector integrates an external service; a node
// (package node) executes a workflow action; a plugin packages extensions.
//
// Settings live outside workflows and carry only credentialsRef (§7.1). Every
// connector declares its version, capabilities and availability. A skeleton
// reports Available false and returns errs.NotImplemented.
package connector

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Family groups connector kinds.
type Family string

const (
	FamilyStorage         Family = "storage"
	FamilyPublishing      Family = "publishing"
	FamilyCatalog         Family = "catalog"
	FamilyPublicCatalog   Family = "public_catalog"
	FamilyTranscription   Family = "transcription"
	FamilyEmbedding       Family = "embedding"
	FamilyLLM             Family = "llm"
	FamilyLogSink         Family = "log_sink"
	FamilyMetricsExporter Family = "metrics_exporter"
	FamilyTraceExporter   Family = "trace_exporter"
)

// Info is what GET /connectors reports for a driver.
type Info struct {
	Family       Family          `json:"family"`
	Driver       string          `json:"driver"`
	Version      string          `json:"version"`
	Available    bool            `json:"available"`
	Reason       string          `json:"reason,omitempty"` // why not available, or the documented gap
	Capabilities map[string]bool `json:"capabilities,omitempty"`
	Limits       map[string]any  `json:"limits,omitempty"`
}

// SecretResolver turns a credentialsRef into its value at the moment of use.
// In the daemon it reads the vault; in a worker it uses the leased connection
// endpoint. Values stay in memory and never reach logs, events or job payloads.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, ref string) ([]byte, error)
}

// Registrar is what a connector package's Register function receives. Plugins
// register through it too.
type Registrar interface {
	RegisterStorage(info Info, f StorageFactory)
	RegisterPublishing(info Info, f PublishingFactory)
	RegisterCatalog(info Info, f CatalogFactory)
	RegisterPublicCatalog(info Info, f PublicCatalogFactory)
	RegisterTranscriber(info Info, f TranscriberFactory)
	RegisterEmbedder(info Info, f EmbedderFactory)
	RegisterLLM(info Info, f LLMFactory)
	RegisterLogSink(info Info, f LogSinkFactory)
	RegisterMetricsExporter(info Info, f MetricsExporterFactory)
	RegisterTraceExporter(info Info, f TraceExporterFactory)
}

// Registry resolves registered drivers. NewRegistry returns the standard one.
type Registry struct {
	mu         sync.RWMutex
	infos      map[string]Info
	storage    map[string]StorageFactory
	publishing map[string]PublishingFactory
	catalog    map[string]CatalogFactory
	public     map[string]PublicCatalogFactory
	transcribe map[string]TranscriberFactory
	embedder   map[string]EmbedderFactory
	llm        map[string]LLMFactory
	logSink    map[string]LogSinkFactory
	metrics    map[string]MetricsExporterFactory
	traces     map[string]TraceExporterFactory
}

var _ Registrar = (*Registry)(nil)

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		infos:      map[string]Info{},
		storage:    map[string]StorageFactory{},
		publishing: map[string]PublishingFactory{},
		catalog:    map[string]CatalogFactory{},
		public:     map[string]PublicCatalogFactory{},
		transcribe: map[string]TranscriberFactory{},
		embedder:   map[string]EmbedderFactory{},
		llm:        map[string]LLMFactory{},
		logSink:    map[string]LogSinkFactory{},
		metrics:    map[string]MetricsExporterFactory{},
		traces:     map[string]TraceExporterFactory{},
	}
}

func put[F any](r *Registry, family Family, m map[string]F, info Info, f F) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := m[info.Driver]; dup {
		panic(fmt.Sprintf("connector: duplicate %s driver %q", family, info.Driver))
	}
	info.Family = family
	m[info.Driver] = f
	r.infos[string(family)+"/"+info.Driver] = info
}

func get[F any](r *Registry, family Family, m map[string]F, driver string) (F, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := m[driver]
	if !ok {
		var zero F
		return zero, errs.Newf(errs.CodeNotFound, "no %s driver named %q", family, driver)
	}
	return f, nil
}

func (r *Registry) RegisterStorage(i Info, f StorageFactory) { put(r, FamilyStorage, r.storage, i, f) }
func (r *Registry) RegisterPublishing(i Info, f PublishingFactory) {
	put(r, FamilyPublishing, r.publishing, i, f)
}
func (r *Registry) RegisterCatalog(i Info, f CatalogFactory) { put(r, FamilyCatalog, r.catalog, i, f) }
func (r *Registry) RegisterPublicCatalog(i Info, f PublicCatalogFactory) {
	put(r, FamilyPublicCatalog, r.public, i, f)
}
func (r *Registry) RegisterTranscriber(i Info, f TranscriberFactory) {
	put(r, FamilyTranscription, r.transcribe, i, f)
}
func (r *Registry) RegisterEmbedder(i Info, f EmbedderFactory) {
	put(r, FamilyEmbedding, r.embedder, i, f)
}
func (r *Registry) RegisterLLM(i Info, f LLMFactory) { put(r, FamilyLLM, r.llm, i, f) }
func (r *Registry) RegisterLogSink(i Info, f LogSinkFactory) {
	put(r, FamilyLogSink, r.logSink, i, f)
}
func (r *Registry) RegisterMetricsExporter(i Info, f MetricsExporterFactory) {
	put(r, FamilyMetricsExporter, r.metrics, i, f)
}
func (r *Registry) RegisterTraceExporter(i Info, f TraceExporterFactory) {
	put(r, FamilyTraceExporter, r.traces, i, f)
}

func (r *Registry) Storage(driver string) (StorageFactory, error) {
	return get(r, FamilyStorage, r.storage, driver)
}
func (r *Registry) Publishing(driver string) (PublishingFactory, error) {
	return get(r, FamilyPublishing, r.publishing, driver)
}
func (r *Registry) Catalog(driver string) (CatalogFactory, error) {
	return get(r, FamilyCatalog, r.catalog, driver)
}
func (r *Registry) PublicCatalog(driver string) (PublicCatalogFactory, error) {
	return get(r, FamilyPublicCatalog, r.public, driver)
}
func (r *Registry) Transcriber(driver string) (TranscriberFactory, error) {
	return get(r, FamilyTranscription, r.transcribe, driver)
}
func (r *Registry) Embedder(driver string) (EmbedderFactory, error) {
	return get(r, FamilyEmbedding, r.embedder, driver)
}
func (r *Registry) LLM(driver string) (LLMFactory, error) {
	return get(r, FamilyLLM, r.llm, driver)
}
func (r *Registry) LogSink(driver string) (LogSinkFactory, error) {
	return get(r, FamilyLogSink, r.logSink, driver)
}
func (r *Registry) MetricsExporter(driver string) (MetricsExporterFactory, error) {
	return get(r, FamilyMetricsExporter, r.metrics, driver)
}
func (r *Registry) TraceExporter(driver string) (TraceExporterFactory, error) {
	return get(r, FamilyTraceExporter, r.traces, driver)
}

// List returns every registered driver's Info, sorted by family and driver.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.infos))
	for _, i := range r.infos {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Family != out[b].Family {
			return out[a].Family < out[b].Family
		}
		return out[a].Driver < out[b].Driver
	})
	return out
}
