package persistence

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// outboxRepo is the transactional outbox (§8.2, D5). Append takes part in the caller's
// transaction, which is how a state change and its event commit together.
type outboxRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.OutboxRepo = outboxRepo{}

func (r outboxRepo) Append(ctx context.Context, e event.Envelope) (int64, error) {
	if e.TenantID == "" {
		e.TenantID = r.tenant
	}
	if e.TenantID != r.tenant {
		return 0, invalidArg("event for tenant %q written to the outbox of %q", e.TenantID, r.tenant)
	}
	if e.Type == "" {
		return 0, invalidArg("an event needs a type")
	}
	if e.EventID == "" {
		e.EventID = id.New()
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = event.SchemaVersion
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = r.now()
	}
	doc, err := json.Marshal(e)
	if err != nil {
		return 0, invalidArg("event does not encode: %v", err)
	}
	var seq int64
	err = r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `INSERT INTO outbox (event_id, type, occurred_at, envelope) VALUES (?, ?, ?, ?)`,
			string(e.EventID), string(e.Type), timeText(e.OccurredAt), string(doc))
		if err != nil {
			return mapErr(err)
		}
		seq, err = res.LastInsertId()
		return mapErr(err)
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

func scanOutboxEntry(s scanner) (repo.OutboxEntry, error) {
	var entry repo.OutboxEntry
	err := s.Scan(&entry.Seq, jsonCol{&entry.Event})
	return entry, err
}

func (r outboxRepo) Pending(ctx context.Context, limit int) ([]repo.OutboxEntry, error) {
	if limit <= 0 || limit > maxPageLimit {
		limit = maxPageLimit
	}
	return queryAll(ctx, r.q(), `SELECT seq, envelope FROM outbox WHERE dispatched_at IS NULL ORDER BY seq LIMIT ?`, []any{limit}, scanOutboxEntry)
}

func (r outboxRepo) MarkDispatched(ctx context.Context, seq int64) error {
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `UPDATE outbox SET dispatched_at = COALESCE(dispatched_at, ?) WHERE seq = ?`, timeText(r.now()), seq)
		return requireAffected(res, err, "event")
	})
}

const deliveryColumns = `event_id, subscriber, status, attempts, last_error, next_attempt, delivered_at`

func scanDelivery(s scanner) (event.Delivery, error) {
	var d event.Delivery
	err := s.Scan(&d.EventID, &d.Subscriber, &d.Status, &d.Attempts, &d.LastError, nullTime{&d.NextAttempt}, nullTime{&d.DeliveredAt})
	return d, err
}

func (r outboxRepo) PutDelivery(ctx context.Context, d event.Delivery) error {
	if d.EventID == "" || d.Subscriber == "" {
		return invalidArg("a delivery needs an event and a subscriber")
	}
	if d.Status == "" {
		d.Status = event.DeliveryPending
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO deliveries (`+deliveryColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (event_id, subscriber) DO UPDATE SET status = excluded.status, attempts = excluded.attempts, last_error = excluded.last_error,
				next_attempt = excluded.next_attempt, delivered_at = excluded.delivered_at`,
			string(d.EventID), d.Subscriber, string(d.Status), d.Attempts, d.LastError, timeArg(d.NextAttempt), timeArg(d.DeliveredAt))
		return mapErr(err)
	})
}

// DueDeliveries returns pending deliveries whose time has come, and failed ones that
// have another attempt scheduled. A failed delivery with no next attempt is final.
func (r outboxRepo) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]event.Delivery, error) {
	if limit <= 0 || limit > maxPageLimit {
		limit = maxPageLimit
	}
	return queryAll(ctx, r.q(), `SELECT `+deliveryColumns+` FROM deliveries
		WHERE (status = ? AND (next_attempt IS NULL OR next_attempt <= ?))
		   OR (status = ? AND next_attempt IS NOT NULL AND next_attempt <= ?)
		ORDER BY COALESCE(next_attempt, ''), event_id LIMIT ?`,
		[]any{string(event.DeliveryPending), timeText(now), string(event.DeliveryFailed), timeText(now), limit}, scanDelivery)
}

