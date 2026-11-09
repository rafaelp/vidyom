package workers

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/version"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
)

// Defaults for Config's timings (I194, I181).
const (
	DefaultShutdownGrace     = 30 * time.Second
	DefaultDaemonOutageLimit = time.Hour
	DefaultScratchMaxIdle    = config.DefaultCacheIdleDays * 24 * time.Hour
	defaultReadBlock         = 2 * time.Second
	defaultClaimInterval     = 30 * time.Second
	defaultHousekeeping      = time.Hour
	defaultRetryMax          = 30 * time.Second
	defaultTranscriberDriver = "faster-whisper"
	defaultEmbedderDriver    = "bge-m3"
)

// Config is everything a worker needs. Coordinator, Queue and Connectors come from
// cmd/vidyom-worker, which holds the seams of master plan section 9.
type Config struct {
	WorkerID string // generated when empty
	Pools    []node.Pool

	Coordinator workerproto.Coordinator
	Queue       jobqueue.Queue
	Connectors  *connector.Registry
	FFmpeg      *ffmpeg.Tool // defaults to ffmpeg.Default()

	ScratchDir  string
	CacheDir    string
	SpoolDir    string
	IngestRoots []string
	AIWorker    config.AIWorker
	Transcriber string // driver for pool ai, default faster-whisper
	Embedder    string // driver for pool ai, default bge-m3

	CacheIdle     time.Duration // default 7 days (D22)
	CacheMaxBytes int64         // default 50 GiB per tenant (I189)

	// Resources is declared to the scheduler (D40). Zero fields are detected.
	Resources domain.WorkerResources

	// HeartbeatInterval overrides the daemon's answer, for tests.
	HeartbeatInterval time.Duration
	ShutdownGrace     time.Duration
	DaemonOutageLimit time.Duration
	ScratchMaxIdle    time.Duration
	ReadBlock         time.Duration
	ClaimInterval     time.Duration
	ClaimMinIdle      time.Duration
	Housekeeping      time.Duration
	RetryMax          time.Duration

	Version string
	Logger  *slog.Logger
	Metrics telemetry.Metrics
	Now     func() time.Time
}

func (c *Config) applyDefaults() error {
	if len(c.Pools) == 0 {
		return errors.New("workers: no pools configured")
	}
	for _, pool := range c.Pools {
		if pool != node.PoolMedia && pool != node.PoolAI && pool != node.PoolDaemon {
			return fmt.Errorf("workers: unknown pool %q", pool)
		}
		if pool == node.PoolDaemon {
			return errors.New("workers: pool daemon runs inside vidyomd, not in a worker")
		}
	}
	if c.Coordinator == nil {
		return errors.New("workers: no coordinator")
	}
	if c.Queue == nil {
		return errors.New("workers: no queue")
	}
	if c.Connectors == nil {
		c.Connectors = connector.NewRegistry()
	}
	if c.ScratchDir == "" {
		return errors.New("workers: no scratch directory")
	}
	if c.CacheDir == "" {
		return errors.New("workers: no cache directory")
	}
	if c.FFmpeg == nil {
		c.FFmpeg = ffmpeg.Default()
	}
	if c.Transcriber == "" {
		c.Transcriber = defaultTranscriberDriver
	}
	if c.Embedder == "" {
		c.Embedder = defaultEmbedderDriver
	}
	if c.WorkerID == "" {
		c.WorkerID = generateWorkerID()
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = DefaultShutdownGrace
	}
	if c.DaemonOutageLimit <= 0 {
		c.DaemonOutageLimit = DefaultDaemonOutageLimit
	}
	if c.ScratchMaxIdle <= 0 {
		c.ScratchMaxIdle = DefaultScratchMaxIdle
	}
	if c.ReadBlock <= 0 {
		c.ReadBlock = defaultReadBlock
	}
	if c.ClaimInterval <= 0 {
		c.ClaimInterval = defaultClaimInterval
	}
	if c.ClaimMinIdle <= 0 {
		c.ClaimMinIdle = 2 * workerproto.DefaultLeaseSeconds * time.Second
	}
	if c.Housekeeping <= 0 {
		c.Housekeeping = defaultHousekeeping
	}
	if c.RetryMax <= 0 {
		c.RetryMax = defaultRetryMax
	}
	if c.Version == "" {
		c.Version = version.Version
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if c.Metrics == nil {
		c.Metrics = telemetry.Noop{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	c.detectResources()
	return nil
}

// detectResources fills what was not declared (D40). Concurrency defaults to a
// quarter of the cores, since a transcode is multi-threaded.
func (c *Config) detectResources() {
	r := &c.Resources
	if r.CPUCores <= 0 {
		r.CPUCores = runtime.NumCPU()
	}
	if r.MemoryMB <= 0 {
		r.MemoryMB = totalMemoryMB()
	}
	if r.DiskMB <= 0 {
		r.DiskMB = freeDiskMB(c.ScratchDir)
	}
	if r.MaxConcurrency <= 0 {
		r.MaxConcurrency = max(1, r.CPUCores/4)
	}
}

func generateWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UnixNano()%1_000_000)
}
