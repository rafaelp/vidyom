package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jsonrpc"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/observability/redact"
)

const (
	maxLogMessage    = 8 << 10
	maxHostCallsRate = 1000 // per second, more is a flood (I271)
)

// process is one running plugin for one tenant.
type process struct {
	r        *Runtime
	tenant   domain.TenantID
	pluginID string
	sha256   string
	manifest plugin.Manifest
	perms    plugin.Permissions
	permKey  string

	cmd      *exec.Cmd
	conn     *jsonrpc.Conn
	runDir   string
	proxy    *egressProxy
	redactor *redact.Redactor
	log      *slog.Logger

	slots    chan struct{} // in-flight limit
	exited   chan struct{}
	stopping atomic.Bool
	started  time.Time
	lastUsed atomic.Int64
	timeouts atomic.Int32

	cleanup sync.Once

	rateMu     sync.Mutex
	rateWindow time.Time
	rateCount  int
}

// spawn starts the process and completes the handshake. Any failure leaves nothing
// running. The caller holds this key's slot.
func (r *Runtime) spawn(ctx context.Context, tenant domain.TenantID, rec domain.PluginRecord, m plugin.Manifest, perms plugin.Permissions) (*process, error) {
	root, err := r.store.verify(rec.ID, rec.Version, rec.SHA256, m)
	if err != nil {
		if errors.Is(err, errIntegrity) {
			r.quarantine(ctx, tenant, rec, err)
		}
		return nil, errs.Wrap(errs.CodeFailedPrecondition, err, "plugin "+rec.ID+" failed its integrity check and was disabled")
	}
	runDir, err := os.MkdirTemp(r.runRoot(tenant), rec.ID+"-")
	if err != nil {
		return nil, fmt.Errorf("create plugin run directory: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(runDir); err == nil {
		runDir = resolved
	}
	p := &process{
		r:        r,
		tenant:   tenant,
		pluginID: rec.ID,
		sha256:   rec.SHA256,
		manifest: m,
		perms:    perms,
		permKey:  permissionsKey(perms),
		runDir:   runDir,
		redactor: redact.New(),
		log:      r.logger.With("tenant_id", string(tenant), "plugin", rec.ID),
		slots:    make(chan struct{}, r.opts.MaxInFlight),
		exited:   make(chan struct{}),
		started:  r.now(),
	}
	p.touch()
	started := false
	defer func() {
		if !started {
			p.releaseResources()
		}
	}()

	tempDir := ""
	if slices.Contains(perms.Filesystem, PermJobTemp) {
		tempDir = filepath.Join(runDir, "tmp")
		if err := os.Mkdir(tempDir, 0o700); err != nil {
			return nil, fmt.Errorf("create plugin temp directory: %w", err)
		}
	}
	proxyURL := ""
	proxyPort := 0
	if len(perms.Network) > 0 {
		rules := make([]networkRule, 0, len(perms.Network))
		for _, text := range perms.Network {
			rule, err := parseNetworkRule(text)
			if err != nil {
				return nil, err
			}
			rules = append(rules, rule)
		}
		p.proxy, err = startProxy(rules, p.log, func(string, int) {
			r.metrics.Add(telemetry.SecurityAnomalies, 1, telemetry.Labels{"kind": "plugin_network_denied", telemetry.LabelTenant: string(tenant)})
		})
		if err != nil {
			return nil, err
		}
		proxyURL = p.proxy.URL()
		proxyPort = p.proxy.Port()
		p.redactor.AddValue(p.proxy.token)
	}

	readGrants, writeGrants, err := splitFilesystem(perms.Filesystem, tempDir)
	if err != nil {
		return nil, err
	}
	entry := entrypointPath(root, m)
	pol := policy{
		entry:      entry,
		pluginDir:  root,
		readPaths:  append(existing(systemReadPaths()), existing(readGrants)...),
		writePaths: existing(writeGrants),
		execPaths:  existing(r.opts.RuntimePaths),
		proxyPort:  proxyPort,
	}
	iso := r.opts.isolator
	if err := iso.check(pol); err != nil {
		if !r.opts.AllowUnsandboxed {
			return nil, errs.Wrap(errs.CodeFailedPrecondition, err,
				"plugin "+rec.ID+" cannot be sandboxed on this host: "+err.Error()+". Set plugins.allowUnsandboxed in daemon.json to run it with no isolation")
		}
		iso = noIsolation{}
	}
	if iso.name() == IsolationNone {
		p.log.Warn("plugin runs with no OS sandbox", "event", "plugin.unsandboxed")
		r.metrics.Add(telemetry.SecurityAnomalies, 1, telemetry.Labels{"kind": "plugin_unsandboxed", telemetry.LabelTenant: string(tenant)})
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("plugin pipes: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, fmt.Errorf("plugin pipes: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("plugin pipes: %w", err)
	}
	cmd := &exec.Cmd{
		Path:   entry,
		Args:   append([]string{entry}, m.Entrypoint.Args...),
		Dir:    root,
		Env:    scrubbedEnv(rec.ID, string(tenant), tempDir, proxyURL),
		Stdin:  stdinR,
		Stdout: stdoutW,
		Stderr: stderrW,
	}
	setProcessGroup(cmd)
	err = iso.start(cmd, pol)
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdinW.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, errs.Wrap(errs.CodeUnavailable, err, "plugin "+rec.ID+" could not start")
	}
	started = true
	p.cmd = cmd
	p.conn = jsonrpc.NewConn(stdoutR, stdinW, stdinW, p.handleHostCall)

	connDone := make(chan struct{})
	go func() {
		defer close(connDone)
		// The context is not the caller's: the connection lives as long as the process.
		_ = p.conn.Run(context.Background())
		stdoutR.Close()
		// A closed connection, for instance over an oversized message, ends the process.
		if !p.hasExited() {
			killGroup(cmd.Process.Pid)
		}
	}()
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		p.drainStderr(stderrR)
		stderrR.Close()
	}()
	go func() {
		err := cmd.Wait()
		close(p.exited)
		_ = p.conn.Close()
		killGroup(cmd.Process.Pid) // anything it left behind
		<-connDone
		<-stderrDone
		p.releaseResources()
		r.onExit(p, err)
	}()

	initCtx, cancel := context.WithTimeout(ctx, r.opts.InitTimeout)
	defer cancel()
	var result plugin.InitializeResult
	params := plugin.InitializeParams{ProtocolVersion: plugin.ProtocolVersion, Permissions: perms, TenantID: tenant, TempDir: tempDir}
	if err := p.conn.Call(initCtx, plugin.MethodInitialize, params, &result); err != nil {
		p.kill()
		return nil, startFailure{errs.Wrap(errs.CodeUnavailable, err, "plugin "+rec.ID+" did not complete initialize")}
	}
	if err := checkHandshake(result, m); err != nil {
		p.kill()
		return nil, startFailure{err}
	}
	p.log.Info("plugin started", "event", "plugin.started", "isolation", iso.name(), "version", rec.Version)
	return p, nil
}

