// Package skeleton is the shared body of publishing connectors that exist only as
// declarations in this version: Vimeo, PeerTube and Instagram Reels (D25, I11).
// A skeleton compiles and registers, reports Available false with its reason, and
// answers every operation with errs.NotImplemented. Nothing here talks to a
// destination.
package skeleton

import (
	"context"
	"io"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Version is what a skeleton reports as its driver version.
const Version = "0.0.0-skeleton"

// Register declares driver as an unavailable publishing connector.
func Register(r connector.Registrar, driver, reason string) {
	info := connector.Info{Driver: driver, Version: Version, Available: false, Reason: reason}
	r.RegisterPublishing(info, func(context.Context, connector.PublishingParams) (connector.Publishing, error) {
		info := info
		info.Family = connector.FamilyPublishing
		return Publishing{info: info}, nil
	})
}

// Publishing refuses everything. It opens without error so that callers can read
// Info and report the documented gap instead of "no such driver".
type Publishing struct{ info connector.Info }

var _ connector.Publishing = Publishing{}

func (p Publishing) gap() error { return errs.NotImplemented(p.info.Driver + " publishing connector") }

func (p Publishing) Info() connector.Info { return p.info }

// Capabilities is empty: a skeleton claims nothing.
func (p Publishing) Capabilities() connector.PublishingCapabilities {
	return connector.PublishingCapabilities{}
}

func (p Publishing) Upload(context.Context, connector.UploadRequest) (connector.RemoteVideo, error) {
	return connector.RemoteVideo{}, p.gap()
}

func (p Publishing) Get(context.Context, string) (connector.RemoteVideo, error) {
	return connector.RemoteVideo{}, p.gap()
}

func (p Publishing) Update(context.Context, string, connector.VideoMetadata) (connector.RemoteVideo, error) {
	return connector.RemoteVideo{}, p.gap()
}

func (p Publishing) Delete(context.Context, string) error { return p.gap() }

func (p Publishing) SetVisibility(context.Context, string, domain.Visibility) (connector.RemoteVideo, error) {
	return connector.RemoteVideo{}, p.gap()
}

func (p Publishing) UploadThumbnail(context.Context, string, io.Reader, string) error {
	return p.gap()
}

func (p Publishing) UploadCaptions(context.Context, string, string, string, io.Reader) error {
	return p.gap()
}

func (p Publishing) GetStatus(context.Context, string) (connector.RemoteVideo, error) {
	return connector.RemoteVideo{}, p.gap()
}

func (p Publishing) GetURL(context.Context, string) (string, error) { return "", p.gap() }

func (p Publishing) Reconcile(context.Context, string) (*connector.RemoteVideo, error) {
	return nil, p.gap()
}
