package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// unsandboxed is for tests about the protocol, which must run on any host. The sandbox
// has its own tests in sandbox_test.go, on the hosts that can provide one.
func unsandboxed(o *Options) {
	o.isolator = unavailableIsolator{}
	o.AllowUnsandboxed = true
}

// ---- the sample plugin, end to end ----

func installSample(t *testing.T, f *fixture, network ...string) domain.PluginRecord {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "plugins", "sample-notifier", plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(source, &m); err != nil {
		t.Fatal(err)
	}
	if len(network) > 0 {
		m["permissions"].(map[string]any)["network"] = network
	}
	m["entrypoint"] = map[string]any{"command": "bin/plugin"}
	dir := f.writeTree("sample", sampleBinary, m)
	rec, err := f.r.Install(context.Background(), admin(), installRequest(dir))
	if err != nil {
		t.Fatal(err)
	}
	f.grants.set("acme", "sample-notifier", Grant{Enabled: true, Permissions: plugin.Permissions{
		Network:    network,
		Secrets:    []string{"notifier-token"},
		Filesystem: []string{PermJobTemp},
		Events:     []string{string(event.PublicationCompleted)},
	}})
	return rec
}

// TestBeforeHookRejectsAnInvalidSource is the first case of §15.7.
func TestBeforeHookRejectsAnInvalidSource(t *testing.T) {
	f := newFixture(t, unsandboxed)
	installSample(t, f)
	result, err := f.call("sample-notifier", "check-source", map[string]string{"title": ""})
	if err != nil {
		t.Fatal(err)
	}
	if result.Allow || result.Reason != "the source has no title" {
		t.Errorf("result: %+v", result)
	}
	result = f.mustCall("sample-notifier", "check-source", map[string]string{"title": "Hatha Yoga Class"})
	if !result.Allow {
		t.Errorf("result: %+v", result)
	}
}

// TestFinishHookIsIdempotent is §8.1: a retried hook has the same key, so the plugin
// counts it once.
func TestFinishHookIsIdempotent(t *testing.T) {
	f := newFixture(t, unsandboxed)
	installSample(t, f)
	ctx := context.Background()
	inv := invocation("acme", "sample-notifier", "record-finish", nil)
	inv.Point = hook.WorkflowFinish
	for range 3 {
		if _, err := f.r.RunHookAction(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	other := inv
	other.JobID = "JOB-002"
	if _, err := f.r.RunHookAction(ctx, other); err != nil {
		t.Fatal(err)
	}
	stats := f.mustCall("sample-notifier", "stats", nil)
	var got map[string]int
	if err := json.Unmarshal(stats.Parameters, &got); err != nil {
		t.Fatal(err)
	}
	if got["finished"] != 2 {
		t.Errorf("finished %d, want one per job: %+v", got["finished"], got)
	}
}

func TestEventsAreDeliveredAtLeastOnceToGrantedPlugins(t *testing.T) {
	f := newFixture(t, unsandboxed)
	installSample(t, f)
	ctx := context.Background()
	ids, err := f.r.EventSubscribers(ctx, "acme", event.PublicationCompleted)
	if err != nil || len(ids) != 1 || ids[0] != "sample-notifier" {
		t.Fatalf("subscribers: %v %v", ids, err)
	}
	if ids, _ := f.r.EventSubscribers(ctx, "acme", event.AssetCreated); len(ids) != 0 {
		t.Errorf("an event type not subscribed: %v", ids)
	}
	if ids, _ := f.r.EventSubscribers(ctx, "other", event.PublicationCompleted); len(ids) != 0 {
		t.Errorf("a tenant that granted nothing (D6): %v", ids)
	}
	if _, err := f.r.EventSubscribers(ctx, "", event.PublicationCompleted); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("no tenant: %v", err)
	}
	e := event.Envelope{EventID: id.ULID("01HZXYABCDEFGHJKMNPQRSTVWX"), Type: event.PublicationCompleted, TenantID: "acme", SchemaVersion: event.SchemaVersion, OccurredAt: time.Now()}
	for range 3 {
		if err := f.r.DeliverEvent(ctx, "sample-notifier", e); err != nil {
			t.Fatal(err)
		}
	}
	second := e
	second.EventID = "01HZXYABCDEFGHJKMNPQRSTVWY"
	if err := f.r.DeliverEvent(ctx, "sample-notifier", second); err != nil {
		t.Fatal(err)
	}
	stats := f.mustCall("sample-notifier", "stats", nil)
	var got map[string]int
	if err := json.Unmarshal(stats.Parameters, &got); err != nil {
		t.Fatal(err)
	}
	if got["events"] != 2 {
		t.Errorf("events %d: the event ID must make redelivery harmless", got["events"])
	}
}

func TestEventDeliveryChecksTheGrantEachTime(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{Events: []string{"publication.completed"}})
	ctx := context.Background()
	e := event.Envelope{EventID: "01HZXYABCDEFGHJKMNPQRSTVWX", Type: event.PublicationCompleted, TenantID: "acme"}
	if err := f.r.DeliverEvent(ctx, "p", e); err != nil {
		t.Fatal(err)
	}
	notSubscribed := e
	notSubscribed.Type = event.AssetDeleted
	if err := f.r.DeliverEvent(ctx, "p", notSubscribed); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("a type it never subscribed to: %v", err)
	}
	otherTenant := e
	otherTenant.TenantID = "other"
	if err := f.r.DeliverEvent(ctx, "p", otherTenant); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("another tenant's event (D6): %v", err)
	}
	f.grants.set("acme", "p", Grant{Enabled: true}) // permission withdrawn
	if err := f.r.DeliverEvent(ctx, "p", e); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("a withdrawn permission: %v", err)
	}
	if ids, _ := f.r.EventSubscribers(ctx, "acme", event.PublicationCompleted); len(ids) != 0 {
		t.Errorf("subscribers after withdrawal: %v", ids)
	}
	f.grants.err = errors.New("grants unreadable")
	if ids, err := f.r.EventSubscribers(ctx, "acme", event.PublicationCompleted); err != nil || len(ids) != 0 {
		t.Errorf("an unreadable grant fails closed and does not stop other plugins: %v %v", ids, err)
	}
}

