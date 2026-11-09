// Package telemetry is the small metrics facade components use, so none depends
// on an exporter (§7.4). Implemented by component extensibility. Tracing uses
// the OpenTelemetry API directly, and logging uses log/slog.
package telemetry

// Labels are metric labels. tenant_id is mandatory on tenant-scoped metrics (D6).
type Labels map[string]string

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
)
