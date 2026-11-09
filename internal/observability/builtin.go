package observability

import (
	"github.com/rafaelp/vidyom/connectors/observability/file"
	"github.com/rafaelp/vidyom/connectors/observability/loki"
	"github.com/rafaelp/vidyom/connectors/observability/otlp"
	"github.com/rafaelp/vidyom/connectors/observability/prometheus"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// RegisterBuiltins registers the drivers this repository ships. Datadog, Syslog and
// Google Cloud Logging are Future (§10.2, I11) and have no driver: a configuration
// entry naming one shows as failed if enabled, and is inert if disabled.
func RegisterBuiltins(r connector.Registrar) {
	file.Register(r)
	loki.Register(r)
	prometheus.Register(r)
	otlp.Register(r)
}
