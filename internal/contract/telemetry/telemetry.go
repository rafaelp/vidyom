// Package telemetry is the small metrics facade components use, so none depends
// on an exporter (§7.4). Implemented by component extensibility. Tracing uses
// the OpenTelemetry API directly, and logging uses log/slog.
package telemetry

import "time"

import "sort"

// Labels are metric labels. tenant_id is mandatory on tenant-scoped metrics (D6).
type Labels map[string]string

// LabelTenant is the tenant label every metric carries (§10.3, D6).
const LabelTenant = "tenant_id"

// SystemTenant is the tenant_id of a measurement that belongs to no tenant, such
// as a worker's CPU. The leading underscore cannot collide with a slug (I16). An
// empty label would be the same as an absent one to Prometheus (I277).
const SystemTenant = "_system"

// Metrics records measurements.
type Metrics interface {
	Add(name string, delta float64, labels Labels)
	Set(name string, value float64, labels Labels)
	Observe(name string, value float64, labels Labels)
}

// Noop discards measurements, for tests and unwired components.
type Noop struct{}

func (Noop) Add(string, float64, Labels)     {}
func (Noop) Set(string, float64, Labels)     {}
func (Noop) Observe(string, float64, Labels) {}

// Metric names of §10.3. Durations are seconds, sizes are bytes.
const (
	JobsByState       = "vidyom_jobs"                  // gauge; state, tenant_id
	NodeDuration      = "vidyom_node_duration_seconds" // histogram; node_type, tenant_id
	WorkflowDuration  = "vidyom_workflow_duration_seconds"
	PluginDuration    = "vidyom_plugin_duration_seconds"
	ConnectorDuration = "vidyom_connector_duration_seconds" // family, driver
	ConnectorFailures = "vidyom_connector_failures_total"
	NodeRetries       = "vidyom_node_retries_total"
	NodeFailures      = "vidyom_node_failures_total"
	StorageBytes      = "vidyom_storage_transfer_bytes_total" // role, driver, direction
	WorkerCPU         = "vidyom_worker_cpu_cores"
	WorkerGPU         = "vidyom_worker_gpu"
	WorkerMemory      = "vidyom_worker_memory_bytes"
	WorkerDisk        = "vidyom_worker_disk_free_bytes"
	QueueDepth        = "vidyom_queue_depth" // pool
	AICostEstimate    = "vidyom_ai_cost_estimate_minor_total"
	UsageCostEstimate = "vidyom_usage_cost_estimate_minor_total" // operation, tenant_id
	BudgetPercent     = "vidyom_budget_consumption_percent"
	AssetsNoBackup    = "vidyom_assets_without_verified_backup"
	ClipsInReview     = "vidyom_clips_in_editorial_review"
	AuthFailures      = "vidyom_auth_failures_total"
	AuthzDenials      = "vidyom_authz_denials_total"
	ExporterFailures  = "vidyom_exporter_failures_total" // exporter
	ExporterBacklog   = "vidyom_exporter_backlog"
	ExporterDropped   = "vidyom_exporter_dropped_total"
	ExporterDelay     = "vidyom_exporter_delay_seconds"

	// Items of §10.3 that the names above did not cover (I277).
	AITimeEstimate    = "vidyom_ai_time_estimate_seconds_total"
	UsageQuantity     = "vidyom_usage_quantity_total" // operation, unit, month, tenant_id
	StorageStored     = "vidyom_storage_stored_bytes" // role, driver
	PluginFailures    = "vidyom_plugin_failures_total"
	SecurityAnomalies = "vidyom_security_anomalies_total" // kind
)

// Kind says how a metric aggregates. Add goes with KindCounter, Set with KindGauge
// and Observe with KindHistogram.
type Kind string

const (
	KindCounter   Kind = "counter"
	KindGauge     Kind = "gauge"
	KindHistogram Kind = "histogram"
)

// MetricSpec is one metric's contract. Labels always include LabelTenant.
type MetricSpec struct {
	Name    string
	Kind    Kind
	Help    string
	Labels  []string
	Buckets []float64 // histograms only; nil means DurationBuckets
}

// DurationBuckets span a quick hook to a long transcode, in seconds.
var DurationBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600, 7200, 14400}

