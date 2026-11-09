package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/workflow"
)

// TriggerSource lists workflows by trigger type. *workflow.Registry does.
type TriggerSource interface {
	Triggered(t wfdef.TriggerType) []*workflow.Entry
}

// StartRequest asks the engine to start a scheduled job.
type StartRequest struct {
	Tenant         domain.TenantID
	WorkflowID     string
	Version        string
	IdempotencyKey string
}

// JobStarter starts scheduled jobs. Starting with a known key starts nothing and
// succeeds. Component jobs implements it.
type JobStarter interface {
	StartScheduledJob(ctx context.Context, req StartRequest) error
}

// DefaultTick is how often schedules are looked at.
const DefaultTick = 30 * time.Second

// Options configures a Scheduler.
type Options struct {
	Tenants  repo.TenantStores
	Triggers TriggerSource
	Starter  JobStarter
	Workers  repo.WorkerRepo
	Tick     time.Duration
	Clock    func() time.Time
	Logger   *slog.Logger
}

// Scheduler is the worker matcher plus the schedule trigger loop.
type Scheduler struct {
	Matcher *Matcher
	opts    Options
	mu      sync.Mutex
	slots   map[string]int64
}

// New returns a scheduler. Run starts its trigger loop.
func New(opts Options) *Scheduler {
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Scheduler{Matcher: NewMatcher(opts.Workers, opts.Clock), opts: opts, slots: map[string]int64{}}
}

// SetStarter binds the engine after both exist.
func (s *Scheduler) SetStarter(st JobStarter) { s.opts.Starter = st }

// Run looks at schedules until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.opts.Tick)
	defer ticker.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.opts.Logger.Warn("schedule check failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Tick starts what is due, once per slot and tenant (I155).
func (s *Scheduler) Tick(ctx context.Context) error {
	if s.opts.Triggers == nil || s.opts.Starter == nil {
		return nil
	}
	tenants, err := s.opts.Tenants.Tenants(ctx)
	if err != nil {
		return err
	}
	now := s.opts.Clock()
	var first error
	for _, wf := range s.opts.Triggers.Triggered(wfdef.TriggerSchedule) {
		every, err := time.ParseDuration(wf.Definition.Trigger.Every)
		if err != nil || every < workflow.MinScheduleInterval {
			continue
		}
		slot := now.UnixNano() / int64(every)
		for _, tenant := range tenants {
			if err := s.check(ctx, tenant, wf, every, slot, now); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (s *Scheduler) check(ctx context.Context, tenant domain.TenantID, wf *workflow.Entry, every time.Duration, slot int64, now time.Time) error {
	key := string(tenant) + "/" + wf.Definition.ID
	s.mu.Lock()
	last, seen := s.slots[key]
	s.mu.Unlock()
	if seen && last >= slot {
		return nil
	}
	if !seen {
		// First look after a boot: catch up a slot missed while the daemon was down,
		// but only for a workflow that has run before (I155).
		store, err := s.opts.Tenants.ForTenant(ctx, tenant)
		if err != nil {
			return err
		}
		jobs, _, err := store.Jobs().List(ctx, repo.JobFilter{WorkflowID: wf.Definition.ID}, repo.Page{Limit: 1})
		if err != nil {
			return err
		}
		if len(jobs) == 0 || now.Sub(jobs[0].CreatedAt) < every {
			s.remember(key, slot)
			return nil
		}
	}
	err := s.opts.Starter.StartScheduledJob(ctx, StartRequest{
		Tenant: tenant, WorkflowID: wf.Definition.ID, Version: wf.Definition.Version,
		IdempotencyKey: fmt.Sprintf("schedule:%s:%d", wf.Definition.ID, slot),
	})
	if err != nil {
		return err
	}
	s.remember(key, slot)
	return nil
}

func (s *Scheduler) remember(key string, slot int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots[key] = slot
}
