package loki

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/connectors/observability/internal/remote"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is this connector's name in observability.json.
const Driver = "loki"

const (
	version         = "1.0.0"
	pushPath        = "/loki/api/v1/push"
	defaultTimeout  = 10 * time.Second
	defaultMaxBytes = 1 << 20
)

// Parameters is the sink's `parameters` object (§7.4).
type Parameters struct {
	URL string `json:"url"`
	// OrgID becomes X-Scope-OrgID, for a multi-tenant Loki.
	OrgID string `json:"orgId,omitempty"`
	// Labels are static stream labels. They cannot override service, level or tenant_id.
	Labels         map[string]string `json:"labels,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	// MaxRequestBytes bounds one request's uncompressed lines. Zero means 1 MiB.
	MaxRequestBytes int `json:"maxRequestBytes,omitempty"`
}

func info() connector.Info {
	return connector.Info{
		Family:       connector.FamilyLogSink,
		Driver:       Driver,
		Version:      version,
		Available:    true,
		Capabilities: map[string]bool{"push": true},
	}
}

// Register adds the driver.
func Register(r connector.Registrar) {
	r.RegisterLogSink(info(), Factory)
}

// Factory opens a sink, resolving its credential. Nothing is contacted yet.
func Factory(ctx context.Context, cfg config.Sink, secrets connector.SecretResolver) (connector.LogSink, error) {
	var params Parameters
	if len(cfg.Parameters) > 0 {
		if err := json.Unmarshal(cfg.Parameters, &params); err != nil {
			return nil, errs.Wrap(errs.CodeValidationFailed, err, "invalid loki parameters")
		}
	}
	u, err := remote.ParseEndpoint(params.URL, cfg.CredentialsRef != "")
	if err != nil {
		return nil, err
	}
	auth, err := remote.Authorization(ctx, secrets, cfg.CredentialsRef)
	if err != nil {
		return nil, err
	}
	timeout := defaultTimeout
	if params.TimeoutSeconds > 0 {
		timeout = time.Duration(params.TimeoutSeconds) * time.Second
	}
	maxBytes := defaultMaxBytes
	if params.MaxRequestBytes > 0 {
		maxBytes = params.MaxRequestBytes
	}
	return &Sink{
		endpoint: u.JoinPath(pushPath),
		auth:     auth,
		orgID:    params.OrgID,
		labels:   params.Labels,
		maxBytes: maxBytes,
		client: &http.Client{
			Timeout: timeout,
			// Following a redirect could send the credential elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Sink pushes batches. It holds no queue: the manager's pump does the buffering,
// retrying and dropping.
type Sink struct {
	endpoint *url.URL
	auth     string
	orgID    string
	labels   map[string]string
	maxBytes int
	client   *http.Client
}

var _ connector.LogSink = (*Sink)(nil)

// Info implements connector.LogSink.
func (s *Sink) Info() connector.Info { return info() }

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type pushRequest struct {
	Streams []stream `json:"streams"`
}

func (s *Sink) streamLabels(rec *connector.LogRecord) map[string]string {
	labels := make(map[string]string, len(s.labels)+3)
	for k, v := range s.labels {
		labels[k] = v
	}
	labels["service"] = rec.Service
	if labels["service"] == "" {
		labels["service"] = "vidyomd"
	}
	labels["level"] = rec.Level
	if rec.TenantID != "" {
		labels["tenant_id"] = rec.TenantID
	} else {
		delete(labels, "tenant_id")
	}
	return labels
}

func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(labels[k])
		b.WriteByte(0)
	}
	return b.String()
}

// Write implements connector.LogSink, sending one or more requests.
func (s *Sink) Write(ctx context.Context, batch []connector.LogRecord) error {
	if len(batch) == 0 {
		return nil
	}
	current := map[string]*stream{}
	var order []string
	size := 0
	send := func() error {
		if size == 0 {
			return nil
		}
		req := pushRequest{Streams: make([]stream, 0, len(order))}
		for _, key := range order {
			req.Streams = append(req.Streams, *current[key])
		}
		current, order, size = map[string]*stream{}, nil, 0
		return s.push(ctx, req)
	}
	for i := range batch {
		rec := &batch[i]
		line, err := json.Marshal(rec)
		if err != nil {
			continue // the redactor already made fields encodable; one record never fails a batch
		}
		if size > 0 && size+len(line) > s.maxBytes {
			if err := send(); err != nil {
				return err
			}
		}
		labels := s.streamLabels(rec)
		key := labelKey(labels)
		st, ok := current[key]
		if !ok {
			st = &stream{Stream: labels}
			current[key] = st
			order = append(order, key)
		}
		ts := rec.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		st.Values = append(st.Values, [2]string{strconv.FormatInt(ts.UnixNano(), 10), string(line)})
		size += len(line)
	}
	return send()
}

func (s *Sink) push(ctx context.Context, body pushRequest) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(body); err != nil {
		return fmt.Errorf("encode push request: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("compress push request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint.String(), &buf)
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	if s.auth != "" {
		req.Header.Set("Authorization", s.auth)
	}
	if s.orgID != "" {
		req.Header.Set("X-Scope-OrgID", s.orgID)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("loki push: %w", err)
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("loki push: status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// Flush implements connector.LogSink. There is nothing buffered here.
func (s *Sink) Flush(context.Context) error { return nil }

// Close implements connector.LogSink.
func (s *Sink) Close(context.Context) error {
	s.client.CloseIdleConnections()
	return nil
}
