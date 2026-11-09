package observability

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// HostSamplerOptions configure StartHostSampler.
type HostSamplerOptions struct {
	// Worker names this process in the worker label.
	Worker string
	// GPUs is what the worker declares (D40). It is not detected.
	GPUs int
	// DiskPaths are measured for free space, such as the scratch and cache directories.
	DiskPaths []string
	// Interval between samples. Zero means 15 s.
	Interval time.Duration
}

// StartHostSampler publishes §10.3's worker metrics until ctx ends or stop is called.
// These belong to no tenant, so they carry telemetry.SystemTenant. Where the OS gives
// no answer (total memory and disk on some platforms), that sample is simply absent.
func StartHostSampler(ctx context.Context, m telemetry.Metrics, opts HostSamplerOptions) (stop func()) {
	interval := opts.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sampleHost(m, opts)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				sampleHost(m, opts)
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func sampleHost(m telemetry.Metrics, opts HostSamplerOptions) {
	labels := func(extra ...string) telemetry.Labels {
		out := telemetry.Labels{"worker": opts.Worker, telemetry.LabelTenant: telemetry.SystemTenant}
		for i := 0; i+1 < len(extra); i += 2 {
			out[extra[i]] = extra[i+1]
		}
		return out
	}
	m.Set(telemetry.WorkerCPU, float64(runtime.NumCPU()), labels())
	m.Set(telemetry.WorkerGPU, float64(opts.GPUs), labels())
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	m.Set(telemetry.WorkerMemory, float64(stats.Sys), labels("kind", "process"))
	if total, ok := totalMemory(); ok {
		m.Set(telemetry.WorkerMemory, float64(total), labels("kind", "total"))
	}
	for _, path := range opts.DiskPaths {
		if free, ok := diskFree(path); ok {
			m.Set(telemetry.WorkerDisk, float64(free), labels("path", path))
		}
	}
}
