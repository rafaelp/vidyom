package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// frame is one Server-Sent Event. JSON data has no raw newlines, so one data line
// is always enough.
type frame struct {
	id    string // empty when the stream has no sequence
	event string
	data  any
}

// serveSSE copies a service's channel to the client until one side ends. It returns
// only errors from before the response started. Reading a stream never changes
// state: leaving this function only cancels the subscription's context (§5.5).
func serveSSE[T any](s *server, c *call, ch <-chan T, toFrame func(T) frame) error {
	rc := http.NewResponseController(c.w)
	header := c.w.Header()
	header.Set("Content-Type", api.SSEContentType)
	header.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return nil
	}

	ticker := time.NewTicker(s.keepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return nil
		case <-s.d.Shutdown:
			// Without an end frame: clients reconnect and resume.
			return nil
		case <-ticker.C:
			// A revoked device or membership loses its open stream (I129).
			if err := s.recheck(c); err != nil {
				s.log.Info("sse.closed_by_recheck", "request_id", c.requestID, "route", c.route.Name, "code", errs.CodeOf(err))
				return nil
			}
			if err := writeFrame(s, c, rc, ": keep-alive\n\n"); err != nil {
				return nil
			}
		case item, ok := <-ch:
			if !ok {
				if c.ctx.Err() == nil {
					_ = writeFrame(s, c, rc, "event: "+api.SSEEnd+"\ndata: {}\n\n")
				}
				return nil
			}
			text, err := encodeFrame(toFrame(item))
			if err != nil {
				s.log.Error("sse.encode_failed", "request_id", c.requestID, "route", c.route.Name, "error", err)
				return nil
			}
			if err := writeFrame(s, c, rc, text); err != nil {
				return nil
			}
		}
	}
}

func encodeFrame(f frame) (string, error) {
	data, err := json.Marshal(f.data)
	if err != nil {
		return "", err
	}
	text := "event: " + f.event + "\ndata: " + string(data) + "\n\n"
	if f.id != "" {
		text = "id: " + f.id + "\n" + text
	}
	return text, nil
}

func writeFrame(s *server, c *call, rc *http.ResponseController, text string) error {
	_ = rc.SetWriteDeadline(s.now().Add(streamFrameWait))
	if _, err := fmt.Fprint(c.w, text); err != nil {
		return err
	}
	return rc.Flush()
}

// recheck repeats authentication and authorization without touching the call's
// final scope, except that it refreshes the principal.
func (s *server) recheck(c *call) error {
	probe := *c
	probe.scope.Tenant = ""
	if err := s.authenticate(&probe); err != nil {
		return err
	}
	if err := s.resolveTenant(&probe); err != nil {
		return err
	}
	if c.route.Permission == "" {
		return nil
	}
	return s.checkPermission(&probe, c.route.Permission)
}

// subscription gives a stream handler a context its subscription ends with.
func subscription(c *call) (context.Context, context.CancelFunc) {
	return context.WithCancel(c.ctx)
}

func seqID(seq int64) string { return strconv.FormatInt(seq, 10) }
