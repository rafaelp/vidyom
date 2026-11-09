package apiserver

import (
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

const (
	resultAllowed = "allowed"
	resultDenied  = "denied"
	resultFailed  = "failed"
)

// notAudited lists mutating routes without a record of their own (I123): a chunk is
// part of its upload, validation reads only, and an unauthenticated pairing attempt
// must not let anyone grow the system chain; service.Devices records real pairings.
// Worker routes are recorded by the engine, which sees what they change.
var notAudited = map[string]bool{
	api.RouteUploadsChunk:      true,
	api.RouteWorkflowsValidate: true,
	api.RouteDevicesPair:       true,
}

func (s *server) audits(rt api.Route) bool {
	return rt.Mutating() && rt.Auth == api.AuthUser && !notAudited[rt.Name]
}

// record writes one audit entry. Only facts: never a body, query or secret.
func (s *server) record(c *call, result string, code errs.Code) error {
	if s.d.Auditor == nil {
		return errs.New(errs.CodeUnavailable, "the audit is not wired in this daemon")
	}
	details := map[string]any{"method": c.route.Method}
	if c.conn.Transport != "" {
		details["transport"] = c.conn.Transport
	}
	if code != "" {
		details["error_code"] = string(code)
	}
	return s.d.Auditor.Record(c.ctx, security.AuditEntry{
		TenantID:   c.scope.Tenant,
		Principal:  c.scope.Principal,
		Action:     c.route.Name,
		Permission: c.route.Permission,
		Resource:   c.resource(),
		Result:     result,
		RequestID:  c.requestID,
		Details:    details,
	})
}

// resource is the route's path parameter, validated by the time this runs.
func (c *call) resource() string {
	for _, name := range []string{"id", "name", "role", "type"} {
		if v := c.r.PathValue(name); v != "" && safeSegment.MatchString(v) {
			return v
		}
	}
	return ""
}
