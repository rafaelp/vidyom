package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/schemas"
)

func validManifest() map[string]any {
	return map[string]any{
		"id":          "asset-validator",
		"version":     "1.2.3",
		"runtime":     "process",
		"entrypoint":  map[string]any{"command": "bin/plugin", "args": []string{"--serve"}},
		"provides":    map[string]any{"hookActions": []string{"check-ready-for-clipping"}, "eventSubscriptions": []string{"publication.completed"}},
		"permissions": map[string]any{"network": []string{"media.example.internal"}, "secrets": []string{"gcs-main"}, "filesystem": []string{"job-temp"}, "events": []string{"publication.completed"}},
		"contracts":   map[string]any{"plugin": "1.x", "hook": "1"},
	}
}

func encode(t *testing.T, m map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validate(t *testing.T, m map[string]any) error {
	t.Helper()
	parsed, err := ParseManifest(encode(t, m))
	if err != nil {
		return err
	}
	return Validate(parsed)
}

func with(key string, value any) map[string]any {
	m := validManifest()
	m[key] = value
	return m
}

func TestValidManifestPasses(t *testing.T) {
	if err := validate(t, validManifest()); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"0.1.0", "1.0.0-rc.1", "1.0.0+build.5"} {
		if err := validate(t, with("version", version)); err != nil {
			t.Errorf("version %s: %v", version, err)
		}
	}
}

func TestManifestsAreRefused(t *testing.T) {
	perms := func(key string, value any) map[string]any {
		m := validManifest()
		m["permissions"] = map[string]any{key: value}
		m["provides"] = map[string]any{"hookActions": []string{"a"}}
		return m
	}
	provides := func(key string, value any) map[string]any {
		m := validManifest()
		m["provides"] = map[string]any{key: value}
		return m
	}
	noEntry := validManifest()
	delete(noEntry, "entrypoint")
	noPerms := validManifest()
	delete(noPerms, "permissions")

	cases := []struct {
		name string
		doc  map[string]any
		code errs.Code
	}{
		{"schema: no permissions", noPerms, errs.CodeValidationFailed},
		{"schema: native runtime", with("runtime", "so"), errs.CodeValidationFailed},
		{"schema: unknown field", with("shell", "sh -c"), errs.CodeValidationFailed},
		{"schema: bad ID", with("id", "../evil"), errs.CodeValidationFailed},
		{"schema: bad hash", with("sha256", "abc"), errs.CodeValidationFailed},
		// The schema only checks a version's prefix, and the version becomes a path.
		{"version escaping a path", with("version", "1.0.0/../../evil"), errs.CodeValidationFailed},
		{"version with a trailing part", with("version", "1.0.0 oops"), errs.CodeValidationFailed},
		{"no entrypoint", noEntry, errs.CodeValidationFailed},
		{"absolute entrypoint", with("entrypoint", map[string]any{"command": "/bin/sh"}), errs.CodeValidationFailed},
		{"escaping entrypoint", with("entrypoint", map[string]any{"command": "../../bin/sh"}), errs.CodeValidationFailed},
		{"backslash entrypoint", with("entrypoint", map[string]any{"command": `..\bin\sh`}), errs.CodeValidationFailed},
		{"NUL in arguments", with("entrypoint", map[string]any{"command": "bin/plugin", "args": []string{"a\x00b"}}), errs.CodeValidationFailed},
		{"provides nothing", provides("hookActions", []string{}), errs.CodeValidationFailed},
		{"bad action name", provides("hookActions", []string{"a b"}), errs.CodeValidationFailed},
		{"bad event name", provides("eventSubscriptions", []string{"Not An Event"}), errs.CodeValidationFailed},
		{"event not covered by permission", with("permissions", map[string]any{"events": []string{"asset.created"}}), errs.CodeValidationFailed},
		{"network with a scheme", perms("network", []string{"https://media.example.internal"}), errs.CodeValidationFailed},
		{"network with a path", perms("network", []string{"media.example.internal/x"}), errs.CodeValidationFailed},
		{"network for everything", perms("network", []string{"*"}), errs.CodeValidationFailed},
		{"network for a whole TLD", perms("network", []string{"*.com"}), errs.CodeValidationFailed},
		{"network with a bad port", perms("network", []string{"media.example.internal:99999"}), errs.CodeValidationFailed},
		{"bad secret name", perms("secrets", []string{"a b"}), errs.CodeValidationFailed},
		{"relative path", perms("filesystem", []string{"data"}), errs.CodeValidationFailed},
		{"unclean path", perms("filesystem", []string{"/data/../etc"}), errs.CodeValidationFailed},
		{"root path", perms("filesystem", []string{"/"}), errs.CodeValidationFailed},
		{"read-only root", perms("filesystem", []string{"ro:/"}), errs.CodeValidationFailed},
		{"unknown contract", with("contracts", map[string]any{"wormhole": "1.x"}), errs.CodeValidationFailed},
		{"bad contract version", with("contracts", map[string]any{"plugin": "one"}), errs.CodeValidationFailed},
		{"incompatible contract", with("contracts", map[string]any{"plugin": "2.x"}), errs.CodeFailedPrecondition},
	}
	for _, c := range cases {
		err := validate(t, c.doc)
		if err == nil {
			t.Errorf("%s: should be refused", c.name)
			continue
		}
		if got := errs.CodeOf(err); got != c.code {
			t.Errorf("%s: code %s, want %s (%v)", c.name, got, c.code, err)
		}
	}
}