func TestPayloadAndDataAreRedactedOnTheWayOut(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{Events: []string{"publication.completed"}})
	result := f.mustCall("p", "echo-data", map[string]any{"password": "hunter2", "note": "Bearer abc.DEF-123", "title": "ok"})
	text := string(result.Parameters)
	if strings.Contains(text, "hunter2") || strings.Contains(text, "abc.DEF-123") {
		t.Errorf("a secret reached the plugin: %s", text)
	}
	if !strings.Contains(text, `"title":"ok"`) {
		t.Errorf("ordinary data was lost: %s", text)
	}
	e := event.Envelope{EventID: "01HZXYABCDEFGHJKMNPQRSTVWX", Type: event.PublicationCompleted, TenantID: "acme", Payload: json.RawMessage(`{"token":"plain-token-value","visibility":"public"}`)}
	if err := f.r.DeliverEvent(context.Background(), "p", e); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the payload to be logged by the plugin", func() bool { return strings.Contains(f.logs.String(), "visibility") })
	if strings.Contains(f.logs.String(), "plain-token-value") {
		t.Errorf("a payload secret reached the plugin: %s", f.logs.String())
	}
}

// ---- hook.ActionRunner behavior ----

func TestRunHookActionRefusals(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	ctx := context.Background()
	cases := []struct {
		name string
		inv  hook.Invocation
		code errs.Code
	}{
		{"no tenant", invocation("", "p", "whoami", nil), errs.CodeInvalidArgument},
		{"a tenant that is a path", invocation("../../etc", "p", "whoami", nil), errs.CodeInvalidArgument},
		{"no plugin", invocation("acme", "", "whoami", nil), errs.CodeInvalidArgument},
		{"unknown plugin", invocation("acme", "ghost", "whoami", nil), errs.CodeNotFound},
		{"unknown action", invocation("acme", "p", "nope", nil), errs.CodeFailedPrecondition},
		{"tenant with no grant", invocation("other", "p", "whoami", nil), errs.CodeForbidden},
	}
	for _, c := range cases {
		if _, err := f.r.RunHookAction(ctx, c.inv); !errs.IsCode(err, c.code) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	f.grants.set("other", "p", Grant{Enabled: false})
	if _, err := f.r.RunHookAction(ctx, invocation("other", "p", "whoami", nil)); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("disabled by the tenant: %v", err)
	}
	f.grants.set("other", "p", Grant{Enabled: true, Permissions: plugin.Permissions{Secrets: []string{"never-declared"}}})
	if _, err := f.r.RunHookAction(ctx, invocation("other", "p", "whoami", nil)); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("a grant beyond the manifest: %v", err)
	}
	f.grants.err = errs.New(errs.CodeFailedPrecondition, "plugins.json is not valid")
	if _, err := f.r.RunHookAction(ctx, invocation("acme", "p", "whoami", nil)); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("an unreadable grant file fails closed: %v", err)
	}
}

