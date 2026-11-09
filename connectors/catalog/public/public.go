// Package public holds the public catalog connectors, which ship an exported
// public.db to where the site reads it (D18, I29, R44). The site never contacts
// the daemon. The first driver, local-file, copies the file to a directory on this
// host. How the file reaches a remote site's host stays a validation (D58).
package public

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is the name used in catalogs.json under publicLibrary.
const Driver = "local-file"

// Version is the driver version.
const Version = "1.0.0"

// fileName is the name the library reads (I29).
const fileName = "public.db"

// Register declares the local-file driver.
func Register(r connector.Registrar) {
	r.RegisterPublicCatalog(info(), open)
}

func info() connector.Info {
	return connector.Info{
		Family:       connector.FamilyPublicCatalog,
		Driver:       Driver,
		Version:      Version,
		Available:    true,
		Capabilities: map[string]bool{"atomic_replace": true, "checksum_verification": true},
	}
}

// parameters is the driver's configuration.
type parameters struct {
	// Directory is an absolute directory; the file lands at
	// <directory>/<tenant>/public.db.
	Directory string `json:"directory"`
}

func open(_ context.Context, p connector.CatalogParams) (connector.PublicCatalog, error) {
	var params parameters
	if len(p.Config.Parameters) > 0 {
		if err := json.Unmarshal(p.Config.Parameters, &params); err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid local-file parameters")
		}
	}
	if params.Directory == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "local-file needs parameters.directory")
	}
	if !filepath.IsAbs(params.Directory) {
		return nil, errs.New(errs.CodeInvalidArgument, "local-file parameters.directory must be absolute")
	}
	return &localFile{directory: filepath.Clean(params.Directory)}, nil
}

type localFile struct {
	directory string
}

func (l *localFile) Info() connector.Info { return info() }

func (l *localFile) target(tenant domain.TenantID) (string, error) {
	if err := tenant.Validate(); err != nil {
		return "", errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant")
	}
	return filepath.Join(l.directory, string(tenant), fileName), nil
}

// Ship replaces the target atomically: a reader sees the old file or the new one,
// and a failed copy leaves the old one in place.
func (l *localFile) Ship(ctx context.Context, artifact connector.PublicArtifact) (connector.ShipReceipt, error) {
	target, err := l.target(artifact.Tenant)
	if err != nil {
		return connector.ShipReceipt{}, err
	}
	if artifact.SHA256 == "" {
		return connector.ShipReceipt{}, errs.New(errs.CodeInvalidArgument, "artifact has no checksum")
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return connector.ShipReceipt{}, errs.Wrap(errs.CodeUnavailable, err, "cannot create the public library directory")
	}
	src, err := os.Open(artifact.Path)
	if err != nil {
		return connector.ShipReceipt{}, errs.Wrap(errs.CodeFailedPrecondition, err, "cannot read the exported public.db")
	}
	defer src.Close()

	tmp, err := os.CreateTemp(dir, ".public.db.tmp-*")
	if err != nil {
		return connector.ShipReceipt{}, errs.Wrap(errs.CodeUnavailable, err, "cannot write to the public library directory")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // a no-op after the rename

	sum := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(tmp, sum), src)
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return connector.ShipReceipt{}, ctxErr
		}
		return connector.ShipReceipt{}, errs.Wrap(errs.CodeUnavailable, err, "cannot write to the public library directory")
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if got != artifact.SHA256 || (artifact.SizeBytes > 0 && written != artifact.SizeBytes) {
		return connector.ShipReceipt{}, errs.Newf(errs.CodeFailedPrecondition, "the exported public.db changed while shipping: got sha256 %s", got)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return connector.ShipReceipt{}, errs.Wrap(errs.CodeUnavailable, err, "cannot replace public.db")
	}
	if err := syncDir(dir); err != nil {
		return connector.ShipReceipt{}, errs.Wrap(errs.CodeUnavailable, err, "cannot sync the public library directory")
	}
	return connector.ShipReceipt{Destination: Driver, SHA256: got}, nil
}

// Clear removes the tenant's dataset. Absence is success, so it is repeatable.
func (l *localFile) Clear(_ context.Context, tenant domain.TenantID) error {
	target, err := l.target(tenant)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot remove public.db")
	}
	if err := syncDir(filepath.Dir(target)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot sync the public library directory")
	}
	return nil
}

// copyWithContext copies in blocks so a cancelled job stops promptly (D11).
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return total, err
			}
			total += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

// syncDir makes a rename durable. Windows cannot flush a directory handle.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}
