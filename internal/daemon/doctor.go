package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	apiserver "github.com/rafaelp/vidyom/internal/api"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/version"
)

const (
	statusOK   = "ok"
	statusWarn = "warn"
	statusFail = "fail"
)

const (
	certExpiryWarning = 30 * 24 * time.Hour
	lowDiskWarning    = 10 << 30
	checkTimeout      = 5 * time.Second
)

// doctor implements service.Doctor, asking the listeners and the services (I128).
type doctor struct {
	cfg  config.Daemon
	deps Deps
	api  apiserver.Deps

	mu        sync.RWMutex
	listeners []ListenerInfo
}

var _ service.Doctor = (*doctor)(nil)

func (d *doctor) setListeners(infos []ListenerInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listeners = infos
}

func (d *doctor) currentListeners() []ListenerInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]ListenerInfo(nil), d.listeners...)
}

// privileged says who gets more than reachability: a system admin, or an admin of
// the request's tenant.
func privileged(s service.Scope) bool {
	return s.Principal.SystemAdmin || (s.Tenant != "" && s.Principal.Role == domain.RoleAdmin)
}

// Run answers GET /doctor. No check means everything the caller may see; naming one
// without the right answers 403.
func (d *doctor) Run(ctx context.Context, s service.Scope, check string) (api.DoctorResponse, error) {
	if check != "" && !privileged(s) {
		return api.DoctorResponse{}, errs.New(errs.CodeForbidden, "this check needs a system admin or an admin of the tenant")
	}
	checks := []api.DoctorCheck{
		{Name: "daemon.version", Status: statusOK, Detail: version.Version},
	}
	switch check {
	case "":
		if privileged(s) {
			checks = append(checks, d.security()...)
			checks = append(checks, d.network(ctx)...)
			checks = append(checks, d.storage(ctx, s)...)
		}
	case "security":
		checks = append(checks, d.security()...)
	case "network":
		checks = append(checks, d.network(ctx)...)
	case "storage":
		if s.Tenant == "" {
			return api.DoctorResponse{}, errs.Newf(errs.CodeTenantRequired, "the storage check needs the %s header", api.HeaderTenant)
		}
		checks = append(checks, d.storage(ctx, s)...)
	default:
		return api.DoctorResponse{}, errs.Newf(errs.CodeInvalidArgument, "unknown check %q", check)
	}
	return api.DoctorResponse{Checks: checks}, nil
}

func (d *doctor) security() []api.DoctorCheck {
	var out []api.DoctorCheck

	// Sealing is stated honestly (I10): file and dev are below D13's target.
	level := d.deps.MasterKey.Sealing()
	sealing := api.DoctorCheck{Name: "security.master_key", Status: statusOK, Detail: fmt.Sprintf("%s through %s", level, d.deps.MasterKey.Name())}
	switch level {
	case security.SealingFile:
		sealing.Status = statusWarn
		sealing.Detail += ": plaintext on the host, below the hardware or KMS target of D13"
	case security.SealingDev:
		sealing.Status = statusWarn
		sealing.Detail += ": development only, never for real data"
	}
	out = append(out, sealing)

	out = append(out, checkPrivateDir("security.data_dir", d.cfg.DataDir))

	for _, l := range d.currentListeners() {
		switch l.Kind {
		case "unix":
			out = append(out, api.DoctorCheck{Name: "security.socket", Status: statusOK, Detail: fmt.Sprintf("%s, mode %s, peer identity on", l.Address, l.Mode)})
		case "https":
			check := api.DoctorCheck{Name: "security.tls", Status: statusOK, Detail: fmt.Sprintf("%s, TLS 1.3 minimum, certificate %s", l.Address, l.TLSMode)}
			if l.MutualTLS {
				check.Detail += ", client certificates required"
			}
			if !l.CertNotAfter.IsZero() {
				switch left := time.Until(l.CertNotAfter); {
				case left <= 0:
					check.Status = statusFail
					check.Detail += ", certificate expired"
				case left < certExpiryWarning:
					check.Status = statusWarn
					check.Detail += fmt.Sprintf(", certificate expires in %d days", int(left.Hours()/24))
				}
			}
			out = append(out, check)
		}
	}
	return out
}

