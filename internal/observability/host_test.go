package observability

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

type recordingMetrics struct {
	telemetry.Noop
	mu   sync.Mutex
	sets map[string][]telemetry.Labels
	last map[string]float64
}

func (r *recordingMetrics) Set(name string, value float64, labels telemetry.Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sets == nil {
		r.sets = map[string][]telemetry.Labels{}
		r.last = map[string]float64{}
	}
	r.sets[name] = append(r.sets[name], labels)
	r.last[name] = value
}

func TestHostSamplerPublishesWorkerMetrics(t *testing.T) {
	m := &recordingMetrics{}
	stop := StartHostSampler(context.Background(), m, HostSamplerOptions{Worker: "w1", GPUs: 2, DiskPaths: []string{t.TempDir(), "/no/such/path"}, Interval: time.Millisecond})
	eventually(t, "two samples", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.sets[telemetry.WorkerCPU]) >= 2
	})
	stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last[telemetry.WorkerCPU] != float64(runtime.NumCPU()) || m.last[telemetry.WorkerGPU] != 2 {
		t.Errorf("values: %v", m.last)
	}
	for _, name := range []string{telemetry.WorkerCPU, telemetry.WorkerGPU, telemetry.WorkerMemory} {
		if len(m.sets[name]) == 0 {
			t.Errorf("%s was not set", name)
			continue
		}
		for _, labels := range m.sets[name] {
			if labels["worker"] != "w1" || labels[telemetry.LabelTenant] != telemetry.SystemTenant {
				t.Errorf("%s labels: %v", name, labels)
			}
		}
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if len(m.sets[telemetry.WorkerDisk]) == 0 {
			t.Error("disk was not measured")
		}
		for _, labels := range m.sets[telemetry.WorkerDisk] {
			if labels["path"] == "/no/such/path" {
				t.Error("a path that cannot be measured is absent, not zero")
			}
		}
		kinds := map[string]bool{}
		for _, labels := range m.sets[telemetry.WorkerMemory] {
			kinds[labels["kind"]] = true
		}
		if !kinds["process"] || !kinds["total"] {
			t.Errorf("memory kinds: %v", kinds)
		}
	}
}

func TestHostSamplerStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := StartHostSampler(ctx, telemetry.Noop{}, HostSamplerOptions{Worker: "w"})
	cancel()
	stop() // returns because the goroutine ended
}
