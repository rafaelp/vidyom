// Package sqlitedriver is the single place that opens a SQLite database (I1). It
// registers one database/sql driver with FTS5 and sqlite-vec built in, and sets
// durable settings on every connection.
//
// The underlying driver is github.com/rqlite/go-sqlite3, whose compile-time
// defaults are tuned for rqlite's Raft log: synchronous off and WAL autocheckpoint
// off. Those are dangerous here, since the tenant database is the source of truth
// for job state (D4). The connect hook below overrides them on every pooled
// connection, because both settings are per connection. Never open SQLite any
// other way.
//
// This package needs cgo, so only vidyomd may link it. `make check-static` fails
// if vidyom or vidyom-worker does.
//
// Component core owns this package. `internal/publicexport` may import it too.
package sqlitedriver

import (
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"time"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/rqlite/go-sqlite3"
)

const (
	driverName = "vidyom-sqlite3"
	readOnly   = driverName + "-ro"
)

// Durable connection settings, asserted by this package's test (I1).
const (
	walAutocheckpointPages = 1000
	defaultBusyTimeout     = 5 * time.Second
)

var registerOnce sync.Once

func register() {
	registerOnce.Do(func() {
		vec.Auto() // every future connection in this process has sqlite-vec
		sql.Register(driverName, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
			return applyPragmas(c, []string{
				"PRAGMA journal_mode = WAL",
				"PRAGMA synchronous = FULL",
				fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", walAutocheckpointPages),
				"PRAGMA foreign_keys = ON",
				fmt.Sprintf("PRAGMA busy_timeout = %d", defaultBusyTimeout.Milliseconds()),
			})
		}})
		sql.Register(readOnly, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
			return applyPragmas(c, []string{
				"PRAGMA foreign_keys = ON",
				fmt.Sprintf("PRAGMA busy_timeout = %d", defaultBusyTimeout.Milliseconds()),
			})
		}})
	})
}

func applyPragmas(c *sqlite3.SQLiteConn, pragmas []string) error {
	for _, p := range pragmas {
		if _, err := c.Exec(p, nil); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// Options controls Open.
type Options struct {
	// ReadOnly opens an existing file for reading.
	ReadOnly bool
	// MaxOpenConns limits the pool. Zero means 8 for read-write databases, enough
	// for concurrent WAL readers while writers queue on the immediate lock.
	MaxOpenConns int
}

// Open opens path with durable settings. Transactions begin with BEGIN IMMEDIATE,
// so a read-then-write transaction never fails on lock upgrade.
func Open(path string, opts Options) (*sql.DB, error) {
	register()
	dsn := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath()}
	query := url.Values{}
	name := driverName
	if opts.ReadOnly {
		name = readOnly
		query.Set("mode", "ro")
	} else {
		query.Set("mode", "rwc")
		query.Set("_txlock", "immediate")
	}
	dsn.RawQuery = query.Encode()

	db, err := sql.Open(name, dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if opts.MaxOpenConns > 0 {
		db.SetMaxOpenConns(opts.MaxOpenConns)
	} else {
		db.SetMaxOpenConns(8)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// SerializeVector encodes a vector as sqlite-vec expects, and as public.db stores
// it: a little-endian float32 BLOB (I29).
func SerializeVector(v []float32) ([]byte, error) {
	return vec.SerializeFloat32(v)
}
