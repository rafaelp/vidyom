// Package workerclient is the Go side of the Python AI worker's protocol (I6): a
// shared JSON-RPC connection over a Unix socket or loopback TCP. Both vidyom-worker
// and vidyomd use it, through the faster-whisper and bge-m3 connectors.
//
// The client refuses a worker running another engine than the configuration names
// (I201), so a fake engine can never stand in for the real one, or the reverse.
package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jsonrpc"
	"github.com/rafaelp/vidyom/internal/contract/version"
)

// dialTimeout bounds connecting and the initialize handshake, which never does heavy
// work in the worker: models load on first use.
const dialTimeout = 10 * time.Second

// Stream is an open connection to a worker. A stdio transport for tests is a Stream
// over the pipes of a child process.
type Stream interface {
	io.Reader
	io.Writer
	io.Closer
}

// Dialer opens a stream to the worker.
type Dialer func(ctx context.Context) (Stream, error)

// Options configures a Client.
type Options struct {
	// Client names this process to the worker, for its logs.
	Client string
	// Token is sent in initialize, for the tcp transport.
	Token string
	// Engine is the engine this client accepts: aiworker.EngineReal when empty, or
	// aiworker.EngineFake (I201).
	Engine string
}

// Client is a connection to the AI worker that re-establishes itself on the next
// call after it drops. It is safe for concurrent use.
type Client struct {
	dial Dialer
	opts Options

	mu        sync.Mutex
	conn      *jsonrpc.Conn
	info      aiworker.InitializeResult
	haveInfo  bool
	listeners sync.Map // progress token -> func(percent float64, message string)
	nextToken atomic.Uint64
}

// New returns a client that dials on first use.
func New(dial Dialer, opts Options) *Client {
	if opts.Client == "" {
		opts.Client = "vidyom/" + version.Version
	}
	if opts.Engine == "" {
		opts.Engine = aiworker.EngineReal
	}
	return &Client{dial: dial, opts: opts}
}

// DialerFor returns the dialer of a configured endpoint: unix:///path or
// tcp://127.0.0.1:port. A tcp endpoint must be loopback (I6).
func DialerFor(endpoint string) (Dialer, error) {
	if endpoint == "" {
		return nil, errs.New(errs.CodeFailedPrecondition, "no AI worker endpoint is configured (aiWorker.endpoint)")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, errs.Newf(errs.CodeInvalidArgument, "invalid AI worker endpoint")
	}
	dialer := net.Dialer{Timeout: dialTimeout}
	switch parsed.Scheme {
	case "unix":
		path := parsed.Path
		if path == "" {
			return nil, errs.New(errs.CodeInvalidArgument, "the AI worker's unix endpoint has no path")
		}
		return func(ctx context.Context) (Stream, error) { return dialer.DialContext(ctx, "unix", path) }, nil
	case "tcp":
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errs.New(errs.CodeInvalidArgument, "the AI worker's tcp endpoint must be a loopback IP address")
		}
		if _, err := strconv.Atoi(parsed.Port()); err != nil {
			return nil, errs.New(errs.CodeInvalidArgument, "the AI worker's tcp endpoint has no port")
		}
		address := net.JoinHostPort(host, parsed.Port())
		return func(ctx context.Context) (Stream, error) { return dialer.DialContext(ctx, "tcp", address) }, nil
	default:
		return nil, errs.New(errs.CodeInvalidArgument, "the AI worker endpoint must be unix:// or tcp://")
	}
}

// FromConfig builds a client from the daemon's configuration, reading the token file
// when the endpoint needs one. The token stays in memory.
func FromConfig(cfg config.AIWorker) (*Client, error) {
	dial, err := DialerFor(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	switch cfg.Engine {
	case "", aiworker.EngineReal, aiworker.EngineFake:
	default:
		return nil, errs.New(errs.CodeInvalidArgument, `aiWorker.engine must be "real" or "fake"`)
	}
	opts := Options{Engine: cfg.Engine}
	if cfg.TokenFile != "" {
		data, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, errs.New(errs.CodeFailedPrecondition, "cannot read the AI worker's token file")
		}
		opts.Token = string(bytes.TrimSpace(data))
	}
	return New(dial, opts), nil
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*Client{}
)

// Shared returns this process's client for a configuration. The transcription and
// embedding connectors of every tenant use one connection; the worker holds no tenant
// state, and files arrive by path.
func Shared(cfg config.AIWorker) (*Client, error) {
	key := cfg.Endpoint + "\x00" + cfg.TokenFile + "\x00" + cfg.Engine
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if c, ok := shared[key]; ok {
		return c, nil
	}
	c, err := FromConfig(cfg)
	if err != nil {
		return nil, err
	}
	shared[key] = c
	return c, nil
}

