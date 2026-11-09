package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rafaelp/vidyom/connectors/ai/bgem3"
	"github.com/rafaelp/vidyom/connectors/ai/openai"
	"github.com/rafaelp/vidyom/connectors/ai/workerclient"
	"github.com/rafaelp/vidyom/connectors/catalog/mediacms"
	"github.com/rafaelp/vidyom/connectors/catalog/public"
	"github.com/rafaelp/vidyom/connectors/catalog/rewind"
	"github.com/rafaelp/vidyom/connectors/catalog/videovortex"
	instagramreels "github.com/rafaelp/vidyom/connectors/publishing/instagram-reels"
	"github.com/rafaelp/vidyom/connectors/publishing/mock"
	"github.com/rafaelp/vidyom/connectors/publishing/peertube"
	pubresolver "github.com/rafaelp/vidyom/connectors/publishing/resolver"
	"github.com/rafaelp/vidyom/connectors/publishing/vimeo"
	"github.com/rafaelp/vidyom/connectors/publishing/youtube"
	"github.com/rafaelp/vidyom/connectors/storage/gcs"
	"github.com/rafaelp/vidyom/connectors/storage/local"
	storageresolver "github.com/rafaelp/vidyom/connectors/storage/resolver"
	fasterwhisper "github.com/rafaelp/vidyom/connectors/transcription/faster-whisper"
	apiserver "github.com/rafaelp/vidyom/internal/api"
	"github.com/rafaelp/vidyom/internal/catalog"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/daemon"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/internal/hooks"
	"github.com/rafaelp/vidyom/internal/jobs"
	"github.com/rafaelp/vidyom/internal/observability"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/plugins"
	"github.com/rafaelp/vidyom/internal/publicexport"
	"github.com/rafaelp/vidyom/internal/queue"
	"github.com/rafaelp/vidyom/internal/retrieval"
	"github.com/rafaelp/vidyom/internal/scheduler"
	"github.com/rafaelp/vidyom/internal/security/audit"
	"github.com/rafaelp/vidyom/internal/security/authentication"
	"github.com/rafaelp/vidyom/internal/security/authorization"
	"github.com/rafaelp/vidyom/internal/security/vault"
	"github.com/rafaelp/vidyom/internal/tenants"
	"github.com/rafaelp/vidyom/internal/workflow"
	ainodes "github.com/rafaelp/vidyom/nodes/ai"
	approvalnodes "github.com/rafaelp/vidyom/nodes/approval"
	catalognodes "github.com/rafaelp/vidyom/nodes/catalog"
	controlnodes "github.com/rafaelp/vidyom/nodes/control"
	publishingnodes "github.com/rafaelp/vidyom/nodes/publishing"
	"github.com/rafaelp/vidyom/workflows"
)

func init() { wireDeps = assemble }

// proxyLinkTTL is how long a signed proxy link lasts where the driver signs one.
const proxyLinkTTL = time.Hour

// closeOrder collects what assemble opened, released in reverse.
type closeOrder struct{ fns []func() }

func (c *closeOrder) add(fn func()) { c.fns = append(c.fns, fn) }

func (c *closeOrder) run() {
	for i := len(c.fns) - 1; i >= 0; i-- {
		c.fns[i]()
	}
	c.fns = nil
}

// lateMetrics breaks a construction cycle: the tenant service records budget gauges,
// and the observability manager reads per-tenant sinks from that service.
type lateMetrics struct {
	next atomic.Pointer[telemetry.Metrics]
}

func (l *lateMetrics) use() telemetry.Metrics {
	if m := l.next.Load(); m != nil {
		return *m
	}
	return telemetry.Noop{}
}

func (l *lateMetrics) Add(name string, delta float64, labels telemetry.Labels) {
	l.use().Add(name, delta, labels)
}

func (l *lateMetrics) Set(name string, value float64, labels telemetry.Labels) {
	l.use().Set(name, value, labels)
}

func (l *lateMetrics) Observe(name string, value float64, labels telemetry.Labels) {
	l.use().Observe(name, value, labels)
}

