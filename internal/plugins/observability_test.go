package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/internal/observability"
)

// TestPluginsThroughTheRealManager wires the two halves of this component together, as
// the daemon will: plugin logs and metrics through a real Manager, into a real JSONL file
// and a real Prometheus endpoint.
func TestPluginsThroughTheRealManager(t *testing.T) {
	logDir := t.TempDir()
	manager, err := observability.New(config.Observability{}, observability.Options{LogDir: logDir, Tuning: observability.Tuning{FlushInterval: 5 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer manager.Close(ctx)

	f := newFixture(t, unsandboxed, func(o *Options) {
		o.Logger = manager.Logger()
		o.Metrics = manager
	})
	value := "a-secret-that-looks-like-nothing-special"
	f.secrets["s"] = value
	f.installProbe("p", plugin.Permissions{Secrets: []string{"s"}})
	f.mustCall("p", "whoami", nil)
	f.mustCall("p", "leak-secret", map[string]string{"name": "s"})
	if _, err := f.call("p", "nope", nil); err == nil {
		t.Fatal("expected a failure")
	}

	eventually(t, "the stderr line", func() bool {
		data, _ := os.ReadFile(filepath.Join(logDir, "vidyomd.jsonl"))
		return strings.Contains(string(data), "plugin.stderr")
	})
	if err := manager.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(logDir, "vidyomd.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, value) || strings.Contains(text, "hunter2") {
		t.Errorf("a secret reached the local log:\n%s", text)
	}
	var started connector.LogRecord
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var rec connector.LogRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSONL: %s", line)
		}
		if rec.Event == "plugin.started" {
			started = rec
		}
	}
	if started.TenantID != "acme" || started.Fields["plugin"] != "p" || started.Fields["isolation"] != IsolationNone {
		t.Errorf("a plugin log line must carry tenant and plugin (§10.1, D6): %+v", started)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(api.HeaderTenant, "acme")
	rec := httptest.NewRecorder()
	manager.MetricsHandler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`vidyom_plugin_duration_seconds_count{method="hook.invoke",plugin="p",tenant_id="acme"}`,
		`vidyom_plugin_failures_total{plugin="p",reason="failed_precondition",tenant_id="acme"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s:\n%s", want, body)
		}
	}
	page, err := manager.Logs(ctx, scopeFor("acme"), api.LogQuery{PageQuery: api.PageQuery{Limit: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Error("the tenant's plugin logs should be readable through the API path")
	}
}