// TestRemoteRecordIsRefusedAtStart covers a record already in the registry that this
// version cannot run, however it got there (I42).
func TestRemoteRecordIsRefusedAtStart(t *testing.T) {
	f := newFixture(t, unsandboxed)
	raw := encode(t, func() map[string]any {
		m := validManifest()
		m["id"] = "remote-one"
		m["runtime"] = "remote"
		delete(m, "entrypoint")
		return m
	}())
	ctx := context.Background()
	if err := f.repo.Upsert(ctx, domain.PluginRecord{ID: "remote-one", Version: "1.2.3", Manifest: raw, SHA256: strings.Repeat("a", 64), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	f.grants.set("acme", "remote-one", Grant{Enabled: true})
	inv := invocation("acme", "remote-one", "check-ready-for-clipping", nil)
	if _, err := f.r.RunHookAction(ctx, inv); !errs.IsCode(err, errs.CodeNotImplemented) {
		t.Errorf("want not_implemented, got %v", err)
	}
}

func TestIdempotencyKeyAndTraceReachThePlugin(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	read := func(inv hook.Invocation) map[string]any {
		t.Helper()
		result, err := f.r.RunHookAction(context.Background(), inv)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(result.Parameters, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	inv := invocation("acme", "p", "whoami", nil)
	first := read(inv)
	if first["key"] == "" || first["key"] != read(inv)["key"] {
		t.Errorf("the key must be stable across retries: %+v", first)
	}
	other := inv
	other.NodeID = "another-node"
	if read(other)["key"] == first["key"] {
		t.Error("another node needs another key")
	}
	explicit := inv
	explicit.IdempotencyKey = "given-by-the-caller"
	explicit.TraceID = "trace-from-caller"
	got := read(explicit)
	if got["key"] != "given-by-the-caller" || got["trace"] != "trace-from-caller" {
		t.Errorf("explicit values must be kept: %+v", got)
	}
	if first["tenant"] != "acme" {
		t.Errorf("initialize names the tenant: %+v", first)
	}
}

func TestMetricsAndAuditCarryTheTenant(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	f.mustCall("p", "whoami", nil)
	if _, err := f.call("p", "nope", nil); err == nil {
		t.Fatal("expected a failure")
	}
	durations := f.metrics.get(telemetry.PluginDuration)
	if len(durations) != 2 || durations[0]["plugin"] != "p" || durations[0]["method"] != plugin.MethodHookInvoke || durations[0][telemetry.LabelTenant] != "acme" {
		t.Errorf("durations: %+v", durations)
	}
	failures := f.metrics.get(telemetry.PluginFailures)
	if len(failures) != 1 || failures[0]["reason"] != string(errs.CodeFailedPrecondition) || failures[0][telemetry.LabelTenant] != "acme" {
		t.Errorf("failures: %+v", failures)
	}
}

func TestOneProcessPerTenant(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{Filesystem: []string{PermJobTemp}})
	f.grants.set("other", "p", Grant{Enabled: true, Permissions: plugin.Permissions{Filesystem: []string{PermJobTemp}}})
	ctx := context.Background()
	who := func(tenant string) map[string]any {
		t.Helper()
		result, err := f.r.RunHookAction(ctx, invocation(tenant, "p", "whoami", nil))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(result.Parameters, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	a1, a2, b := who("acme"), who("acme"), who("other")
	if a1["pid"] != a2["pid"] {
		t.Error("the same tenant reuses its process")
	}
	if a1["pid"] == b["pid"] {
		t.Error("two tenants must never share a process (D6)")
	}
	if a1["temp"] == b["temp"] || a1["temp"] == "" {
		t.Errorf("temp directories: %v %v", a1["temp"], b["temp"])
	}
	if a1["tenant"] != "acme" || b["tenant"] != "other" {
		t.Errorf("tenants: %v %v", a1["tenant"], b["tenant"])
	}
}

func TestGrantChangeRestartsTheProcess(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{Filesystem: []string{PermJobTemp}})
	pid := func() any {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &got); err != nil {
			t.Fatal(err)
		}
		return got["pid"]
	}
	first := pid()
	if pid() != first {
		t.Fatal("an unchanged grant keeps the process")
	}
	f.grants.set("acme", "p", Grant{Enabled: true}) // narrower: a sandbox cannot change while running
	if second := pid(); second == first {
		t.Error("a changed grant must restart the process")
	}
}

// ---- failure handling ----

func TestTimeoutAndRestart(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	ctx := context.Background()
	slow := invocation("acme", "p", "ignore-cancel", map[string]int{"millis": 3000})
	slow.Hook.TimeoutSeconds = 1
	start := time.Now()
	if _, err := f.r.RunHookAction(ctx, slow); !errs.IsCode(err, errs.CodeTimeout) {
		t.Fatalf("want timeout: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the timeout took %v", time.Since(start))
	}
	// A context deadline shorter than the hook's also applies.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	long := invocation("acme", "p", "sleep", map[string]int{"millis": 3000})
	if _, err := f.r.RunHookAction(short, long); !errs.IsCode(err, errs.CodeTimeout) {
		t.Errorf("the caller's deadline: %v", err)
	}
	// The default applies when timeoutSeconds is omitted; just prove it runs.
	noTimeout := invocation("acme", "p", "sleep", map[string]int{"millis": 1})
	noTimeout.Hook.TimeoutSeconds = 0
	if _, err := f.r.RunHookAction(ctx, noTimeout); err != nil {
		t.Errorf("default timeout: %v", err)
	}

	// Timeouts in a row mean the plugin ignores cancellation: it gets replaced.
	var first map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &first); err != nil {
		t.Fatal(err)
	}
	for range consecutiveTimeouts {
		short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		_, _ = f.r.RunHookAction(short, invocation("acme", "p", "ignore-cancel", map[string]int{"millis": 200}))
		cancel()
	}
	eventually(t, "a new process", func() bool {
		result, err := f.call("p", "whoami", nil)
		if err != nil {
			return false
		}
		var next map[string]any
		return json.Unmarshal(result.Parameters, &next) == nil && next["pid"] != first["pid"]
	})
}

func TestCrashIsReportedAndRestartedWithBackoff(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	if _, err := f.call("p", "crash", nil); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("a crash during a call: %v", err)
	}
	eventually(t, "the exit to be logged", func() bool { return strings.Contains(f.logs.String(), "plugin.exited") })
	// During the backoff the runtime answers quickly and clearly.
	if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Errorf("during the backoff: %v", err)
	}
	eventually(t, "a restart after the backoff", func() bool {
		_, err := f.call("p", "whoami", nil)
		return err == nil
	})
	found := false
	for _, labels := range f.metrics.get(telemetry.PluginFailures) {
		found = found || labels["reason"] == "exited"
	}
	if !found {
		t.Error("the crash must be counted")
	}
}

