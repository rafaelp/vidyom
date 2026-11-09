package apiserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/rafaelp/vidyom/internal/api/keylock"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// Time limits per request. Uploads and streams refresh theirs as data flows.
const (
	readLimit       = 60 * time.Second
	writeLimit      = 30 * time.Minute
	chunkIdleLimit  = 60 * time.Second
	streamFrameWait = 30 * time.Second
)

// Body limits (I125).
const (
	defaultBodyLimit  = 8 << 20
	completeBodyLimit = 48 << 20 // the 32 MiB output cap of I35 plus envelope
	pairBodyLimit     = 16 << 10
)

// handlerFunc serves one route after the pipeline. An error it returns becomes the
// JSON error body, unless the response has already started.
type handlerFunc func(c *call) error

type server struct {
	d          Deps
	log        *slog.Logger
	metrics    telemetry.Metrics
	now        func() time.Time
	keepAlive  time.Duration
	tracer     trace.Tracer
	propagator propagation.TraceContext
	idemLocks  *keylock.Locks
	pairLimit  *ipLimiter
	health     *healthCache
	mux        *http.ServeMux
}

// NewHandler returns the API. Every route of api.Routes is registered; a route with
// no handler in this package answers 501, and a test keeps that list empty.
func NewHandler(d Deps) http.Handler {
	s := &server{
		d:         d,
		log:       d.Logger,
		metrics:   d.Metrics,
		now:       d.Now,
		keepAlive: d.SSEKeepAlive,
		tracer:    otel.Tracer("github.com/rafaelp/vidyom/internal/api"),
		idemLocks: keylock.New(),
		mux:       http.NewServeMux(),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.metrics == nil {
		s.metrics = telemetry.Noop{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.keepAlive <= 0 {
		s.keepAlive = api.SSEKeepAlive
	}
	s.pairLimit = newIPLimiter(10, time.Minute, s.now)
	s.health = newHealthCache(d.HealthChecks, s.now)

	handlers := s.handlers()
	for _, rt := range api.Routes {
		h, ok := handlers[rt.Name]
		if !ok {
			h = unwired(rt.Name)
		}
		s.mux.Handle(rt.Pattern(), s.wrap(rt, h))
	}
	s.mux.Handle("/", s.wrap(api.Route{Name: "unmatched", Auth: api.AuthNone, Tenant: api.TenantNone}, s.unmatched))
	return s.mux
}

func unwired(what string) handlerFunc {
	return func(*call) error {
		return errs.Newf(errs.CodeNotImplemented, "%s is not wired in this daemon", what)
	}
}

// unmatched answers paths and methods outside the route table with the error body,
// telling a wrong method from a wrong path.
func (s *server) unmatched(c *call) error {
	var allow []string
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		probe := c.r.Clone(c.ctx)
		probe.Method = method
		if _, pattern := s.mux.Handler(probe); pattern != "/" && pattern != "" {
			allow = append(allow, method)
		}
	}
	if len(allow) > 0 {
		c.w.Header().Set("Allow", strings.Join(allow, ", "))
		c.statusOverride = http.StatusMethodNotAllowed
		return errs.Newf(errs.CodeInvalidArgument, "method %s is not allowed here", c.r.Method)
	}
	return errs.NotFound("route")
}

// call is one request in flight.
type call struct {
	w   http.ResponseWriter
	r   *http.Request
	rec *recorder
	ctx context.Context

	route     api.Route
	scope     service.Scope
	requestID string
	worker    string
	conn      ConnInfo
	body      []byte

	// statusOverride replaces the code's status, for 405.
	statusOverride int
}

func (s *server) wrap(rt api.Route, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := &recorder{ResponseWriter: w}
		requestID := string(id.New())
		header := rec.Header()
		header.Set(api.HeaderRequestID, requestID)
		header.Set("Cache-Control", "no-store")
		header.Set("X-Content-Type-Options", "nosniff")

		ctx := s.propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := s.tracer.Start(ctx, "api."+rt.Name, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		traceID := s.traceHeaders(header, span)

		c := &call{
			w: rec, r: r, rec: rec, ctx: ctx,
			route:     rt,
			requestID: requestID,
			conn:      connInfoFrom(r.Context()),
			scope:     service.Scope{RequestID: requestID, TraceID: traceID},
		}

		defer func() {
			if p := recover(); p != nil {
				s.log.Error("http.panic", "request_id", requestID, "route", rt.Name, "panic", p)
				s.writeError(c, errs.New(errs.CodeInternal, "internal error"))
			}
			s.finish(c, span, start)
		}()

		if err := s.pipeline(c, h); err != nil {
			s.writeError(c, err)
		}
	})
}

// traceHeaders returns the trace ID for the request and echoes traceparent. With no
// tracer provider and no valid parent, the span has no IDs, so one is generated
// and the job still carries a trace (I124).
func (s *server) traceHeaders(header http.Header, span trace.Span) string {
	sc := span.SpanContext()
	traceID, spanID := sc.TraceID().String(), sc.SpanID().String()
	if !sc.HasTraceID() {
		traceID = randomHex(16)
		spanID = randomHex(8)
	} else if !sc.HasSpanID() {
		spanID = randomHex(8)
	}
	flags := "00"
	if sc.IsSampled() {
		flags = "01"
	}
	header.Set(api.HeaderTraceParent, "00-"+traceID+"-"+spanID+"-"+flags)
	return traceID
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails on supported platforms
	return hex.EncodeToString(b)
}

func (s *server) finish(c *call, span trace.Span, start time.Time) {
	status := c.rec.status
	if status == 0 {
		status = http.StatusOK
	}
	span.SetAttributes(
		attribute.String("vidyom.route", c.route.Name),
		attribute.Int("http.response.status_code", status),
		attribute.String("vidyom.request_id", c.requestID),
	)
	if c.scope.Tenant != "" {
		span.SetAttributes(attribute.String("tenant_id", string(c.scope.Tenant)))
	}
	if status >= 500 {
		span.SetStatus(codes.Error, http.StatusText(status))
	}

	level := slog.LevelInfo
	switch {
	case status >= 500:
		level = slog.LevelError
	case quietRoutes[c.route.Name]:
		level = slog.LevelDebug
	}
	// Route name, never the raw URL: queries can hold search text, and paths of
	// unmatched requests are whatever the client sent.
	attrs := []any{
		"request_id", c.requestID,
		"trace_id", c.scope.TraceID,
		"route", c.route.Name,
		"method", c.r.Method,
		"status", status,
		"duration_ms", s.now().Sub(start).Milliseconds(),
		"transport", c.conn.Transport,
	}
	if c.scope.Tenant != "" {
		attrs = append(attrs, "tenant_id", string(c.scope.Tenant))
	}
	if actor := c.actor(); actor != "" {
		attrs = append(attrs, "actor", actor)
	}
	if c.scope.Principal.DeviceID != "" {
		attrs = append(attrs, "device_id", string(c.scope.Principal.DeviceID))
	}
	s.log.Log(c.ctx, level, "http.request", attrs...)
}

// quietRoutes are too frequent to log at info.
var quietRoutes = map[string]bool{
	api.RouteHealth:              true,
	api.RouteMetrics:             true,
	api.RouteWorkerHeartbeat:     true,
	api.RouteWorkerTaskHeartbeat: true,
	api.RouteWorkerTaskLogs:      true,
	api.RouteUploadsChunk:        true,
}

func (c *call) actor() string {
	if c.worker != "" {
		return "worker:" + c.worker
	}
	p := c.scope.Principal
	if p.Email == "" && p.DeviceID == "" && p.Method == "" {
		return ""
	}
	return p.Actor()
}

// pipeline is the fixed order of I120.
func (s *server) pipeline(c *call, h handlerFunc) error {
	if err := s.authenticate(c); err != nil {
		return err
	}
	if err := s.resolveTenant(c); err != nil {
		return err
	}
	if err := s.authorize(c); err != nil {
		return err
	}
	if err := validatePathParams(c); err != nil {
		return err
	}
	c.setDeadlines(s.now())
	return s.execute(c, h)
}

func (c *call) setDeadlines(now time.Time) {
	rc := http.NewResponseController(c.w)
	if c.route.Stream {
		// Streams set a deadline per frame.
		return
	}
	if c.route.Name == api.RouteUploadsChunk {
		// The chunk's reader refreshes this as bytes arrive.
		_ = rc.SetReadDeadline(now.Add(chunkIdleLimit))
	} else {
		_ = rc.SetReadDeadline(now.Add(readLimit))
	}
	_ = rc.SetWriteDeadline(now.Add(writeLimit))
}

// ---- Authentication ----

func bearerToken(r *http.Request) (string, bool) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return "", true
	}
	scheme, token, ok := strings.Cut(raw, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func (s *server) credentials(c *call) (security.Credentials, error) {
	token, ok := bearerToken(c.r)
	if !ok {
		s.countAuthFailure("malformed_credential")
		return security.Credentials{}, errs.New(errs.CodeUnauthenticated, "malformed Authorization header; expected a bearer token")
	}
	return security.Credentials{BearerToken: token, PeerUID: c.conn.PeerUID, RemoteAddr: c.conn.RemoteAddr}, nil
}

// countAuthFailure counts refusals the authenticator never sees; it counts the rest.
func (s *server) countAuthFailure(reason string) {
	s.metrics.Add(telemetry.AuthFailures, 1, telemetry.Labels{"reason": reason, telemetry.LabelTenant: telemetry.SystemTenant})
}

func (s *server) authenticate(c *call) error {
	switch c.route.Auth {
	case api.AuthNone:
		return nil
	case api.AuthWorker, api.AuthUser:
	default:
		return errs.New(errs.CodeInternal, "route has an unknown authentication mode")
	}
	if s.d.Authenticator == nil {
		return errs.New(errs.CodeUnavailable, "authentication is not wired in this daemon")
	}
	creds, err := s.credentials(c)
	if err != nil {
		return err
	}

	if c.route.Auth == api.AuthWorker {
		if creds.BearerToken == "" {
			s.countAuthFailure("missing_credential")
			return errs.New(errs.CodeUnauthenticated, "a worker token is required")
		}
		name, err := s.d.Authenticator.AuthenticateWorker(c.ctx, creds)
		if err != nil {
			return err
		}
		c.worker = name
		c.scope.Principal = domain.Principal{Method: domain.AuthWorker}
		return nil
	}

	if creds.BearerToken == "" && creds.PeerUID == nil {
		s.countAuthFailure("missing_credential")
		return errs.New(errs.CodeUnauthenticated, "a device credential is required")
	}
	p, err := s.d.Authenticator.Authenticate(c.ctx, creds)
	if err != nil {
		return err
	}
	c.scope.Principal = p
	return nil
}

// ---- Tenant (D32) ----

func (s *server) resolveTenant(c *call) error {
	if c.route.Tenant == api.TenantNone {
		return nil
	}
	values := c.r.Header.Values(api.HeaderTenant)
	if len(values) == 0 || values[0] == "" {
		if c.route.Tenant == api.TenantRequired {
			return errs.Newf(errs.CodeTenantRequired, "the %s header is required on this route", api.HeaderTenant)
		}
		return nil
	}
	tenant := domain.TenantID(values[0])
	if len(values) > 1 || tenant.Validate() != nil {
		return errs.Newf(errs.CodeInvalidArgument, "the %s header must hold one tenant slug", api.HeaderTenant)
	}
	if s.d.Authorizer == nil {
		return errs.New(errs.CodeUnavailable, "authorization is not wired in this daemon")
	}
	p, err := s.d.Authorizer.Resolve(c.ctx, c.scope.Principal, tenant)
	if err != nil {
		return err
	}
	c.scope.Tenant = tenant
	c.scope.Principal = p
	return nil
}

// ---- Permission ----

// serviceDecidesPermission lists routes whose table permission covers only part of
// who may call them: anyone may revoke their own device, so service.Devices decides
// (I112, I121). Everything else with a permission is checked here first.
var serviceDecidesPermission = map[string]bool{
	api.RouteDevicesRevoke: true,
	api.RouteClientsRevoke: true,
}

func (s *server) authorize(c *call) error {
	perm := c.route.Permission
	if perm == "" || serviceDecidesPermission[c.route.Name] {
		return nil
	}
	err := s.checkPermission(c, perm)
	if err == nil {
		return nil
	}
	if errs.IsCode(err, errs.CodeForbidden) && c.route.Mutating() {
		// A denial that cannot be recorded still denies; the failure is logged.
		if auditErr := s.record(c, resultDenied, errs.CodeForbidden); auditErr != nil {
			s.log.Error("audit.denial_not_recorded", "request_id", c.requestID, "route", c.route.Name, "error", auditErr)
		}
	}
	return err
}

func (s *server) checkPermission(c *call, perm domain.Permission) error {
	if s.d.Authorizer == nil {
		return errs.New(errs.CodeUnavailable, "authorization is not wired in this daemon")
	}
	if perm.SystemLevel() || c.scope.Tenant != "" {
		return s.d.Authorizer.Require(c.ctx, c.scope.Principal, perm)
	}
	// An optional-tenant route called with no tenant: only a system admin can hold
	// a tenant permission here (I121). The authorizer never sees this, so count it.
	if c.scope.Principal.SystemAdmin {
		return nil
	}
	s.metrics.Add(telemetry.AuthzDenials, 1, telemetry.Labels{"permission": string(perm), telemetry.LabelTenant: telemetry.SystemTenant})
	return errs.Forbidden(string(perm)).With("hint", "send "+api.HeaderTenant+" with a tenant you administer")
}

// ---- Path parameters (I126) ----

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9._@:+\-_]{1,255}$`)

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// refPrefixes maps a resource's first path segment to the friendly prefixes valid
// in its {id}. A valid ID of another resource answers 404.
var refPrefixes = map[string][]id.Prefix{
	"assets":       {id.PrefixOriginal, id.PrefixClip, id.PrefixEdited},
	"jobs":         {id.PrefixJob},
	"approvals":    {id.PrefixApproval},
	"publications": {id.PrefixPublication},
	"uploads":      {id.PrefixUpload},
	"devices":      {id.PrefixClient},
	"clients":      {id.PrefixClient},
}

func resourceOf(path string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return first
}

func validatePathParams(c *call) error {
	for _, match := range pathParam.FindAllStringSubmatch(c.route.Path, -1) {
		name := match[1]
		value := c.r.PathValue(name)
		if !safeSegment.MatchString(value) {
			return errs.Newf(errs.CodeInvalidArgument, "invalid %s in the path", name)
		}
		if name == "role" && !domain.StorageRole(value).Valid() {
			return errs.Newf(errs.CodeInvalidArgument, "unknown storage role %q", value)
		}
		if name != "id" {
			continue
		}
		prefixes, ok := refPrefixes[resourceOf(c.route.Path)]
		if !ok {
			continue
		}
		if !validRefFor(value, prefixes) {
			if id.IsFriendly(value) {
				return errs.NotFound(resourceOf(c.route.Path))
			}
			return errs.Newf(errs.CodeInvalidArgument, "invalid ID %q: expected a friendly ID or a ULID", value)
		}
	}
	return nil
}

func validRefFor(value string, prefixes []id.Prefix) bool {
	if id.IsULID(value) {
		return true
	}
	if !id.IsFriendly(value) {
		return false
	}
	got := id.Friendly(value).Prefix()
	for _, p := range prefixes {
		if p == got {
			return true
		}
	}
	return false
}

// ref returns the validated {id} parameter.
func (c *call) ref() id.Ref { return id.Ref(c.r.PathValue("id")) }

// param returns a validated path parameter.
func (c *call) param(name string) string { return c.r.PathValue(name) }
