package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	apiserver "github.com/rafaelp/vidyom/internal/api"
	"github.com/rafaelp/vidyom/internal/api/uploads"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/version"
)

const (
	masterKeySize   = 32
	shutdownTimeout = 30 * time.Second
	uploadSweep     = time.Hour
)

// BackgroundService runs for the daemon's life: the engine's loops, the scheduler,
// the event dispatcher, maintenance. An error stops the daemon.
type BackgroundService struct {
	Name string
	Run  func(ctx context.Context) error
}

// Deps is everything vidyomd runs on, assembled by cmd/vidyomd/wire.go.
type Deps struct {
	// API holds the services. Authenticator, Authorizer, Auditor and TenantStores
	// are required. Uploads and Doctor default to this component's own.
	API apiserver.Deps
	// MasterKey is checked before any listener. A nil provider, or one that fails,
	// means no listener at all (D13).
	MasterKey security.MasterKeyProvider
	// Queue is reported by /health and doctor. Optional.
	Queue jobqueue.Queue
	// Preflight runs in order before any listener, for what must be true before
	// serving, such as migrations. An error aborts startup.
	Preflight []func(ctx context.Context) error
	// Background services start once every listener is bound.
	Background []BackgroundService
	// Reload runs on SIGHUP, re-reading tenant configuration (I34). Optional.
	Reload func(ctx context.Context) error
	// Bootstrap backs `vidyomd init`. It is not used by Run.
	Bootstrap BootstrapFunc
	// OnReady is called once every listener is bound. cmd/vidyomd uses it for the
	// --detach handshake; tests use it to find their ports.
	OnReady func([]ListenerInfo)
	Logger  *slog.Logger
	// interfaces overrides the interface lookup, for tests, which cannot create
	// network interfaces.
	interfaces func(string) ([]net.IP, error)
}

func (d Deps) validate() error {
	var missing []string
	if d.API.Authenticator == nil {
		missing = append(missing, "Authenticator")
	}
	if d.API.Authorizer == nil {
		missing = append(missing, "Authorizer")
	}
	if d.API.Auditor == nil {
		missing = append(missing, "Auditor")
	}
	if d.API.TenantStores == nil {
		missing = append(missing, "TenantStores")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the daemon is missing required dependencies: %v", missing)
	}
	return nil
}

// checkMasterKey confirms the key can be unsealed, then forgets it: the vault keeps
// its own copy. Any failure means no listener (D13, I10).
func checkMasterKey(ctx context.Context, provider security.MasterKeyProvider) error {
	if provider == nil {
		return errors.New("no master key provider; the daemon does not start without one (D13)")
	}
	key, err := provider.Unseal(ctx)
	if err != nil {
		return fmt.Errorf("the master key cannot be unsealed through %s: %w", provider.Name(), err)
	}
	defer clear(key)
	if len(key) != masterKeySize {
		return fmt.Errorf("the master key from %s has %d bytes, not %d", provider.Name(), len(key), masterKeySize)
	}
	return nil
}

