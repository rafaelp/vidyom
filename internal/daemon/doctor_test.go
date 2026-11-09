package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiserver "github.com/rafaelp/vidyom/internal/api"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

type fakeStorages struct {
	service.Storages
	failRole domain.StorageRole
}

func (f fakeStorages) Test(_ context.Context, _ service.Scope, role domain.StorageRole) (api.StorageTestResponse, error) {
	if role == f.failRole {
		return api.StorageTestResponse{OK: false, Detail: "bucket not reachable"}, nil
	}
	return api.StorageTestResponse{OK: true}, nil
}

type fakeQueue struct {
	jobqueue.Queue
	err error
}

func (q fakeQueue) Ping(context.Context) error { return q.err }

func newDoctorFixture(t *testing.T, level security.SealingLevel) *doctor {
	t.Helper()
	dir := tempDir(t)
	cfg := config.Daemon{DataDir: dir, SpoolDir: filepath.Join(dir, "spool"), ScratchDir: filepath.Join(dir, "scratch")}
	d := &doctor{
		cfg:  cfg,
		deps: Deps{MasterKey: &fakeProvider{key: make([]byte, 32), sealing: level}},
		api:  apiserver.Deps{Storages: fakeStorages{}},
	}
	d.setListeners([]ListenerInfo{
		{Kind: "unix", Address: "/run/v.sock", Mode: "0600"},
		{Kind: "https", Address: "127.0.0.1:8443", TLSMode: config.TLSAuto, CertNotAfter: time.Now().Add(365 * 24 * time.Hour)},
	})
	return d
}

func find(checks []api.DoctorCheck, name string) (api.DoctorCheck, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return api.DoctorCheck{}, false
}

var (
	sysAdmin = service.Scope{Principal: domain.Principal{SystemAdmin: true}}
	tenantOp = service.Scope{Tenant: "acme", Principal: domain.Principal{Role: domain.RoleAdmin}}
	viewer   = service.Scope{Tenant: "acme", Principal: domain.Principal{Role: domain.RoleViewer}}
)

func TestDoctorAccess(t *testing.T) {
	d := newDoctorFixture(t, security.SealingOS)
	ctx := context.Background()

	// A viewer gets reachability only, and is told no when asking for more.
	out, err := d.Run(ctx, viewer, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Checks) != 1 || out.Checks[0].Name != "daemon.version" {
		t.Fatalf("a viewer saw: %+v", out.Checks)
	}
	for _, check := range []string{"security", "network", "storage"} {
		if _, err := d.Run(ctx, viewer, check); !errs.IsCode(err, errs.CodeForbidden) {
			t.Errorf("%s: %v", check, err)
		}
	}

	// An admin gets all of it.
	out, err = d.Run(ctx, tenantOp, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"security.master_key", "network.listener", "storage.master"} {
		if _, ok := find(out.Checks, name); !ok {
			t.Errorf("no %s in %+v", name, out.Checks)
		}
	}

	// The storage check needs a tenant, even for a system admin.
	if _, err := d.Run(ctx, sysAdmin, "storage"); !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Errorf("storage with no tenant: %v", err)
	}
	if out, err := d.Run(ctx, sysAdmin, ""); err != nil {
		t.Fatal(err)
	} else if _, ok := find(out.Checks, "storage.master"); ok {
		t.Errorf("tenant storage with no tenant: %+v", out.Checks)
	}
	if _, err := d.Run(ctx, sysAdmin, "nope"); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("unknown check: %v", err)
	}
}

// §I10: sealing below D13's target is stated, not hidden.
func TestDoctorStatesSealingHonestly(t *testing.T) {
	for level, want := range map[security.SealingLevel]string{
		security.SealingHardware: statusOK,
		security.SealingKMS:      statusOK,
		security.SealingOS:       statusOK,
		security.SealingFile:     statusWarn,
		security.SealingDev:      statusWarn,
	} {
		d := newDoctorFixture(t, level)
		out, err := d.Run(context.Background(), sysAdmin, "security")
		if err != nil {
			t.Fatal(err)
		}
		check, ok := find(out.Checks, "security.master_key")
		if !ok || check.Status != want || !strings.Contains(check.Detail, string(level)) {
			t.Errorf("%s: %+v", level, check)
		}
	}
}

