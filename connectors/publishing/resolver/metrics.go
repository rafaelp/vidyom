package resolver

import (
	"context"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// meteredConnector records the calls that talk to a destination (§10.3). It passes
// PublicLinker through, so the export still gets per-platform links.
type meteredConnector struct {
	connector.Publishing
	metrics telemetry.Metrics
	tenant  string
}

func (m meteredConnector) observe(start time.Time, err error) {
	telemetry.ObserveConnector(m.metrics, "publishing", m.Info().Driver, m.tenant, start, err)
}

func (m meteredConnector) Upload(ctx context.Context, req connector.UploadRequest) (connector.RemoteVideo, error) {
	start := time.Now()
	out, err := m.Publishing.Upload(ctx, req)
	m.observe(start, err)
	return out, err
}

func (m meteredConnector) GetStatus(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
	start := time.Now()
	out, err := m.Publishing.GetStatus(ctx, remoteID)
	m.observe(start, err)
	return out, err
}

func (m meteredConnector) SetVisibility(ctx context.Context, remoteID string, v domain.Visibility) (connector.RemoteVideo, error) {
	start := time.Now()
	out, err := m.Publishing.SetVisibility(ctx, remoteID, v)
	m.observe(start, err)
	return out, err
}

func (m meteredConnector) Delete(ctx context.Context, remoteID string) error {
	start := time.Now()
	err := m.Publishing.Delete(ctx, remoteID)
	m.observe(start, err)
	return err
}

func (m meteredConnector) Reconcile(ctx context.Context, key string) (*connector.RemoteVideo, error) {
	start := time.Now()
	out, err := m.Publishing.Reconcile(ctx, key)
	m.observe(start, err)
	return out, err
}

// PublicLinks implements connector.PublicLinker when the driver does; otherwise it gives
// empty links, which is what the export treats a driver without the capability as.
func (m meteredConnector) PublicLinks(remote connector.RemoteVideo) connector.PublicLinks {
	if linker, ok := m.Publishing.(connector.PublicLinker); ok {
		return linker.PublicLinks(remote)
	}
	return connector.PublicLinks{}
}
