package apiserver

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/rafaelp/vidyom/internal/api/uploads"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

// UploadService is service.Uploads plus the chunk protocol (I20), which
// *uploads.Service implements.
type UploadService interface {
	service.Uploads
	AppendChunk(ctx context.Context, s service.Scope, ref id.Ref, chunk uploads.Chunk) (domain.Upload, error)
}

// HealthCheck is one component reported by GET /health. A failing critical check
// makes the whole answer 503.
type HealthCheck struct {
	Name     string
	Critical bool
	Check    func(ctx context.Context) error
}

// Deps is what the handler needs. Authenticator, Authorizer, Auditor and
// TenantStores are required for any authenticated route to work; without them
// those routes answer 503, never a pass. A nil service makes its routes answer
// 501, so partial wiring is visible.
type Deps struct {
	Authenticator security.Authenticator
	Authorizer    security.Authorizer
	Auditor       security.Auditor
	TenantStores  repo.TenantStores // idempotency records (§12.1)

	Workflows     service.Workflows
	Jobs          service.Jobs
	Approvals     service.Approvals
	Events        service.Events
	Introspection service.Introspection
	Assets        service.Assets
	Search        service.Search
	Publications  service.Publications
	Uploads       UploadService
	Storages      service.Storages
	Secrets       service.Secrets
	Devices       service.Devices
	Tenants       service.Tenants
	Users         service.Users
	Budgets       service.Budgets
	Audit         service.Audit
	Plugins       service.Plugins
	Observability service.Observability
	Connectors    service.Connectors
	Doctor        service.Doctor
	Coordinator   workerproto.Coordinator
	// TenantConfig supplies the cost table for the restore preview (I132).
	TenantConfig config.TenantSource

	// IngestRoots limits server: references (R20). Empty refuses them all.
	IngestRoots []string
	// TLSFingerprint is returned with pairing codes when a listener uses an
	// automatic certificate (I18, I135).
	TLSFingerprint string
	// Sealing is what /health reports about the master key (I10).
	Sealing      string
	HealthChecks []HealthCheck

	Logger  *slog.Logger
	Metrics telemetry.Metrics
	// Shutdown ends open streams when closed.
	Shutdown <-chan struct{}
	// SSEKeepAlive overrides api.SSEKeepAlive, for tests.
	SSEKeepAlive time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// ConnInfo is what a listener knows about a connection, attached to its context by
// the daemon's http.Server.ConnContext.
type ConnInfo struct {
	PeerUID    *int     // Unix socket peer identity (R19); nil elsewhere
	RemoteAddr net.Addr // nil when unknown
	Transport  string   // "unix" or "https"
}

type connInfoKey struct{}

// WithConnInfo attaches connection facts to a context.
func WithConnInfo(ctx context.Context, info ConnInfo) context.Context {
	return context.WithValue(ctx, connInfoKey{}, info)
}

func connInfoFrom(ctx context.Context) ConnInfo {
	info, _ := ctx.Value(connInfoKey{}).(ConnInfo)
	return info
}
