package apiserver

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/version"
)

const (
	healthCacheFor = 2 * time.Second
	healthTimeout  = 2 * time.Second
)

// healthCache runs checks at most once per interval, because /health needs no
// credential and must not let anyone hammer Redis through it.
type healthCache struct {
	checks []HealthCheck
	now    func() time.Time

	mu      sync.Mutex
	at      time.Time
	status  string
	details map[string]api.ComponentHealth
}

func newHealthCache(checks []HealthCheck, now func() time.Time) *healthCache {
	return &healthCache{checks: checks, now: now}
}

func (h *healthCache) get(ctx context.Context) (string, map[string]api.ComponentHealth) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.at.IsZero() && h.now().Sub(h.at) < healthCacheFor {
		return h.status, h.details
	}

	type outcome struct {
		check HealthCheck
		err   error
	}
	results := make(chan outcome, len(h.checks))
	// The checks outlive a client that goes away, since the result is shared.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthTimeout)
	defer cancel()
	for _, check := range h.checks {
		go func() {
			results <- outcome{check, check.Check(runCtx)}
		}()
	}

	status := "ok"
	details := make(map[string]api.ComponentHealth, len(h.checks))
	for range h.checks {
		r := <-results
		// Status only: this route has no credential, and details belong in /doctor.
		if r.err == nil {
			details[r.check.Name] = api.ComponentHealth{Status: "ok"}
			continue
		}
		if r.check.Critical {
			details[r.check.Name] = api.ComponentHealth{Status: "down"}
			status = "down"
		} else {
			details[r.check.Name] = api.ComponentHealth{Status: "degraded"}
			if status == "ok" {
				status = "degraded"
			}
		}
	}
	if len(details) == 0 {
		details = nil
	}
	h.at, h.status, h.details = h.now(), status, details
	return status, details
}

func (s *server) handleHealth(c *call) error {
	status, components := s.health.get(c.ctx)
	code := http.StatusOK
	if status == "down" {
		code = http.StatusServiceUnavailable
	}
	return c.writeJSON(code, api.HealthResponse{
		Status: status, Version: version.Version, Sealing: s.d.Sealing, Components: components,
	})
}

func (s *server) handleVersion(c *call) error {
	return c.writeJSON(http.StatusOK, api.VersionResponse{Version: version.Version, APIVersion: version.APIVersion})
}