func checkPrivateDir(name, dir string) api.DoctorCheck {
	info, err := os.Stat(dir)
	if err != nil {
		return api.DoctorCheck{Name: name, Status: statusFail, Detail: "cannot read the directory's mode"}
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return api.DoctorCheck{Name: name, Status: statusWarn, Detail: fmt.Sprintf("%s has mode %04o; it holds transcripts and vault ciphertext", dir, perm)}
	}
	return api.DoctorCheck{Name: name, Status: statusOK, Detail: dir}
}

func (d *doctor) network(ctx context.Context) []api.DoctorCheck {
	var out []api.DoctorCheck
	for _, l := range d.currentListeners() {
		check := api.DoctorCheck{Name: "network.listener", Status: statusOK, Detail: l.Kind + " " + l.Address}
		if l.Interface != "" {
			check.Detail += ", interface " + l.Interface
		}
		switch {
		case l.Public:
			check.Status = statusWarn
			check.Detail += ": reachable beyond private networks; authentication and TLS still apply"
		case l.LoopbackPublished:
			check.Status = statusWarn
			check.Detail += ": relies on the container port being published only on the host's loopback, which the daemon cannot see (I133)"
		}
		out = append(out, check)
	}
	if d.deps.Queue != nil {
		pingCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		check := api.DoctorCheck{Name: "network.redis", Status: statusOK}
		if err := d.deps.Queue.Ping(pingCtx); err != nil {
			check.Status = statusFail
			check.Detail = "Redis does not answer; jobs cannot be dispatched"
		}
		out = append(out, check)
	}
	for _, c := range d.api.HealthChecks {
		if c.Name == "redis" {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		check := api.DoctorCheck{Name: "network." + c.Name, Status: statusOK}
		if err := c.Check(checkCtx); err != nil {
			check.Status = statusWarn
			if c.Critical {
				check.Status = statusFail
			}
			check.Detail = "does not answer"
		}
		cancel()
		out = append(out, check)
	}
	return out
}

func (d *doctor) storage(ctx context.Context, s service.Scope) []api.DoctorCheck {
	var out []api.DoctorCheck
	for name, dir := range map[string]string{"storage.spool": d.cfg.SpoolDir, "storage.scratch": d.cfg.ScratchDir} {
		out = append(out, checkWritable(name, dir))
	}
	if s.Tenant == "" {
		return out
	}
	if d.api.Storages == nil {
		return append(out, api.DoctorCheck{Name: "storage.roles", Status: statusWarn, Detail: "the storage service is not wired in this daemon"})
	}
	for _, role := range domain.StorageRoles {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		result, err := d.api.Storages.Test(checkCtx, s, role)
		cancel()
		check := api.DoctorCheck{Name: "storage." + string(role), Status: statusOK, Detail: result.Detail}
		switch {
		case err != nil:
			check.Status = statusFail
			check.Detail = errs.Public(err).Message
		case !result.OK:
			check.Status = statusFail
		}
		out = append(out, check)
	}
	return out
}

func checkWritable(name, dir string) api.DoctorCheck {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return api.DoctorCheck{Name: name, Status: statusFail, Detail: "cannot create " + dir}
	}
	probe, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return api.DoctorCheck{Name: name, Status: statusFail, Detail: "cannot write to " + dir}
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	check := api.DoctorCheck{Name: name, Status: statusOK, Detail: filepath.Clean(dir)}
	if free, ok := freeBytes(dir); ok {
		check.Detail += fmt.Sprintf(", %d GiB free", free>>30)
		if free < lowDiskWarning {
			check.Status = statusWarn
		}
	}
	return check
}
