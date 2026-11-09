package prometheus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

func scrape(t *testing.T, e *Exporter, tenant string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if tenant != "" {
		req.Header.Set(api.HeaderTenant, tenant)
	}
	rec := httptest.NewRecorder()
	e.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func point(spec telemetry.MetricSpec, tenant string) connector.MetricPoint {
	labels := map[string]string{}
	for _, l := range spec.Labels {
		labels[l] = "x"
	}
	labels[telemetry.LabelTenant] = tenant
	return connector.MetricPoint{Name: spec.Name, Kind: string(spec.Kind), Value: 2, Labels: labels, At: time.Now()}
}

func noRuntime() Parameters {
	off := false
	return Parameters{RuntimeMetrics: &off}
}

func TestEveryCatalogMetricIsExposedWithTenantLabel(t *testing.T) {
	e := New(noRuntime())
	var batch []connector.MetricPoint
	for _, spec := range telemetry.Catalog {
		batch = append(batch, point(spec, "acme"))
	}
	if err := e.Export(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	text := scrape(t, e, "")
	for _, spec := range telemetry.Catalog {
		if !strings.Contains(text, "# TYPE "+spec.Name+" "+string(spec.Kind)) {
			t.Errorf("%s is not exposed as %s", spec.Name, spec.Kind)
		}
		sample := spec.Name
		if spec.Kind == telemetry.KindHistogram {
			sample += "_count"
		}
		found := false
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, sample+"{") && strings.Contains(line, `tenant_id="acme"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no sample with tenant_id", spec.Name)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "vidyom_") && !strings.Contains(line, `tenant_id="`) {
			t.Errorf("a sample without tenant_id (D6): %s", line)
		}
	}
}

func TestTenantFilterIsolates(t *testing.T) {
	e := New(Parameters{})
	spec, _ := telemetry.Spec(telemetry.BudgetPercent)
	other, _ := telemetry.Spec(telemetry.WorkerCPU)
	if err := e.Export(context.Background(), []connector.MetricPoint{
		point(spec, "acme"), point(spec, "other"), point(other, telemetry.SystemTenant),
	}); err != nil {
		t.Fatal(err)
	}
	acme := scrape(t, e, "acme")
	if !strings.Contains(acme, `tenant_id="acme"`) {
		t.Errorf("tenant lost its own series:\n%s", acme)
	}
	for _, leak := range []string{`tenant_id="other"`, telemetry.SystemTenant, "go_goroutines", "process_"} {
		if strings.Contains(acme, leak) {
			t.Errorf("tenant view leaked %q:\n%s", leak, acme)
		}
	}
	if ghost := scrape(t, e, "ghost"); strings.Contains(ghost, "vidyom_") {
		t.Errorf("an unknown tenant sees nothing:\n%s", ghost)
	}
	all := scrape(t, e, "")
	for _, want := range []string{`tenant_id="acme"`, `tenant_id="other"`, telemetry.SystemTenant, "go_goroutines"} {
		if !strings.Contains(all, want) {
			t.Errorf("the unfiltered view lost %q", want)
		}
	}
}

func TestLabelsAreNormalizedToTheCatalog(t *testing.T) {
	e := New(noRuntime())
	err := e.Export(context.Background(), []connector.MetricPoint{{
		Name:   telemetry.NodeRetries,
		Kind:   string(telemetry.KindCounter),
		Value:  1,
		Labels: map[string]string{"node_type": "video.cut", "job_id": "JOB-001"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := scrape(t, e, "")
	if strings.Contains(text, "JOB-001") {
		t.Errorf("an unknown label must be dropped, it would explode cardinality:\n%s", text)
	}
	if !strings.Contains(text, `tenant_id="_system"`) {
		t.Errorf("a missing tenant becomes the system tenant:\n%s", text)
	}
}

func TestKindsAggregateAndMismatchesAreIgnored(t *testing.T) {
	e := New(noRuntime())
	ctx := context.Background()
	labels := map[string]string{"state": "running", telemetry.LabelTenant: "acme"}
	gauge := func(kind string, v float64) connector.MetricPoint {
		return connector.MetricPoint{Name: telemetry.JobsByState, Kind: kind, Value: v, Labels: labels}
	}
	// Set, then Add on a gauge is meaningful; Observe on it is not.
	if err := e.Export(ctx, []connector.MetricPoint{gauge("gauge", 5), gauge("counter", 2), gauge("histogram", 100)}); err != nil {
		t.Fatal(err)
	}
	if text := scrape(t, e, ""); !strings.Contains(text, `vidyom_jobs{state="running",tenant_id="acme"} 7`) {
		t.Errorf("gauge:\n%s", text)
	}
	// A counter never goes down and cannot be set.
	cLabels := map[string]string{"node_type": "a", telemetry.LabelTenant: "acme"}
	c := func(kind string, v float64) connector.MetricPoint {
		return connector.MetricPoint{Name: telemetry.NodeFailures, Kind: kind, Value: v, Labels: cLabels}
	}
	if err := e.Export(ctx, []connector.MetricPoint{c("counter", 3), c("counter", -1), c("gauge", 100), c("counter", 1)}); err != nil {
		t.Fatal(err)
	}
	if text := scrape(t, e, ""); !strings.Contains(text, `vidyom_node_failures_total{node_type="a",tenant_id="acme"} 4`) {
		t.Errorf("counter:\n%s", text)
	}
	// Histograms use the long buckets.
	h := connector.MetricPoint{Name: telemetry.NodeDuration, Kind: "histogram", Value: 7200, Labels: map[string]string{"node_type": "video.transcode", telemetry.LabelTenant: "acme"}}
	if err := e.Export(ctx, []connector.MetricPoint{h}); err != nil {
		t.Fatal(err)
	}
	text := scrape(t, e, "")
	if !strings.Contains(text, `vidyom_node_duration_seconds_bucket{node_type="video.transcode",tenant_id="acme",le="7200"} 1`) {
		t.Errorf("histogram:\n%s", text)
	}
	if !strings.Contains(text, `vidyom_node_duration_seconds_sum{node_type="video.transcode",tenant_id="acme"} 7200`) {
		t.Errorf("histogram sum:\n%s", text)
	}
}

func TestMetricOutsideTheCatalogIsRegisteredWithTenant(t *testing.T) {
	e := New(noRuntime())
	ctx := context.Background()
	p := connector.MetricPoint{Name: "vidyom_custom_total", Kind: "counter", Value: 1, Labels: map[string]string{"pool": "media"}}
	if err := e.Export(ctx, []connector.MetricPoint{p, p}); err != nil {
		t.Fatal(err)
	}
	if text := scrape(t, e, ""); !strings.Contains(text, `vidyom_custom_total{pool="media",tenant_id="_system"} 2`) {
		t.Errorf("custom:\n%s", text)
	}
	bad := connector.MetricPoint{Name: "not a metric name", Kind: "counter", Value: 1}
	if err := e.Export(ctx, []connector.MetricPoint{bad}); err != nil {
		t.Errorf("an invalid name must be dropped, not fail: %v", err)
	}
}

func TestSeriesCapDrops(t *testing.T) {
	e := New(Parameters{MaxSeries: 2, RuntimeMetrics: noRuntime().RuntimeMetrics})
	var batch []connector.MetricPoint
	for _, node := range []string{"a", "b", "c", "a"} {
		batch = append(batch, connector.MetricPoint{Name: telemetry.NodeRetries, Kind: "counter", Value: 1, Labels: map[string]string{"node_type": node, telemetry.LabelTenant: "acme"}})
	}
	if err := e.Export(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	text := scrape(t, e, "")
	if strings.Contains(text, `node_type="c"`) {
		t.Errorf("series beyond the cap must drop:\n%s", text)
	}
	if !strings.Contains(text, `vidyom_node_retries_total{node_type="a",tenant_id="acme"} 2`) {
		t.Errorf("existing series must keep counting:\n%s", text)
	}
	if !strings.Contains(text, `vidyom_exporter_dropped_total{exporter="prometheus",kind="metrics",tenant_id="_system"} 1`) {
		t.Errorf("a drop must be counted:\n%s", text)
	}
}

func TestFactoryRegisterAndClose(t *testing.T) {
	reg := connector.NewRegistry()
	Register(reg)
	f, err := reg.MetricsExporter(Driver)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(Parameters{MaxSeries: 5})
	exp, err := f(context.Background(), config.Sink{ID: "prometheus", Driver: Driver, Enabled: true, Parameters: params}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := exp.(connector.MetricsHandler); !ok {
		t.Error("must serve /metrics")
	}
	if info := exp.Info(); !info.Available || !info.Capabilities["pull"] || info.Family != connector.FamilyMetricsExporter {
		t.Errorf("info: %+v", info)
	}
	if err := exp.Close(context.Background()); err != nil {
		t.Error(err)
	}
	if _, err := f(context.Background(), config.Sink{Parameters: json.RawMessage(`{"maxSeries":"x"}`)}, nil); err == nil {
		t.Error("invalid parameters must be refused")
	}
	var infos []connector.Info = reg.List()
	if len(infos) != 1 || infos[0].Family != connector.FamilyMetricsExporter {
		t.Errorf("registry: %+v", infos)
	}
}