func (r outboxRepo) GetEvent(ctx context.Context, eventID id.ULID) (event.Envelope, error) {
	var e event.Envelope
	err := r.q().QueryRowContext(ctx, `SELECT envelope FROM outbox WHERE event_id = ?`, string(eventID)).Scan(jsonCol{&e})
	if err != nil {
		return event.Envelope{}, notFoundIfNone(err, "event")
	}
	return e, nil
}

func (r outboxRepo) ListEvents(ctx context.Context, f repo.OutboxFilter, p repo.Page) ([]event.Envelope, string, error) {
	var w where
	if f.Since != nil {
		w.add("occurred_at >= ?", timeText(*f.Since))
	}
	if f.Type != "" {
		w.add("type = ?", string(f.Type))
	}
	return listDesc(ctx, r.q(), `SELECT seq, envelope FROM outbox`, "seq", w, p, func(s scanner) (int64, event.Envelope, error) {
		entry, err := scanOutboxEntry(s)
		return entry.Seq, entry.Event, err
	})
}

func (r outboxRepo) ListDeliveries(ctx context.Context, f repo.DeliveryFilter, p repo.Page) ([]event.Delivery, string, error) {
	var w where
	if f.Status != "" {
		w.add("status = ?", string(f.Status))
	}
	if f.Subscriber != "" {
		w.add("subscriber = ?", f.Subscriber)
	}
	return listDesc(ctx, r.q(), `SELECT rowid, `+deliveryColumns+` FROM deliveries`, "rowid", w, p, func(s scanner) (int64, event.Delivery, error) {
		var (
			k int64
			d event.Delivery
		)
		err := s.Scan(&k, &d.EventID, &d.Subscriber, &d.Status, &d.Attempts, &d.LastError, nullTime{&d.NextAttempt}, nullTime{&d.DeliveredAt})
		return k, d, err
	})
}

func (r outboxRepo) Stream(ctx context.Context, afterSeq int64, limit int) ([]repo.OutboxEntry, error) {
	if limit <= 0 || limit > maxPageLimit {
		limit = maxPageLimit
	}
	return queryAll(ctx, r.q(), `SELECT seq, envelope FROM outbox WHERE seq > ? ORDER BY seq LIMIT ?`, []any{afterSeq, limit}, scanOutboxEntry)
}

// DeleteDispatchedBefore drops dispatched events past retention and their deliveries
// (I118). The sequence keeps growing, so SSE resume stays correct.
func (r outboxRepo) DeleteDispatchedBefore(ctx context.Context, t time.Time) (int64, error) {
	var removed int64
	err := r.write(ctx, func(q querier) error {
		if _, err := q.ExecContext(ctx, `DELETE FROM deliveries WHERE event_id IN (SELECT event_id FROM outbox WHERE dispatched_at IS NOT NULL AND occurred_at < ?)`, timeText(t)); err != nil {
			return mapErr(err)
		}
		res, err := q.ExecContext(ctx, `DELETE FROM outbox WHERE dispatched_at IS NOT NULL AND occurred_at < ?`, timeText(t))
		if err != nil {
			return mapErr(err)
		}
		removed, err = res.RowsAffected()
		return mapErr(err)
	})
	return removed, err
}

// ---- Vault rows ----

type secretRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.SecretRepo = secretRepo{}

const secretMetaColumns = `name, location, created_by, created_at, rotated_at, expires_at, last_used_at`

func (r secretRepo) scanMeta(s scanner) (domain.SecretMeta, error) {
	m := domain.SecretMeta{TenantID: r.tenant}
	err := s.Scan(&m.Name, &m.Location, &m.CreatedBy, reqTime{&m.CreatedAt}, nullTime{&m.RotatedAt}, nullTime{&m.ExpiresAt}, nullTime{&m.LastUsedAt})
	return m, err
}

