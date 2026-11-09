// Package client is the worker's side of the daemon's worker API (I4): JSON over
// HTTP on a Unix socket or HTTPS, with the worker token as a bearer credential. It
// implements workerproto.Coordinator, so the worker loop cannot tell it from the
// in-process coordinator. It logs nothing: responses can hold credentials (I39).
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

const (
	defaultTimeout = 2 * time.Minute
	// maxResponse bounds what the daemon can make a worker hold. A lease's parameters
	// can carry a whole node output, such as a transcript, up to the 32 MiB cap (I35),
	// and JSON escaping can swell it.
	maxResponse = 2*domain.MaxNodeOutputBytes + 1<<20
	unixHost    = "vidyomd"
)

// Options configures a Client.
type Options struct {
	// Daemon is unix:///path/to.sock or https://host:port. Plain HTTP is refused.
	Daemon string
	// Token is the worker token.
	Token []byte
	// TLSFingerprint pins the daemon's certificate: "sha256:<hex>" of its DER (I18).
	TLSFingerprint string
	// TLSCAFile verifies the daemon against this CA instead.
	TLSCAFile string
	// Timeout bounds each call. Zero means 2 minutes.
	Timeout time.Duration
}

// Client calls the worker routes.
type Client struct {
	base    string
	token   string
	http    *http.Client
	timeout time.Duration
	routes  map[string]api.Route
}

var _ workerproto.Coordinator = (*Client)(nil)

