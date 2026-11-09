package workers

import (
	"context"
	"io"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// Usage operations (I195). Pricing is the daemon's, from its cost table.
const (
	opEgress  = "storage.egress"
	opIngress = "storage.ingress"
)

const bytesPerGB = 1e9

// meter counts bytes per operation and driver for one task.
type meter struct {
	mu      sync.Mutex
	counts  map[meterKey]*atomic.Int64
	metrics telemetry.Metrics
	tenant  domain.TenantID
}

type meterKey struct {
	operation string
	driver    string
	role      domain.StorageRole
}

func newMeter(metrics telemetry.Metrics, tenant domain.TenantID) *meter {
	return &meter{counts: map[meterKey]*atomic.Int64{}, metrics: metrics, tenant: tenant}
}

func (m *meter) add(operation, driver string, role domain.StorageRole, n int64) {
	if n <= 0 {
		return
	}
	key := meterKey{operation, driver, role}
	m.mu.Lock()
	counter, ok := m.counts[key]
	if !ok {
		counter = &atomic.Int64{}
		m.counts[key] = counter
	}
	m.mu.Unlock()
	counter.Add(n)
	direction := "in"
	if operation == opEgress {
		direction = "out"
	}
	m.metrics.Add(telemetry.StorageBytes, float64(n), telemetry.Labels{
		"role": string(role), "driver": driver, "direction": direction, "tenant_id": string(m.tenant),
	})
}

// records renders what was counted as usage, with no price (D19).
func (m *meter) records() []domain.UsageRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	totals := map[[2]string]int64{}
	for key, counter := range m.counts {
		totals[[2]string{key.operation, key.driver}] += counter.Load()
	}
	out := make([]domain.UsageRecord, 0, len(totals))
	for key, n := range totals {
		out = append(out, domain.UsageRecord{Operation: key[0], Provider: key[1], Quantity: float64(n) / bytesPerGB, Unit: "GB"})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Operation != out[b].Operation {
			return out[a].Operation < out[b].Operation
		}
		return out[a].Provider < out[b].Provider
	})
	return out
}

// meteredStorage counts what passes through a storage instance.
type meteredStorage struct {
	connector.Storage
	meter  *meter
	driver string
}

func meterStorage(s connector.Storage, m *meter) *meteredStorage {
	return &meteredStorage{Storage: s, meter: m, driver: s.Info().Driver}
}

// Unwrap returns the driver's instance.
func (m *meteredStorage) Unwrap() connector.Storage { return m.Storage }

func (m *meteredStorage) count(n int) {
	m.meter.add(opEgress, m.driver, m.Storage.Role(), int64(n))
}

func (m *meteredStorage) Get(ctx context.Context, key string) (io.ReadCloser, connector.ObjectInfo, error) {
	rc, info, err := m.Storage.Get(ctx, key)
	if err != nil {
		return nil, info, err
	}
	return countingReader{rc, m.count}, info, nil
}

func (m *meteredStorage) DownloadRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rc, err := m.Storage.DownloadRange(ctx, key, offset, length)
	if err != nil {
		return nil, err
	}
	return countingReader{rc, m.count}, nil
}

// Put passes the source straight through, so a driver can still seek it to resume,
// and counts the object's size once written.
func (m *meteredStorage) Put(ctx context.Context, key string, r io.Reader, opts connector.PutOptions) (connector.ObjectInfo, error) {
	info, err := m.Storage.Put(ctx, key, r, opts)
	if err == nil {
		m.meter.add(opIngress, m.driver, m.Storage.Role(), info.SizeBytes)
	}
	return info, err
}

// CopyFrom forwards to the driver with the source unwrapped, so a server-side copy
// sees its own type. Nothing passes through the worker, so nothing is counted.
func (m *meteredStorage) CopyFrom(ctx context.Context, src connector.Storage, srcKey, dstKey string) (connector.ObjectInfo, error) {
	copier, ok := m.Storage.(connector.RoleCopier)
	if !ok {
		return connector.ObjectInfo{}, errs.New(errs.CodeCapabilityUnsupported, "this driver cannot copy from another instance")
	}
	if unwrapper, ok := src.(interface{ Unwrap() connector.Storage }); ok {
		src = unwrapper.Unwrap()
	}
	return copier.CopyFrom(ctx, src, srcKey, dstKey)
}

type countingReader struct {
	io.ReadCloser
	count func(int)
}

func (r countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.count(n)
	return n, err
}