// assemble builds the whole daemon from each component's entry point (master plan,
// section 9). With cfg.Bootstrapping, for `vidyomd init`, it stops once the identity
// services exist: init needs neither Redis nor a running engine (I138, I280).
func assemble(ctx context.Context, cfg config.Daemon) (_ daemon.Deps, cleanup func(), err error) {
	closers := &closeOrder{}
	cleanup = closers.run
	defer func() {
		if err != nil {
			closers.run()
			cleanup = func() {}
		}
	}()

	stores, err := persistence.Open(ctx, persistence.Options{
		DataDir:         cfg.DataDir,
		RequireExisting: !cfg.Bootstrapping,
	})
	if err != nil {
		return daemon.Deps{}, cleanup, fmt.Errorf("open the data directory: %w", err)
	}
	closers.add(func() { _ = stores.Close() })

	provider, err := vault.ProviderFor(cfg.Vault.Unseal)
	if err != nil {
		return daemon.Deps{}, cleanup, err
	}
	auditor := audit.New(stores.Tenants, audit.Options{System: stores.System})
	keys, err := vault.New(stores.System, stores.Tenants, provider, vault.Options{Auditor: auditor})
	if err != nil {
		return daemon.Deps{}, cleanup, err
	}
	closers.add(keys.Close)

	metrics := &lateMetrics{}
	auth := authentication.New(stores.System, authentication.Options{
		LocalUsers: cfg.LocalUsers,
		Auditor:    auditor,
		Metrics:    metrics,
	})
	authz := authorization.New(stores.System, authorization.Options{Metrics: metrics})
	tenantService := tenants.New(stores.System, stores.Tenants, tenants.Options{
		ConfigDir: cfg.ConfigDir,
		Keys:      keys,
		Metrics:   metrics,
	})

	deps := daemon.Deps{
		MasterKey: provider,
		API: apiserver.Deps{
			Authenticator: auth,
			Authorizer:    authz,
			Auditor:       auditor,
			Audit:         auditor.Service(),
			TenantStores:  stores.Tenants,
			Devices:       auth.Devices(),
			Tenants:       tenantService,
			Users:         tenantService.Users(),
			Budgets:       tenantService.Budgets(),
			Secrets:       keys.Secrets(),
			TenantConfig:  tenantService,
		},
		Bootstrap: func(ctx context.Context, req daemon.BootstrapRequest) (daemon.BootstrapResult, error) {
			out, err := tenantService.Bootstrap(ctx, tenants.BootstrapRequest{
				Tenant:      req.Tenant,
				AdminEmail:  req.AdminEmail,
				Credentials: auth,
			})
			if err != nil {
				return daemon.BootstrapResult{}, err
			}
			return daemon.BootstrapResult{
				PairingCode:      out.PairingCode,
				PairingExpiresAt: out.PairingExpiresAt,
				WorkerToken:      out.WorkerToken,
			}, nil
		},
	}
	if cfg.Bootstrapping {
		return deps, cleanup, nil
	}

	connectors := connector.NewRegistry()
	registerConnectors(connectors)

	observabilityOptions := observability.OptionsFromConfig(cfg)
	observabilityOptions.Registry = connectors
	observabilityOptions.Auditor = auditor
	observabilityOptions.Tenants = tenantService
	observabilityOptions.TenantSecrets = keys.Resolver
	manager, err := observability.New(cfg.Observability, observabilityOptions)
	if err != nil {
		return daemon.Deps{}, cleanup, fmt.Errorf("start observability: %w", err)
	}
	// Closed last: everything above may still log while shutting down.
	closers.add(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Close(closeCtx)
	})
	var managerMetrics telemetry.Metrics = manager
	metrics.next.Store(&managerMetrics)
	logger := manager.Logger()
	slog.SetDefault(logger)
	manager.InstallGlobals()

	assets := catalog.New(stores.Tenants, catalog.Options{})
	storages := storageresolver.New(storageresolver.Options{
		Drivers: connectors,
		Tenants: tenantService,
		Secrets: keys.Resolver,
		Assets:  assets,
		LinkTTL: proxyLinkTTL,
	})
	auditor.SetStorage(storages)

	pluginOptions := plugins.OptionsFromConfig(cfg)
	pluginOptions.Repo = stores.System.Plugins()
	pluginOptions.Auditor = auditor
	pluginOptions.Secrets = keys.Resolver
	pluginOptions.Metrics = manager
	pluginOptions.Logger = logger
	pluginRuntime, err := plugins.New(pluginOptions)
	if err != nil {
		return daemon.Deps{}, cleanup, fmt.Errorf("start the plugin runtime: %w", err)
	}
	closers.add(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = pluginRuntime.Close(closeCtx)
	})

	exporter := publicexport.New(publicexport.Options{DataDir: cfg.DataDir, Keys: keys, Logger: logger})

	nodes := node.NewRegistry()
	controlnodes.Register(nodes)
	approvalnodes.Register(nodes)
	catalognodes.Register(nodes)
	ainodes.Register(nodes)
	publishingnodes.Register(nodes, publishingnodes.Options{Exporter: exporter, Connectors: connectors})

	validator := workflow.NewValidator(nodes)
	registry, err := workflow.LoadRegistry(validator, workflows.FS, cfg.Workflows.Dirs)
	if err != nil {
		return daemon.Deps{}, cleanup, fmt.Errorf("load the workflows: %w", err)
	}

	redisOptions, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return daemon.Deps{}, cleanup, fmt.Errorf("parse the Redis URL: %w", err)
	}
	redisClient := redis.NewClient(redisOptions)
	closers.add(func() { _ = redisClient.Close() })
	jobQueue := queue.NewRedis(redisClient)

	sched := scheduler.New(scheduler.Options{
		Tenants:  stores.Tenants,
		Triggers: registry,
		Workers:  stores.System.Workers(),
		Logger:   logger,
	})
	bus := events.New(events.Options{
		Tenants:  stores.Tenants,
		Triggers: registry,
		Plugins:  pluginRuntime,
		Logger:   logger,
	})
	publishingResolver := pubresolver.New(connectors, tenantService, keys.Resolver)
	engine := jobs.New(jobs.Deps{
		Tenants:    stores.Tenants,
		Queue:      jobQueue,
		Nodes:      nodes,
		Workflows:  registry,
		Validator:  validator,
		Bus:        bus,
		Scheduler:  sched,
		Hooks:      hooks.New(pluginRuntime, hooks.Options{Logger: logger}),
		Config:     tenantService,
		Daemon:     cfg,
		Storage:    storages,
		Publishing: publishingResolver,
		Connectors: connectors,
		SecretsFor: keys.Resolver,
		Keys:       keys,
		Budgets:    tenantService.Budgets(),
		Plugins:    stores.System.Plugins(),
		Metrics:    manager,
		Logger:     logger,
	})

	search := retrieval.New(retrieval.Options{
		Stores: stores.Tenants,
		Embedder: func(ctx context.Context, tenant domain.TenantID) (connector.Embedder, error) {
			tenantConfig, err := tenantService.TenantConfig(ctx, tenant)
			if err != nil {
				return nil, err
			}
			factory, err := connectors.Embedder(jobs.DefaultEmbedderDriver)
			if err != nil {
				return nil, err
			}
			return factory(ctx, connector.AIParams{
				Tenant:  tenant,
				AI:      tenantConfig.AI,
				Daemon:  cfg.AIWorker,
				Secrets: keys.Resolver(tenant),
			})
		},
		Logger: logger,
	})

	publications := publishingnodes.NewService(publishingnodes.ServiceDeps{
		Stores:     stores.Tenants,
		Workflows:  engine.Workflows(),
		Resolver:   publishingResolver,
		Config:     tenantService,
		Connectors: connectors,
	})

	maintenance := tenants.NewMaintenance(stores.Tenants, tenants.MaintenanceOptions{
		Audit:     auditor,
		Retention: cfg.Retention,
	})

	deps.API.Workflows = engine.Workflows()
	deps.API.Jobs = engine.Jobs()
	deps.API.Approvals = engine.Approvals()
	deps.API.Introspection = engine.Introspection()
	deps.API.Coordinator = engine.Coordinator()
	deps.API.Events = engine.Bus()
	deps.API.Assets = assets
	// The guard is required: without it a viewer would read sensitive text through
	// search hits (I118).
	deps.API.Search = catalog.NewSearchGuard(search, stores.Tenants)
	deps.API.Publications = publications
	deps.API.Storages = storages
	deps.API.Plugins = pluginRuntime
	deps.API.Observability = manager
	deps.API.Connectors = connectors
	deps.API.Metrics = manager
	deps.API.Logger = logger
	if check := aiWorkerHealth(cfg.AIWorker); check != nil {
		deps.API.HealthChecks = append(deps.API.HealthChecks, *check)
	}

	deps.Logger = logger
	deps.Queue = jobQueue
	deps.Background = []daemon.BackgroundService{
		{Name: "workflow engine", Run: engine.Run},
		{Name: "maintenance", Run: func(ctx context.Context) error {
			maintenance.Run(ctx, func(err error) {
				if err != nil {
					logger.Warn("maintenance.sweep_incomplete", "error", err)
				}
			})
			return nil
		}},
	}
	deps.Reload = func(ctx context.Context) error {
		return tenantService.Reload(ctx)
	}
	return deps, cleanup, nil
}

// registerConnectors adds every driver the daemon can open. Skeletons register too and
// report Available: false, so an operator sees them refuse rather than go missing (I11).
func registerConnectors(r *connector.Registry) {
	local.Register(r)
	gcs.Register(r)

	fasterwhisper.Register(r)
	bgem3.Register(r)
	openai.Register(r)

	mock.Register(r)
	youtube.Register(r)
	vimeo.Register(r)
	peertube.Register(r)
	instagramreels.Register(r)

	public.Register(r)
	mediacms.Register(r)
	videovortex.Register(r)
	rewind.Register(r)

	observability.RegisterBuiltins(r)
}

// aiWorkerHealth reports the Python worker in /health, not as critical: a daemon with
// no AI worker still serves everything except AI work (I280).
func aiWorkerHealth(cfg config.AIWorker) *apiserver.HealthCheck {
	if cfg.Endpoint == "" {
		return nil
	}
	return &apiserver.HealthCheck{
		Name: "ai_worker",
		Check: func(ctx context.Context) error {
			client, err := workerclient.Shared(cfg)
			if err != nil {
				return err
			}
			_, err = client.Health(ctx)
			return err
		},
	}
}
