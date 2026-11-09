// Command vidyom-worker runs tasks for the daemon: FFmpeg and storage in pool media,
// the Python AI worker's clients in pool ai. It stays free of cgo and of database
// code, and everything a task needs comes from its lease (D5, I4).
//
// The registries and the queue are assembled in wire.go, which belongs to the
// integration stage (master plan section 9). Until then this command exits with a
// clear "not wired" error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/version"
	"github.com/rafaelp/vidyom/internal/workers"
	"github.com/rafaelp/vidyom/internal/workers/client"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
)

// tokenFileEnv names the worker token's file, for deployments that prefer the
// environment. The token itself never travels in an argument or a variable (§9.1).
const tokenFileEnv = "VIDYOM_WORKER_TOKEN_FILE"

// wireRegistries registers the nodes and connectors this binary serves. wire.go sets
// it in init().
var wireRegistries func(node.Registrar, connector.Registrar) error

// wireQueue opens the job queue, whose implementation belongs to another component
// (I180). wire.go sets it in init(). The returned function releases the queue.
var wireQueue func(ctx context.Context, redis config.Redis) (jobqueue.Queue, func(), error)

// wireMetrics optionally gives the worker a metrics exporter. It stays nil until
// wire.go sets it, and the worker then records nothing.
var wireMetrics func(ctx context.Context, observability config.Observability) (telemetry.Metrics, func(), error)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// options is what the flags hold before they meet the configuration file.
type options struct {
	configPath     string
	pools          string
	workerID       string
	daemon         string
	tokenFile      string
	tlsFingerprint string
	tlsCAFile      string
	redisURL       string
	scratchDir     string
	cacheDir       string
	spoolDir       string
	ingestRoots    stringList
	ffmpegPath     string
	ffprobePath    string
	transcriber    string
	embedder       string
	maxConcurrency int
	cpuCores       int
	memoryMB       int
	diskMB         int
	gpu            bool
	shutdownGrace  time.Duration
	scratchMaxIdle time.Duration
	logLevel       string
	showVersion    bool
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("vidyom-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.configPath, "config", "", "the daemon.json to read directories, Redis and the AI worker from")
	fs.StringVar(&o.pools, "pools", "media", "comma-separated pools to serve: media, ai")
	fs.StringVar(&o.workerID, "worker-id", "", "worker ID; generated when empty")
	fs.StringVar(&o.daemon, "daemon", "", "daemon address: unix:///path or https://host:port")
	fs.StringVar(&o.tokenFile, "token-file", "", "file holding the worker token; or "+tokenFileEnv)
	fs.StringVar(&o.tlsFingerprint, "tls-fingerprint", "", "pin the daemon's certificate: sha256:<hex>")
	fs.StringVar(&o.tlsCAFile, "tls-ca-file", "", "verify the daemon against this CA")
	fs.StringVar(&o.redisURL, "redis-url", "", "Redis URL; overrides the configuration file")
	fs.StringVar(&o.scratchDir, "scratch-dir", "", "shared job scratch; overrides the configuration file")
	fs.StringVar(&o.cacheDir, "cache-dir", "", "worker cache; overrides the configuration file")
	fs.StringVar(&o.spoolDir, "spool-dir", "", "upload spool; overrides the configuration file")
	fs.Var(&o.ingestRoots, "ingest-root", "directory server: inputs may read; repeatable; overrides the configuration file")
	fs.StringVar(&o.ffmpegPath, "ffmpeg", "", "ffmpeg binary; default ffmpeg on PATH")
	fs.StringVar(&o.ffprobePath, "ffprobe", "", "ffprobe binary; default ffprobe on PATH")
	fs.StringVar(&o.transcriber, "transcriber", "", "transcription driver for pool ai; default faster-whisper")
	fs.StringVar(&o.embedder, "embedder", "", "embedding driver for pool ai; default bge-m3")
	fs.IntVar(&o.maxConcurrency, "max-concurrency", 0, "tasks at once; default cores / 4, at least 1")
	fs.IntVar(&o.cpuCores, "cpu-cores", 0, "declared CPU cores; detected when zero")
	fs.IntVar(&o.memoryMB, "memory-mb", 0, "declared memory; detected when zero")
	fs.IntVar(&o.diskMB, "disk-mb", 0, "declared disk; detected when zero")
	fs.BoolVar(&o.gpu, "gpu", false, "declare a GPU")
	fs.DurationVar(&o.shutdownGrace, "shutdown-grace", 0, "how long running nodes may finish at shutdown; default 30s")
	fs.DurationVar(&o.scratchMaxIdle, "scratch-max-idle", 0, "reap job scratch idle this long; default 168h")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

func parsePools(s string) ([]node.Pool, error) {
	var pools []node.Pool
	for _, name := range strings.Split(s, ",") {
		switch pool := node.Pool(strings.TrimSpace(name)); pool {
		case node.PoolMedia, node.PoolAI:
			pools = append(pools, pool)
		default:
			return nil, fmt.Errorf("unknown pool %q: a worker serves media or ai", name)
		}
	}
	return pools, nil
}

func loadDaemonConfig(path string) (config.Daemon, error) {
	var cfg config.Daemon
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read the configuration: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	applyDirectoryDefaults(&cfg)
	return cfg, nil
}

// applyDirectoryDefaults gives the shared directories the same defaults the daemon
// gives them (I138), so a daemon.json that leaves them out means the same thing to
// both processes.
func applyDirectoryDefaults(cfg *config.Daemon) {
	if cfg.DataDir == "" {
		return
	}
	if cfg.SpoolDir == "" {
		cfg.SpoolDir = filepath.Join(cfg.DataDir, "spool")
	}
	if cfg.ScratchDir == "" {
		cfg.ScratchDir = filepath.Join(cfg.DataDir, "scratch")
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = filepath.Join(cfg.DataDir, "cache")
	}
}

// readToken reads the worker token from its file. The value never appears in a flag
// or in the environment, so it stays out of process listings and crash reports.
func readToken(path string) ([]byte, error) {
	if path == "" {
		path = os.Getenv(tokenFileEnv)
	}
	if path == "" {
		return nil, errors.New("no worker token: set --token-file or " + tokenFileEnv)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the worker token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, fmt.Errorf("the worker token file %s is empty", path)
	}
	return []byte(token), nil
}

func daemonAddress(o options, cfg config.Daemon) (string, error) {
	switch {
	case o.daemon != "":
		return o.daemon, nil
	case cfg.Listeners.Unix != nil && cfg.Listeners.Unix.Path != "":
		return "unix://" + cfg.Listeners.Unix.Path, nil
	}
	return "", errors.New("no daemon address: set --daemon, or give a --config with a Unix listener")
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func logger(level string, w io.Writer) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l})), nil
}