// Run starts the daemon and blocks until ctx ends or something fails. Everything
// that can refuse startup happens before the first socket is bound, and all sockets
// are bound before the first is served (I19, I134).
func Run(ctx context.Context, cfg config.Daemon, deps Deps) (err error) {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	if err := deps.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create the data directory: %w", err)
	}
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()

	if err := checkMasterKey(ctx, deps.MasterKey); err != nil {
		return err
	}
	for _, check := range deps.Preflight {
		if err := check(ctx); err != nil {
			return fmt.Errorf("preflight: %w", err)
		}
	}

	lookup := deps.interfaces
	if lookup == nil {
		lookup = interfaceAddresses
	}
	plans, fingerprint, err := planListeners(cfg, lookup)
	if err != nil {
		return err
	}

	api := deps.API
	if api.Logger == nil {
		api.Logger = log
	}
	if api.IngestRoots == nil {
		api.IngestRoots = cfg.IngestRoots
	}
	api.TLSFingerprint = fingerprint
	api.Sealing = string(deps.MasterKey.Sealing())
	if deps.Queue != nil {
		api.HealthChecks = append(api.HealthChecks, apiserver.HealthCheck{Name: "redis", Critical: true, Check: deps.Queue.Ping})
	}
	var sweeper *uploads.Service
	if api.Uploads == nil {
		sweeper, err = uploads.New(uploads.Options{Stores: api.TenantStores, SpoolDir: cfg.SpoolDir, Logger: log})
		if err != nil {
			return err
		}
		api.Uploads = sweeper
	} else if s, ok := api.Uploads.(*uploads.Service); ok {
		sweeper = s
	}
	stopping := make(chan struct{})
	api.Shutdown = stopping
	diagnostics := &doctor{cfg: cfg, deps: deps, api: api}
	if api.Doctor == nil {
		api.Doctor = diagnostics
	}

	// Recorded before anything can be served (I10). The audit is required for every
	// mutation, so failing here fails where everything else would.
	if err := api.Auditor.Record(ctx, security.AuditEntry{
		Principal: domain.Principal{Method: domain.AuthSystem},
		Action:    "daemon.boot",
		Result:    "allowed",
		Details: map[string]any{
			"provider": deps.MasterKey.Name(),
			"sealing":  string(deps.MasterKey.Sealing()),
			"version":  version.Version,
		},
	}); err != nil {
		return fmt.Errorf("the boot record cannot be written to the audit: %w", err)
	}

	bound, err := bindAll(ctx, plans)
	if err != nil {
		return err
	}
	infos := make([]ListenerInfo, len(bound))
	for i, b := range bound {
		infos[i] = b.info
	}
	diagnostics.setListeners(infos)

	handler := apiserver.NewHandler(api)
	servers := make([]*http.Server, len(bound))
	serveErrs := make(chan error, len(bound))
	for i, b := range bound {
		srv := newHTTPServer(handler, b, log)
		servers[i] = srv
		go func() {
			var err error
			if b.tls != nil {
				err = srv.ServeTLS(b.ln, "", "")
			} else {
				err = srv.Serve(b.ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrs <- fmt.Errorf("listener %s: %w", b.info.Address, err)
			}
		}()
	}

	bgCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	var bgWait sync.WaitGroup
	bgErrs := make(chan error, len(deps.Background)+1)
	startBackground := func(name string, run func(context.Context) error) {
		bgWait.Add(1)
		go func() {
			defer bgWait.Done()
			if err := run(bgCtx); err != nil && !errors.Is(err, context.Canceled) {
				bgErrs <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}
	for _, svc := range deps.Background {
		startBackground(svc.Name, svc.Run)
	}
	if sweeper != nil {
		startBackground("upload sweep", func(ctx context.Context) error {
			return sweepLoop(ctx, sweeper, log)
		})
	}

	removePID := writePID(cfg.DataDir, log)
	defer removePID()
	stopReload := watchReload(bgCtx, deps.Reload, log)
	defer stopReload()

	for _, info := range infos {
		log.Info("daemon.listening", "kind", info.Kind, "address", info.Address, "tls", string(info.TLSMode), "public", info.Public)
	}
	log.Info("daemon.ready", "version", version.Version, "sealing", string(deps.MasterKey.Sealing()))
	if deps.OnReady != nil {
		deps.OnReady(infos)
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("daemon.stopping")
	case runErr = <-serveErrs:
		log.Error("daemon.listener_failed", "error", runErr)
	case runErr = <-bgErrs:
		log.Error("daemon.service_failed", "error", runErr)
	}

	close(stopping)
	stopBackground()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	var wait sync.WaitGroup
	for _, srv := range servers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				_ = srv.Close()
			}
		}()
	}
	wait.Wait()
	bgWait.Wait()
	log.Info("daemon.stopped")
	return runErr
}

func newHTTPServer(handler http.Handler, b boundListener, log *slog.Logger) *http.Server {
	transport := "https"
	if b.info.Kind == "unix" {
		transport = "unix"
	}
	return &http.Server{
		Handler:   handler,
		TLSConfig: b.tls,
		// No read or write timeouts here: uploads and streams are long. Each request
		// sets its own deadlines (apiserver).
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			info := apiserver.ConnInfo{RemoteAddr: conn.RemoteAddr(), Transport: transport}
			if unixConn, ok := conn.(*net.UnixConn); ok {
				// With no readable credentials the connection simply has no identity.
				if uid, err := peerUID(unixConn); err == nil {
					info.PeerUID = &uid
				} else {
					log.Warn("daemon.peer_credentials_failed", "error", err)
				}
			}
			return apiserver.WithConnInfo(ctx, info)
		},
	}
}

func sweepLoop(ctx context.Context, sweeper *uploads.Service, log *slog.Logger) error {
	ticker := time.NewTicker(uploadSweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := sweeper.Sweep(ctx); err != nil {
				log.Warn("uploads.sweep_incomplete", "error", err)
			}
		}
	}
}

// writePID records the running daemon (I139). A failure is logged: nothing depends
// on the file, and the lock is what protects the data directory.
func writePID(dataDir string, log *slog.Logger) func() {
	path := filepath.Join(dataDir, "vidyomd.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		log.Warn("daemon.pid_file_failed", "error", err)
		return func() {}
	}
	return func() { _ = os.Remove(path) }
}

func watchReload(ctx context.Context, reload func(context.Context) error, log *slog.Logger) func() {
	if reload == nil {
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				if err := reload(ctx); err != nil {
					log.Error("daemon.reload_failed", "error", err)
				} else {
					log.Info("daemon.reloaded")
				}
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		<-done
	}
}
