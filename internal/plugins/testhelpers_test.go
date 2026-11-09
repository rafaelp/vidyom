package plugins

import (
	"net/url"
	"os"
	"syscall"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }

// signalZero reports whether a process exists.
func signalZero(p *os.Process) error { return p.Signal(syscall.Signal(0)) }

// isolatorCheck reports whether this host's default sandbox can run.
func (o *Options) isolatorCheck() error { return defaultIsolator().check(policy{}) }

func scopeFor(tenant string) service.Scope {
	return service.Scope{Tenant: domain.TenantID(tenant), Principal: domain.Principal{Email: "viewer@example.com", Role: domain.RoleViewer}}
}
