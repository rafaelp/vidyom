package prometheus

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// Driver is this connector's name in observability.json.
const Driver = "prometheus"

const (
	version          = "1.0.0"
	defaultMaxSeries = 10000
)

// Parameters is the exporter's `parameters` object.
type Parameters struct {
	// MaxSeries caps the series per metric, against accidental cardinality. Zero means
	// 10 000.
	MaxSeries int `json:"maxSeries,omitempty"`
	// RuntimeMetrics adds Go and process collectors. Default true.
	RuntimeMetrics *bool `json:"runtimeMetrics,omitempty"`
}

func info() connector.Info {
	return connector.Info{
		Family:       connector.FamilyMetricsExporter,
		Driver:       Driver,
		Version:      version,
		Available:    true,
		Capabilities: map[string]bool{"pull": true},
	}
}

// Register adds the driver.
func Register(r connector.Registrar) {
	r.RegisterMetricsExporter(info(), Factory)
}

// Factory opens an exporter from its configuration entry.
func Factory(_ context.Context, cfg config.Sink, _ connector.SecretResolver) (connector.MetricsExporter, error) {
	var params Parameters
	if len(cfg.Parameters) > 0 {
		if err := json.Unmarshal(cfg.Parameters, &params); err != nil {
			return nil, errs.Wrap(errs.CodeValidationFailed, err, "invalid prometheus parameters")
		}
	}
	return New(params), nil
}

type metric struct {
	spec      telemetry.MetricSpec
	counter   *prom.CounterVec
	gauge     *prom.GaugeVec
	histogram *prom.HistogramVec
	series    map[string]struct{}
}

// Exporter holds its own registry, so tests and instances share no global state.
type Exporter struct {
	reg       *prom.Registry
	maxSeries int
	handler   http.Handler

	mu      sync.Mutex
	metrics map[string]*metric
}

var (
	_ connector.MetricsExporter = (*Exporter)(nil)
	_ connector.MetricsHandler  = (*Exporter)(nil)
)