// Catalog lists every metric of §10.3. Exporters use it for fixed label sets.
var Catalog = []MetricSpec{
	{Name: JobsByState, Kind: KindGauge, Help: "Jobs by state.", Labels: []string{"state", LabelTenant}},
	{Name: NodeDuration, Kind: KindHistogram, Help: "Node execution time.", Labels: []string{"node_type", LabelTenant}},
	{Name: WorkflowDuration, Kind: KindHistogram, Help: "Workflow execution time.", Labels: []string{"workflow", LabelTenant}},
	{Name: PluginDuration, Kind: KindHistogram, Help: "Plugin call time.", Labels: []string{"plugin", "method", LabelTenant}},
	{Name: ConnectorDuration, Kind: KindHistogram, Help: "Connector call time.", Labels: []string{"family", "driver", LabelTenant}},
	{Name: ConnectorFailures, Kind: KindCounter, Help: "Connector call failures.", Labels: []string{"family", "driver", LabelTenant}},
	{Name: NodeRetries, Kind: KindCounter, Help: "Node retries.", Labels: []string{"node_type", LabelTenant}},
	{Name: NodeFailures, Kind: KindCounter, Help: "Node failures.", Labels: []string{"node_type", LabelTenant}},
	{Name: StorageBytes, Kind: KindCounter, Help: "Bytes moved per logical role and provider.", Labels: []string{"role", "driver", "direction", LabelTenant}},
	{Name: StorageStored, Kind: KindGauge, Help: "Bytes stored per logical role and provider.", Labels: []string{"role", "driver", LabelTenant}},
	{Name: WorkerCPU, Kind: KindGauge, Help: "CPU cores of a worker.", Labels: []string{"worker", LabelTenant}},
	{Name: WorkerGPU, Kind: KindGauge, Help: "GPUs of a worker.", Labels: []string{"worker", LabelTenant}},
	{Name: WorkerMemory, Kind: KindGauge, Help: "Memory of a worker, in bytes.", Labels: []string{"worker", "kind", LabelTenant}},
	{Name: WorkerDisk, Kind: KindGauge, Help: "Free disk of a worker, in bytes.", Labels: []string{"worker", "path", LabelTenant}},
	{Name: QueueDepth, Kind: KindGauge, Help: "Tasks waiting per pool.", Labels: []string{"pool", LabelTenant}},
	{Name: AICostEstimate, Kind: KindCounter, Help: "Estimated AI cost, in minor units.", Labels: []string{"operation", LabelTenant}},
	{Name: AITimeEstimate, Kind: KindCounter, Help: "Estimated AI processing time.", Labels: []string{"operation", LabelTenant}},
	{Name: UsageCostEstimate, Kind: KindCounter, Help: "Estimated cost per operation and month, in minor units.", Labels: []string{"operation", "month", LabelTenant}},
	{Name: UsageQuantity, Kind: KindCounter, Help: "Usage per operation and month.", Labels: []string{"operation", "unit", "month", LabelTenant}},
	{Name: BudgetPercent, Kind: KindGauge, Help: "Budget consumption, in percent.", Labels: []string{LabelTenant}},
	{Name: AssetsNoBackup, Kind: KindGauge, Help: "Originals and derivatives without a verified backup.", Labels: []string{LabelTenant}},
	{Name: ClipsInReview, Kind: KindGauge, Help: "Clips awaiting editorial review.", Labels: []string{LabelTenant}},
	{Name: AuthFailures, Kind: KindCounter, Help: "Authentication failures.", Labels: []string{"reason", LabelTenant}},
	{Name: AuthzDenials, Kind: KindCounter, Help: "Authorization denials.", Labels: []string{"permission", LabelTenant}},
	{Name: SecurityAnomalies, Kind: KindCounter, Help: "Security anomalies.", Labels: []string{"kind", LabelTenant}},
	{Name: PluginFailures, Kind: KindCounter, Help: "Plugin call failures.", Labels: []string{"plugin", "reason", LabelTenant}},
	{Name: ExporterFailures, Kind: KindCounter, Help: "Failed writes to a telemetry exporter.", Labels: []string{"exporter", "kind", LabelTenant}},
	{Name: ExporterBacklog, Kind: KindGauge, Help: "Entries waiting for a telemetry exporter.", Labels: []string{"exporter", "kind", LabelTenant}},
	{Name: ExporterDropped, Kind: KindCounter, Help: "Entries dropped for a telemetry exporter.", Labels: []string{"exporter", "kind", LabelTenant}},
	{Name: ExporterDelay, Kind: KindGauge, Help: "Delivery delay of a telemetry exporter's last batch.", Labels: []string{"exporter", "kind", LabelTenant}},
}

// Spec returns a metric's entry in Catalog.
func Spec(name string) (MetricSpec, bool) {
	i := sort.Search(len(catalogIndex), func(i int) bool { return catalogIndex[i].Name >= name })
	if i < len(catalogIndex) && catalogIndex[i].Name == name {
		return catalogIndex[i], true
	}
	return MetricSpec{}, false
}

var catalogIndex = func() []MetricSpec {
	out := append([]MetricSpec(nil), Catalog...)
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}()

// ObserveConnector records one connector call: its duration and, when it failed, a
// failure (§10.3). Family is a connector family such as storage or publishing.
func ObserveConnector(m Metrics, family, driver, tenant string, start time.Time, err error) {
	labels := Labels{"family": family, "driver": driver, LabelTenant: tenant}
	m.Observe(ConnectorDuration, time.Since(start).Seconds(), labels)
	if err != nil {
		m.Add(ConnectorFailures, 1, labels)
	}
}