func TestBackoffGrows(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &slot{}
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		s.recordFailure(now)
		if got := s.nextStart.Sub(now); got != want {
			t.Errorf("failure %d: backoff %v, want %v", i+1, got, want)
		}
	}
	for range 20 {
		s.recordFailure(now)
	}
	if got := s.nextStart.Sub(now); got != maxRestartBackoff {
		t.Errorf("backoff must cap: %v", got)
	}
}

func TestHandshakeFailures(t *testing.T) {
	cases := []struct {
		flag string
		code errs.Code
	}{
		{"--wrong-id", errs.CodeFailedPrecondition},
		{"--wrong-protocol", errs.CodeFailedPrecondition},
		{"--claim-more", errs.CodeFailedPrecondition},
		{"--crash-at-start", errs.CodeUnavailable},
	}
	for _, c := range cases {
		t.Run(c.flag, func(t *testing.T) {
			f := newFixture(t, unsandboxed)
			f.installProbe("p", plugin.Permissions{}, c.flag)
			if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, c.code) {
				t.Errorf("want %s, got %v", c.code, err)
			}
			eventually(t, "nothing to be left running", func() bool { return f.r.liveProcesses() == 0 })
			// A process that started and failed counts toward the backoff, so it is not
			// started again on every call.
			if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeUnavailable) {
				t.Errorf("during the backoff: %v", err)
			}
		})
	}
	t.Run("--no-init", func(t *testing.T) {
		f := newFixture(t, unsandboxed, func(o *Options) { o.InitTimeout = 100 * time.Millisecond })
		f.installProbe("p", plugin.Permissions{}, "--no-init")
		if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeTimeout) {
			t.Errorf("no answer to initialize: %v", err)
		}
		eventually(t, "nothing to be left running", func() bool { return f.r.liveProcesses() == 0 })
	})
}

func TestGarbageOnStdoutIsTolerated(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{}, "--noisy-stdout")
	f.mustCall("p", "whoami", nil)
}

func TestOversizedLineEndsTheProcess(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	inv := invocation("acme", "p", "oversized", nil)
	inv.Hook.TimeoutSeconds = 30
	if _, err := f.r.RunHookAction(context.Background(), inv); err == nil {
		t.Fatal("an oversized message must fail the call")
	}
	eventually(t, "the process to be gone", func() bool { return f.r.liveProcesses() == 0 })
}

func TestFloodKillsThePlugin(t *testing.T) {
	f := newFixture(t, unsandboxed, func(o *Options) { o.MaxHostCallsPerSecond = 50 })
	f.installProbe("p", plugin.Permissions{})
	_, _ = f.call("p", "flood", map[string]int{"count": 5000})
	eventually(t, "the flooder to be killed", func() bool { return strings.Contains(f.logs.String(), "plugin.flood") })
	found := false
	for _, labels := range f.metrics.get(telemetry.SecurityAnomalies) {
		found = found || labels["kind"] == "plugin_flood"
	}
	if !found {
		t.Error("a flood is a security anomaly")
	}
}

func TestResultLimits(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	if _, err := f.call("p", "big-parameters", map[string]int{"count": maxHookParameters + 10}); !errs.IsCode(err, errs.CodeOutputTooLarge) {
		t.Errorf("parameters over the limit: %v", err)
	}
	result := f.mustCall("p", "long-reason", map[string]int{"count": maxHookReason * 2})
	if len(result.Reason) > maxHookReason {
		t.Errorf("reason length %d", len(result.Reason))
	}
}

func TestInFlightLimit(t *testing.T) {
	f := newFixture(t, unsandboxed, func(o *Options) { o.MaxInFlight = 2 })
	f.installProbe("p", plugin.Permissions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var started atomic.Int32
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			_, _ = f.r.RunHookAction(ctx, invocation("acme", "p", "sleep", map[string]int{"millis": 1500}))
		}()
	}
	eventually(t, "two calls in flight", func() bool {
		r := f.r
		key := "acme\x00p"
		r.mu.Lock()
		s := r.slots[key]
		var p *process
		if s != nil {
			p = s.proc
		}
		r.mu.Unlock()
		return p != nil && p.inFlight() == 2
	})
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := f.r.RunHookAction(short, invocation("acme", "p", "whoami", nil)); !errs.IsCode(err, errs.CodeTimeout) {
		t.Errorf("over the limit a call waits for its deadline: %v", err)
	}
	cancel()
	wg.Wait()
}

func TestMaxProcesses(t *testing.T) {
	f := newFixture(t, unsandboxed, func(o *Options) { o.MaxProcesses = 1 })
	f.installProbe("p", plugin.Permissions{})
	f.grants.set("other", "p", Grant{Enabled: true})
	f.mustCall("p", "whoami", nil)
	if _, err := f.r.RunHookAction(context.Background(), invocation("other", "p", "whoami", nil)); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Errorf("over the process limit: %v", err)
	}
	f.mustCall("p", "whoami", nil) // the running one keeps working
}

