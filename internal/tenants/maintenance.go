package tenants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// IdempotencyRetention is how long the API remembers a mutation (I118).
const IdempotencyRetention = 7 * 24 * time.Hour

// AuditMaintainer is what the sweep needs from the audit. *audit.Auditor satisfies it.
type AuditMaintainer interface {
	Export(ctx context.Context, tenant domain.TenantID) (string, error)
	Prune(ctx context.Context, tenant domain.TenantID, retentionDays int) (int64, error)
}

// Maintenance runs the daily sweeps (I45, I118): job records past retention with their
// nodes and events, dispatched outbox events, API idempotency records, the daily audit
// export (D23) and the audit prune. Logs live in the file sink's JSONL, whose rotation
// belongs to extensibility; expired uploads belong to api, which owns the spool.
type Maintenance struct {
	stores    repo.TenantStores
	audit     AuditMaintainer
	retention config.Retention
	clock     func() time.Time
}

// MaintenanceOptions configures the sweeps.
type MaintenanceOptions struct {
	// Audit exports and prunes the chain. Nil skips both.
	Audit AuditMaintainer
	// Retention holds the periods; zero values mean the defaults (§10.4).
	Retention config.Retention
	// Clock is used for cutoffs. Nil means time.Now.
	Clock func() time.Time
}

// NewMaintenance returns the sweeps.
func NewMaintenance(stores repo.TenantStores, opts MaintenanceOptions) *Maintenance {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Maintenance{stores: stores, audit: opts.Audit, retention: opts.Retention, clock: opts.Clock}
}

// Run sweeps at start and then every 24 h until ctx ends. A failed sweep waits for the
// next tick; report receives each outcome and may be nil.
func (m *Maintenance) Run(ctx context.Context, report func(error)) {
	sweep := func() {
		err := m.RunOnce(ctx)
		if report != nil {
			report(err)
		}
	}
	sweep()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// RunOnce sweeps every tenant. One tenant's failure does not stop the others.
func (m *Maintenance) RunOnce(ctx context.Context) error {
	tenants, err := m.stores.Tenants(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, tenant := range tenants {
		if err := m.sweepTenant(ctx, tenant); err != nil {
			failures = append(failures, fmt.Errorf("tenant %s: %w", tenant, err))
		}
	}
	return errors.Join(failures...)
}

func daysOr(days, fallback int) int {
	if days > 0 {
		return days
	}
	return fallback
}

func (m *Maintenance) sweepTenant(ctx context.Context, tenant domain.TenantID) error {
	store, err := m.stores.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	now := m.clock()
	var failures []error
	jobCutoff := now.AddDate(0, 0, -daysOr(m.retention.JobDays, config.DefaultJobDays))
	if _, err := store.Jobs().DeleteFinishedBefore(ctx, jobCutoff); err != nil {
		failures = append(failures, fmt.Errorf("job retention: %w", err))
	}
	if maintainer, ok := store.Outbox().(repo.OutboxMaintainer); ok {
		if _, err := maintainer.DeleteDispatchedBefore(ctx, jobCutoff); err != nil {
			failures = append(failures, fmt.Errorf("outbox retention: %w", err))
		}
	}
	if _, err := store.Idempotency().DeleteBefore(ctx, now.Add(-IdempotencyRetention)); err != nil {
		failures = append(failures, fmt.Errorf("idempotency retention: %w", err))
	}
	if m.audit != nil {
		if _, err := m.audit.Export(ctx, tenant); err != nil {
			failures = append(failures, fmt.Errorf("audit export: %w", err))
		} else if _, err := m.audit.Prune(ctx, tenant, daysOr(m.retention.AuditDays, config.MinAuditDays)); err != nil {
			failures = append(failures, fmt.Errorf("audit prune: %w", err))
		}
	}
	return errors.Join(failures...)
}