// New returns an exporter with every metric of the catalog registered.
func New(params Parameters) *Exporter {
	maxSeries := params.MaxSeries
	if maxSeries <= 0 {
		maxSeries = defaultMaxSeries
	}
	e := &Exporter{
		reg:       prom.NewRegistry(),
		maxSeries: maxSeries,
		metrics:   map[string]*metric{},
	}
	if params.RuntimeMetrics == nil || *params.RuntimeMetrics {
		e.reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	for _, spec := range telemetry.Catalog {
		// The catalog is a compile-time constant with unique names; a clash is a bug.
		if _, err := e.register(spec); err != nil {
			panic("prometheus: " + err.Error())
		}
	}
	e.handler = e.buildHandler()
	return e
}

func (e *Exporter) register(spec telemetry.MetricSpec) (*metric, error) {
	m := &metric{spec: spec, series: map[string]struct{}{}}
	var collector prom.Collector
	switch spec.Kind {
	case telemetry.KindCounter:
		m.counter = prom.NewCounterVec(prom.CounterOpts{Name: spec.Name, Help: spec.Help}, spec.Labels)
		collector = m.counter
	case telemetry.KindHistogram:
		buckets := spec.Buckets
		if buckets == nil {
			buckets = telemetry.DurationBuckets
		}
		m.histogram = prom.NewHistogramVec(prom.HistogramOpts{Name: spec.Name, Help: spec.Help, Buckets: buckets}, spec.Labels)
		collector = m.histogram
	default:
		m.gauge = prom.NewGaugeVec(prom.GaugeOpts{Name: spec.Name, Help: spec.Help}, spec.Labels)
		collector = m.gauge
	}
	if err := e.reg.Register(collector); err != nil {
		return nil, err
	}
	e.metrics[spec.Name] = m
	return m, nil
}

// lookup returns the metric, registering one outside the catalog on first use with
// the labels it arrived with and tenant_id added (I277).
func (e *Exporter) lookup(p connector.MetricPoint) *metric {
	if m, ok := e.metrics[p.Name]; ok {
		return m
	}
	labels := make([]string, 0, len(p.Labels)+1)
	hasTenant := false
	for k := range p.Labels {
		labels = append(labels, k)
		hasTenant = hasTenant || k == telemetry.LabelTenant
	}
	if !hasTenant {
		labels = append(labels, telemetry.LabelTenant)
	}
	sort.Strings(labels)
	spec := telemetry.MetricSpec{Name: p.Name, Kind: telemetry.Kind(p.Kind), Help: "Not in the metric catalog.", Labels: labels}
	m, err := e.register(spec)
	if err != nil {
		return nil // an invalid name; the point is dropped
	}
	return m
}

func (e *Exporter) values(m *metric, p connector.MetricPoint) ([]string, bool) {
	values := make([]string, len(m.spec.Labels))
	var key strings.Builder
	for i, name := range m.spec.Labels {
		v := p.Labels[name]
		if name == telemetry.LabelTenant && v == "" {
			v = telemetry.SystemTenant
		}
		values[i] = v
		key.WriteString(strconv.Quote(v))
	}
	k := key.String()
	if _, ok := m.series[k]; !ok {
		if len(m.series) >= e.maxSeries {
			e.metrics[telemetry.ExporterDropped].counter.WithLabelValues(Driver, "metrics", telemetry.SystemTenant).Inc()
			return nil, false
		}
		m.series[k] = struct{}{}
	}
	return values, true
}

// Export applies points. A mismatched kind or an over-limit series is dropped. It
// never fails: the registry is in memory.
func (e *Exporter) Export(_ context.Context, batch []connector.MetricPoint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range batch {
		m := e.lookup(p)
		if m == nil {
			continue
		}
		values, ok := e.values(m, p)
		if !ok {
			continue
		}
		kind := telemetry.Kind(p.Kind)
		switch {
		case m.counter != nil && kind == telemetry.KindCounter && p.Value >= 0:
			m.counter.WithLabelValues(values...).Add(p.Value)
		case m.gauge != nil && kind == telemetry.KindGauge:
			m.gauge.WithLabelValues(values...).Set(p.Value)
		case m.gauge != nil && kind == telemetry.KindCounter:
			m.gauge.WithLabelValues(values...).Add(p.Value)
		case m.histogram != nil && kind == telemetry.KindHistogram:
			m.histogram.WithLabelValues(values...).Observe(p.Value)
		}
	}
	return nil
}

// Info implements connector.MetricsExporter.
func (e *Exporter) Info() connector.Info { return info() }

// Close implements connector.MetricsExporter. The registry just goes away.
func (e *Exporter) Close(context.Context) error { return nil }

// Handler implements connector.MetricsHandler.
func (e *Exporter) Handler() http.Handler { return e.handler }

func (e *Exporter) buildHandler() http.Handler {
	all := promhttp.HandlerFor(e.reg, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Header.Get(api.HeaderTenant)
		if tenant == "" {
			all.ServeHTTP(w, r)
			return
		}
		promhttp.HandlerFor(tenantGatherer{gatherer: e.reg, tenant: tenant}, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	})
}

// tenantGatherer keeps only series of one tenant. Families with no tenant_id, such
// as Go runtime metrics, are not tenant data and are left out too.
type tenantGatherer struct {
	gatherer prom.Gatherer
	tenant   string
}

func (g tenantGatherer) Gather() ([]*dto.MetricFamily, error) {
	families, err := g.gatherer.Gather()
	if err != nil {
		return nil, err
	}
	out := families[:0]
	for _, family := range families {
		kept := family.Metric[:0]
		for _, m := range family.Metric {
			for _, label := range m.Label {
				if label.GetName() == telemetry.LabelTenant && label.GetValue() == g.tenant {
					kept = append(kept, m)
					break
				}
			}
		}
		if len(kept) > 0 {
			family.Metric = kept
			out = append(out, family)
		}
	}
	return out, nil
}
