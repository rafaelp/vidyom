package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// maxRemembered bounds what a key stores (I122).
const maxRemembered = 1 << 20

// noIdempotencyStore lists tenant-scoped mutations that do not use keys (I122): a
// chunk is idempotent by offset, validation reads only, and hashing a request that
// holds a secret value would store a digest of it. Those two are idempotent by name.
var noIdempotencyStore = map[string]bool{
	api.RouteUploadsChunk:      true,
	api.RouteWorkflowsValidate: true,
	api.RouteSecretsPut:        true,
	api.RouteSecretsRotate:     true,
}

func usesIdempotency(rt api.Route) bool {
	return rt.Mutating() && rt.Auth == api.AuthUser && rt.Tenant == api.TenantRequired && !noIdempotencyStore[rt.Name]
}

func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// idempotencyKey reads the key: the header, or a run request's body field, which
// must agree when both are sent (I15).
func (c *call) idempotencyKey() (string, error) {
	header := c.r.Header.Get(api.HeaderIdempotencyKey)
	key := header
	if c.route.Name == api.RouteWorkflowsRun || c.route.Name == api.RouteWorkflowsRunBody {
		var peek struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		// A malformed body is reported by the handler's strict decode.
		if json.Unmarshal(c.body, &peek) == nil && peek.IdempotencyKey != "" {
			if header != "" && header != peek.IdempotencyKey {
				return "", errs.Newf(errs.CodeInvalidArgument, "the %s header and idempotency_key differ", api.HeaderIdempotencyKey)
			}
			key = peek.IdempotencyKey
		}
	}
	if key == "" {
		return "", nil
	}
	if !validIdempotencyKey(key) {
		return "", errs.Newf(errs.CodeInvalidArgument, "%s must be 1 to 255 printable ASCII characters", api.HeaderIdempotencyKey)
	}
	return key, nil
}

func requestHash(c *call) string {
	h := sha256.New()
	h.Write([]byte(c.r.Method))
	h.Write([]byte{0})
	h.Write([]byte(c.r.URL.Path))
	h.Write([]byte{0})
	h.Write(c.body)
	return hex.EncodeToString(h.Sum(nil))
}

// execute runs the handler with the mutation's protections around it: replay,
// audit before acting, and remembering the outcome.
func (s *server) execute(c *call, h handlerFunc) error {
	if err := c.readBody(); err != nil {
		return err
	}
	// Everywhere else the header is ignored, as I122 states.
	if !usesIdempotency(c.route) {
		return s.runAudited(c, h)
	}
	key, err := c.idempotencyKey()
	if err != nil {
		return err
	}
	c.scope.IdempotencyKey = key
	if key == "" {
		return errs.Newf(errs.CodeInvalidArgument, "the %s header is required on this route", api.HeaderIdempotencyKey)
	}
	if s.d.TenantStores == nil {
		return errs.New(errs.CodeUnavailable, "idempotency records are not wired in this daemon")
	}
	store, err := s.d.TenantStores.ForTenant(c.ctx, c.scope.Tenant)
	if err != nil {
		return err
	}

	// Keys are namespaced by user, so one user's key never replays another's answer.
	stored := string(c.scope.Principal.UserULID) + ":" + key
	unlock, err := s.idemLocks.Lock(c.ctx, string(c.scope.Tenant)+"/"+stored)
	if err != nil {
		return errs.Wrap(errs.CodeTimeout, err, "the request ended while waiting for a duplicate")
	}
	defer unlock()

	hash := requestHash(c)
	existing, found, err := store.Idempotency().Get(c.ctx, stored)
	if err != nil {
		return err
	}
	if found {
		if existing.RequestHash != hash {
			return errs.Newf(errs.CodeIdempotencyMismatch, "this %s was used with a different request", api.HeaderIdempotencyKey)
		}
		header := c.w.Header()
		header.Set(api.HeaderIdempotencyReplayed, "true")
		header.Set("Content-Type", "application/json")
		c.w.WriteHeader(existing.Status)
		_, _ = c.w.Write(existing.Body)
		return nil
	}

	c.rec.capture = &captureBuffer{limit: maxRemembered}
	if err := s.runAudited(c, h); err != nil {
		return err
	}
	if c.rec.status < 200 || c.rec.status >= 300 {
		return nil
	}
	if c.rec.capture.overflow {
		s.log.Warn("idempotency.response_too_large", "request_id", c.requestID, "route", c.route.Name)
		return nil
	}
	record := repo.IdempotentResponse{
		Key: stored, RequestHash: hash, Status: c.rec.status,
		Body: json.RawMessage(c.rec.capture.data), CreatedAt: s.now().UTC(),
	}
	// The action is done, so a client that hung up must not stop this record.
	ctx, cancel := afterwards(c)
	defer cancel()
	if err := store.Idempotency().Put(ctx, record); err != nil {
		// The answer is already on its way. A retry repeats the call, and a service
		// that starts a job dedupes on Scope.IdempotencyKey too.
		s.log.Error("idempotency.not_recorded", "request_id", c.requestID, "route", c.route.Name, "error", err)
	}
	return nil
}

// runAudited writes the audit record before the handler, so no mutation can happen
// unaudited, and a second record when the outcome is a failure (I123).
func (s *server) runAudited(c *call, h handlerFunc) error {
	if !s.audits(c.route) {
		return h(c)
	}
	if err := s.record(c, resultAllowed, ""); err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "the audit trail is unavailable; the request was not executed")
	}
	err := h(c)
	if err != nil {
		// A client that hung up is one way to get here, and the record is still owed.
		ctx, cancel := afterwards(c)
		defer cancel()
		after := *c
		after.ctx = ctx
		if auditErr := s.record(&after, resultFailed, errs.Public(err).Code); auditErr != nil {
			s.log.Error("audit.failure_not_recorded", "request_id", c.requestID, "route", c.route.Name, "error", auditErr)
		}
	}
	return err
}

// afterwards is a context for bookkeeping once the handler has run: it keeps the
// trace but not the client's cancellation.
func afterwards(c *call) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.ctx), bookkeepingLimit)
}
