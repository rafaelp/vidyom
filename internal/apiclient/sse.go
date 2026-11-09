package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// maxSSEEvent bounds one event, so a broken stream cannot exhaust memory. A job
// event's data can hold a node output, capped at 32 MiB (I35), plus its encoding.
const maxSSEEvent = 64 << 20

// Event is one frame of a Server-Sent Events stream.
type Event struct {
	ID   string
	Name string
	Data []byte
}

// sseReader parses the wire format: "field: value" lines, comments starting with a
// colon, and a blank line that dispatches the event.
type sseReader struct {
	r      *bufio.Reader
	onLine func() // called for every line read, comments included
	id     string
}

// next returns the next event. The duration carries a "retry:" hint when one arrived.
func (s *sseReader) next() (Event, *time.Duration, error) {
	var (
		ev      Event
		data    bytes.Buffer
		hasData bool
		retry   *time.Duration
	)
	for {
		line, err := s.readLine()
		if err != nil {
			return Event{}, retry, err
		}
		if s.onLine != nil {
			s.onLine()
		}
		if len(line) == 0 {
			if !hasData && ev.Name == "" {
				continue // keep-alive or an id-only block
			}
			ev.ID = s.id
			ev.Data = data.Bytes()
			return ev, retry, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.Name = string(value)
		case "data":
			if data.Len()+len(value) > maxSSEEvent {
				return Event{}, retry, errs.New(errs.CodeOutputTooLarge, "an event on the stream is too large")
			}
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasData = true
		case "id":
			if !bytes.ContainsRune(value, 0) {
				s.id = string(value)
			}
		case "retry":
			if ms, err := strconv.Atoi(string(value)); err == nil && ms >= 0 {
				d := time.Duration(ms) * time.Millisecond
				retry = &d
			}
		}
	}
}

func (s *sseReader) readLine() ([]byte, error) {
	var out []byte
	for {
		fragment, err := s.r.ReadSlice('\n')
		out = append(out, fragment...)
		if len(out) > maxSSEEvent+1024 {
			return nil, errs.New(errs.CodeOutputTooLarge, "a line on the stream is too large")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = bytes.TrimSuffix(out, []byte("\n"))
		out = bytes.TrimSuffix(out, []byte("\r"))
		return out, nil
	}
}

// StreamOptions configures a stream.
type StreamOptions struct {
	Call Call
	// LastEventID resumes after this event.
	LastEventID string
	// OnEvent handles a frame. Keep-alives never reach it, and neither does the
	// api.SSEEnd frame, which ends the stream. Returning an error stops the stream.
	OnEvent func(Event) error
	// OnReconnect is told about each reconnection attempt.
	OnReconnect func(err error, delay time.Duration)
	// IdleTimeout drops a silent connection; default three keep-alive periods.
	IdleTimeout time.Duration
	// MinBackoff and MaxBackoff bound the delay between attempts; 1 s and 10 s.
	MinBackoff, MaxBackoff time.Duration
}

// Stream reads an SSE route. It only ever issues the route's GET: following a
// stream cannot change a job (§11.5). It reconnects with Last-Event-ID across lost
// connections and daemon restarts. It returns nil on the api.SSEEnd frame, ctx.Err()
// on cancellation, and the coded error on a non-retryable answer.
func (c *Client) Stream(ctx context.Context, opts StreamOptions) error {
	route := api.ByName(opts.Call.Route)
	if !route.Stream {
		return errs.Newf(errs.CodeInternal, "route %s is not a stream", opts.Call.Route)
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 3 * api.SSEKeepAlive
	}
	if opts.MinBackoff == 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = 10 * time.Second
	}

	lastID := opts.LastEventID
	backoff := opts.MinBackoff
	for {
		progressed, serverDelay, err := c.streamOnce(ctx, opts, &lastID)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errStreamStopped):
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, errHandler):
			return errors.Unwrap(err)
		case !retryableStreamError(err):
			return err
		}
		if progressed {
			backoff = opts.MinBackoff
		}
		delay := backoff
		if serverDelay != nil {
			delay = *serverDelay
		}
		if opts.OnReconnect != nil {
			opts.OnReconnect(err, delay)
		}
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
		if backoff *= 2; backoff > opts.MaxBackoff {
			backoff = opts.MaxBackoff
		}
	}
}

