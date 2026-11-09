package apiclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/version"
)

const (
	// defaultCallTimeout bounds a plain JSON call. Streams and chunks have their own
	// limits; slow synchronous calls set Call.Timeout.
	defaultCallTimeout = 5 * time.Minute

	// LongCall is for calls that hash or scan a large amount of data on the server.
	LongCall = 2 * time.Hour

	// maxServerDelay caps a delay a server asks for, so a wrong header cannot hang a
	// client for days.
	maxServerDelay = 5 * time.Minute

	maxErrorBody    = 1 << 20
	maxResponseBody = 1 << 30
)

// TokenFunc returns the device credential. It returns security.ErrKeyringNotFound
// when this client is not paired and security.ErrKeyringUnavailable when there is
// no usable keyring.
type TokenFunc func() (string, error)

// Options configures a Client.
type Options struct {
	Endpoint Endpoint
	// Tenant is the current tenant; empty means none selected.
	Tenant string
	// Token supplies the credential on user routes. Nil means none.
	Token TokenFunc
	// UserAgent overrides the default "vidyom/<version>".
	UserAgent string
	// CallTimeout overrides the default per-call timeout.
	CallTimeout time.Duration
	// Sleep is a seam for tests; it waits or returns ctx.Err().
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client talks to one daemon. Safe for concurrent use.
type Client struct {
	opts      Options
	http      *http.Client
	baseURL   string
	transport *transport
	sleep     func(context.Context, time.Duration) error
}

// New builds a client. It does not connect.
func New(opts Options) (*Client, error) {
	tr, err := newTransport(opts.Endpoint)
	if err != nil {
		return nil, err
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "vidyom/" + version.Version
	}
	if opts.CallTimeout == 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	return &Client{
		opts:      opts,
		baseURL:   tr.baseURL,
		transport: tr,
		sleep:     sleep,
		http: &http.Client{
			Transport: tr.roundTripper,
			// A redirect could carry the bearer token to another host; the daemon
			// never redirects, so a 3xx is reported as an error (I163).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Close releases the transport, including an SSH tunnel.
func (c *Client) Close() error { return c.transport.close() }

// Tenant returns the tenant sent on tenant-scoped routes.
func (c *Client) Tenant() string { return c.opts.Tenant }

// Call is one API operation, always named by its route.
type Call struct {
	Route string
	// Path holds name, value pairs for the route's path parameters.
	Path []string
	// Query holds query parameters.
	Query url.Values
	// Body is marshaled as JSON when set.
	Body any
	// IdempotencyKey overrides the random key sent on mutating routes.
	IdempotencyKey string
	// Timeout replaces the default per-call timeout.
	Timeout time.Duration
	// AcceptStatus lists non-2xx statuses whose body is the answer, such as the 503
	// of an unhealthy GET /health.
	AcceptStatus []int
}

// PathParam builds Call.Path for the common case of one parameter.
func PathParam(name, value string) []string { return []string{name, value} }

func (c *Client) buildURL(route api.Route, path []string, query url.Values) (string, error) {
	if len(path)%2 != 0 {
		return "", errs.New(errs.CodeInternal, "path parameters must come in name, value pairs")
	}
	escaped := make([]string, len(path))
	for i, v := range path {
		if i%2 == 1 {
			if v == "" {
				return "", errs.Newf(errs.CodeInvalidArgument, "empty value for %s", path[i-1])
			}
			v = url.PathEscape(v)
		}
		escaped[i] = v
	}
	target := c.baseURL + route.URL(escaped...)
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target, nil
}

// request prepares an HTTP request according to the route's declared tenant scope,
// authentication mode and mutability.
func (c *Client) request(ctx context.Context, call Call, body io.Reader, contentType string) (*http.Request, api.Route, error) {
	route := api.ByName(call.Route)
	target, err := c.buildURL(route, call.Path, call.Query)
	if err != nil {
		return nil, route, err
	}
	req, err := http.NewRequestWithContext(ctx, route.Method, target, body)
	if err != nil {
		return nil, route, errs.Wrap(errs.CodeInternal, err, "cannot build request")
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	switch route.Tenant {
	case api.TenantRequired:
		if c.opts.Tenant == "" {
			return nil, route, errs.New(errs.CodeTenantRequired, "no tenant selected")
		}
		req.Header.Set(api.HeaderTenant, c.opts.Tenant)
	case api.TenantOptional:
		if c.opts.Tenant != "" {
			req.Header.Set(api.HeaderTenant, c.opts.Tenant)
		}
	}

	if route.Auth == api.AuthUser {
		if err := c.authorize(req); err != nil {
			return nil, route, err
		}
	}

	if route.Mutating() {
		key := call.IdempotencyKey
		if key == "" {
			key, err = RandomKey()
			if err != nil {
				return nil, route, err
			}
		}
		req.Header.Set(api.HeaderIdempotencyKey, key)
	}
	return req, route, nil
}

func (c *Client) authorize(req *http.Request) error {
	required := c.opts.Endpoint.Kind != KindUnix
	if c.opts.Token == nil {
		if required {
			return errNotPaired()
		}
		return nil
	}
	token, err := c.opts.Token()
	switch {
	case err == nil && token != "":
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	case !required:
		// On a Unix socket the peer identity authenticates the call (I161).
		return nil
	case err == nil, errors.Is(err, security.ErrKeyringNotFound):
		return errNotPaired()
	case errors.Is(err, security.ErrKeyringUnavailable):
		return errs.Wrap(errs.CodeUnauthenticated, err, security.ErrKeyringUnavailable.Error())
	default:
		return errs.Wrap(errs.CodeUnauthenticated, err, "cannot read the device credential from the OS keyring")
	}
}

func errNotPaired() *errs.Error {
	return errs.New(errs.CodeUnauthenticated, "this client is not paired with the server")
}

// RandomKey returns a fresh random idempotency key.
func RandomKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errs.Wrap(errs.CodeInternal, err, "cannot generate an idempotency key")
	}
	return hex.EncodeToString(b[:]), nil
}

// Do runs a JSON call and decodes the answer into out, which may be nil.
func (c *Client) Do(ctx context.Context, call Call, out any) error {
	var body io.Reader
	contentType := ""
	if call.Body != nil {
		encoded, err := json.Marshal(call.Body)
		if err != nil {
			return errs.Wrap(errs.CodeInternal, err, "cannot encode request")
		}
		body = bytes.NewReader(encoded)
		contentType = "application/json"
	}
	timeout := call.Timeout
	if timeout == 0 {
		timeout = c.opts.CallTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, _, err := c.request(ctx, call, body, contentType)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && !slices.Contains(call.AcceptStatus, resp.StatusCode) {
		return decodeError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return c.transportError(ctx, err)
	}
	if len(raw) > maxResponseBody {
		return errs.New(errs.CodeOutputTooLarge, "the server's answer is too large")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "the server's answer is not valid JSON for "+call.Route)
	}
	return nil
}

// Download runs a call whose answer is a file and copies it to w. It has no per-call
// timeout, because a large file takes as long as it takes; the context ends it. A body
// shorter than the declared Content-Length is an error (the server cannot say so once
// it has started).
func (c *Client) Download(ctx context.Context, call Call, w io.Writer) (int64, error) {
	req, _, err := c.request(ctx, call, nil, "")
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, decodeError(resp)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, c.transportError(ctx, err)
	}
	return n, nil
}

// Invoke runs a call and returns its decoded answer.
func Invoke[T any](ctx context.Context, c *Client, call Call) (T, error) {
	var out T
	err := c.Do(ctx, call, &out)
	return out, err
}

// Items runs a list call. It decodes the {items, next_cursor} form of I15 and also
// a bare array, since several service methods return plain slices (I171).
func Items[T any](ctx context.Context, c *Client, call Call) (api.List[T], error) {
	var raw json.RawMessage
	if err := c.Do(ctx, call, &raw); err != nil {
		return api.List[T]{}, err
	}
	var list api.List[T]
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
	case trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &list.Items); err != nil {
			return list, errs.Wrap(errs.CodeInternal, err, "the server's answer is not valid JSON for "+call.Route)
		}
	default:
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return list, errs.Wrap(errs.CodeInternal, err, "the server's answer is not valid JSON for "+call.Route)
		}
	}
	return list, nil
}