func (r secretRepo) Put(ctx context.Context, rec repo.SecretRecord) error {
	if rec.Meta.Name == "" || len(rec.Ciphertext) == 0 || rec.KeyVersion <= 0 {
		return invalidArg("a secret record needs a name, ciphertext and a key version")
	}
	if rec.Meta.TenantID != "" && rec.Meta.TenantID != r.tenant {
		return invalidArg("secret for tenant %q written to the database of %q", rec.Meta.TenantID, r.tenant)
	}
	if rec.Meta.CreatedAt.IsZero() {
		rec.Meta.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO secrets (name, location, created_by, created_at, rotated_at, expires_at, last_used_at, ciphertext, key_version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET location = excluded.location, created_by = excluded.created_by, created_at = excluded.created_at,
				rotated_at = excluded.rotated_at, expires_at = excluded.expires_at, last_used_at = excluded.last_used_at,
				ciphertext = excluded.ciphertext, key_version = excluded.key_version`,
			rec.Meta.Name, string(rec.Meta.Location), rec.Meta.CreatedBy, timeText(rec.Meta.CreatedAt), timeArg(rec.Meta.RotatedAt), timeArg(rec.Meta.ExpiresAt), timeArg(rec.Meta.LastUsedAt),
			rec.Ciphertext, rec.KeyVersion)
		return mapErr(err)
	})
}

func (r secretRepo) Get(ctx context.Context, name string) (repo.SecretRecord, error) {
	rec := repo.SecretRecord{Meta: domain.SecretMeta{TenantID: r.tenant}}
	m := &rec.Meta
	err := r.q().QueryRowContext(ctx, `SELECT `+secretMetaColumns+`, ciphertext, key_version FROM secrets WHERE name = ?`, name).
		Scan(&m.Name, &m.Location, &m.CreatedBy, reqTime{&m.CreatedAt}, nullTime{&m.RotatedAt}, nullTime{&m.ExpiresAt}, nullTime{&m.LastUsedAt}, &rec.Ciphertext, &rec.KeyVersion)
	if err != nil {
		return repo.SecretRecord{}, notFoundIfNone(err, "secret")
	}
	return rec, nil
}

func (r secretRepo) List(ctx context.Context) ([]domain.SecretMeta, error) {
	return queryAll(ctx, r.q(), `SELECT `+secretMetaColumns+` FROM secrets ORDER BY name`, nil, r.scanMeta)
}

func (r secretRepo) Delete(ctx context.Context, name string) error {
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name)
		return requireAffected(res, err, "secret")
	})
}

func (r secretRepo) TouchUsed(ctx context.Context, name string, at time.Time) error {
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `UPDATE secrets SET last_used_at = ? WHERE name = ?`, timeText(at), name)
		return requireAffected(res, err, "secret")
	})
}

// ---- Budgets ----

type budgetRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.BudgetRepo = budgetRepo{}

func (r budgetRepo) Get(ctx context.Context) (domain.Budget, bool, error) {
	b := domain.Budget{TenantID: r.tenant}
	var monthly *int64
	var currency *string
	err := r.q().QueryRowContext(ctx, `SELECT monthly_minor, currency FROM budget WHERE id = 1`).Scan(&monthly, &currency)
	if err != nil {
		if isNoRows(err) {
			return b, false, nil
		}
		return b, false, mapErr(err)
	}
	if monthly == nil {
		return b, false, nil
	}
	b.MonthlyMinor = *monthly
	if currency != nil {
		b.Currency = *currency
	}
	return b, true, nil
}

func (r budgetRepo) Set(ctx context.Context, b domain.Budget) error {
	if b.MonthlyMinor < 0 {
		return invalidArg("a budget cannot be negative")
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO budget (id, monthly_minor, currency) VALUES (1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET monthly_minor = excluded.monthly_minor, currency = excluded.currency`, b.MonthlyMinor, b.Currency)
		return mapErr(err)
	})
}

// periodOf is the budget month of a time, in UTC (I116).
func periodOf(t time.Time) string { return t.UTC().Format("2006-01") }

func (r budgetRepo) RecordUsage(ctx context.Context, u domain.UsageRecord) error {
	if u.TenantID != "" && u.TenantID != r.tenant {
		return invalidArg("usage for tenant %q written to the database of %q", u.TenantID, r.tenant)
	}
	if u.Operation == "" {
		return invalidArg("usage needs an operation")
	}
	if u.At.IsZero() {
		u.At = r.now()
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO usage_records (period, operation, provider, quantity, unit, estimated_minor, job_id, node_id, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			periodOf(u.At), u.Operation, u.Provider, u.Quantity, u.Unit, u.EstimatedMinor, u.JobID, u.NodeID, timeText(u.At))
		return mapErr(err)
	})
}