var (
	errStreamStopped = errors.New("stream stopped")
	errHandler       = errors.New("stream handler")
)

type handlerError struct{ err error }

func (h *handlerError) Error() string        { return h.err.Error() }
func (h *handlerError) Unwrap() error        { return h.err }
func (h *handlerError) Is(target error) bool { return target == errHandler }

// retryableStreamError keeps a follower alive across anything but a refusal.
func retryableStreamError(err error) bool {
	var coded *errs.Error
	if !errors.As(err, &coded) {
		return true // a read error on the open stream
	}
	return IsTransient(err)
}

func (c *Client) streamOnce(ctx context.Context, opts StreamOptions, lastID *string) (progressed bool, retry *time.Duration, err error) {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, _, err := c.request(connCtx, opts.Call, nil, "")
	if err != nil {
		return false, nil, err
	}
	req.Header.Set("Accept", api.SSEContentType)
	req.Header.Set("Cache-Control", "no-cache")
	if *lastID != "" {
		req.Header.Set(api.HeaderLastEventID, *lastID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, nil, c.transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return false, nil, errs.Newf(errs.CodeFailedPrecondition, "the server answered %s on a stream", resp.Status)
		}
		return false, parseRetryAfter(resp), decodeError(resp)
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != api.SSEContentType {
		return false, nil, errs.Newf(errs.CodeFailedPrecondition, "expected an event stream, got %q", resp.Header.Get("Content-Type"))
	}

	// The watchdog drops a connection that stopped even sending keep-alives. A lost
	// peer on an idle TCP connection would otherwise hang the follower forever.
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	done := make(chan struct{})
	defer close(done)
	go func() {
		interval := opts.IdleTimeout / 4
		if interval <= 0 {
			interval = time.Millisecond
		}
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if time.Since(time.Unix(0, lastRead.Load())) > opts.IdleTimeout {
					cancel()
					return
				}
			}
		}
	}()

	reader := &sseReader{
		r:      bufio.NewReaderSize(resp.Body, 64<<10),
		id:     *lastID,
		onLine: func() { lastRead.Store(time.Now().UnixNano()) },
	}
	for {
		ev, hint, err := reader.next()
		if hint != nil {
			retry = hint
		}
		if err != nil {
			if ctx.Err() != nil {
				return progressed, retry, ctx.Err()
			}
			if connCtx.Err() != nil {
				// Only the watchdog cancels this context while ctx is alive.
				return progressed, retry, errs.New(errs.CodeUnavailable, "the stream went silent")
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return progressed, retry, errs.Wrap(errs.CodeUnavailable, err, "the stream was closed")
			}
			var coded *errs.Error
			if errors.As(err, &coded) {
				return progressed, retry, coded
			}
			return progressed, retry, errs.Wrap(errs.CodeUnavailable, err, "the stream was interrupted")
		}
		if ev.ID != "" {
			*lastID = ev.ID
		}
		progressed = true
		if ev.Name == api.SSEEnd {
			return true, retry, errStreamStopped
		}
		if opts.OnEvent != nil {
			if err := opts.OnEvent(ev); err != nil {
				return true, retry, &handlerError{err: err}
			}
		}
	}
}

func parseRetryAfter(resp *http.Response) *time.Duration {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return nil
	}
	if secs, err := strconv.Atoi(value); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		return &d
	}
	if at, err := http.ParseTime(value); err == nil {
		d := time.Until(at)
		if d < 0 {
			d = 0
		}
		return &d
	}
	return nil
}