func TestIdleProcessesStopAndRestartOnUse(t *testing.T) {
	f := newFixture(t, unsandboxed, func(o *Options) { o.IdleTimeout = 40 * time.Millisecond })
	f.installProbe("p", plugin.Permissions{Filesystem: []string{PermJobTemp}})
	var first map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &first); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle process to stop", func() bool { return f.r.liveProcesses() == 0 })
	if _, err := os.Stat(first["temp"].(string)); !os.IsNotExist(err) {
		t.Errorf("the private temp directory must go with the process: %v", err)
	}
	if !strings.Contains(f.logs.String(), `"event":"plugin.started"`) {
		t.Errorf("logs: %s", f.logs.String())
	}
	var second map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &second); err != nil {
		t.Fatal(err)
	}
	if first["pid"] == second["pid"] {
		t.Error("expected a new process")
	}
	for _, labels := range f.metrics.get(telemetry.PluginFailures) {
		if labels["reason"] == "exited" {
			t.Error("an idle stop is not a failure")
		}
	}
}

func TestCloseStopsEverythingAndRefusesMore(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	f.installProbe("stubborn", plugin.Permissions{}, "--ignore-shutdown")
	var pid map[string]any
	if err := json.Unmarshal(f.mustCall("stubborn", "whoami", nil).Parameters, &pid); err != nil {
		t.Fatal(err)
	}
	f.mustCall("p", "whoami", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := f.r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("Close took %v", time.Since(start))
	}
	if f.r.liveProcesses() != 0 {
		t.Errorf("live processes: %d", f.r.liveProcesses())
	}
	if runtime.GOOS != "windows" {
		if p, err := os.FindProcess(int(pid["pid"].(float64))); err == nil && signalZero(p) == nil {
			t.Error("a plugin ignoring shutdown must still be gone")
		}
	}
	if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Errorf("after Close: %v", err)
	}
	if err := f.r.Close(ctx); err != nil {
		t.Errorf("a second Close is a no-op: %v", err)
	}
}

func TestChildrenDieWithThePlugin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups")
	}
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	result := f.mustCall("p", "spawn-child", nil)
	if !result.Allow {
		t.Fatalf("spawn: %+v", result)
	}
	childPID, err := strconv.Atoi(result.Reason)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the child to be gone", func() bool {
		p, err := os.FindProcess(childPID)
		return err != nil || signalZero(p) != nil
	})
}

// TestDetachedChildCannotHangCleanup is a child that left the process group and keeps
// stderr open. It survives, since nothing here can find it, but it must not keep the
// daemon's goroutines or the private temp directory.
func TestDetachedChildCannotHangCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sessions")
	}
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{Filesystem: []string{PermJobTemp}})
	var who map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &who); err != nil {
		t.Fatal(err)
	}
	result := f.mustCall("p", "spawn-detached", nil)
	if !result.Allow {
		t.Fatalf("spawn: %+v", result)
	}
	childPID, err := strconv.Atoi(result.Reason)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Kill()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if err := f.r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("Close took %v", elapsed)
	}
	if _, err := os.Stat(who["temp"].(string)); !os.IsNotExist(err) {
		t.Errorf("the temp directory must be gone when stop returns: %v", err)
	}
}

func TestStderrAndHostLogAreRedactedAndNeverBlock(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	f.mustCall("p", "stderr-only", map[string]string{"text": "note about password=hunter2 and tenant"})
	eventually(t, "the line to be logged", func() bool { return strings.Contains(f.logs.String(), "plugin.stderr") })
	logs := f.logs.String()
	if strings.Contains(logs, "hunter2") {
		t.Errorf("stderr leaked: %s", logs)
	}
	if !strings.Contains(logs, `"tenant_id":"acme"`) || !strings.Contains(logs, `"plugin":"p"`) {
		t.Errorf("logs must name tenant and plugin: %s", logs)
	}
}

// ---- secrets ----

func TestSecretsOnlyThroughAPermissionCheckAndAudited(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.secrets["granted"] = "granted-value-123"
	f.secrets["ungranted"] = "ungranted-value-456"
	f.installProbe("p", plugin.Permissions{Secrets: []string{"granted", "missing"}})

	result := f.mustCall("p", "secret", map[string]string{"name": "granted"})
	if !result.Allow || result.Reason == "granted-value-123" {
		// The value comes back as a reason here only because this probe does that, and
		// the runtime scrubs what it handed out from every reason.
		if !result.Allow {
			t.Fatalf("a granted secret: %+v", result)
		}
		t.Errorf("a secret must not come back through a reason: %+v", result)
	}
	denied := f.mustCall("p", "secret", map[string]string{"name": "ungranted"})
	if denied.Allow || strings.Contains(denied.Reason, "ungranted-value-456") || !strings.Contains(denied.Reason, "permission") {
		t.Errorf("an ungranted secret: %+v", denied)
	}
	gone := f.mustCall("p", "secret", map[string]string{"name": "missing"})
	if gone.Allow || strings.Contains(gone.Reason, "internal text") {
		t.Errorf("a granted secret missing from the vault: %+v", gone)
	}
	entries := f.audit.find("plugin.secret.read")
	results := map[string]string{}
	for _, e := range entries {
		results[e.Resource] = e.Result
		if e.TenantID != "acme" || e.Principal.Email != "plugin:p" {
			t.Errorf("audit entry: %+v", e)
		}
		data, _ := json.Marshal(e)
		if strings.Contains(string(data), "value-") {
			t.Errorf("a value in the audit: %s", data)
		}
	}
	if results["secret:granted"] != "allowed" || results["secret:ungranted"] != "denied" {
		t.Errorf("audit results: %v", results)
	}
	anomaly := false
	for _, labels := range f.metrics.get(telemetry.SecurityAnomalies) {
		anomaly = anomaly || labels["kind"] == "plugin_secret_denied"
	}
	if !anomaly {
		t.Error("a denied secret is a security anomaly")
	}
}

