package tenants

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/security/audit"
)

// Budgets returns the service of D19.
func (s *Service) Budgets() service.Budgets { return budgetService{s} }

type budgetService struct{ s *Service }

// alertPercent is where budget.threshold_reached fires (D19).
const alertPercent = 80

func period(t time.Time) string { return t.UTC().Format("2006-01") }

// endOfMonth is when a month's budget period closes, in UTC (I116).
func endOfMonth(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}

func status(ctx context.Context, r repo.BudgetRepo, tenant domain.TenantID, now time.Time) (domain.BudgetStatus, error) {
	out := domain.BudgetStatus{Budget: domain.Budget{TenantID: tenant}, Period: period(now)}
	budget, found, err := r.Get(ctx)
	if err != nil {
		return out, err
	}
	if found {
		out.Budget = budget
	}
	out.SpentMinor, err = r.SpentMinor(ctx, out.Period)
	if err != nil {
		return out, err
	}
	until, err := r.Override(ctx)
	if err != nil {
		return out, err
	}
	active := until != nil && now.Before(*until)
	if active {
		out.OverrideUntil = until
	}
	switch {
	case !found:
	case budget.MonthlyMinor > 0:
		out.Percent = float64(out.SpentMinor) / float64(budget.MonthlyMinor) * 100
		out.Blocked = out.SpentMinor >= budget.MonthlyMinor && !active
	default:
		// A stored zero is an admin stopping paid work, not the absence of a budget.
		out.Percent = 100
		out.Blocked = !active
	}
	return out, nil
}

func (b budgetService) Status(ctx context.Context, scope service.Scope) (domain.BudgetStatus, error) {
	store, err := b.s.stores.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return domain.BudgetStatus{}, err
	}
	return status(ctx, store.Budgets(), scope.Tenant, b.s.clock())
}

func (b budgetService) Set(ctx context.Context, scope service.Scope, req api.SetBudgetRequest) (domain.BudgetStatus, error) {
	if req.MonthlyMinor < 0 {
		return domain.BudgetStatus{}, errs.New(errs.CodeInvalidArgument, "a budget cannot be negative")
	}
	if req.Currency == "" {
		return domain.BudgetStatus{}, errs.New(errs.CodeInvalidArgument, "a budget needs a currency")
	}
	store, err := b.s.stores.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return domain.BudgetStatus{}, err
	}
	var out domain.BudgetStatus
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Budgets().Set(ctx, domain.Budget{TenantID: scope.Tenant, MonthlyMinor: req.MonthlyMinor, Currency: req.Currency}); err != nil {
			return err
		}
		if err := audit.AppendTx(ctx, tx.Audit(), security.AuditEntry{
			TenantID: scope.Tenant, Principal: scope.Principal, Action: "budget.set", Permission: domain.PermBudgetManage,
			Resource: "budget", RequestID: scope.RequestID, Details: map[string]any{"monthly_minor": req.MonthlyMinor, "currency": req.Currency},
		}, b.s.clock()); err != nil {
			return err
		}
		out, err = status(ctx, tx.Budgets(), scope.Tenant, b.s.clock())
		return err
	})
	if err != nil {
		return domain.BudgetStatus{}, err
	}
	return out, nil
}

// Override lets paid operations run past 100% until a time, by default the end of the
// month. It needs a reason and is audited (D19).
func (b budgetService) Override(ctx context.Context, scope service.Scope, req api.BudgetOverrideRequest) (domain.BudgetStatus, error) {
	if req.Reason == "" {
		return domain.BudgetStatus{}, errs.New(errs.CodeInvalidArgument, "a budget override needs a reason")
	}
	now := b.s.clock()
	until := endOfMonth(now)
	if req.TTLSeconds > 0 {
		until = now.Add(time.Duration(req.TTLSeconds) * time.Second)
	}
	store, err := b.s.stores.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return domain.BudgetStatus{}, err
	}
	var out domain.BudgetStatus
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Budgets().SetOverride(ctx, until, scope.Principal.Actor(), req.Reason); err != nil {
			return err
		}
		if err := audit.AppendTx(ctx, tx.Audit(), security.AuditEntry{
			TenantID: scope.Tenant, Principal: scope.Principal, Action: "budget.override", Permission: domain.PermBudgetOverride,
			Resource: "budget", RequestID: scope.RequestID, Details: map[string]any{"reason": req.Reason, "until": until.UTC().Format(time.RFC3339)},
		}, now); err != nil {
			return err
		}
		out, err = status(ctx, tx.Budgets(), scope.Tenant, now)
		return err
	})
	if err != nil {
		return domain.BudgetStatus{}, err
	}
	return out, nil
}

