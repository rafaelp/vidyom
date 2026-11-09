package apiserver

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// bufferWriter collects a handler's whole response.
type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferWriter) Header() http.Header { return b.header }

func (b *bufferWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// handleMetrics serves the exposition with controlled access (§12.1, I128). With no
// tenant the pipeline has required a system admin, who sees everything. With a
// tenant, only that tenant's samples leave, even if the handler returned more. The
// handler is asked for plain text, the one format this filter can read.
func (s *server) handleMetrics(c *call) error {
	h := s.d.Observability.MetricsHandler()
	if h == nil {
		return unwired("the metrics handler")(c)
	}
	if c.scope.Tenant == "" {
		h.ServeHTTP(c.w, c.r)
		return nil
	}

	r := c.r.Clone(c.ctx)
	r.Header.Set("Accept", "text/plain")
	r.Header.Del("Accept-Encoding")
	out := &bufferWriter{header: http.Header{}}
	h.ServeHTTP(out, r)

	if contentType := out.header.Get("Content-Type"); contentType != "" {
		c.w.Header().Set("Content-Type", contentType)
	}
	status := out.status
	if status == 0 {
		status = http.StatusOK
	}
	c.w.WriteHeader(status)
	_, _ = c.w.Write(filterMetrics(out.body.Bytes(), c.scope.Tenant))
	return nil
}

// filterMetrics keeps comments and samples labeled with the tenant. A sample with
// no tenant_id label is host-wide and is dropped too, because it can reveal another
// tenant's activity.
func filterMetrics(text []byte, tenant domain.TenantID) []byte {
	var out bytes.Buffer
	want := `tenant_id="` + string(tenant) + `"`
	for _, line := range strings.Split(string(text), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") || hasLabel(line, want) {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// hasLabel finds an exact label pair inside a sample's braces.
func hasLabel(line, pair string) bool {
	open := strings.IndexByte(line, '{')
	end := strings.LastIndexByte(line, '}')
	if open < 0 || end < open {
		return false
	}
	labels := line[open+1 : end]
	for {
		i := strings.Index(labels, pair)
		if i < 0 {
			return false
		}
		if i == 0 || labels[i-1] == ',' {
			return true
		}
		labels = labels[i+len(pair):]
	}
}