func TestASecretThatCannotBeAuditedIsNotHandedOut(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.secrets["s"] = "value-of-the-secret"
	f.installProbe("p", plugin.Permissions{Secrets: []string{"s"}})
	f.mustCall("p", "whoami", nil) // running before the audit goes down
	f.audit.mu.Lock()
	f.audit.err = errors.New("audit is down")
	f.audit.mu.Unlock()
	result := f.mustCall("p", "secret", map[string]string{"name": "s"})
	if result.Allow || !strings.Contains(result.Reason, "audited") {
		t.Errorf("result: %+v", result)
	}
}

func TestNoResolverMeansNoSecrets(t *testing.T) {
	f := newFixture(t, unsandboxed, func(o *Options) {
		o.Secrets = func(domain.TenantID) connector.SecretResolver { return nil }
	})
	f.installProbe("p", plugin.Permissions{Secrets: []string{"s"}})
	if result := f.mustCall("p", "secret", map[string]string{"name": "s"}); result.Allow {
		t.Errorf("result: %+v", result)
	}
	g := newFixture(t, unsandboxed, func(o *Options) { o.Secrets = nil })
	g.installProbe("p", plugin.Permissions{Secrets: []string{"s"}})
	if result := g.mustCall("p", "secret", map[string]string{"name": "s"}); result.Allow {
		t.Errorf("result: %+v", result)
	}
}

func TestSecretValuesAreScrubbedFromEverythingThePluginSays(t *testing.T) {
	f := newFixture(t, unsandboxed)
	value := "this-looks-completely-ordinary"
	f.secrets["s"] = value
	f.installProbe("p", plugin.Permissions{Secrets: []string{"s"}})
	result := f.mustCall("p", "leak-secret", map[string]string{"name": "s"})
	if strings.Contains(result.Reason, value) {
		t.Errorf("reason: %s", result.Reason)
	}
	eventually(t, "both log lines", func() bool {
		logs := f.logs.String()
		return strings.Contains(logs, "plugin.stderr") && strings.Contains(logs, "probe.leak")
	})
	if logs := f.logs.String(); strings.Contains(logs, value) || strings.Contains(logs, "hunter2") {
		t.Errorf("a secret reached the logs:\n%s", logs)
	}
	_, err := f.call("p", "fail-with-secret", map[string]string{"name": "s"})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(errs.Public(err).Message, value) {
		t.Errorf("an error leaked: %v", errs.Public(err).Message)
	}
}

func TestEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("VIDYOM_MASTER_KEY_B64", "bWFzdGVyLWtleS1tYXRlcmlhbA==")
	t.Setenv("VIDYOM_INSECURE_DEV_UNSEAL", "1")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-for-plugins")
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	var env []string
	if err := json.Unmarshal(f.mustCall("p", "env", nil).Parameters, &env); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(env, "\n")
	for _, leak := range []string{"MASTER_KEY", "UNSEAL", "AWS_SECRET", "not-for-plugins"} {
		if strings.Contains(text, leak) {
			t.Errorf("the daemon's environment reached the plugin: %s", text)
		}
	}
	if !strings.Contains(text, "VIDYOM_PLUGIN_ID=p") || !strings.Contains(text, "VIDYOM_TENANT=acme") {
		t.Errorf("env: %s", text)
	}
	for _, unexpected := range []string{"HTTP_PROXY", "TMPDIR"} {
		if strings.Contains(text, unexpected) {
			t.Errorf("%s with nothing granted: %s", unexpected, text)
		}
	}
}

// ---- integrity ----

func tamper(t *testing.T, f *fixture, rec domain.PluginRecord, rel string, data []byte) {
	t.Helper()
	root := f.r.store.path(rec.ID, rec.Version, rec.SHA256)
	path := filepath.Join(root, rel)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedPluginIsRefusedDisabledAndAudited(t *testing.T) {
	for name, rel := range map[string]string{
		"entrypoint": "bin/plugin",
		"manifest":   plugin.ManifestFile,
		"added file": "bin/extra",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, unsandboxed)
			rec := f.installProbe("p", plugin.Permissions{})
			checks, err := f.r.Verify(context.Background())
			if err != nil || len(checks) != 1 || !checks[0].OK {
				t.Fatalf("before: %+v %v", checks, err)
			}
			tamper(t, f, rec, rel, []byte("#!/bin/sh\nexit 0\n"))
			checks, err = f.r.Verify(context.Background())
			if err != nil || len(checks) != 1 || checks[0].OK || checks[0].Reason == "" {
				t.Errorf("verify must see it: %+v %v", checks, err)
			}
			if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeFailedPrecondition) {
				t.Fatalf("a tampered plugin: %v", err)
			}
			stored, err := f.repo.Get(context.Background(), "p")
			if err != nil || stored.Enabled {
				t.Errorf("it must be disabled: %+v %v", stored, err)
			}
			if entries := f.audit.find("plugin.integrity_failed"); len(entries) != 1 || entries[0].Result != "failed" {
				t.Errorf("audit: %+v", entries)
			}
			if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeFailedPrecondition) {
				t.Errorf("it stays disabled: %v", err)
			}
			if ids, _ := f.r.EventSubscribers(context.Background(), "acme", event.PublicationCompleted); len(ids) != 0 {
				t.Errorf("a disabled plugin gets no events: %v", ids)
			}
			// A fresh install of the original content repairs and re-enables it.
			source := filepath.Join(f.root, "p")
			if _, err := f.r.Install(context.Background(), admin(), installRequest(source)); err != nil {
				t.Fatal(err)
			}
			f.mustCall("p", "whoami", nil)
		})
	}
}

