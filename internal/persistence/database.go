package persistence

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
)

// gateTimeout bounds waiting for the writer gate (I102). Hitting it almost always
// means a write through the outer store from inside an InTx callback.
const gateTimeout = 30 * time.Second

// Page limits (I102).
const (
	defaultPageLimit = 50
	maxPageLimit     = 500
)

// querier is what *sql.DB, *sql.Conn and *sql.Tx have in common.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface {
	Scan(dest ...any) error
}

// database is one SQLite file with its writer gate.
type database struct {
	db    *sql.DB
	path  string
	gate  chan struct{}
	clock func() time.Time
	opts  Options
}

// openDatabase opens path, creating it private if needed, and migrates it. tenant, when
// set, is recorded in the same transaction as the first migration, so no file can exist
// with a schema and without an owner (I103).
func openDatabase(ctx context.Context, path string, kind dbKind, tenant domain.TenantID, opts Options) (*database, error) {
	// SQLite creates the -wal and -shm files with the database's own mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	f.Close()
	db, err := sqlitedriver.Open(path, sqlitedriver.Options{MaxOpenConns: opts.MaxOpenConns})
	if err != nil {
		return nil, err
	}
	db.SetConnMaxIdleTime(5 * time.Minute)
	d := &database{db: db, path: path, gate: make(chan struct{}, 1), clock: opts.Clock, opts: opts}
	if err := d.migrate(ctx, kind, tenant); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

func (d *database) close() error { return d.db.Close() }

// acquire takes the writer gate.
func (d *database) acquire(ctx context.Context) error {
	timer := time.NewTimer(gateTimeout)
	defer timer.Stop()
	select {
	case d.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return mapErr(ctx.Err())
	case <-timer.C:
		return errs.New(errs.CodeUnavailable, "timed out waiting to write to the database")
	}
}

func (d *database) release() { <-d.gate }

// inTx runs fn in a BEGIN IMMEDIATE transaction under the writer gate.
func (d *database) inTx(ctx context.Context, fn func(querier) error) (err error) {
	if err := d.acquire(ctx); err != nil {
		return err
	}
	defer d.release()
	return d.txLocked(ctx, func(tx *sql.Tx) error { return fn(tx) })
}

func (d *database) txLocked(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return mapErr(tx.Commit())
}

// readTx runs fn in a deferred transaction on one connection, so several statements
// see one snapshot. WAL readers never block the writer.
func (d *database) readTx(ctx context.Context, fn func(querier) error) (err error) {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return mapErr(err)
	}
	defer func() {
		// The connection returns to the pool; it must not carry the transaction, even
		// when ctx has ended.
		if _, rerr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rerr != nil && err == nil {
			err = mapErr(rerr)
		}
	}()
	return mapErr(fn(conn))
}

// conn is the base of every repository. tx is nil outside a transaction.
type conn struct {
	d  *database
	tx querier
}

func (c conn) q() querier {
	if c.tx != nil {
		return c.tx
	}
	return c.d.db
}

// write runs fn atomically: in the surrounding transaction, or in its own.
func (c conn) write(ctx context.Context, fn func(querier) error) error {
	if c.tx != nil {
		return fn(c.tx)
	}
	return c.d.inTx(ctx, fn)
}

// read runs fn with one snapshot: the surrounding transaction, or a deferred one.
func (c conn) read(ctx context.Context, fn func(querier) error) error {
	if c.tx != nil {
		return fn(c.tx)
	}
	return c.d.readTx(ctx, fn)
}

func (c conn) now() time.Time { return c.d.clock().UTC() }

// ---- Errors ----