func (r budgetRepo) SpentMinor(ctx context.Context, period string) (int64, error) {
	var spent int64
	err := r.q().QueryRowContext(ctx, `SELECT COALESCE(SUM(estimated_minor), 0) FROM usage_records WHERE period = ?`, period).Scan(&spent)
	return spent, mapErr(err)
}

func (r budgetRepo) ListUsage(ctx context.Context, period string, p repo.Page) ([]domain.UsageRecord, string, error) {
	var w where
	w.add("period = ?", period)
	return listDesc(ctx, r.q(), `SELECT id, operation, provider, quantity, unit, estimated_minor, job_id, node_id, at FROM usage_records`, "id", w, p,
		func(s scanner) (int64, domain.UsageRecord, error) {
			var k int64
			u := domain.UsageRecord{TenantID: r.tenant}
			err := s.Scan(&k, &u.Operation, &u.Provider, &u.Quantity, &u.Unit, &u.EstimatedMinor, &u.JobID, &u.NodeID, reqTime{&u.At})
			return k, u, err
		})
}

func (r budgetRepo) SetOverride(ctx context.Context, until time.Time, by, reason string) error {
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO budget (id, override_until, override_by, override_reason) VALUES (1, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET override_until = excluded.override_until, override_by = excluded.override_by, override_reason = excluded.override_reason`,
			timeText(until), by, reason)
		return mapErr(err)
	})
}

// Override returns the stored override time, whether or not it has passed.
func (r budgetRepo) Override(ctx context.Context) (*time.Time, error) {
	var until *time.Time
	err := r.q().QueryRowContext(ctx, `SELECT override_until FROM budget WHERE id = 1`).Scan(nullTime{&until})
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, mapErr(err)
	}
	return until, nil
}

// ---- API idempotency ----

type idempotencyRepo struct{ conn }

var _ repo.IdempotencyRepo = idempotencyRepo{}

func (r idempotencyRepo) Get(ctx context.Context, key string) (repo.IdempotentResponse, bool, error) {
	v := repo.IdempotentResponse{Key: key}
	err := r.q().QueryRowContext(ctx, `SELECT request_hash, status, body, created_at FROM idempotency_keys WHERE key = ?`, key).
		Scan(&v.RequestHash, &v.Status, rawCol{&v.Body}, reqTime{&v.CreatedAt})
	if err != nil {
		if isNoRows(err) {
			return repo.IdempotentResponse{}, false, nil
		}
		return repo.IdempotentResponse{}, false, mapErr(err)
	}
	return v, true, nil
}

// Put remembers the first result for a key. Repeating it with the same request hash
// is a no-op; another hash for the same key is a conflict.
func (r idempotencyRepo) Put(ctx context.Context, v repo.IdempotentResponse) error {
	if v.Key == "" {
		return invalidArg("an idempotent response needs a key")
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = r.now()
	}
	body, _ := jsonArg(v.Body)
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `INSERT INTO idempotency_keys (key, request_hash, status, body, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
			v.Key, v.RequestHash, v.Status, body, timeText(v.CreatedAt))
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapErr(err)
		}
		if n == 1 {
			return nil
		}
		var existing string
		if err := q.QueryRowContext(ctx, `SELECT request_hash FROM idempotency_keys WHERE key = ?`, v.Key).Scan(&existing); err != nil {
			return mapErr(err)
		}
		if existing != v.RequestHash {
			return errs.New(errs.CodeIdempotencyMismatch, "this idempotency key was used with a different request")
		}
		return nil
	})
}

func (r idempotencyRepo) DeleteBefore(ctx context.Context, t time.Time) (int64, error) {
	var removed int64
	err := r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE created_at < ?`, timeText(t))
		if err != nil {
			return mapErr(err)
		}
		removed, err = res.RowsAffected()
		return mapErr(err)
	})
	return removed, err
}
