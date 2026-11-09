package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rafaelp/vidyom/connectors/ai/bgem3"
	"github.com/rafaelp/vidyom/connectors/storage/gcs"
	"github.com/rafaelp/vidyom/connectors/storage/local"
	fasterwhisper "github.com/rafaelp/vidyom/connectors/transcription/faster-whisper"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/observability"
	"github.com/rafaelp/vidyom/internal/queue"
	ainodes "github.com/rafaelp/vidyom/nodes/ai"
	audionodes "github.com/rafaelp/vidyom/nodes/audio"
	storagenodes "github.com/rafaelp/vidyom/nodes/storage"
	videonodes "github.com/rafaelp/vidyom/nodes/video"
)

func init() {
	wireRegistries = registerWorker
	wireQueue = openQueue
	wireMetrics = startMetrics
}

// metricsCloseTimeout bounds flushing exporters at shutdown.
const metricsCloseTimeout = 10 * time.Second

// startMetrics gives this worker its own exporters from the same configuration file.
// Log sinks stay the daemon's: a worker logs to stderr, and a file sink there would mean
// two writers. With a --metrics-addr, a Prometheus scraper reads this worker directly;
// an OTLP exporter pushes on its own.
func startMetrics(ctx context.Context, cfg config.Observability) (telemetry.Metrics, func(), error) {
	registry := connector.NewRegistry()
	observability.RegisterBuiltins(registry)
	manager, err := observability.New(config.Observability{Metrics: cfg.Metrics, Traces: cfg.Traces}, observability.Options{
		Service:  "vidyom-worker",
		Registry: registry,
	})
	if err != nil {
		return nil, nil, err
	}
	manager.InstallGlobals()
	stopSampler := observability.StartHostSampler(ctx, manager, observability.HostSamplerOptions{})
	var server *http.Server
	if metricsAddr != "" {
		listener, err := net.Listen("tcp", metricsAddr)
		if err != nil {
			stopSampler()
			_ = manager.Close(ctx)
			return nil, nil, fmt.Errorf("listen for metrics: %w", err)
		}
		server = &http.Server{Handler: manager.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = server.Serve(listener) }()
	}
	return manager, func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), metricsCloseTimeout)
		defer cancel()
		if server != nil {
			_ = server.Shutdown(closeCtx)
		}
		stopSampler()
		_ = manager.Close(closeCtx)
	}, nil
}

// registerWorker adds what this binary can run. nodes/publishing links cgo through
// internal/publicexport and never registers here (I7). ainodes also registers its
// daemon-pool nodes, which a worker never leases.
func registerWorker(nodes node.Registrar, connectors connector.Registrar) error {
	storagenodes.Register(nodes)
	videonodes.Register(nodes)
	audionodes.Register(nodes)
	ainodes.Register(nodes)

	local.Register(connectors)
	gcs.Register(connectors)
	fasterwhisper.Register(connectors)
	bgem3.Register(connectors)
	return nil
}

func openQueue(_ context.Context, cfg config.Redis) (jobqueue.Queue, func(), error) {
	options, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse the Redis URL: %w", err)
	}
	client := redis.NewClient(options)
	return queue.NewRedis(client), func() { _ = client.Close() }, nil
}