func TestUpgradeRestartsRunningProcesses(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	var first map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &first); err != nil {
		t.Fatal(err)
	}
	m := probeManifest("p", nil)
	m["version"] = "2.0.0"
	dir := f.writeTree("v2", probeBinary, m)
	if _, err := f.r.Install(context.Background(), admin(), installRequest(dir)); err != nil {
		t.Fatal(err)
	}
	var second map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &second); err != nil {
		t.Fatal(err)
	}
	if first["pid"] == second["pid"] {
		t.Error("an upgrade must replace the running process")
	}
}

// ---- isolation selection ----

func TestNoSandboxRefusesUnlessAccepted(t *testing.T) {
	refuse := newFixture(t, func(o *Options) { o.isolator = unavailableIsolator{} })
	refuse.installProbe("p", plugin.Permissions{})
	_, err := refuse.call("p", "whoami", nil)
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("no sandbox must refuse: %v", err)
	}
	if !strings.Contains(errs.Public(err).Message, "allowUnsandboxed") {
		t.Errorf("the message must say how to accept it: %s", errs.Public(err).Message)
	}
	rec, _ := refuse.r.Get(context.Background(), admin(), "p")
	if rec.Isolation != IsolationUnavailable {
		t.Errorf("isolation: %q", rec.Isolation)
	}

	accept := newFixture(t, unsandboxed)
	accept.installProbe("p", plugin.Permissions{})
	accept.mustCall("p", "whoami", nil)
	rec, _ = accept.r.Get(context.Background(), admin(), "p")
	if rec.Isolation != IsolationNone {
		t.Errorf("isolation: %q", rec.Isolation)
	}
	if !strings.Contains(accept.logs.String(), "plugin.unsandboxed") {
		t.Error("running unsandboxed must be logged")
	}
	if entries := accept.audit.find("plugin.started_unsandboxed"); len(entries) != 1 {
		t.Errorf("running unsandboxed must be audited at each start (I260): %+v", entries)
	}
	anomaly := false
	for _, labels := range accept.metrics.get(telemetry.SecurityAnomalies) {
		anomaly = anomaly || labels["kind"] == "plugin_unsandboxed"
	}
	if !anomaly {
		t.Error("running unsandboxed must be counted")
	}
}

// ---- egress proxy, independent of the sandbox ----

func TestProxyEnforcesTheAllowList(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("the proxy credential must not reach the destination")
		}
		fmt.Fprint(w, "hello from upstream")
	}))
	defer upstream.Close()
	addr := upstream.Listener.Addr().String()
	f := newFixture(t, unsandboxed)
	f.installProbe("granted", plugin.Permissions{Network: []string{addr}})
	f.installProbe("other", plugin.Permissions{Network: []string{"127.0.0.1:1"}})
	f.installProbe("none", plugin.Permissions{})

	result := f.mustCall("granted", "http-get", map[string]string{"url": upstream.URL})
	if !result.Allow || !strings.Contains(result.Reason, "hello from upstream") {
		t.Errorf("a granted host: %+v", result)
	}
	result = f.mustCall("other", "http-get", map[string]string{"url": upstream.URL})
	if result.Allow || !strings.Contains(result.Reason, "403") {
		t.Errorf("a host not granted: %+v", result)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream saw %d requests", hits.Load())
	}
	if !strings.Contains(f.logs.String(), "plugin.network_denied") {
		t.Error("a denial must be logged")
	}
	anomaly := false
	for _, labels := range f.metrics.get(telemetry.SecurityAnomalies) {
		anomaly = anomaly || labels["kind"] == "plugin_network_denied"
	}
	if !anomaly {
		t.Error("a denial must be counted")
	}
	var env []string
	if err := json.Unmarshal(f.mustCall("none", "env", nil).Parameters, &env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(env, "\n"), "PROXY") {
		t.Errorf("no grant, no proxy: %v", env)
	}
}