// buildConfig joins flags and the configuration file. Flags win.
func buildConfig(o options, cfg config.Daemon, token []byte) (workers.Config, error) {
	pools, err := parsePools(o.pools)
	if err != nil {
		return workers.Config{}, err
	}
	address, err := daemonAddress(o, cfg)
	if err != nil {
		return workers.Config{}, err
	}
	coordinator, err := client.New(client.Options{
		Daemon:         address,
		Token:          token,
		TLSFingerprint: o.tlsFingerprint,
		TLSCAFile:      o.tlsCAFile,
	})
	if err != nil {
		return workers.Config{}, err
	}
	roots := []string(o.ingestRoots)
	if len(roots) == 0 {
		roots = cfg.IngestRoots
	}
	out := workers.Config{
		WorkerID:       o.workerID,
		Pools:          pools,
		Coordinator:    coordinator,
		FFmpeg:         ffmpeg.New(o.ffmpegPath, o.ffprobePath),
		ScratchDir:     first(o.scratchDir, cfg.ScratchDir),
		CacheDir:       first(o.cacheDir, cfg.CacheDir),
		SpoolDir:       first(o.spoolDir, cfg.SpoolDir),
		IngestRoots:    roots,
		AIWorker:       cfg.AIWorker,
		Transcriber:    o.transcriber,
		Embedder:       o.embedder,
		CacheMaxBytes:  cfg.Retention.CacheMaxBytes,
		ShutdownGrace:  o.shutdownGrace,
		ScratchMaxIdle: o.scratchMaxIdle,
		Version:        version.Version,
		Resources: domain.WorkerResources{
			CPUCores:       o.cpuCores,
			MemoryMB:       o.memoryMB,
			DiskMB:         o.diskMB,
			GPU:            o.gpu,
			MaxConcurrency: o.maxConcurrency,
		},
	}
	if days := cfg.Retention.CacheIdleDays; days > 0 {
		out.CacheIdle = time.Duration(days) * 24 * time.Hour
	}
	return out, nil
}

func run(args []string, stderr io.Writer) int {
	o, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if o.showVersion {
		fmt.Fprintf(stderr, "vidyom-worker %s\n", version.Version)
		return 0
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "vidyom-worker: "+format+"\n", a...)
		return 1
	}
	if wireRegistries == nil || wireQueue == nil {
		return fail("not wired: this build has no cmd/vidyom-worker/wire.go, so it has no nodes, connectors or job queue (master plan section 9)")
	}
	log, err := logger(o.logLevel, stderr)
	if err != nil {
		return fail("%v", err)
	}
	daemonConfig, err := loadDaemonConfig(o.configPath)
	if err != nil {
		return fail("%v", err)
	}
	token, err := readToken(o.tokenFile)
	if err != nil {
		return fail("%v", err)
	}
	cfg, err := buildConfig(o, daemonConfig, token)
	if err != nil {
		return fail("%v", err)
	}
	cfg.Logger = log
	ffmpeg.SetDefault(cfg.FFmpeg)

	nodes := node.NewRegistry()
	connectors := connector.NewRegistry()
	if err := wireRegistries(nodes, connectors); err != nil {
		return fail("wire the registries: %v", err)
	}
	cfg.Connectors = connectors

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	redis := config.Redis{URL: first(o.redisURL, daemonConfig.Redis.URL)}
	if redis.URL == "" {
		return fail("no Redis URL: set --redis-url, or give a --config that has one")
	}
	queue, closeQueue, err := wireQueue(ctx, redis)
	if err != nil {
		return fail("open the job queue: %v", err)
	}
	defer closeQueue()
	cfg.Queue = queue
	if wireMetrics != nil {
		metrics, closeMetrics, err := wireMetrics(ctx, daemonConfig.Observability)
		if err != nil {
			return fail("start the metrics exporter: %v", err)
		}
		defer closeMetrics()
		cfg.Metrics = metrics
	}

	if err := workers.Run(ctx, cfg, nodes); err != nil {
		return fail("%v", err)
	}
	return 0
}
