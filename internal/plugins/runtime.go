package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/jsonrpc"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/observability/redact"
)

const (
	tracerName          = "github.com/rafaelp/vidyom/internal/plugins"
	maxHookParameters   = 4 << 20
	maxHookReason       = 4 << 10
	maxRestartBackoff   = time.Minute
	firstRestartBackoff = time.Second
	stableAfter         = time.Minute
	consecutiveTimeouts = 3
)

// Options configure a Runtime. Repo, Dir, Grants and Auditor are required: there is no
// way to run plugins without a registry, a place for them, grants or an audit trail.
type Options struct {
	// Repo is the registry in system.db.
	Repo repo.PluginRepo
	// Dir is the installed plugin store. The runtime owns it.
	Dir string
	// RunDir holds per-process directories, emptied at start. Empty means <Dir>/.run.
	RunDir string
	// InstallRoots are the only directories Install reads from (I268).
	InstallRoots []string
	// RuntimePaths may be read and executed by plugins: interpreters (I265).
	RuntimePaths []string
	// ForbiddenPaths may never be granted: the daemon's data, config, spool, cache and
	// scratch directories (I264).
	ForbiddenPaths []string
	// AllowUnsandboxed runs plugins with no OS sandbox where none is available (I260).
	AllowUnsandboxed bool
	// Grants is what tenants allow (I261).
	Grants GrantSource
	// Secrets resolves granted secrets per tenant. Nil makes every host.secret fail.
	Secrets func(domain.TenantID) connector.SecretResolver
	// Auditor records install, integrity failures and secret access.
	Auditor security.Auditor
	// Metrics records §10.3's plugin measurements. Nil discards them.
	Metrics telemetry.Metrics
	// Logger receives plugin logs, redacted. Nil means slog.Default.
	Logger *slog.Logger
	// Now is a test seam.
	Now func() time.Time

	// The numbers of I262 and I271. Zero means the default.
	IdleTimeout           time.Duration
	InitTimeout           time.Duration
	ShutdownTimeout       time.Duration
	KillTimeout           time.Duration
	MaxProcesses          int
	MaxInFlight           int
	MaxHostCallsPerSecond int

	isolator isolator
}

// OptionsFromConfig fills what comes from daemon.json. Repo, Auditor, Secrets, Metrics
// and Logger are left for wiring.
func OptionsFromConfig(cfg config.Daemon) Options {
	dir := cfg.Plugins.Dir
	if dir == "" && cfg.DataDir != "" {
		dir = filepath.Join(cfg.DataDir, "plugins")
	}
	forbidden := make([]string, 0, 5)
	for _, path := range []string{cfg.DataDir, cfg.ConfigDir, cfg.SpoolDir, cfg.CacheDir, cfg.ScratchDir} {
		if path != "" {
			forbidden = append(forbidden, path)
		}
	}
	return Options{
		Dir:              dir,
		InstallRoots:     cfg.Plugins.InstallRoots,
		RuntimePaths:     cfg.Plugins.RuntimePaths,
		AllowUnsandboxed: cfg.Plugins.AllowUnsandboxed,
		ForbiddenPaths:   forbidden,
		Grants:           FileGrants{ConfigDir: cfg.ConfigDir},
	}
}

