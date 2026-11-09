package apiserver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// proxyReader finds the optional capability of the storage service.
func (s *server) proxyReader() (service.ProxyReader, error) {
	reader, ok := s.d.Storages.(service.ProxyReader)
	if !ok {
		return nil, errs.New(errs.CodeNotImplemented, "reading proxy content is not wired in this daemon")
	}
	return reader, nil
}

// handleProxyContent streams an asset's proxy (§3.2 step 6, I285). It is the same
// permission as a proxy link, and works when a link would not.
func (s *server) handleProxyContent(c *call) error {
	reader, err := s.proxyReader()
	if err != nil {
		return err
	}
	body, info, err := reader.OpenAssetProxy(c.ctx, c.scope, c.ref())
	if err != nil {
		return err
	}
	return s.streamMedia(c, body, info)
}

// handleApprovalPreview streams the candidate a reviewer decides on. A preview that
// was never stored, or was removed once decided, answers 404.
func (s *server) handleApprovalPreview(c *call) error {
	reader, err := s.proxyReader()
	if err != nil {
		return err
	}
	approval, err := s.d.Approvals.Get(c.ctx, c.scope, c.ref())
	if err != nil {
		return err
	}
	if approval.Preview == "" {
		return errs.New(errs.CodeNotFound, "this approval has no preview")
	}
	body, info, err := reader.OpenProxyURI(c.ctx, c.scope, approval.Preview)
	if err != nil {
		return err
	}
	return s.streamMedia(c, body, info)
}

func (s *server) streamMedia(c *call, body io.ReadCloser, info connector.ObjectInfo) error {
	defer body.Close()
	contentType := info.ContentType
	if contentType == "" {
		contentType = "video/mp4"
	}
	header := c.w.Header()
	header.Set("Content-Type", contentType)
	if info.SizeBytes > 0 {
		header.Set("Content-Length", strconv.FormatInt(info.SizeBytes, 10))
	}
	if info.SHA256 != "" {
		header.Set("ETag", fmt.Sprintf("%q", info.SHA256))
	}
	c.w.WriteHeader(http.StatusOK)
	// The headers are gone, so a failure can only end the connection. Content-Length
	// makes that visible to the client as a short body.
	if _, err := io.Copy(c.w, body); err != nil && !errors.Is(err, c.ctx.Err()) {
		s.d.Logger.Warn("http.media_copy_failed", "route", c.route.Name, "request_id", c.requestID, "error", err)
	}
	return nil
}