func (c *Client) connection(ctx context.Context) (*jsonrpc.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	stream, err := c.dial(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.CodeUnavailable, err, "the AI worker is not reachable")
	}
	conn := jsonrpc.NewConn(stream, stream, stream, c.handle)
	go func() {
		_ = conn.Run(context.Background())
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
	}()

	handshake, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var info aiworker.InitializeResult
	err = conn.Call(handshake, aiworker.MethodInitialize, aiworker.InitializeParams{
		ProtocolVersion: aiworker.ProtocolVersion,
		Client:          c.opts.Client,
		Token:           c.opts.Token,
	}, &info)
	if err != nil {
		_ = conn.Close()
		return nil, mapError(err)
	}
	if info.ProtocolVersion != aiworker.ProtocolVersion {
		_ = conn.Close()
		return nil, errs.Newf(errs.CodeFailedPrecondition, "the AI worker speaks protocol %d, this client %d", info.ProtocolVersion, aiworker.ProtocolVersion)
	}
	if info.Engine != c.opts.Engine {
		_ = conn.Close()
		return nil, errs.Newf(errs.CodeFailedPrecondition,
			"the AI worker runs engine %q but aiWorker.engine accepts %q; there is no fallback between engines", info.Engine, c.opts.Engine)
	}
	c.conn = conn
	c.info = info
	c.haveInfo = true
	return conn, nil
}

// handle receives the worker's notifications.
func (c *Client) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != jsonrpc.MethodProgress {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
	}
	var p jsonrpc.ProgressParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, nil
	}
	if listener, ok := c.listeners.Load(p.Token); ok {
		listener.(func(float64, string))(p.Percent, p.Message)
	}
	return nil, nil
}

// Close drops the connection. The next call dials again.
func (c *Client) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Describe connects if needed and returns what the worker reported.
func (c *Client) Describe(ctx context.Context) (aiworker.InitializeResult, error) {
	if _, err := c.connection(ctx); err != nil {
		return aiworker.InitializeResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info, nil
}

// LastDescribed returns the worker's description from the last handshake, if any.
func (c *Client) LastDescribed() (aiworker.InitializeResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info, c.haveInfo
}

// Health asks the worker for its readiness.
func (c *Client) Health(ctx context.Context) (aiworker.HealthResult, error) {
	var out aiworker.HealthResult
	err := c.call(ctx, aiworker.MethodHealth, struct{}{}, &out)
	return out, err
}

// Shutdown asks the worker to finish in-flight work and exit.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, aiworker.MethodShutdown, struct{}{}, nil)
}

// Transcribe runs a transcription. progress, when set, receives the worker's
// notifications; they may arrive slightly out of order and never go backwards here.
func (c *Client) Transcribe(ctx context.Context, params aiworker.TranscribeParams, progress func(float64, string)) (aiworker.TranscribeResult, error) {
	var out aiworker.TranscribeResult
	if progress != nil {
		token := "p" + strconv.FormatUint(c.nextToken.Add(1), 10)
		params.ProgressToken = token
		var mu sync.Mutex
		highest := -1.0
		c.listeners.Store(token, func(percent float64, message string) {
			mu.Lock()
			defer mu.Unlock()
			if percent < highest {
				return
			}
			highest = percent
			progress(percent, message)
		})
		defer c.listeners.Delete(token)
	}
	err := c.call(ctx, aiworker.MethodTranscribe, params, &out)
	return out, err
}

// Embed computes vectors for at most aiworker.MaxEmbedBatch texts.
func (c *Client) Embed(ctx context.Context, params aiworker.EmbedParams) (aiworker.EmbedResult, error) {
	var out aiworker.EmbedResult
	err := c.call(ctx, aiworker.MethodEmbed, params, &out)
	return out, err
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	conn, err := c.connection(ctx)
	if err != nil {
		return err
	}
	if err := conn.Call(ctx, method, params, result); err != nil {
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) && ctx.Err() == nil {
			// The transport broke. Drop it so the next call dials again.
			c.mu.Lock()
			if c.conn == conn {
				c.conn = nil
			}
			c.mu.Unlock()
			_ = conn.Close()
		}
		return mapError(err)
	}
	return nil
}

// mapError turns protocol failures into the shared error model. Cancellation stays
// a context error, so a node's caller sees what it asked for.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case jsonrpc.CodeInvalidParams:
			return errs.Newf(errs.CodeInvalidArgument, "AI worker: %s", rpcErr.Message)
		case jsonrpc.CodeUnavailable:
			return errs.Newf(errs.CodeUnavailable, "AI worker: %s", rpcErr.Message)
		case jsonrpc.CodeUnauthorized:
			return errs.Newf(errs.CodeFailedPrecondition, "AI worker refused this client: %s", rpcErr.Message)
		case jsonrpc.CodeRequestCancel:
			return context.Canceled
		default:
			return errs.Newf(errs.CodeInternal, "AI worker: %s", rpcErr.Message)
		}
	}
	var coded *errs.Error
	if errors.As(err, &coded) {
		return err
	}
	return errs.Wrap(errs.CodeUnavailable, err, "the AI worker connection failed")
}