func (o Options) withDefaults() Options {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.IdleTimeout, 5*time.Minute)
	def(&o.InitTimeout, 10*time.Second)
	def(&o.ShutdownTimeout, 3*time.Second)
	def(&o.KillTimeout, 2*time.Second)
	if o.MaxProcesses <= 0 {
		o.MaxProcesses = 64
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = 8
	}
	if o.MaxHostCallsPerSecond <= 0 {
		o.MaxHostCallsPerSecond = maxHostCallsRate
	}
	if o.Metrics == nil {
		o.Metrics = telemetry.Noop{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.isolator == nil {
		o.isolator = defaultIsolator()
	}
	return o
}

// slot serializes everything about one tenant's process of one plugin.
type slot struct {
	lock      chan struct{}
	proc      *process
	failures  int
	nextStart time.Time
}

// Runtime is the plugin runtime: the registry behind service.Plugins, the action runner
// behind hooks, and the subscriber behind the Event Bus.
type Runtime struct {
	opts    Options
	store   *store
	logger  *slog.Logger
	metrics telemetry.Metrics
	now     func() time.Time
	runDir  string
	redact  *redact.Redactor

	mu     sync.Mutex
	slots  map[string]*slot
	closed bool

	reapStop chan struct{}
	reapDone chan struct{}
}

var (
	_ service.Plugins        = (*Runtime)(nil)
	_ hook.ActionRunner      = (*Runtime)(nil)
	_ plugin.EventSubscriber = (*Runtime)(nil)
)

// New prepares the store and the run directory. It starts no plugin.
func New(opts Options) (*Runtime, error) {
	opts = opts.withDefaults()
	switch {
	case opts.Repo == nil:
		return nil, errors.New("plugins: a registry is required")
	case opts.Dir == "":
		return nil, errors.New("plugins: a plugin directory is required")
	case opts.Grants == nil:
		return nil, errors.New("plugins: a grant source is required")
	case opts.Auditor == nil:
		return nil, errors.New("plugins: an auditor is required")
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("create plugin directory: %w", err)
	}
	dir, err := filepath.EvalSymlinks(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve plugin directory: %w", err)
	}
	runDir := opts.RunDir
	if runDir == "" {
		runDir = filepath.Join(dir, ".run")
	}
	if err := os.RemoveAll(runDir); err != nil {
		return nil, fmt.Errorf("clear plugin run directory: %w", err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, fmt.Errorf("create plugin run directory: %w", err)
	}
	r := &Runtime{
		opts:     opts,
		store:    &store{dir: dir},
		logger:   opts.Logger,
		metrics:  opts.Metrics,
		now:      opts.Now,
		runDir:   runDir,
		redact:   redact.New(),
		slots:    map[string]*slot{},
		reapStop: make(chan struct{}),
		reapDone: make(chan struct{}),
	}
	go r.reapLoop()
	return r, nil
}

func (r *Runtime) runRoot(tenant domain.TenantID) string {
	path := filepath.Join(r.runDir, string(tenant))
	_ = os.MkdirAll(path, 0o700)
	return path
}

// isolationName is what this host would run a plugin under.
func (r *Runtime) isolationName() string {
	if err := r.opts.isolator.check(policy{}); err != nil {
		if r.opts.AllowUnsandboxed {
			return IsolationNone
		}
		return IsolationUnavailable
	}
	return r.opts.isolator.name()
}

// ---- service.Plugins ----

// decorate adds what is computed at read time, and removes who installed it from anyone
// who is not a system admin: that is another tenant's person (I268).
func (r *Runtime) decorate(ctx context.Context, s service.Scope, rec domain.PluginRecord) domain.PluginRecord {
	rec.Isolation = r.isolationName()
	if !s.Principal.SystemAdmin {
		rec.InstalledBy = ""
	}
	if s.Tenant != "" {
		if m, err := ParseManifest(rec.Manifest); err == nil {
			if grant, found, err := r.opts.Grants.PluginGrant(ctx, s.Tenant, rec.ID); err == nil && found && grant.Enabled {
				if perms, err := effectivePermissions(m, grant); err == nil {
					if data, err := json.Marshal(perms); err == nil {
						rec.TenantGrant = data
					}
				}
			}
		}
	}
	return rec
}

// List implements service.Plugins.
func (r *Runtime) List(ctx context.Context, s service.Scope) ([]domain.PluginRecord, error) {
	records, err := r.opts.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.PluginRecord, 0, len(records))
	for _, rec := range records {
		out = append(out, r.decorate(ctx, s, rec))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

// Get implements service.Plugins.
func (r *Runtime) Get(ctx context.Context, s service.Scope, pluginID string) (domain.PluginRecord, error) {
	rec, err := r.opts.Repo.Get(ctx, pluginID)
	if err != nil {
		return domain.PluginRecord{}, err
	}
	return r.decorate(ctx, s, rec), nil
}

func (r *Runtime) resolveInstallPath(raw string) (string, error) {
	if len(r.opts.InstallRoots) == 0 {
		return "", errs.New(errs.CodeFailedPrecondition, "plugin install is off: set plugins.installRoots in daemon.json")
	}
	if !filepath.IsAbs(raw) {
		return "", errs.New(errs.CodeInvalidArgument, "the plugin path must be absolute on the server")
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", errs.Newf(errs.CodeInvalidArgument, "plugin directory %q cannot be read", raw)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errs.Newf(errs.CodeInvalidArgument, "%q is not a directory", raw)
	}
	for _, root := range r.opts.InstallRoots {
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if resolved == rootResolved || hasPathPrefix(resolved, rootResolved, string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", errs.Newf(errs.CodeForbidden, "%q is outside the configured plugin install roots", raw)
}

// Install implements service.Plugins. It needs a system admin beyond the route's
// permission, because a plugin is code on the host (I268). It grants nothing.
func (r *Runtime) Install(ctx context.Context, s service.Scope, req api.InstallPluginRequest) (domain.PluginRecord, error) {
	if !s.Principal.Can(domain.PermPluginInstall) {
		return domain.PluginRecord{}, errs.Forbidden(string(domain.PermPluginInstall))
	}
	if !s.Principal.SystemAdmin {
		return domain.PluginRecord{}, errs.Forbidden(string(domain.PermSystemAdmin))
	}
	src, err := r.resolveInstallPath(req.Path)
	if err != nil {
		return domain.PluginRecord{}, err
	}
	in, err := loadTree(src)
	if err != nil {
		return domain.PluginRecord{}, err
	}
	if err := checkForbiddenPaths(in.manifest.Permissions, r.opts.ForbiddenPaths); err != nil {
		return domain.PluginRecord{}, err
	}
	m := in.manifest
	previous, err := r.opts.Repo.Get(ctx, m.ID)
	hadPrevious := err == nil
	if err != nil && !errs.IsCode(err, errs.CodeNotFound) {
		return domain.PluginRecord{}, err
	}
	if hadPrevious && previous.Version == m.Version && previous.SHA256 != in.digest {
		return domain.PluginRecord{}, errs.Newf(errs.CodeConflict, "plugin %s version %s is installed with different content: release a new version", m.ID, m.Version)
	}
	if hadPrevious && previous.Version == m.Version && previous.Enabled {
		return r.decorate(ctx, s, previous), nil
	}
	if _, err := r.store.add(src, in); err != nil {
		return domain.PluginRecord{}, err
	}
	entry := security.AuditEntry{
		TenantID:   s.Tenant,
		Principal:  s.Principal,
		Action:     "plugin.install",
		Permission: domain.PermPluginInstall,
		Resource:   "plugin:" + m.ID,
		Result:     "allowed",
		RequestID:  s.RequestID,
		Details:    map[string]any{"version": m.Version, "sha256": in.digest, "source": src},
	}
	if err := r.opts.Auditor.Record(ctx, entry); err != nil {
		return domain.PluginRecord{}, err
	}
	rec := domain.PluginRecord{
		ID:          m.ID,
		Version:     m.Version,
		Manifest:    in.raw,
		SHA256:      in.digest,
		Enabled:     true,
		InstalledBy: s.Principal.Email,
		InstalledAt: r.now().UTC(),
	}
	if err := r.opts.Repo.Upsert(ctx, rec); err != nil {
		entry.Result = "failed"
		_ = r.opts.Auditor.Record(ctx, entry)
		return domain.PluginRecord{}, err
	}
	r.stopPlugin(ctx, m.ID)
	if hadPrevious && (previous.Version != m.Version || previous.SHA256 != in.digest) {
		if err := removeTree(r.store.path(previous.ID, previous.Version, previous.SHA256)); err != nil {
			r.logger.Warn("could not remove a previous plugin version", "event", "plugin.cleanup_failed", "plugin", m.ID, "error", err.Error())
		}
	}
	return r.decorate(ctx, s, rec), nil
}

// quarantine takes a plugin that failed its integrity check out of service (I267).
func (r *Runtime) quarantine(ctx context.Context, tenant domain.TenantID, rec domain.PluginRecord, cause error) {
	rec.Enabled = false
	r.logger.Error("plugin failed its integrity check and was disabled", "event", "plugin.integrity_failed", "plugin", rec.ID, "tenant_id", string(tenant))
	r.metrics.Add(telemetry.SecurityAnomalies, 1, telemetry.Labels{"kind": "plugin_integrity", telemetry.LabelTenant: string(tenant)})
	if err := r.opts.Repo.Upsert(ctx, rec); err != nil {
		r.logger.Error("could not disable a plugin that failed its integrity check", "event", "plugin.quarantine_failed", "plugin", rec.ID, "error", err.Error())
	}
	if err := r.opts.Auditor.Record(ctx, security.AuditEntry{
		TenantID:  tenant,
		Principal: domain.Principal{Method: domain.AuthSystem},
		Action:    "plugin.integrity_failed",
		Resource:  "plugin:" + rec.ID,
		Result:    "failed",
		Details:   map[string]any{"version": rec.Version, "reason": r.redact.Error(cause)},
	}); err != nil {
		r.logger.Error("could not audit a plugin integrity failure", "event", "plugin.audit_failed", "plugin", rec.ID, "error", err.Error())
	}
}

// ---- Process management ----

func (r *Runtime) slotFor(key string) (*slot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errs.New(errs.CodeUnavailable, "the plugin runtime is shutting down")
	}
	s := r.slots[key]
	if s == nil {
		s = &slot{lock: make(chan struct{}, 1)}
		r.slots[key] = s
	}
	return s, nil
}

func (s *slot) acquire(ctx context.Context) error {
	select {
	case s.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slot) release() { <-s.lock }

func (r *Runtime) liveProcesses() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.slots {
		if s.proc != nil {
			n++
		}
	}
	return n
}

// resolvePlugin finds a plugin this tenant may use, with the permissions in force.
func (r *Runtime) resolvePlugin(ctx context.Context, tenant domain.TenantID, pluginID string) (domain.PluginRecord, plugin.Manifest, plugin.Permissions, error) {
	var perms plugin.Permissions
	var m plugin.Manifest
	if !tenantSlug.MatchString(string(tenant)) {
		return domain.PluginRecord{}, m, perms, errs.New(errs.CodeInvalidArgument, "a plugin call needs a valid tenant")
	}
	if pluginID == "" {
		return domain.PluginRecord{}, m, perms, errs.New(errs.CodeInvalidArgument, "a plugin call needs a plugin")
	}
	rec, err := r.opts.Repo.Get(ctx, pluginID)
	if err != nil {
		return rec, m, perms, err
	}
	if !rec.Enabled {
		return rec, m, perms, errs.Newf(errs.CodeFailedPrecondition, "plugin %s is disabled; install it again", pluginID)
	}
	m, err = ParseManifest(rec.Manifest)
	if err != nil {
		return rec, m, perms, err
	}
	// Stored manifests go through validation again, so a record from an older or a
	// hand-edited registry cannot start what this version refuses (I42, I269).
	if err := Validate(m); err != nil {
		return rec, m, perms, err
	}
	grant, found, err := r.opts.Grants.PluginGrant(ctx, tenant, pluginID)
	if err != nil {
		return rec, m, perms, err
	}
	if !found || !grant.Enabled {
		return rec, m, perms, errs.Newf(errs.CodeForbidden, "plugin %s is not enabled for this tenant", pluginID)
	}
	perms, err = effectivePermissions(m, grant)
	if err != nil {
		return rec, m, perms, err
	}
	if err := checkForbiddenPaths(perms, r.opts.ForbiddenPaths); err != nil {
		return rec, m, perms, err
	}
	return rec, m, perms, nil
}

// acquire returns a live process for this tenant and plugin, starting one when needed
// and replacing one whose version or permissions have changed.
func (r *Runtime) acquire(ctx context.Context, tenant domain.TenantID, rec domain.PluginRecord, m plugin.Manifest, perms plugin.Permissions) (*process, error) {
	key := string(tenant) + "\x00" + rec.ID
	s, err := r.slotFor(key)
	if err != nil {
		return nil, err
	}
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	r.mu.Lock()
	current := s.proc
	nextStart := s.nextStart
	r.mu.Unlock()
	if current != nil && !current.hasExited() && !current.stopping.Load() {
		if current.sha256 == rec.SHA256 && current.permKey == permissionsKey(perms) {
			return current, nil
		}
		current.stop(ctx) // permissions of a running sandbox cannot change (I261)
		r.mu.Lock()
		if s.proc == current {
			s.proc = nil
		}
		r.mu.Unlock()
	}
	if now := r.now(); now.Before(nextStart) {
		return nil, errs.Newf(errs.CodeUnavailable, "plugin %s keeps failing; it will be retried in %s", rec.ID, nextStart.Sub(now).Round(time.Second))
	}
	if r.liveProcesses() >= r.opts.MaxProcesses {
		return nil, errs.New(errs.CodeUnavailable, "too many plugin processes are running")
	}
	p, err := r.spawn(ctx, tenant, rec, m, perms)
	if err != nil {
		var started startFailure
		if errors.As(err, &started) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
			r.mu.Lock()
			s.recordFailure(r.now())
			r.mu.Unlock()
		}
		return nil, err
	}
	r.mu.Lock()
	closed := r.closed
	if !closed {
		s.proc = p
	}
	r.mu.Unlock()
	if closed {
		p.kill()
		return nil, errs.New(errs.CodeUnavailable, "the plugin runtime is shutting down")
	}
	return p, nil
}

// recordFailure runs under Runtime.mu.
func (s *slot) recordFailure(now time.Time) {
	s.failures++
	backoff := firstRestartBackoff
	for i := 1; i < s.failures && backoff < maxRestartBackoff; i++ {
		backoff *= 2
	}
	s.nextStart = now.Add(min(backoff, maxRestartBackoff))
}

// onExit runs after a process has ended, however it ended.
func (r *Runtime) onExit(p *process, waitErr error) {
	key := string(p.tenant) + "\x00" + p.pluginID
	r.mu.Lock()
	s := r.slots[key]
	r.mu.Unlock()
	expected := p.stopping.Load()
	if !expected {
		message := "plugin process exited"
		if waitErr != nil {
			message = "plugin process exited: " + waitErr.Error()
		}
		p.log.Error(p.clean(message), "event", "plugin.exited")
		r.metrics.Add(telemetry.PluginFailures, 1, telemetry.Labels{"plugin": p.pluginID, "reason": "exited", telemetry.LabelTenant: string(p.tenant)})
	}
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.proc == p {
		s.proc = nil
	}
	if !expected {
		if r.now().Sub(p.started) >= stableAfter {
			s.failures = 0
		}
		s.recordFailure(r.now())
	}
}

func (r *Runtime) reapLoop() {
	defer close(r.reapDone)
	interval := max(r.opts.IdleTimeout/4, 10*time.Millisecond)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			r.reap()
		case <-r.reapStop:
			return
		}
	}
}

// reap stops processes idle for too long. They restart on next use (I262).
func (r *Runtime) reap() {
	r.mu.Lock()
	slots := make([]*slot, 0, len(r.slots))
	for _, s := range r.slots {
		slots = append(slots, s)
	}
	r.mu.Unlock()
	now := r.now()
	for _, s := range slots {
		select {
		case s.lock <- struct{}{}:
		default:
			continue // someone is using it
		}
		r.mu.Lock()
		p := s.proc
		r.mu.Unlock()
		if p != nil && p.inFlight() == 0 && now.Sub(p.idleSince()) >= r.opts.IdleTimeout {
			ctx, cancel := context.WithTimeout(context.Background(), r.opts.ShutdownTimeout+r.opts.KillTimeout)
			p.stop(ctx)
			cancel()
			r.mu.Lock()
			if s.proc == p {
				s.proc = nil
			}
			r.mu.Unlock()
		}
		s.release()
	}
}

// stopPlugin stops a plugin's processes in every tenant, as after an upgrade.
func (r *Runtime) stopPlugin(ctx context.Context, pluginID string) {
	suffix := "\x00" + pluginID
	r.mu.Lock()
	var slots []*slot
	for key, s := range r.slots {
		if strings.HasSuffix(key, suffix) {
			slots = append(slots, s)
		}
	}
	r.mu.Unlock()
	for _, s := range slots {
		if err := s.acquire(ctx); err != nil {
			continue
		}
		r.mu.Lock()
		p := s.proc
		s.proc = nil
		r.mu.Unlock()
		if p != nil {
			p.stop(ctx)
		}
		s.release()
	}
}

// Close stops every plugin process. It is the last call.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	slots := make([]*slot, 0, len(r.slots))
	for _, s := range r.slots {
		slots = append(slots, s)
	}
	r.mu.Unlock()
	close(r.reapStop)
	<-r.reapDone
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.acquire(ctx); err == nil {
				defer s.release()
			}
			r.mu.Lock()
			p := s.proc
			s.proc = nil
			r.mu.Unlock()
			if p != nil {
				p.stop(ctx)
			}
		}()
	}
	wg.Wait()
	return nil
}

