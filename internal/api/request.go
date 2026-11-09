package apiserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// bodyLimit returns how large a route's body may be (I125).
func bodyLimit(name string) int64 {
	switch name {
	case api.RouteWorkerComplete:
		return completeBodyLimit
	case api.RouteDevicesPair:
		return pairBodyLimit
	default:
		return defaultBodyLimit
	}
}

// readBody buffers a mutating request's body. Chunks stream instead.
func (c *call) readBody() error {
	if c.route.Name == api.RouteUploadsChunk || !c.route.Mutating() {
		return nil
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, bodyLimit(c.route.Name)))
	if err != nil {
		return bodyError(err)
	}
	c.body = data
	return nil
}

// decode reads the required JSON body, strictly (I125).
func (c *call) decode(v any) error {
	if len(bytes.TrimSpace(c.body)) == 0 {
		return errs.New(errs.CodeInvalidArgument, "a JSON body is required")
	}
	return decodeStrict(c.body, v)
}

// decodeOptional reads a JSON body that may be absent.
func (c *call) decodeOptional(v any) error {
	if len(bytes.TrimSpace(c.body)) == 0 {
		return nil
	}
	return decodeStrict(c.body, v)
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "invalid JSON body: "+describeJSONError(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errs.New(errs.CodeInvalidArgument, "invalid JSON body: unexpected data after the value")
	}
	return nil
}

// describeJSONError keeps field names and positions, never values, since a body
// can hold a secret.
func describeJSONError(err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return "malformed JSON at offset " + strconv.FormatInt(syntax.Offset, 10)
	case errors.As(err, &typeErr):
		return "field " + typeErr.Field + " has the wrong type"
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return strings.TrimPrefix(err.Error(), "json: ")
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "unexpected end of input"
	default:
		return "cannot decode"
	}
}

func (c *call) query(name string) string { return c.r.URL.Query().Get(name) }

// pageQuery reads limit and cursor (I15).
func (c *call) pageQuery() (api.PageQuery, error) {
	q := api.PageQuery{Cursor: c.query("cursor")}
	raw := c.query("limit")
	if raw == "" {
		return q, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 1000 {
		return q, errs.New(errs.CodeInvalidArgument, "limit must be between 1 and 1000")
	}
	q.Limit = n
	return q, nil
}

// lastEventID reads the SSE resume position (I3, I129). Absent means from the start.
func (c *call) lastEventID() (int64, error) {
	raw := strings.TrimSpace(c.r.Header.Get(api.HeaderLastEventID))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errs.Newf(errs.CodeInvalidArgument, "%s must be a non-negative integer", api.HeaderLastEventID)
	}
	return n, nil
}

// chunkReader refreshes the read deadline as bytes arrive, so a slow transfer lives
// and a silent connection releases the upload's lock within chunkIdleLimit.
func (s *server) chunkReader(c *call) io.Reader {
	return &idleDeadlineReader{r: c.r.Body, rc: http.NewResponseController(c.w), now: s.now}
}

type idleDeadlineReader struct {
	r   io.Reader
	rc  *http.ResponseController
	now func() time.Time
}

func (d *idleDeadlineReader) Read(p []byte) (int, error) {
	_ = d.rc.SetReadDeadline(d.now().Add(chunkIdleLimit))
	return d.r.Read(p)
}
