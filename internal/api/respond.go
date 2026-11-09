package apiserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// recorder remembers the status, and captures the body while idempotency needs it.
type recorder struct {
	http.ResponseWriter
	status  int
	capture *captureBuffer
}

// captureBuffer stops keeping bytes past its limit, and says so.
type captureBuffer struct {
	data     []byte
	limit    int
	overflow bool
}

func (b *captureBuffer) write(p []byte) {
	if b.overflow {
		return
	}
	if len(b.data)+len(p) > b.limit {
		b.overflow = true
		b.data = nil
		return
	}
	b.data = append(b.data, p...)
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.capture != nil {
		r.capture.write(p)
	}
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush is for code that asserts http.Flusher directly.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *call) started() bool { return c.rec.status != 0 }

// writeJSON sends a response. Encoding happens first, so a value that cannot be
// encoded still produces a proper error rather than half a body.
func (c *call) writeJSON(status int, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "internal error")
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(append(data, '\n'))
	return nil
}

// empty answers routes with no natural body (I125).
var empty = struct{}{}

// reply writes v with a status, or passes err on.
func reply[T any](c *call, status int, v T, err error) error {
	if err != nil {
		return err
	}
	return c.writeJSON(status, v)
}

// listOf wraps a service's slice in the list shape of I15, never a null array.
func listOf[T any](items []T, err error) (api.List[T], error) {
	if err != nil {
		return api.List[T]{}, err
	}
	if items == nil {
		items = []T{}
	}
	return api.List[T]{Items: items}, nil
}

// page normalizes a service's list so items is never null.
func page[T any](l api.List[T], err error) (api.List[T], error) {
	if err != nil {
		return api.List[T]{}, err
	}
	if l.Items == nil {
		l.Items = []T{}
	}
	return l, nil
}

// writeError turns err into the error body. Uncoded errors become a generic
// internal error, with the real cause only in the log (errs.Public).
func (s *server) writeError(c *call, err error) {
	public := errs.Public(err)
	public.RequestID = c.requestID

	status := errs.HTTPStatus(public.Code)
	if c.statusOverride != 0 {
		status = c.statusOverride
	}
	if status >= 500 && c.ctx.Err() == nil {
		s.log.LogAttrs(c.ctx, slog.LevelError, "http.error",
			slog.String("request_id", c.requestID),
			slog.String("route", c.route.Name),
			slog.String("code", string(public.Code)),
			slog.Any("error", err),
		)
	}
	if c.started() {
		// A stream has begun; the connection is all that can be closed.
		return
	}
	header := c.w.Header()
	switch public.Code {
	case errs.CodeUnauthenticated:
		header.Set("WWW-Authenticate", `Bearer realm="vidyom"`)
	case errs.CodeRateLimited:
		retry := "60"
		if seconds, ok := public.Details["retry_after_seconds"]; ok {
			if n, ok := seconds.(int); ok && n > 0 {
				retry = strconv.Itoa(n)
			}
		}
		header.Set("Retry-After", retry)
	case errs.CodeUnavailable, errs.CodeSealed:
		header.Set("Retry-After", "5")
	}
	if offset, ok := public.Details["offset"]; ok {
		if n, ok := offset.(int64); ok {
			header.Set(api.HeaderUploadOffset, strconv.FormatInt(n, 10))
		}
	}
	data, marshalErr := json.Marshal(errs.Body{Error: public})
	if marshalErr != nil {
		s.log.Error("http.error_encoding", "request_id", c.requestID, "error", marshalErr)
		data = []byte(`{"error":{"code":"internal","message":"internal error"}}`)
		status = http.StatusInternalServerError
	}
	header.Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(append(data, '\n'))
}

// bodyError maps a read failure on a limited body.
func bodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errs.Newf(errs.CodePayloadTooLarge, "the request body exceeds %d bytes", tooLarge.Limit)
	}
	return errs.Wrap(errs.CodeInvalidArgument, err, "the request body could not be read")
}

// retryAfter renders a duration for the rate limit detail.
func retryAfter(d time.Duration) int {
	seconds := int(d.Seconds()) + 1
	if seconds < 1 {
		return 1
	}
	return seconds
}