// ---- hook.ActionRunner ----

func hookIdempotencyKey(inv hook.Invocation) string {
	h := sha256.New()
	for _, part := range []string{string(inv.TenantID), inv.JobID, inv.WorkflowID, inv.NodeID, string(inv.Point), inv.Hook.ID} {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RunHookAction implements hook.ActionRunner. Order, retry and failure policy belong to
// the dispatcher (§8.1); this runs one action under its timeout.
func (r *Runtime) RunHookAction(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
	timeout := time.Duration(inv.Hook.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = hook.DefaultTimeoutSeconds * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "plugin.hook")
	span.SetAttributes(
		attribute.String(telemetry.LabelTenant, string(inv.TenantID)),
		attribute.String("plugin", inv.Hook.Plugin),
		attribute.String("hook.action", inv.Hook.Action),
		attribute.String("hook.point", string(inv.Point)),
	)
	defer span.End()

	start := r.now()
	result, err := r.runHook(ctx, inv)
	r.measure(inv.TenantID, inv.Hook.Plugin, plugin.MethodHookInvoke, start, err)
	if err != nil {
		span.SetStatus(codes.Error, string(errs.CodeOf(err)))
		r.logger.Warn("plugin hook action failed", "event", "plugin.hook_failed", "tenant_id", string(inv.TenantID), "plugin", inv.Hook.Plugin,
			"job_id", inv.JobID, "node_id", inv.NodeID, "status", string(errs.CodeOf(err)))
		return hook.Result{}, err
	}
	span.SetStatus(codes.Ok, "")
	r.logger.Debug("plugin hook action finished", "event", "plugin.hook_finished", "tenant_id", string(inv.TenantID), "plugin", inv.Hook.Plugin,
		"job_id", inv.JobID, "node_id", inv.NodeID, "duration", r.now().Sub(start))
	return result, nil
}

func (r *Runtime) runHook(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
	rec, m, perms, err := r.resolvePlugin(ctx, inv.TenantID, inv.Hook.Plugin)
	if err != nil {
		return hook.Result{}, err
	}
	if !slices.Contains(m.Provides.HookActions, inv.Hook.Action) {
		return hook.Result{}, errs.Newf(errs.CodeFailedPrecondition, "plugin %s provides no hook action %q", rec.ID, inv.Hook.Action)
	}
	p, err := r.acquire(ctx, inv.TenantID, rec, m, perms)
	if err != nil {
		return hook.Result{}, r.mapContext(ctx, err)
	}
	if inv.IdempotencyKey == "" {
		inv.IdempotencyKey = hookIdempotencyKey(inv)
	}
	if inv.TraceID == "" {
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			inv.TraceID = sc.TraceID().String()
		}
	}
	if len(inv.Data) > 0 {
		inv.Data = r.redact.JSON(inv.Data)
	}
	var result hook.Result
	if err := p.call(ctx, plugin.MethodHookInvoke, inv, &result); err != nil {
		return hook.Result{}, r.mapCallError(ctx, p, err)
	}
	p.timeouts.Store(0)
	if len(result.Parameters) > maxHookParameters {
		return hook.Result{}, errs.New(errs.CodeOutputTooLarge, "the hook returned parameters over the limit")
	}
	if len(result.Reason) > maxHookReason {
		result.Reason = result.Reason[:maxHookReason]
	}
	result.Reason = p.clean(result.Reason)
	return result, nil
}

func (r *Runtime) measure(tenant domain.TenantID, pluginID, method string, start time.Time, err error) {
	r.metrics.Observe(telemetry.PluginDuration, r.now().Sub(start).Seconds(), telemetry.Labels{"plugin": pluginID, "method": method, telemetry.LabelTenant: string(tenant)})
	if err != nil {
		r.metrics.Add(telemetry.PluginFailures, 1, telemetry.Labels{"plugin": pluginID, "reason": string(errs.CodeOf(err)), telemetry.LabelTenant: string(tenant)})
	}
}

// mapContext names a deadline that ran out while waiting for the process.
func (r *Runtime) mapContext(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errs.New(errs.CodeTimeout, "the plugin did not answer in time")
	}
	return err
}