// startFailure marks an error from a process that did start, which counts toward the
// restart backoff. A refusal before starting does not: nothing ran, and nothing would
// get better with waiting.
type startFailure struct{ error }

func (e startFailure) Unwrap() error { return e.error }

// checkHandshake refuses a process that is not what was installed, or claims more than
// its manifest (I271).
func checkHandshake(result plugin.InitializeResult, m plugin.Manifest) error {
	if result.ProtocolVersion != plugin.ProtocolVersion {
		return errs.Newf(errs.CodeFailedPrecondition, "plugin %s speaks protocol %d, this build speaks %d", m.ID, result.ProtocolVersion, plugin.ProtocolVersion)
	}
	if result.PluginID != m.ID {
		return errs.Newf(errs.CodeFailedPrecondition, "plugin %s answered as %q", m.ID, result.PluginID)
	}
	claims := []struct {
		field    string
		claimed  []string
		declared []string
	}{
		{"hookActions", result.Provides.HookActions, m.Provides.HookActions},
		{"eventSubscriptions", result.Provides.EventSubscriptions, m.Provides.EventSubscriptions},
		{"nodeTypes", result.Provides.NodeTypes, m.Provides.NodeTypes},
		{"storageDrivers", result.Provides.StorageDrivers, m.Provides.StorageDrivers},
		{"publishingDrivers", result.Provides.PublishingDrivers, m.Provides.PublishingDrivers},
		{"catalogDrivers", result.Provides.CatalogDrivers, m.Provides.CatalogDrivers},
		{"logSinkDrivers", result.Provides.LogSinkDrivers, m.Provides.LogSinkDrivers},
	}
	for _, c := range claims {
		for _, item := range c.claimed {
			if !contains(c.declared, item) {
				return errs.Newf(errs.CodeFailedPrecondition, "plugin %s claims %s %q, which its manifest does not declare", m.ID, c.field, item)
			}
		}
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func (p *process) touch() { p.lastUsed.Store(p.r.now().UnixNano()) }

func (p *process) hasExited() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

func (p *process) idleSince() time.Time { return time.Unix(0, p.lastUsed.Load()) }

func (p *process) inFlight() int { return len(p.slots) }

// kill ends the process now. Once it has exited its PID is not ours to signal.
func (p *process) kill() {
	p.stopping.Store(true)
	if p.cmd != nil && p.cmd.Process != nil && !p.hasExited() {
		killGroup(p.cmd.Process.Pid)
	}
}

// stop shuts the process down gracefully within limits: shutdown, then SIGTERM, then
// SIGKILL (I271).
func (p *process) stop(ctx context.Context) {
	if p.cmd == nil {
		return
	}
	p.stopping.Store(true)
	if !p.hasExited() {
		callCtx, cancel := context.WithTimeout(ctx, p.r.opts.ShutdownTimeout)
		_ = p.conn.Call(callCtx, plugin.MethodShutdown, struct{}{}, nil)
		cancel()
		_ = p.conn.Close() // closes stdin, which is the signal for a plugin that ignored shutdown
	}
	if p.waitExit(ctx, p.r.opts.ShutdownTimeout) {
		return
	}
	terminateGroup(p.cmd.Process.Pid)
	if p.waitExit(ctx, p.r.opts.KillTimeout) {
		return
	}
	killGroup(p.cmd.Process.Pid)
	<-p.exited
}

func (p *process) waitExit(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-p.exited:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (p *process) releaseResources() {
	p.cleanup.Do(func() {
		if p.proxy != nil {
			p.proxy.Close()
		}
		_ = os.RemoveAll(p.runDir)
	})
}

// allowHostCall is the flood guard: Conn.Run starts one goroutine per message, so a
// plugin must not be able to send unlimited ones (I271).
func (p *process) allowHostCall() bool {
	now := p.r.now()
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	if now.Sub(p.rateWindow) >= time.Second {
		p.rateWindow = now
		p.rateCount = 0
	}
	p.rateCount++
	return p.rateCount <= p.r.opts.MaxHostCallsPerSecond
}

func (p *process) handleHostCall(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if !p.allowHostCall() {
		if p.stopping.CompareAndSwap(false, true) {
			p.log.Error("plugin flooded the daemon and was killed", "event", "plugin.flood")
			p.r.metrics.Add(telemetry.SecurityAnomalies, 1, telemetry.Labels{"kind": "plugin_flood", telemetry.LabelTenant: string(p.tenant)})
			p.kill()
		}
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeUnauthorized, Message: "too many calls"}
	}
	switch method {
	case plugin.MethodHostSecret:
		return p.hostSecret(ctx, params)
	case plugin.MethodHostLog:
		p.hostLog(ctx, params)
		return struct{}{}, nil
	case jsonrpc.MethodProgress, jsonrpc.MethodCancelRequest:
		return struct{}{}, nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
}

// hostSecret is the only way a plugin gets a secret (I266). Both outcomes are audited,
// by name, and a secret whose access cannot be audited is not handed out.
func (p *process) hostSecret(ctx context.Context, params json.RawMessage) (any, error) {
	var req plugin.HostSecretParams
	if err := json.Unmarshal(params, &req); err != nil || req.Name == "" {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "host.secret needs a name"}
	}
	granted := contains(p.perms.Secrets, req.Name)
	result := "allowed"
	if !granted {
		result = "denied"
	}
	if err := p.r.auditSecret(ctx, p, req.Name, result); err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeUnavailable, Message: "secret access cannot be audited"}
	}
	if !granted {
		p.log.Warn("plugin asked for a secret it was not granted", "event", "plugin.secret_denied", "secret_name", req.Name)
		p.r.metrics.Add(telemetry.SecurityAnomalies, 1, telemetry.Labels{"kind": "plugin_secret_denied", telemetry.LabelTenant: string(p.tenant)})
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeUnauthorized, Message: "this plugin has no permission for that secret"}
	}
	if p.r.opts.Secrets == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeUnavailable, Message: "secrets are not available"}
	}
	resolver := p.r.opts.Secrets(p.tenant)
	if resolver == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeUnavailable, Message: "secrets are not available"}
	}
	value, err := resolver.ResolveSecret(ctx, req.Name)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeUnavailable, Message: "the secret is not available"}
	}
	p.redactor.AddValue(string(value))
	return plugin.HostSecretResult{Value: string(value)}, nil
}