// TestRemoteAndConnectorsValidateThenRefuse is I42 and I269: valid against the schema
// (§7.6's own example must stay valid), refused as not implemented by semantic validation.
func TestRemoteAndConnectorsValidateThenRefuse(t *testing.T) {
	remote := with("runtime", "remote")
	delete(remote, "entrypoint")
	cases := map[string]map[string]any{"remote": remote}
	for _, field := range []string{"nodeTypes", "storageDrivers", "publishingDrivers", "catalogDrivers", "logSinkDrivers"} {
		m := validManifest()
		m["provides"] = map[string]any{field: []string{"x"}, "hookActions": []string{"a"}}
		m["permissions"] = map[string]any{}
		cases[field] = m
	}
	for name, doc := range cases {
		data := encode(t, doc)
		if err := schemas.Validate(schemas.PluginManifest, data); err != nil {
			t.Errorf("%s must stay valid against the schema: %v", name, err)
		}
		err := validate(t, doc)
		if !errs.IsCode(err, errs.CodeNotImplemented) {
			t.Errorf("%s: want not_implemented, got %v", name, err)
		}
	}
	spec, err := os.ReadFile(filepath.Join("..", "..", "schemas", "testdata", "plugin-7.6.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(spec)
	if err != nil {
		t.Fatalf("§7.6's example must parse: %v", err)
	}
	if err := Validate(m); !errs.IsCode(err, errs.CodeNotImplemented) {
		t.Errorf("§7.6's example is a connector plugin, not activated in this version: %v", err)
	}
}

func TestSamplePluginManifestIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "plugins", "sample-notifier", plugin.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(m); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkRules(t *testing.T) {
	good := []struct {
		text   string
		host   string
		port   int
		accept bool
	}{
		{"media.example.internal", "media.example.internal", 443, true},
		{"media.example.internal", "media.example.internal", 80, false},
		{"MEDIA.example.internal", "media.example.internal.", 443, true},
		{"media.example.internal:8443", "media.example.internal", 8443, true},
		{"media.example.internal:8443", "media.example.internal", 443, false},
		{"*.example.internal", "a.b.example.internal", 443, true},
		{"*.example.internal", "example.internal", 443, false},
		{"*.example.internal", "evilexample.internal", 443, false},
		{"*.example.internal:80", "a.example.internal", 80, true},
		{"127.0.0.1:9000", "127.0.0.1", 9000, true},
		{"127.0.0.1", "127.0.0.1", 443, true},
		{"[::1]:9000", "::1", 9000, true},
		{"[::1]", "0:0:0:0:0:0:0:1", 443, true},
		{"::1", "::1", 443, true},
	}
	for _, c := range good {
		rule, err := parseNetworkRule(c.text)
		if err != nil {
			t.Errorf("%q: %v", c.text, err)
			continue
		}
		if got := rule.matches(c.host, c.port); got != c.accept {
			t.Errorf("%q matching %s:%d: got %v", c.text, c.host, c.port, got)
		}
	}
	for _, text := range []string{"", "*", "*.com", "a*.example.internal", "https://a.example.internal", "a.example.internal/x", "user@a.example.internal", "a.example.internal:0", "a.example.internal:x", "a:b:c", "...", "a b"} {
		if _, err := parseNetworkRule(text); err == nil {
			t.Errorf("%q should be refused", text)
		}
	}
}

func TestFileGrants(t *testing.T) {
	dir := t.TempDir()
	tenantDir := filepath.Join(dir, "tenants", "acme")
	if err := os.MkdirAll(tenantDir, 0o750); err != nil {
		t.Fatal(err)
	}
	source := FileGrants{ConfigDir: dir}
	ctx := context.Background()

	if _, found, err := source.PluginGrant(ctx, "acme", "x"); err != nil || found {
		t.Errorf("no file: found %v, err %v", found, err)
	}
	file := filepath.Join(tenantDir, GrantFileName)
	os.WriteFile(file, []byte(`{"plugins":{"x":{"enabled":true,"permissions":{"secrets":["s"],"filesystem":["job-temp"]}}}}`), 0o640)
	grant, found, err := source.PluginGrant(ctx, "acme", "x")
	if err != nil || !found || !grant.Enabled || grant.Permissions.Secrets[0] != "s" {
		t.Errorf("grant: %+v %v %v", grant, found, err)
	}
	if _, found, err := source.PluginGrant(ctx, "acme", "other"); err != nil || found {
		t.Errorf("another plugin: %v %v", found, err)
	}
	if _, found, err := source.PluginGrant(ctx, "other-tenant", "x"); err != nil || found {
		t.Errorf("another tenant has its own file: %v %v", found, err)
	}
	// A typo must fail loudly, not grant or revoke quietly.
	os.WriteFile(file, []byte(`{"plugins":{"x":{"enabled":true,"permisions":{}}}}`), 0o640)
	if _, _, err := source.PluginGrant(ctx, "acme", "x"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("unknown field: %v", err)
	}
	os.WriteFile(file, []byte(`{`), 0o640)
	if _, _, err := source.PluginGrant(ctx, "acme", "x"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("malformed: %v", err)
	}
	if _, _, err := source.PluginGrant(ctx, "../../etc", "x"); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("a tenant that is a path: %v", err)
	}
	if _, found, err := (FileGrants{}).PluginGrant(ctx, "acme", "x"); err != nil || found {
		t.Errorf("no config directory grants nothing: %v %v", found, err)
	}
}

func TestEffectivePermissionsAreTheIntersection(t *testing.T) {
	m := plugin.Manifest{ID: "p", Permissions: plugin.Permissions{
		Network:    []string{"a.example.internal", "b.example.internal"},
		Secrets:    []string{"s1", "s2"},
		Filesystem: []string{"job-temp", "/srv/data"},
		Events:     []string{"publication.completed", "asset.created"},
	}}
	got, err := effectivePermissions(m, Grant{Enabled: true, Permissions: plugin.Permissions{Secrets: []string{"s2", "s2"}, Events: []string{"asset.created"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Network) != 0 || len(got.Filesystem) != 0 || len(got.Secrets) != 1 || got.Secrets[0] != "s2" || got.Events[0] != "asset.created" {
		t.Errorf("effective: %+v", got)
	}
	for name, extra := range map[string]plugin.Permissions{
		"network":    {Network: []string{"c.example.internal"}},
		"secret":     {Secrets: []string{"s3"}},
		"filesystem": {Filesystem: []string{"/etc"}},
		"event":      {Events: []string{"budget.exceeded"}},
	} {
		if _, err := effectivePermissions(m, Grant{Enabled: true, Permissions: extra}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
			t.Errorf("a grant beyond the manifest (%s): %v", name, err)
		}
	}
}

func TestForbiddenPathOverlap(t *testing.T) {
	forbidden := []string{"/var/lib/vidyom", "/etc/vidyom"}
	for _, entry := range []string{"/var/lib/vidyom", "/var/lib/vidyom/tenants", "/var/lib", "ro:/var", "/etc/vidyom/daemon.json"} {
		if err := checkForbiddenPaths(plugin.Permissions{Filesystem: []string{entry}}, forbidden); err == nil {
			t.Errorf("%s reaches a daemon directory", entry)
		}
	}
	for _, entry := range []string{"job-temp", "/var/lib/vidyom-other", "/srv/data", "ro:/opt/models"} {
		if err := checkForbiddenPaths(plugin.Permissions{Filesystem: []string{entry}}, forbidden); err != nil {
			t.Errorf("%s: %v", entry, err)
		}
	}
}

func installRequest(path string) api.InstallPluginRequest {
	return api.InstallPluginRequest{Path: path}
}

func TestInstallCopiesVerifiesAndGrantsNothing(t *testing.T) {
	f := newFixture(t)
	dir := f.writeTree("v1", probeBinary, probeManifest("p", map[string]any{"secrets": []string{"s"}, "filesystem": []string{"job-temp"}}))
	if err := os.WriteFile(filepath.Join(dir, "module.py"), []byte("x = 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	rec, err := f.r.Install(context.Background(), admin(), installRequest(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Enabled || rec.Version != "1.0.0" || len(rec.SHA256) != 64 || rec.InstalledBy != "admin@example.com" || rec.InstalledAt.IsZero() {
		t.Errorf("record: %+v", rec)
	}
	if rec.TenantGrant != nil {
		t.Errorf("installing grants nothing: %s", rec.TenantGrant)
	}
	stored, err := f.repo.Get(context.Background(), "p")
	if err != nil || stored.SHA256 != rec.SHA256 {
		t.Errorf("registry: %+v %v", stored, err)
	}
	if rec.Isolation == "" {
		t.Error("isolation must be reported")
	}
	// The source can change or vanish: what runs is the private copy.
	target := f.r.store.path("p", "1.0.0", rec.SHA256)
	if err := os.WriteFile(filepath.Join(dir, "module.py"), []byte("x = 2\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(target, "module.py"))
	if err != nil || string(copied) != "x = 1\n" {
		t.Errorf("copy: %q %v", copied, err)
	}
	for _, path := range []string{filepath.Join(target, "module.py"), filepath.Join(target, "bin", "plugin")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s is writable: %v", path, info.Mode())
		}
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Errorf("store directory is writable: %v %v", info.Mode(), err)
	}
	audited := f.audit.find("plugin.install")
	if len(audited) != 1 || audited[0].Result != "allowed" || audited[0].Details["sha256"] != rec.SHA256 || audited[0].Details["source"] == nil || audited[0].Permission != domain.PermPluginInstall {
		t.Errorf("audit: %+v", audited)
	}
	// Not granted: the plugin does not run for this tenant.
	if _, err := f.call("p", "whoami", nil); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("an installed plugin with no grant must not run: %v", err)
	}
}

func TestInstallAuthorization(t *testing.T) {
	f := newFixture(t)
	dir := f.writeTree("v1", probeBinary, probeManifest("p", nil))
	ctx := context.Background()
	tenantAdmin := admin()
	tenantAdmin.Principal.SystemAdmin = false
	if _, err := f.r.Install(ctx, tenantAdmin, installRequest(dir)); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("a tenant admin alone: %v", err)
	}
	viewer := admin()
	viewer.Principal.Role = domain.RoleViewer
	if _, err := f.r.Install(ctx, viewer, installRequest(dir)); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("a system admin with no tenant permission: %v", err)
	}
	if _, err := f.repo.Get(ctx, "p"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Error("nothing should be installed")
	}
}

func TestInstallPathRules(t *testing.T) {
	f := newFixture(t)
	dir := f.writeTree("v1", probeBinary, probeManifest("p", nil))
	ctx := context.Background()
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, plugin.ManifestFile), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.root, "link-outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		code errs.Code
	}{
		{"relative", "./v1", errs.CodeInvalidArgument},
		{"missing", filepath.Join(f.root, "nothing"), errs.CodeInvalidArgument},
		{"a file", filepath.Join(dir, plugin.ManifestFile), errs.CodeInvalidArgument},
		{"outside the roots", outside, errs.CodeForbidden},
		{"a symlink out of the roots", link, errs.CodeForbidden},
	}
	for _, c := range cases {
		if _, err := f.r.Install(ctx, admin(), installRequest(c.path)); !errs.IsCode(err, c.code) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	noRoots := newFixture(t, func(o *Options) { o.InstallRoots = nil })
	if _, err := noRoots.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("no roots means install is off: %v", err)
	}
}

func TestInstallRefusesBadTrees(t *testing.T) {
	ctx := context.Background()
	t.Run("no manifest", func(t *testing.T) {
		f := newFixture(t)
		dir := filepath.Join(f.root, "x")
		os.MkdirAll(dir, 0o750)
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
	t.Run("symlink in the tree", func(t *testing.T) {
		f := newFixture(t)
		dir := f.writeTree("x", probeBinary, probeManifest("p", nil))
		if err := os.Symlink("/etc/hosts", filepath.Join(dir, "hosts")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
	t.Run("setuid file", func(t *testing.T) {
		f := newFixture(t)
		dir := f.writeTree("x", probeBinary, probeManifest("p", nil))
		if err := os.Chmod(filepath.Join(dir, "bin", "plugin"), 0o755|os.ModeSetuid); err != nil {
			t.Skipf("cannot set setuid here: %v", err)
		}
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
	t.Run("entrypoint missing", func(t *testing.T) {
		f := newFixture(t)
		dir := f.writeTree("x", "", probeManifest("p", nil))
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
	t.Run("entrypoint not executable", func(t *testing.T) {
		f := newFixture(t)
		dir := f.writeTree("x", probeBinary, probeManifest("p", nil))
		if err := os.Chmod(filepath.Join(dir, "bin", "plugin"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
	t.Run("bare python3 is not a lookup on PATH", func(t *testing.T) {
		f := newFixture(t)
		m := probeManifest("p", nil)
		m["entrypoint"] = map[string]any{"command": "python3"}
		dir := f.writeTree("x", probeBinary, m)
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
	t.Run("hash mismatch", func(t *testing.T) {
		f := newFixture(t)
		m := probeManifest("p", nil)
		m["sha256"] = strings.Repeat("0", 64)
		dir := f.writeTree("x", probeBinary, m)
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
	t.Run("hash match", func(t *testing.T) {
		f := newFixture(t)
		sum, err := hashFile(probeBinary)
		if err != nil {
			t.Fatal(err)
		}
		m := probeManifest("p", nil)
		m["sha256"] = sum
		dir := f.writeTree("x", probeBinary, m)
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); err != nil {
			t.Errorf("%v", err)
		}
	})
	t.Run("a daemon directory in permissions", func(t *testing.T) {
		data := t.TempDir()
		f := newFixture(t, func(o *Options) { o.ForbiddenPaths = []string{data} })
		dir := f.writeTree("x", probeBinary, probeManifest("p", map[string]any{"filesystem": []string{"ro:" + data}}))
		if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%v", err)
		}
	})
}

func TestInstallVersionRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := f.writeTree("v1", probeBinary, probeManifest("p", nil))
	first, err := f.r.Install(ctx, admin(), installRequest(dir))
	if err != nil {
		t.Fatal(err)
	}
	// Same content again is a no-op, and audits nothing new.
	again, err := f.r.Install(ctx, admin(), installRequest(dir))
	if err != nil || again.SHA256 != first.SHA256 {
		t.Errorf("idempotent: %+v %v", again, err)
	}
	if len(f.audit.find("plugin.install")) != 1 {
		t.Error("a no-op must not audit an install")
	}
	// Same version with other content is a conflict.
	os.WriteFile(filepath.Join(dir, "extra"), []byte("changed"), 0o640)
	if _, err := f.r.Install(ctx, admin(), installRequest(dir)); !errs.IsCode(err, errs.CodeConflict) {
		t.Errorf("same version, new content: %v", err)
	}
	// A new version replaces the record and removes the old files.
	m := probeManifest("p", nil)
	m["version"] = "1.1.0"
	v2 := f.writeTree("v2", probeBinary, m)
	second, err := f.r.Install(ctx, admin(), installRequest(v2))
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != "1.1.0" {
		t.Errorf("record: %+v", second)
	}
	if _, err := os.Stat(f.r.store.path("p", "1.0.0", first.SHA256)); !os.IsNotExist(err) {
		t.Errorf("the old version should be gone: %v", err)
	}
}

func TestInstallStopsWhenItCannotBeAudited(t *testing.T) {
	f := newFixture(t)
	f.audit.err = os.ErrPermission
	dir := f.writeTree("v1", probeBinary, probeManifest("p", nil))
	ctx := context.Background()
	if _, err := f.r.Install(ctx, admin(), installRequest(dir)); err == nil {
		t.Fatal("an unaudited install must not happen")
	}
	if _, err := f.repo.Get(ctx, "p"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Error("nothing should be in the registry")
	}
}

func TestInstallSurvivesRegistryFailure(t *testing.T) {
	f := newFixture(t)
	f.repo.err = errs.New(errs.CodeInternal, "registry down")
	dir := f.writeTree("v1", probeBinary, probeManifest("p", nil))
	if _, err := f.r.Install(context.Background(), admin(), installRequest(dir)); err == nil {
		t.Fatal("a failed registry write must fail")
	}
	entries := f.audit.find("plugin.install")
	if len(entries) == 0 || entries[len(entries)-1].Result != "failed" {
		t.Errorf("the failure should be audited: %+v", entries)
	}
}

func TestListAndGetHideOtherTenantsPeople(t *testing.T) {
	f := newFixture(t)
	dir := f.writeTree("v1", probeBinary, probeManifest("p", map[string]any{"secrets": []string{"s"}}))
	ctx := context.Background()
	if _, err := f.r.Install(ctx, admin(), installRequest(dir)); err != nil {
		t.Fatal(err)
	}
	f.grants.set("other", "p", Grant{Enabled: true, Permissions: plugin.Permissions{Secrets: []string{"s"}}})
	tenantUser := service.Scope{Tenant: "other", Principal: domain.Principal{Email: "u@example.com", Role: domain.RoleViewer}}
	list, err := f.r.List(ctx, tenantUser)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list[0].InstalledBy != "" {
		t.Errorf("another tenant's admin must not be shown: %+v", list[0])
	}
	var grant plugin.Permissions
	if err := json.Unmarshal(list[0].TenantGrant, &grant); err != nil || grant.Secrets[0] != "s" {
		t.Errorf("this tenant's grant: %s %v", list[0].TenantGrant, err)
	}
	got, err := f.r.Get(ctx, admin(), "p")
	if err != nil || got.InstalledBy == "" {
		t.Errorf("a system admin sees it: %+v %v", got, err)
	}
	if got.TenantGrant != nil {
		t.Errorf("acme granted nothing: %s", got.TenantGrant)
	}
	if _, err := f.r.Get(ctx, admin(), "ghost"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	good := Options{Repo: newFakeRepo(), Dir: t.TempDir(), Grants: &grantMap{}, Auditor: &fakeAuditor{}}
	for name, change := range map[string]func(*Options){
		"repo":    func(o *Options) { o.Repo = nil },
		"dir":     func(o *Options) { o.Dir = "" },
		"grants":  func(o *Options) { o.Grants = nil },
		"auditor": func(o *Options) { o.Auditor = nil },
	} {
		opts := good
		change(&opts)
		if _, err := New(opts); err == nil {
			t.Errorf("no %s should be refused", name)
		}
	}
}