// mapCallError turns a failed call into a coded error with a clean message. A process
// that keeps timing out is restarted, because a plugin can ignore $/cancelRequest.
func (r *Runtime) mapCallError(ctx context.Context, p *process, err error) error {
	var rpcErr *jsonrpc.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		if p.timeouts.Add(1) >= consecutiveTimeouts {
			p.log.Warn("plugin keeps timing out and is restarted", "event", "plugin.restarted")
			p.kill()
		}
		return errs.New(errs.CodeTimeout, "the plugin did not answer in time")
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &rpcErr):
		message := rpcErr.Message
		if len(message) > maxHookReason {
			message = message[:maxHookReason]
		}
		if rpcErr.Code == jsonrpc.CodeMethodNotFound {
			return errs.Newf(errs.CodeFailedPrecondition, "plugin %s does not implement this call", p.pluginID)
		}
		return errs.Newf(errs.CodeInternal, "plugin %s failed: %s", p.pluginID, p.clean(message))
	}
	return errs.Wrap(errs.CodeUnavailable, err, "plugin "+p.pluginID+" is not available")
}

// ---- plugin.EventSubscriber ----

// EventSubscribers implements plugin.EventSubscriber. A plugin whose grant cannot be
// read is left out and logged: it must not receive events it may not be allowed, and
// the others must still get theirs.
func (r *Runtime) EventSubscribers(ctx context.Context, tenant domain.TenantID, eventType event.Type) ([]string, error) {
	if tenant == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "events are tenant-scoped")
	}
	records, err := r.opts.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rec := range records {
		if !rec.Enabled {
			continue
		}
		m, err := ParseManifest(rec.Manifest)
		if err != nil || !slices.Contains(m.Provides.EventSubscriptions, string(eventType)) {
			continue
		}
		_, _, perms, err := r.resolvePlugin(ctx, tenant, rec.ID)
		if err != nil {
			if !errs.IsCode(err, errs.CodeForbidden) {
				r.logger.Warn("plugin left out of event delivery", "event", "plugin.grant_invalid", "tenant_id", string(tenant), "plugin", rec.ID, "status", string(errs.CodeOf(err)))
			}
			continue
		}
		if slices.Contains(perms.Events, string(eventType)) {
			out = append(out, rec.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// DeliverEvent implements plugin.EventSubscriber. Every call checks the grant again,
// and the plugin sees only its own tenant's events because a process serves one tenant.
func (r *Runtime) DeliverEvent(ctx context.Context, pluginID string, e event.Envelope) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "plugin.event")
	span.SetAttributes(
		attribute.String(telemetry.LabelTenant, string(e.TenantID)),
		attribute.String("plugin", pluginID),
		attribute.String("event.type", string(e.Type)),
	)
	defer span.End()
	start := r.now()
	err := r.deliver(ctx, pluginID, e)
	r.measure(e.TenantID, pluginID, plugin.MethodEventDeliver, start, err)
	if err != nil {
		span.SetStatus(codes.Error, string(errs.CodeOf(err)))
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (r *Runtime) deliver(ctx context.Context, pluginID string, e event.Envelope) error {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, hook.DefaultTimeoutSeconds*time.Second)
		defer cancel()
	}
	rec, m, perms, err := r.resolvePlugin(ctx, e.TenantID, pluginID)
	if err != nil {
		return err
	}
	if !slices.Contains(m.Provides.EventSubscriptions, string(e.Type)) || !slices.Contains(perms.Events, string(e.Type)) {
		return errs.Newf(errs.CodeForbidden, "plugin %s may not receive event %s", pluginID, e.Type)
	}
	p, err := r.acquire(ctx, e.TenantID, rec, m, perms)
	if err != nil {
		return r.mapContext(ctx, err)
	}
	if len(e.Payload) > 0 {
		e.Payload = r.redact.JSON(e.Payload)
	}
	if err := p.call(ctx, plugin.MethodEventDeliver, e, nil); err != nil {
		return r.mapCallError(ctx, p, err)
	}
	p.timeouts.Store(0)
	return nil
}

// ---- Doctor ----

// Check is one plugin's state for `vidyom doctor`.
type Check struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Enabled   bool   `json:"enabled"`
	OK        bool   `json:"ok"`
	Isolation string `json:"isolation"`
	Reason    string `json:"reason,omitempty"`
}

// Verify checks every installed plugin's integrity. It changes nothing; the check at
// process start is what disables a plugin.
func (r *Runtime) Verify(ctx context.Context) ([]Check, error) {
	records, err := r.opts.Repo.List(ctx)
	if err != nil {
		return nil, err
	}
	isolation := r.isolationName()
	out := make([]Check, 0, len(records))
	for _, rec := range records {
		c := Check{ID: rec.ID, Version: rec.Version, Enabled: rec.Enabled, Isolation: isolation}
		m, err := ParseManifest(rec.Manifest)
		if err == nil {
			_, err = r.store.verify(rec.ID, rec.Version, rec.SHA256, m)
		}
		if err != nil {
			c.Reason = r.redact.Error(err)
		} else {
			c.OK = true
		}
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}