// mapErr turns driver errors into coded errors. Coded errors pass through; the
// original text stays as the internal cause, which errs.Public never serializes.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var coded *errs.Error
	if errors.As(err, &coded) {
		return err
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errs.Wrap(errs.CodeNotFound, err, "not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeTimeout, err, "the operation was cancelled or timed out")
	case sqlitedriver.IsUnique(err):
		return errs.Wrap(errs.CodeConflict, err, "the record already exists")
	case sqlitedriver.IsForeignKey(err):
		return errs.Wrap(errs.CodeFailedPrecondition, err, "the record refers to something that does not exist")
	case sqlitedriver.IsConstraint(err):
		return errs.Wrap(errs.CodeFailedPrecondition, err, "the record violates a data rule")
	case sqlitedriver.IsBusy(err):
		return errs.Wrap(errs.CodeUnavailable, err, "the database is busy")
	default:
		return errs.Wrap(errs.CodeInternal, err, "database error")
	}
}

// ---- Time ----

// timeLayout has a fixed width, so text comparison in SQL matches time order.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func timeText(t time.Time) string { return t.UTC().Format(timeLayout) }

func timeArg(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return timeText(*t)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// nullTime scans a nullable timestamp into a *time.Time.
type nullTime struct{ dst **time.Time }

func (n nullTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n.dst = nil
		return nil
	case string:
		t, err := parseTime(v)
		if err != nil {
			return err
		}
		*n.dst = &t
		return nil
	case []byte:
		t, err := parseTime(string(v))
		if err != nil {
			return err
		}
		*n.dst = &t
		return nil
	default:
		return fmt.Errorf("unexpected time column type %T", src)
	}
}

// reqTime scans a non-null timestamp.
type reqTime struct{ dst *time.Time }

func (r reqTime) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("unexpected time column type %T", src)
	}
	t, err := parseTime(s)
	if err != nil {
		return err
	}
	*r.dst = t
	return nil
}

// ---- Pagination ----

func encodeCursor(n int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(n, 10)))
}

func decodeCursor(s string) (int64, error) {
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, errs.New(errs.CodeInvalidArgument, "invalid cursor")
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return 0, errs.New(errs.CodeInvalidArgument, "invalid cursor")
	}
	return n, nil
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultPageLimit
	case limit > maxPageLimit:
		return maxPageLimit
	default:
		return limit
	}
}

// where accumulates SQL conditions.
type where struct {
	clauses []string
	args    []any
}

func (w *where) add(clause string, args ...any) {
	w.clauses = append(w.clauses, clause)
	w.args = append(w.args, args...)
}

func (w *where) sql() string {
	if len(w.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.clauses, " AND ")
}

// listDesc runs a keyset-paginated query, newest first. selectFrom must select the
// sort key first ("SELECT a.rowid, ... FROM ..."), key names it in conditions. scan
// receives each row and returns the sort key and the item.
func listDesc[T any](ctx context.Context, q querier, selectFrom, key string, w where, p repo.Page, scan func(scanner) (int64, T, error)) ([]T, string, error) {
	if p.Cursor != "" {
		after, err := decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		w.add(key+" < ?", after)
	}
	limit := clampLimit(p.Limit)
	query := selectFrom + w.sql() + " ORDER BY " + key + " DESC LIMIT ?"
	args := append(append([]any(nil), w.args...), limit+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", mapErr(err)
	}
	defer rows.Close()
	var (
		items []T
		last  int64
		next  string
	)
	for rows.Next() {
		if len(items) == limit {
			next = encodeCursor(last)
			break
		}
		k, item, err := scan(rows)
		if err != nil {
			return nil, "", mapErr(err)
		}
		last = k
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", mapErr(err)
	}
	return items, next, nil
}

// queryAll runs a query and scans every row.
func queryAll[T any](ctx context.Context, q querier, query string, args []any, scan func(scanner) (T, error)) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// notFoundIfNone maps a no-rows error to a not-found error naming the resource.
func notFoundIfNone(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errs.NotFound(what)
	}
	return mapErr(err)
}

// requireAffected fails with not-found when an update touched no row.
func requireAffected(res sql.Result, err error, what string) error {
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return errs.NotFound(what)
	}
	return nil
}
