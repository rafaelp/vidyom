package persistence

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

//go:embed migrations
var migrationFiles embed.FS

// dbKind is what a database file is for (I103).
type dbKind string

const (
	kindSystem dbKind = "system"
	kindTenant dbKind = "tenant"
)

type migration struct {
	version int
	name    string
	sql     string
	sha256  string
}

func loadMigrations(kind dbKind) ([]migration, error) {
	dir := path.Join("migrations", string(kind))
	entries, err := fs.ReadDir(migrationFiles, dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		number, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("bad migration name %q", e.Name())
		}
		version, err := strconv.Atoi(number)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("bad migration number in %q", e.Name())
		}
		data, err := fs.ReadFile(migrationFiles, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		out = append(out, migration{version, e.Name(), string(data), hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations of %s are not consecutive at %q", kind, m.name)
		}
	}
	return out, nil
}

// migrate brings a database to the latest version in one transaction. It refuses a
// database of another kind, one newer than this binary, and an applied migration
// whose text changed (I103).
func (d *database) migrate(ctx context.Context, kind dbKind, tenant domain.TenantID) error {
	migrations, err := loadMigrations(kind)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	return d.inTx(ctx, func(q querier) error {
		if _, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			sha256     TEXT NOT NULL,
			applied_at TEXT NOT NULL
		) STRICT`); err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, `SELECT version, name, sha256 FROM schema_migrations ORDER BY version`)
		if err != nil {
			return err
		}
		applied := map[int]migration{}
		for rows.Next() {
			var m migration
			if err := rows.Scan(&m.version, &m.name, &m.sha256); err != nil {
				rows.Close()
				return err
			}
			applied[m.version] = m
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for version, m := range applied {
			if version > len(migrations) {
				return errs.Newf(errs.CodeFailedPrecondition, "%s was written by a newer version of Vidyom (schema %d, this binary knows %d)", d.path, version, len(migrations))
			}
			if migrations[version-1].sha256 != m.sha256 {
				return errs.Newf(errs.CodeFailedPrecondition, "%s: applied migration %s no longer matches this binary", d.path, m.name)
			}
		}
		if len(applied) > 0 {
			var stored string
			if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'kind'`).Scan(&stored); err != nil {
				return fmt.Errorf("read database kind: %w", err)
			}
			if stored != string(kind) {
				return errs.Newf(errs.CodeFailedPrecondition, "%s is a %s database, expected %s", d.path, stored, kind)
			}
		}
		now := timeText(d.clock())
		for _, m := range migrations {
			if _, done := applied[m.version]; done {
				continue
			}
			if _, err := q.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("apply %s: %w", m.name, err)
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`, m.version, m.name, m.sha256, now); err != nil {
				return err
			}
		}
		// Only a brand-new database takes an owner, in the transaction that gives it a
		// schema. An existing one keeps the owner it has, which ForTenant then compares.
		if tenant != "" && len(applied) == 0 {
			if _, err := q.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('tenant_id', ?)`, string(tenant)); err != nil {
				return err
			}
		}
		// PRAGMA takes no bound parameters; the value is our own integer.
		_, err = q.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations)))
		return err
	})
}
