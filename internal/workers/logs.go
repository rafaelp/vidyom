package workers

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

const (
	maxBufferedLines = 2000
	maxBatchLines    = 200
)

// lineBuffer collects a task's log lines for the job's stream.
type lineBuffer struct {
	mu      sync.Mutex
	lines   []workerproto.LogLine
	dropped int
}

func (b *lineBuffer) add(line workerproto.LogLine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) >= maxBufferedLines {
		b.lines = b.lines[1:]
		b.dropped++
	}
	b.lines = append(b.lines, line)
}

// take removes up to a batch of lines, reporting any dropped ones.
func (b *lineBuffer) take() []workerproto.LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(b.lines), maxBatchLines)
	if n == 0 && b.dropped == 0 {
		return nil
	}
	out := append([]workerproto.LogLine(nil), b.lines[:n]...)
	b.lines = b.lines[n:]
	if b.dropped > 0 {
		out = append(out, workerproto.LogLine{
			At: time.Now().UTC(), Level: "warn",
			Message: fmt.Sprintf("%d log lines were dropped by the worker", b.dropped),
		})
		b.dropped = 0
	}
	return out
}

// taskHandler sends a record to the worker's own log and to the job's stream.
type taskHandler struct {
	inner  slog.Handler
	buffer *lineBuffer
	attrs  []slog.Attr
}

func newTaskLogger(base slog.Handler, buffer *lineBuffer, attrs ...slog.Attr) *slog.Logger {
	return slog.New(&taskHandler{inner: base.WithAttrs(attrs), buffer: buffer})
}

func (h *taskHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *taskHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		var text strings.Builder
		text.WriteString(r.Message)
		for _, a := range h.attrs {
			fmt.Fprintf(&text, " %s=%v", a.Key, a.Value)
		}
		r.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&text, " %s=%v", a.Key, a.Value)
			return true
		})
		h.buffer.add(workerproto.LogLine{At: r.Time.UTC(), Level: strings.ToLower(r.Level.String()), Message: text.String()})
	}
	return h.inner.Handle(ctx, r)
}

func (h *taskHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &taskHandler{inner: h.inner.WithAttrs(attrs), buffer: h.buffer, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *taskHandler) WithGroup(name string) slog.Handler {
	return &taskHandler{inner: h.inner.WithGroup(name), buffer: h.buffer, attrs: h.attrs}
}