// All follows cursors to the end of the list.
func All[T any](ctx context.Context, c *Client, call Call) (api.List[T], error) {
	var out api.List[T]
	query := url.Values{}
	for k, v := range call.Query {
		query[k] = v
	}
	for {
		call.Query = query
		page, err := Items[T](ctx, c, call)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		query = cloneValues(query)
		query.Set("cursor", page.NextCursor)
	}
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

// decodeError turns a non-2xx answer into a coded error, never echoing a body that
// is not the API's error format.
func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var (
		body  errs.Body
		coded *errs.Error
	)
	switch {
	case json.Unmarshal(raw, &body) == nil && body.Error != nil && body.Error.Code != "":
		coded = body.Error
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		coded = errs.Newf(errs.CodeFailedPrecondition, "the server answered %s, and this client does not follow redirects", resp.Status)
	default:
		coded = errs.Newf(codeForStatus(resp.StatusCode), "the server answered %s", resp.Status)
	}
	if coded.RequestID == "" {
		coded.RequestID = resp.Header.Get(api.HeaderRequestID)
	}
	if delay := parseRetryAfter(resp); delay != nil {
		coded = coded.With(retryAfterDetail, delay.Seconds())
	}
	return coded
}

// retryAfterDetail carries a Retry-After header on a coded error.
const retryAfterDetail = "retry_after_seconds"

