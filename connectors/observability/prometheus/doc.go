// Package prometheus is the Prometheus metrics exporter (D38), the default metrics
// path. It aggregates in its own registry and serves the text format through
// connector.MetricsHandler, which the API exposes at GET /metrics.
//
// A request carrying the Vidyom-Tenant header gets only that tenant's series; the
// API layer sets it for anyone who is not a system admin (I272).
package prometheus