func TestProxyRefusesWithoutItsTokenAndTunnels(t *testing.T) {
	logs := &logCapture{}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "tls ok") }))
	defer upstream.Close()
	host, portText, _ := strings.Cut(upstream.Listener.Addr().String(), ":")
	port, _ := strconv.Atoi(portText)
	rule, err := parseNetworkRule(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := startProxy([]networkRule{rule}, logs.logger(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if !strings.HasPrefix(proxy.URL(), "http://plugin:") {
		t.Errorf("url: %s", proxy.URL())
	}
	// No token: refused, whatever the destination.
	resp, err := http.Get("http://" + proxy.listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	// With the token, a CONNECT tunnel carries TLS.
	if !rule.matches(host, port) {
		t.Fatalf("rule %+v", rule)
	}
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	proxyURL, err := parseURL(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	got, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, got.Body); body != "tls ok" {
		t.Errorf("tunnel: %q", body)
	}
	got.Body.Close()
	transport.CloseIdleConnections()
	// CONNECT to anywhere else is denied.
	if _, err := client.Get("https://example.invalid:9/"); err == nil {
		t.Error("a CONNECT to a host not granted must fail")
	}
	if !strings.Contains(logs.String(), "plugin.network_denied") {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestProxyRefusesLinkLocalForAHostnameRule(t *testing.T) {
	logs := &logCapture{}
	rule := networkRule{host: "metadata.example.internal", port: 80}
	proxy, err := startProxy([]networkRule{rule}, logs.logger(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	dialer := proxy.dialer(rule)
	if err := dialer.Control("tcp4", "169.254.169.254:80", nil); err == nil {
		t.Error("a link-local address must be refused for a hostname rule")
	}
	if err := dialer.Control("tcp6", "[fe80::1]:80", nil); err == nil {
		t.Error("a link-local IPv6 address must be refused for a hostname rule")
	}
	if err := dialer.Control("tcp4", "10.0.0.5:80", nil); err != nil {
		t.Errorf("a private address is the operator's decision: %v", err)
	}
	if err := proxy.dialer(networkRule{host: "169.254.169.254", port: 80, ip: true}).Control("tcp4", "169.254.169.254:80", nil); err != nil {
		t.Errorf("a rule naming the IP says so explicitly: %v", err)
	}
}

// ---- language neutrality ----

func TestPluginInPython(t *testing.T) {
	out, err := exec.Command("python3", "-c", "import sys; print(sys.executable); print(sys.prefix); print(sys.base_prefix)").Output()
	if err != nil {
		t.Skipf("no usable python3: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	interpreter, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		t.Skipf("cannot resolve python3: %v", err)
	}
	roots := []string{interpreter, lines[1], lines[2]}
	f := newFixture(t, func(o *Options) {
		if err := o.isolatorCheck(); err != nil {
			o.isolator = unavailableIsolator{}
			o.AllowUnsandboxed = true
		}
		o.RuntimePaths = roots
	})
	source, err := os.ReadFile(filepath.Join("testdata", "pyplugin", "plugin.py"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{
		"id":          "py-validator",
		"version":     "1.0.0",
		"runtime":     "process",
		"entrypoint":  map[string]any{"command": "plugin.py"},
		"provides":    map[string]any{"hookActions": []string{"check"}},
		"permissions": map[string]any{},
	}
	dir := f.writeTree("py", "", m)
	script := append([]byte("#!"+interpreter+"\n"), source...)
	if err := os.WriteFile(filepath.Join(dir, "plugin.py"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Install(context.Background(), admin(), installRequest(dir)); err != nil {
		t.Fatal(err)
	}
	f.grants.set("acme", "py-validator", Grant{Enabled: true})
	result, err := f.call("py-validator", "check", map[string]string{"title": "ok"})
	if err != nil {
		t.Fatalf("%v\nlogs: %s", err, f.logs.String())
	}
	if !result.Allow {
		t.Errorf("result: %+v", result)
	}
	result = f.mustCall("py-validator", "check", map[string]string{"title": ""})
	if result.Allow || result.Reason != "python says no title" {
		t.Errorf("result: %+v", result)
	}
}

func TestAnUnauditableUnsandboxedStartIsRefused(t *testing.T) {
	f := newFixture(t, unsandboxed)
	f.installProbe("p", plugin.Permissions{})
	f.audit.mu.Lock()
	f.audit.err = errors.New("audit is down")
	f.audit.mu.Unlock()
	if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Errorf("no audit, no unsandboxed start: %v", err)
	}
	if f.r.liveProcesses() != 0 {
		t.Error("nothing should be running")
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Daemon{
		ConfigDir: "/etc/vidyom",
		DataDir:   "/var/lib/vidyom",
		SpoolDir:  "/var/spool/vidyom",
		CacheDir:  "/var/cache/vidyom",
		Plugins:   config.Plugins{InstallRoots: []string{"/srv/plugins"}, RuntimePaths: []string{"/usr/bin/python3"}, AllowUnsandboxed: true},
	}
	o := OptionsFromConfig(cfg)
	if o.Dir != "/var/lib/vidyom/plugins" || !o.AllowUnsandboxed || o.InstallRoots[0] != "/srv/plugins" || o.RuntimePaths[0] != "/usr/bin/python3" {
		t.Errorf("options: %+v", o)
	}
	if len(o.ForbiddenPaths) != 4 {
		t.Errorf("forbidden: %v", o.ForbiddenPaths)
	}
	if g, ok := o.Grants.(FileGrants); !ok || g.ConfigDir != "/etc/vidyom" {
		t.Errorf("grants: %+v", o.Grants)
	}
	cfg.Plugins.Dir = "/opt/plugins"
	if o := OptionsFromConfig(cfg); o.Dir != "/opt/plugins" {
		t.Errorf("an explicit directory wins: %s", o.Dir)
	}
}
