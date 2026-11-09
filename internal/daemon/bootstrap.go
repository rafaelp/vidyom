package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// BootstrapRequest is the first tenant and first admin of a new data directory (I17).
type BootstrapRequest struct {
	Tenant     domain.TenantID
	AdminEmail string
}

// BootstrapResult holds what can be shown only once.
type BootstrapResult struct {
	PairingCode      string
	PairingExpiresAt time.Time
	WorkerToken      string
}

// BootstrapFunc creates the first tenant, the first admin, a worker token and one
// pairing code, and refuses a data directory that already has tenants or users.
// cmd/vidyomd/wire.go supplies it over core's tenants.Bootstrap (I114).
type BootstrapFunc func(ctx context.Context, req BootstrapRequest) (BootstrapResult, error)

// InitOptions are the flags of `vidyomd init`.
type InitOptions struct {
	Tenant          domain.TenantID
	AdminEmail      string
	WorkerTokenFile string // default <data-dir>/secrets/worker.token
}

// InitResult is what `vidyomd init` reports. The worker token itself goes to a file.
type InitResult struct {
	Tenant           domain.TenantID `json:"tenant"`
	Admin            string          `json:"admin"`
	PairingCode      string          `json:"pairing_code"`
	PairingExpiresAt time.Time       `json:"pairing_expires_at,omitzero"`
	WorkerTokenFile  string          `json:"worker_token_file"`
	TLSFingerprint   string          `json:"tls_fingerprint,omitempty"`
}

// DefaultWorkerTokenFile is where init stores the worker token (I138).
func DefaultWorkerTokenFile(cfg config.Daemon) string {
	return filepath.Join(cfg.DataDir, "secrets", "worker.token")
}

// Init initializes a data directory. The token file is reserved first, because the
// bootstrap returns the token only once: no outcome may leave a token nobody has.
func Init(ctx context.Context, cfg config.Daemon, bootstrap BootstrapFunc, opts InitOptions) (InitResult, error) {
	if bootstrap == nil {
		return InitResult{}, errors.New("no bootstrap is wired in this build")
	}
	if err := opts.Tenant.Validate(); err != nil {
		return InitResult{}, fmt.Errorf("--tenant: %w", err)
	}
	if opts.AdminEmail == "" {
		return InitResult{}, errors.New("--admin is required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return InitResult{}, fmt.Errorf("create the data directory: %w", err)
	}
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return InitResult{}, err
	}
	defer unlock()

	tokenPath := opts.WorkerTokenFile
	if tokenPath == "" {
		tokenPath = DefaultWorkerTokenFile(cfg)
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		return InitResult{}, fmt.Errorf("create the worker token's directory: %w", err)
	}
	tokenFile, err := os.OpenFile(tokenPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return InitResult{}, fmt.Errorf("%s already exists: this data directory looks initialized", tokenPath)
		}
		return InitResult{}, fmt.Errorf("reserve the worker token file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = tokenFile.Close()
			_ = os.Remove(tokenPath)
		}
	}()

	// Prepare the pin before the bootstrap too, so a certificate problem surfaces
	// while init can still be repeated.
	var fingerprint string
	if h := cfg.Listeners.HTTPS; h != nil && h.TLS.Mode == config.TLSAuto {
		ips := []net.IP{}
		if h.Bind != "" {
			if ip := net.ParseIP(h.Bind); ip != nil {
				ips = append(ips, ip)
			}
		}
		if _, fingerprint, err = ensureAutoCert(cfg.DataDir, ips); err != nil {
			return InitResult{}, fmt.Errorf("prepare the TLS certificate: %w", err)
		}
	}

	out, err := bootstrap(ctx, BootstrapRequest{Tenant: opts.Tenant, AdminEmail: opts.AdminEmail})
	if err != nil {
		return InitResult{}, err
	}
	if _, err := tokenFile.WriteString(out.WorkerToken + "\n"); err != nil {
		return InitResult{}, fmt.Errorf("write the worker token: %w", err)
	}
	if err := tokenFile.Sync(); err != nil {
		return InitResult{}, fmt.Errorf("write the worker token: %w", err)
	}
	if err := tokenFile.Close(); err != nil {
		return InitResult{}, fmt.Errorf("write the worker token: %w", err)
	}
	closed = true

	return InitResult{
		Tenant:           opts.Tenant,
		Admin:            opts.AdminEmail,
		PairingCode:      out.PairingCode,
		PairingExpiresAt: out.PairingExpiresAt,
		WorkerTokenFile:  tokenPath,
		TLSFingerprint:   fingerprint,
	}, nil
}

// GenerateMasterKey writes a new 32-byte key as standard base64 with mode 0400,
// refusing to overwrite (I10, I111, I138). Only `init --generate-master-key` makes a
// key; the daemon never does at startup.
func GenerateMasterKey(path string) error {
	key := make([]byte, masterKeySize)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create the key's directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite a master key", path)
		}
		return err
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}
