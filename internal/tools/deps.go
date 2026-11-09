//go:build tools

// Blank imports keep selected modules in go.mod until real code uses them. The
// tag means none of this compiles into anything; `go mod tidy` still sees them,
// because it considers all build tags. integration removes entries that end up
// used for real, and any that end up unused.
package tools

import (
	_ "cel.dev/cel-go/cel"
	_ "cloud.google.com/go/storage"
	_ "github.com/alicebob/miniredis/v2"
	_ "github.com/fsouza/fake-gcs-server/fakestorage"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/spf13/cobra"
	_ "github.com/zalando/go-keyring"
	_ "go.opentelemetry.io/otel"
	_ "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	_ "go.opentelemetry.io/otel/sdk/metric"
	_ "go.opentelemetry.io/otel/sdk/trace"
	_ "go.opentelemetry.io/otel/trace"
	_ "golang.org/x/oauth2"
	_ "golang.org/x/oauth2/google"
	_ "golang.org/x/sync/errgroup"
	_ "golang.org/x/sys/unix"
	_ "golang.org/x/term"
	_ "google.golang.org/api/youtube/v3"
)
