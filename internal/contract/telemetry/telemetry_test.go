package telemetry

import (
	"slices"
	"testing"
)

func TestCatalogCoversEveryMetricWithTenantLabel(t *testing.T) {
	names := []string{
		JobsByState, NodeDuration, WorkflowDuration, PluginDuration, ConnectorDuration,
		ConnectorFailures, NodeRetries, NodeFailures, StorageBytes, StorageStored,
		WorkerCPU, WorkerGPU, WorkerMemory, WorkerDisk, QueueDepth, AICostEstimate,
		AITimeEstimate, UsageCostEstimate, UsageQuantity, BudgetPercent, AssetsNoBackup,
		ClipsInReview, AuthFailures, AuthzDenials, SecurityAnomalies, PluginFailures,
		ExporterFailures, ExporterBacklog, ExporterDropped, ExporterDelay,
	}
	if len(names) != len(Catalog) {
		t.Errorf("catalog has %d entries, this test lists %d", len(Catalog), len(names))
	}
	for _, name := range names {
		spec, ok := Spec(name)
		if !ok {
			t.Errorf("%s is not in the catalog", name)
			continue
		}
		if !slices.Contains(spec.Labels, LabelTenant) {
			t.Errorf("%s has no %s label (D6)", name, LabelTenant)
		}
		if spec.Help == "" {
			t.Errorf("%s has no help text", name)
		}
		switch spec.Kind {
		case KindCounter, KindGauge, KindHistogram:
		default:
			t.Errorf("%s has unknown kind %q", name, spec.Kind)
		}
	}
	if _, ok := Spec("vidyom_nothing"); ok {
		t.Error("an unknown metric must not resolve")
	}
}