func TestDoctorSecurity(t *testing.T) {
	d := newDoctorFixture(t, security.SealingOS)
	d.setListeners([]ListenerInfo{
		{Kind: "https", Address: "a", TLSMode: config.TLSFiles, MutualTLS: true, CertNotAfter: time.Now().Add(time.Hour)},
		{Kind: "https", Address: "b", TLSMode: config.TLSFiles, CertNotAfter: time.Now().Add(-time.Hour)},
		{Kind: "unix", Address: "/run/v.sock", Mode: "0600"},
	})
	out, err := d.Run(context.Background(), sysAdmin, "security")
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, c := range out.Checks {
		if c.Name == "security.tls" {
			statuses[c.Detail[:1]] = c.Status
			if c.Detail[:1] == "a" && !strings.Contains(c.Detail, "client certificates required") {
				t.Errorf("mutual TLS not stated: %s", c.Detail)
			}
		}
	}
	if statuses["a"] != statusWarn || statuses["b"] != statusFail {
		t.Fatalf("certificates: %v", statuses)
	}

	if err := os.Chmod(d.cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	out, err = d.Run(context.Background(), sysAdmin, "security")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := find(out.Checks, "security.data_dir"); c.Status != statusWarn {
		t.Fatalf("data dir: %+v", c)
	}
}

func TestDoctorNetwork(t *testing.T) {
	d := newDoctorFixture(t, security.SealingOS)
	d.deps.Queue = fakeQueue{err: errors.New("connection refused to redis.internal:6379")}
	d.api.HealthChecks = []apiserver.HealthCheck{
		{Name: "ai_worker", Check: func(context.Context) error { return errors.New("gone") }},
		{Name: "redis", Critical: true, Check: func(context.Context) error { return nil }},
	}
	d.setListeners([]ListenerInfo{
		{Kind: "https", Address: "203.0.113.5:443", Public: true},
		{Kind: "https", Address: "172.20.0.5:8443", Interface: "eth0", LoopbackPublished: true},
		{Kind: "unix", Address: "/run/v.sock"},
	})
	out, err := d.Run(context.Background(), sysAdmin, "network")
	if err != nil {
		t.Fatal(err)
	}
	var warned int
	for _, c := range out.Checks {
		if c.Name == "network.listener" && c.Status == statusWarn {
			warned++
		}
	}
	if warned != 2 {
		t.Fatalf("listener warnings: %+v", out.Checks)
	}
	redis, _ := find(out.Checks, "network.redis")
	if redis.Status != statusFail || strings.Contains(redis.Detail, "redis.internal") {
		t.Fatalf("redis: %+v", redis)
	}
	if ai, _ := find(out.Checks, "network.ai_worker"); ai.Status != statusWarn {
		t.Fatalf("ai worker: %+v", ai)
	}
	if strings.Count(toText(out.Checks), "network.redis") != 1 {
		t.Fatalf("redis reported twice: %+v", out.Checks)
	}
}

func toText(checks []api.DoctorCheck) string {
	var b strings.Builder
	for _, c := range checks {
		b.WriteString(c.Name + "\n")
	}
	return b.String()
}

func TestDoctorStorage(t *testing.T) {
	d := newDoctorFixture(t, security.SealingOS)
	d.api.Storages = fakeStorages{failRole: domain.RoleProxy}
	out, err := d.Run(context.Background(), tenantOp, "storage")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := find(out.Checks, "storage.proxy"); c.Status != statusFail || c.Detail != "bucket not reachable" {
		t.Fatalf("proxy: %+v", c)
	}
	if c, _ := find(out.Checks, "storage.master"); c.Status != statusOK {
		t.Fatalf("master: %+v", c)
	}
	if c, _ := find(out.Checks, "storage.spool"); c.Status == statusFail {
		t.Fatalf("spool: %+v", c)
	}

	d.api.Storages = nil
	out, err = d.Run(context.Background(), tenantOp, "storage")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := find(out.Checks, "storage.roles"); c.Status != statusWarn {
		t.Fatalf("unwired: %+v", out.Checks)
	}
}

func TestCheckWritable(t *testing.T) {
	dir := tempDir(t)
	if c := checkWritable("x", filepath.Join(dir, "a", "b")); c.Status == statusFail {
		t.Fatalf("%+v", c)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if c := checkWritable("x", filepath.Join(file, "sub")); c.Status != statusFail {
		t.Fatalf("%+v", c)
	}
}