// clean removes what this process was given, then applies the general rules.
func (p *process) clean(text string) string { return p.redactor.String(text) }

func (p *process) hostLog(ctx context.Context, params json.RawMessage) {
	var req plugin.HostLogParams
	if err := json.Unmarshal(params, &req); err != nil {
		return
	}
	level := slog.LevelInfo
	switch strings.ToLower(req.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	event := req.Event
	if event == "" {
		event = "plugin.log"
	}
	if len(req.Message) > maxLogMessage {
		req.Message = req.Message[:maxLogMessage]
	}
	args := []any{"event", p.clean(event)}
	for key, value := range p.redactor.Fields(req.Fields) {
		args = append(args, slog.Any("plugin."+key, value))
	}
	p.log.Log(ctx, level, p.clean(req.Message), args...)
}

// drainStderr logs what a plugin prints. It never stops reading, so the plugin can
// never block on stderr.
func (p *process) drainStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	logged := 0
	window := p.r.now()
	for scanner.Scan() {
		if now := p.r.now(); now.Sub(window) >= time.Second {
			window, logged = now, 0
		}
		logged++
		if logged > p.r.opts.MaxHostCallsPerSecond {
			continue
		}
		line := scanner.Text()
		if len(line) > maxLogMessage {
			line = line[:maxLogMessage]
		}
		p.log.Warn(p.clean(line), "event", "plugin.stderr")
	}
	// A line over the buffer ends the scanner: keep the pipe from filling.
	_, _ = io.Copy(io.Discard, r)
}

// call runs one method on the process, inside the in-flight limit.
func (p *process) call(ctx context.Context, method string, params, result any) error {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return ctx.Err()
	case <-p.exited:
		return errProcessExited
	}
	p.touch()
	defer p.touch()
	return p.conn.Call(ctx, method, params, result)
}

var errProcessExited = errors.New("plugin process exited")

func (r *Runtime) auditSecret(ctx context.Context, p *process, name, result string) error {
	return r.opts.Auditor.Record(ctx, security.AuditEntry{
		TenantID:  p.tenant,
		Principal: domain.Principal{Email: "plugin:" + p.pluginID, Method: domain.AuthSystem},
		Action:    "plugin.secret.read",
		Resource:  "secret:" + name,
		Result:    result,
		Details:   map[string]any{"plugin": p.pluginID},
	})
}
