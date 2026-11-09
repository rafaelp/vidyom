package persistence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// Layout of the data directory (§14.1).
const (
	systemDBName = "system.db"
	tenantsDir   = "tenants"
	tenantDBName = "tenant.db"
)

// Options configures Open.
type Options struct {
	// DataDir holds system.db and tenants/ (§14.1). Required.
	DataDir string
	// RequireExisting fails with errs.CodeFailedPrecondition when system.db is
	// absent, so a normal start never initializes a data directory by accident.
	// Only `vidyomd init` leaves it false.
	RequireExisting bool
	// MaxOpenConns limits each database's pool. Zero uses the driver default.
	MaxOpenConns int
	// VectorScanCap bounds tenant-wide semantic search (I43, I106). Zero means
	// DefaultVectorScanCap.
	VectorScanCap int
	// Clock is used for timestamps a caller left empty. Nil means time.Now.
	Clock func() time.Time
}

// DefaultVectorScanCap is the tenant-wide semantic search cap until load tests set
// another (I43).
const DefaultVectorScanCap = 50_000

// Stores is the opened persistence layer.
type Stores struct {
	System  repo.SystemStore
	Tenants repo.TenantStores
}

// Open opens or creates system.db under Options.DataDir and applies migrations.
func Open(ctx context.Context, opts Options) (*Stores, error) {
	if opts.DataDir == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "persistence: a data directory is required")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.VectorScanCap <= 0 {
		opts.VectorScanCap = DefaultVectorScanCap
	}
	systemPath := filepath.Join(opts.DataDir, systemDBName)
	if opts.RequireExisting {
		if _, err := os.Stat(systemPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, errs.Newf(errs.CodeFailedPrecondition, "no %s in the data directory: run `vidyomd init` first", systemDBName)
			}
			return nil, fmt.Errorf("stat %s: %w", systemDBName, err)
		}
	}
	if err := ensurePrivateDir(opts.DataDir); err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(filepath.Join(opts.DataDir, tenantsDir)); err != nil {
		return nil, err
	}
	sys, err := openDatabase(ctx, systemPath, kindSystem, "", opts)
	if err != nil {
		return nil, err
	}
	return &Stores{
		System:  newSystemStore(sys),
		Tenants: newTenantStores(opts),
	}, nil
}

// Close closes every open database.
func (s *Stores) Close() error {
	return errors.Join(s.Tenants.Close(), s.System.Close())
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return nil
}
