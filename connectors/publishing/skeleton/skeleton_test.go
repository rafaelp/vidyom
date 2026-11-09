package skeleton_test

import (
	"context"
	"strings"
	"testing"

	instagramreels "github.com/rafaelp/vidyom/connectors/publishing/instagram-reels"
	"github.com/rafaelp/vidyom/connectors/publishing/peertube"
	"github.com/rafaelp/vidyom/connectors/publishing/vimeo"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func TestSkeletonsDeclareThemselvesUnavailable(t *testing.T) {
	cases := []struct {
		driver   string
		register func(connector.Registrar)
	}{
		{vimeo.Driver, vimeo.Register},
		{peertube.Driver, peertube.Register},
		{instagramreels.Driver, instagramreels.Register},
	}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			reg := connector.NewRegistry()
			tc.register(reg)

			listed := reg.List()
			if len(listed) != 1 || listed[0].Family != connector.FamilyPublishing || listed[0].Available {
				t.Fatalf("registry lists %+v", listed)
			}
			if !strings.Contains(listed[0].Reason, "skeleton") {
				t.Fatalf("reason must state the gap, got %q", listed[0].Reason)
			}

			factory, err := reg.Publishing(tc.driver)
			if err != nil {
				t.Fatal(err)
			}
			p, err := factory(ctx, connector.PublishingParams{Tenant: "acme", Destination: "d"})
			if err != nil {
				t.Fatalf("a skeleton must open so callers can read its reason: %v", err)
			}
			if p.Info().Available || p.Info().Driver != tc.driver {
				t.Fatalf("Info says %+v", p.Info())
			}
			if p.Capabilities().Reconcile || p.Capabilities().ResumableUpload {
				t.Fatal("a skeleton must claim no capability")
			}

			calls := map[string]func() error{
				"Upload": func() error { _, e := p.Upload(ctx, connector.UploadRequest{}); return e },
				"Get":    func() error { _, e := p.Get(ctx, "x"); return e },
				"Update": func() error { _, e := p.Update(ctx, "x", connector.VideoMetadata{}); return e },
				"Delete": func() error { return p.Delete(ctx, "x") },
				"SetVisibility": func() error {
					_, e := p.SetVisibility(ctx, "x", domain.VisibilityPublic)
					return e
				},
				"UploadThumbnail": func() error { return p.UploadThumbnail(ctx, "x", nil, "image/jpeg") },
				"UploadCaptions":  func() error { return p.UploadCaptions(ctx, "x", "en", "vtt", nil) },
				"GetStatus":       func() error { _, e := p.GetStatus(ctx, "x"); return e },
				"GetURL":          func() error { _, e := p.GetURL(ctx, "x"); return e },
				"Reconcile":       func() error { _, e := p.Reconcile(ctx, "x"); return e },
			}
			for name, call := range calls {
				if err := call(); !errs.IsCode(err, errs.CodeNotImplemented) {
					t.Errorf("%s: got %v, want not_implemented", name, err)
				}
			}
		})
	}
}