// Check is what the engine calls before dispatching a paid node (D19, I33).
func (b budgetService) Check(ctx context.Context, tenant domain.TenantID) error {
	store, err := b.s.stores.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	st, err := status(ctx, store.Budgets(), tenant, b.s.clock())
	if err != nil {
		return err
	}
	if st.Blocked {
		return errs.Newf(errs.CodeBudgetExceeded, "the monthly budget of tenant %s is used up: an admin can override it", tenant).
			With("spent_minor", st.SpentMinor).With("monthly_minor", st.Budget.MonthlyMinor).With("period", st.Period)
	}
	return nil
}

// Record adds usage and fires the threshold events once per crossing. The usage and
// the events are one transaction, so concurrent records cannot both or neither fire
// (I116).
func (b budgetService) Record(ctx context.Context, tenant domain.TenantID, usage []domain.UsageRecord) error {
	if len(usage) == 0 {
		return nil
	}
	store, err := b.s.stores.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	cfg, err := b.costs(ctx, tenant)
	if err != nil {
		return err
	}
	now := b.s.clock()
	var (
		after   int64
		monthly int64
		costed  = make([]domain.UsageRecord, 0, len(usage))
	)
	current := period(now)
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		costed = costed[:0]
		before, err := tx.Budgets().SpentMinor(ctx, current)
		if err != nil {
			return err
		}
		for _, u := range usage {
			u.TenantID = tenant
			if u.At.IsZero() {
				u.At = now
			}
			if u.EstimatedMinor == 0 {
				u.EstimatedMinor = int64(math.Round(u.Quantity * cfg[u.Operation]))
			}
			if err := tx.Budgets().RecordUsage(ctx, u); err != nil {
				return err
			}
			costed = append(costed, u)
		}
		after, err = tx.Budgets().SpentMinor(ctx, current)
		if err != nil {
			return err
		}
		budget, found, err := tx.Budgets().Get(ctx)
		if err != nil || !found || budget.MonthlyMinor <= 0 {
			return err
		}
		monthly = budget.MonthlyMinor
		emit := func(typ event.Type) error {
			payload, err := json.Marshal(map[string]any{
				"period": current, "spent_minor": after, "monthly_minor": budget.MonthlyMinor,
				"currency": budget.Currency, "percent": float64(after) / float64(budget.MonthlyMinor) * 100,
			})
			if err != nil {
				return err
			}
			_, err = tx.Outbox().Append(ctx, event.Envelope{EventID: id.New(), Type: typ, TenantID: tenant, OccurredAt: now, SchemaVersion: event.SchemaVersion, Payload: payload})
			return err
		}
		// Integer math, so 80% of any budget is exact.
		if before*100 < budget.MonthlyMinor*alertPercent && after*100 >= budget.MonthlyMinor*alertPercent {
			if err := emit(event.BudgetThresholdReached); err != nil {
				return err
			}
		}
		if before < budget.MonthlyMinor && after >= budget.MonthlyMinor {
			if err := emit(event.BudgetExceeded); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, u := range costed {
		b.s.metrics.Add(telemetry.UsageCostEstimate, float64(u.EstimatedMinor), telemetry.Labels{"operation": u.Operation, "tenant_id": string(tenant)})
	}
	if monthly > 0 {
		b.s.metrics.Set(telemetry.BudgetPercent, float64(after)/float64(monthly)*100, telemetry.Labels{"tenant_id": string(tenant)})
	}
	return nil
}

// costs reads the estimated cost table. A broken configuration file must not stop a
// node that has already done its paid work from recording it, but must not silently
// price it at zero either: the error goes back to the caller.
func (b budgetService) costs(ctx context.Context, tenant domain.TenantID) (map[string]float64, error) {
	cfg, err := b.s.TenantConfig(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return cfg.Budget.PerUnitMinor, nil
}