// New builds a Client. It does not contact the daemon.
func New(opts Options) (*Client, error) {
	if len(opts.Token) == 0 {
		return nil, errors.New("worker client: no worker token")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	var base string
	switch {
	case strings.HasPrefix(opts.Daemon, "unix://"):
		path := strings.TrimPrefix(opts.Daemon, "unix://")
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("worker client: socket path %q must be absolute", path)
		}
		dialer := net.Dialer{}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		}
		base = "http://" + unixHost
	case strings.HasPrefix(opts.Daemon, "https://"):
		u, err := url.Parse(opts.Daemon)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("worker client: invalid daemon URL")
		}
		tlsConfig, err := tlsConfigFor(opts)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsConfig
		base = "https://" + u.Host
	default:
		return nil, fmt.Errorf("worker client: the daemon address must be unix:// or https://, got %q", opts.Daemon)
	}
	routes := make(map[string]api.Route)
	for _, r := range api.Routes {
		if r.Auth == api.AuthWorker {
			routes[r.Name] = r
		}
	}
	return &Client{
		base:    base,
		token:   strings.TrimSpace(string(opts.Token)),
		timeout: opts.Timeout,
		routes:  routes,
		http: &http.Client{
			Transport: transport,
			// The token must not follow a redirect anywhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func tlsConfigFor(opts Options) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	switch {
	case opts.TLSFingerprint != "":
		hexSum, ok := strings.CutPrefix(opts.TLSFingerprint, "sha256:")
		if !ok {
			return nil, errors.New("worker client: the TLS fingerprint must be sha256:<hex>")
		}
		want, err := hex.DecodeString(strings.ReplaceAll(hexSum, ":", ""))
		if err != nil || len(want) != sha256.Size {
			return nil, errors.New("worker client: invalid TLS fingerprint")
		}
		// The pin replaces chain verification: this is how a self-signed loopback
		// certificate is trusted, and nothing else is accepted (I18).
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(raw[0])
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return errors.New("the daemon's certificate does not match the pinned fingerprint")
			}
			return nil
		}
	case opts.TLSCAFile != "":
		pem, err := os.ReadFile(opts.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("worker client: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("worker client: no certificates in the CA file")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func (c *Client) call(ctx context.Context, routeName, taskID string, in, out any) error {
	route, ok := c.routes[routeName]
	if !ok {
		return errs.Newf(errs.CodeInternal, "unknown worker route %q", routeName)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode a worker request")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	path := strings.ReplaceAll(route.Path, "{id}", url.PathEscape(taskID))
	req, err := http.NewRequestWithContext(callCtx, route.Method, c.base+api.Prefix+path, bytes.NewReader(body))
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot build a worker request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.Wrap(errs.CodeUnavailable, err, "the daemon is unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errs.Wrap(errs.CodeUnavailable, err, "the daemon's response was cut off")
	}
	if len(data) > maxResponse {
		return errs.New(errs.CodeInternal, "the daemon's response exceeds what a worker accepts")
	}
	if resp.StatusCode >= 300 {
		return decodeError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "the daemon's response is not valid")
	}
	return nil
}

func decodeError(status int, data []byte) error {
	var body errs.Body
	if json.Unmarshal(data, &body) == nil && body.Error != nil && body.Error.Code != "" {
		return body.Error
	}
	switch status {
	case http.StatusGone:
		return errs.New(errs.CodeGone, "the task is obsolete")
	case http.StatusConflict:
		return errs.New(errs.CodeConflict, "the task is not available to this worker")
	case http.StatusUnauthorized:
		return errs.New(errs.CodeUnauthenticated, "the daemon refused the worker token")
	case http.StatusForbidden:
		return errs.New(errs.CodeForbidden, "the daemon refused this request")
	case http.StatusNotFound:
		return errs.New(errs.CodeNotFound, "the task or route was not found")
	case http.StatusTooManyRequests:
		return errs.New(errs.CodeRateLimited, "the daemon is rate limiting")
	}
	if status >= 500 {
		return errs.Newf(errs.CodeUnavailable, "the daemon answered %d", status)
	}
	return errs.Newf(errs.CodeInternal, "the daemon answered %d", status)
}

// Register declares this worker.
func (c *Client) Register(ctx context.Context, req workerproto.RegisterRequest) (workerproto.RegisterResponse, error) {
	var out workerproto.RegisterResponse
	err := c.call(ctx, api.RouteWorkerRegister, "", req, &out)
	return out, err
}

// Heartbeat keeps the worker registered.
func (c *Client) Heartbeat(ctx context.Context, req workerproto.HeartbeatRequest) error {
	return c.call(ctx, api.RouteWorkerHeartbeat, "", req, nil)
}

// Lease asks to run a task.
func (c *Client) Lease(ctx context.Context, taskID string, req workerproto.LeaseRequest) (workerproto.Lease, error) {
	var out workerproto.Lease
	err := c.call(ctx, api.RouteWorkerLease, taskID, req, &out)
	return out, err
}

// TaskHeartbeat extends a lease and reports progress.
func (c *Client) TaskHeartbeat(ctx context.Context, taskID string, req workerproto.TaskHeartbeatRequest) (workerproto.TaskHeartbeatResponse, error) {
	var out workerproto.TaskHeartbeatResponse
	err := c.call(ctx, api.RouteWorkerTaskHeartbeat, taskID, req, &out)
	return out, err
}

// TaskLogs posts log lines.
func (c *Client) TaskLogs(ctx context.Context, taskID string, req workerproto.TaskLogsRequest) error {
	return c.call(ctx, api.RouteWorkerTaskLogs, taskID, req, nil)
}

// Complete finishes a task.
func (c *Client) Complete(ctx context.Context, taskID string, req workerproto.CompleteRequest) error {
	return c.call(ctx, api.RouteWorkerComplete, taskID, req, nil)
}

// Fail reports a failure.
func (c *Client) Fail(ctx context.Context, taskID string, req workerproto.FailRequest) error {
	return c.call(ctx, api.RouteWorkerFail, taskID, req, nil)
}

// Connection fetches what a leased task needs to open a role.
func (c *Client) Connection(ctx context.Context, taskID string, req workerproto.ConnectionRequest) (workerproto.ConnectionResponse, error) {
	var out workerproto.ConnectionResponse
	err := c.call(ctx, api.RouteWorkerConnection, taskID, req, &out)
	return out, err
}

// CacheKey fetches the leased tenant's cache key.
func (c *Client) CacheKey(ctx context.Context, taskID string, req workerproto.CacheKeyRequest) (workerproto.CacheKeyResponse, error) {
	var out workerproto.CacheKeyResponse
	err := c.call(ctx, api.RouteWorkerCacheKey, taskID, req, &out)
	return out, err
}