// serverDelay reads the delay a server asked for from a coded error, capped.
func serverDelay(err error) (time.Duration, bool) {
	var coded *errs.Error
	if !errors.As(err, &coded) {
		return 0, false
	}
	seconds, ok := coded.Details[retryAfterDetail].(float64)
	if !ok || seconds < 0 {
		return 0, false
	}
	delay := time.Duration(seconds * float64(time.Second))
	if delay > maxServerDelay {
		delay = maxServerDelay
	}
	return delay, true
}

func codeForStatus(status int) errs.Code {
	switch status {
	case http.StatusBadRequest:
		return errs.CodeInvalidArgument
	case http.StatusUnauthorized:
		return errs.CodeUnauthenticated
	case http.StatusForbidden:
		return errs.CodeForbidden
	case http.StatusNotFound:
		return errs.CodeNotFound
	case http.StatusConflict:
		return errs.CodeConflict
	case http.StatusGone:
		return errs.CodeGone
	case http.StatusTooManyRequests:
		return errs.CodeRateLimited
	case http.StatusNotImplemented:
		return errs.CodeNotImplemented
	case http.StatusServiceUnavailable, http.StatusBadGateway:
		return errs.CodeUnavailable
	case http.StatusGatewayTimeout:
		return errs.CodeTimeout
	default:
		return errs.CodeInternal
	}
}

// transportError explains a failure that happened before an HTTP answer.
func (c *Client) transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return errs.Wrap(errs.CodeTimeout, err, "the server did not answer in time")
		}
		return ctxErr
	}
	if tlsErr := classifyTLS(err); tlsErr != nil {
		return tlsErr
	}
	// http.Client wraps errors with the full URL, query included. The endpoint is
	// enough, and search terms have no place in an error message.
	var withURL *url.Error
	if errors.As(err, &withURL) {
		err = withURL.Err
	}
	return errs.Wrap(errs.CodeUnavailable, err, fmt.Sprintf("cannot reach the daemon at %s", c.opts.Endpoint.Display()))
}

// IsTransient reports whether err may go away on retry without any change on the
// client: lost connections, timeouts, throttling and server-side unavailability.
// TLS failures are never transient (fail closed).
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var coded *errs.Error
	if !errors.As(err, &coded) {
		return false
	}
	if reason, _ := coded.Details["reason"].(string); strings.HasPrefix(reason, "tls_") {
		return false
	}
	switch coded.Code {
	case errs.CodeUnavailable, errs.CodeSealed, errs.CodeTimeout, errs.CodeRateLimited, errs.CodeInternal:
		// CodeInternal is how a 500 arrives. Only streams and uploads consult this,
		// and both re-read the server's state before continuing.
		return true
	default:
		return false
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
