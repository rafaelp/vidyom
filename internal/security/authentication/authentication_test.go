package authentication

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
	"github.com/rafaelp/vidyom/internal/security/audit"
	"github.com/rafaelp/vidyom/internal/security/authorization"
)

var ctx = context.Background()

type fixture struct {
	dir     string
	stores  *persistence.Stores
	sys     repo.SystemStore
	auth    *Service
	authz   *authorization.Authorizer
	auditor *audit.Auditor
	clock   *time.Time
}

func setup(t *testing.T, opts ...Options) *fixture {
	t.Helper()
	dir := t.TempDir()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stores.Close() })
	for _, tn := range []domain.TenantID{"acme", "beta"} {
		if _, err := stores.Tenants.Create(ctx, tn); err != nil {
			t.Fatal(err)
		}
		if err := stores.System.Tenants().Create(ctx, domain.Tenant{ID: tn, ULID: id.New()}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	clock := &now
	auditor := audit.New(stores.Tenants, audit.Options{System: stores.System})
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	o.Auditor = auditor
	o.Clock = func() time.Time { return *clock }
	return &fixture{dir, stores, stores.System, New(stores.System, o), authorization.New(stores.System, authorization.Options{}), auditor, clock}
}

func (f *fixture) user(t *testing.T, email string, systemAdmin bool, tenant domain.TenantID, role domain.Role) domain.Principal {
	t.Helper()
	u := domain.User{ULID: id.New(), Email: email, SystemAdmin: systemAdmin}
	if err := f.sys.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	p := domain.Principal{UserULID: u.ULID, Email: email, Method: domain.AuthDevice, SystemAdmin: systemAdmin}
	if tenant != "" {
		if err := f.sys.Users().SetMembership(ctx, domain.Membership{UserULID: u.ULID, TenantID: tenant, Role: role}); err != nil {
			t.Fatal(err)
		}
		resolved, err := f.authz.Resolve(ctx, p, tenant)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	return p
}

// pair goes through the whole flow an admin and a client do.
func (f *fixture) pair(t *testing.T, admin domain.Principal, email string) security.IssuedCredential {
	t.Helper()
	code, err := f.auth.CreateCode(ctx, admin, email, 0)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := f.auth.Pair(ctx, code, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestPairingFlow(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", false, "acme", domain.RoleAdmin)
	f.user(t, "editor@example.com", false, "acme", domain.RoleEditor)

	code, err := f.auth.CreateCode(ctx, admin, "Editor@Example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){3}$`).MatchString(code) {
		t.Fatalf("code = %s", code)
	}
	cred, err := f.auth.Pair(ctx, code, "maria's laptop")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Device.ID != "CLIENT-001" || cred.Device.Status != domain.DeviceActive || cred.Device.Name != "maria's laptop" || cred.Device.Email != "editor@example.com" {
		t.Fatalf("device = %+v", cred.Device)
	}
	if !strings.HasPrefix(cred.Token, "vdm_"+string(cred.Device.ULID)+"_") {
		t.Fatalf("token = %s", cred.Token)
	}

	// One time only.
	if _, err := f.auth.Pair(ctx, code, "other"); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("second use: %v", err)
	}
	if _, err := f.auth.Pair(ctx, "garbage", "other"); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("bad code: %v", err)
	}

	// The credential works and identifies the device and user.
	p, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token})
	if err != nil {
		t.Fatal(err)
	}
	if p.Email != "editor@example.com" || p.DeviceID != "CLIENT-001" || p.Method != domain.AuthDevice || p.TenantID != "" {
		t.Fatalf("principal = %+v", p)
	}

	// A resolved principal then has its role.
	resolved, err := f.authz.Resolve(ctx, p, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Role != domain.RoleEditor {
		t.Fatalf("role = %s", resolved.Role)
	}

	// Next device gets the next system-wide number (I14).
	next := f.pair(t, admin, "admin@example.com")
	if next.Device.ID != "CLIENT-002" {
		t.Fatalf("next = %s", next.Device.ID)
	}
}

func TestCodeIsCaseAndLookalikeInsensitive(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", true, "", "")
	code, err := f.auth.CreateCode(ctx, admin, "admin@example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	typed = strings.NewReplacer("0", "o", "1", "l").Replace(typed)
	if _, err := f.auth.Pair(ctx, "  "+typed+" ", "x"); err != nil {
		t.Fatalf("typed %q: %v", typed, err)
	}
}

func TestCodeExpires(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", true, "", "")
	code, err := f.auth.CreateCode(ctx, admin, "", 60)
	if err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(61 * time.Second)
	if _, err := f.auth.Pair(ctx, code, "x"); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, admin, "", int(2*MaxPairingTTL/time.Second)); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("too long: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, domain.Principal{SystemAdmin: true}, "", 0); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no user: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, admin, "nobody@example.com", 0); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestConcurrentPairingUsesACodeOnce(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", true, "", "")
	code, err := f.auth.CreateCode(ctx, admin, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.auth.Pair(ctx, code, "x")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d pairings won", wins)
	}
}

// §1.1 step 2 and D12: a credential can be revoked.
func TestRevocation(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", false, "acme", domain.RoleAdmin)
	f.user(t, "editor@example.com", false, "acme", domain.RoleEditor)
	cred := f.pair(t, admin, "editor@example.com")
	if err := f.auth.Revoke(ctx, admin, id.Ref(cred.Device.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token}); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("revoked: %v", err)
	}
	stored, err := f.sys.Devices().Get(ctx, id.Ref(cred.Device.ULID))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.DeviceRevoked || stored.RevokedAt == nil {
		t.Fatalf("device = %+v", stored)
	}
	// Idempotent, and a revoked device cannot rotate back to life.
	if err := f.auth.Revoke(ctx, admin, id.Ref(cred.Device.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Rotate(ctx, admin, id.Ref(cred.Device.ID)); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("rotate revoked: %v", err)
	}
}

func TestRotation(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", false, "acme", domain.RoleAdmin)
	cred := f.pair(t, admin, "admin@example.com")
	rotated, err := f.auth.Rotate(ctx, admin, id.Ref(cred.Device.ID))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Token == cred.Token || rotated.Device.ID != cred.Device.ID || rotated.Device.RotatedAt == nil {
		t.Fatalf("rotated = %+v", rotated.Device)
	}
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token}); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("old token: %v", err)
	}
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: rotated.Token}); err != nil {
		t.Fatalf("new token: %v", err)
	}
}

// I112: who may act on which device.
func TestDeviceScope(t *testing.T) {
	f := setup(t)
	root := f.user(t, "root@example.com", true, "", "")
	acmeAdmin := f.user(t, "admin@example.com", false, "acme", domain.RoleAdmin)
	betaAdmin := f.user(t, "beta-admin@example.com", false, "beta", domain.RoleAdmin)
	editor := f.user(t, "editor@example.com", false, "acme", domain.RoleEditor)
	viewer := f.user(t, "viewer@example.com", false, "acme", domain.RoleViewer)

	// Codes: tenant admins for their members, never for a system admin or outsiders.
	if _, err := f.auth.CreateCode(ctx, acmeAdmin, "editor@example.com", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.CreateCode(ctx, acmeAdmin, "beta-admin@example.com", 0); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, acmeAdmin, "root@example.com", 0); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("system admin target: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, editor, "editor@example.com", 0); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("editor: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, viewer, "", 0); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("viewer: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, domain.Principal{UserULID: editor.UserULID, Email: editor.Email}, "", 0); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("no tenant: %v", err)
	}
	if _, err := f.auth.CreateCode(ctx, root, "beta-admin@example.com", 0); err != nil {
		t.Fatal(err)
	}

	editorCred := f.pair(t, acmeAdmin, "editor@example.com")
	viewerCred := f.pair(t, acmeAdmin, "viewer@example.com")
	rootCred := f.pair(t, root, "root@example.com")

	// Owners act on their own devices; others in the tenant do not.
	if _, err := f.auth.Rotate(ctx, editor, id.Ref(editorCred.Device.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Rotate(ctx, editor, id.Ref(viewerCred.Device.ID)); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("another user's device: %v", err)
	}
	if err := f.auth.Revoke(ctx, viewer, id.Ref(editorCred.Device.ID)); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("viewer revoking: %v", err)
	}
	// Another tenant's admin sees it as absent.
	if err := f.auth.Revoke(ctx, betaAdmin, id.Ref(editorCred.Device.ID)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("other tenant's admin: %v", err)
	}
	// A tenant admin cannot touch a system admin's device.
	if err := f.auth.Revoke(ctx, acmeAdmin, id.Ref(rootCred.Device.ID)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("no shared tenant: %v", err)
	}
	if err := f.sys.Users().SetMembership(ctx, domain.Membership{UserULID: root.UserULID, TenantID: "acme", Role: domain.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.Revoke(ctx, acmeAdmin, id.Ref(rootCred.Device.ID)); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("system admin's device: %v", err)
	}
	if err := f.auth.Revoke(ctx, acmeAdmin, "CLIENT-999"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	if err := f.auth.Revoke(ctx, root, id.Ref(viewerCred.Device.ID)); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.Revoke(ctx, viewer, id.Ref(viewerCred.Device.ID)); err != nil {
		t.Fatal(err)
	}

	// Lists are scoped the same way.
	svc := f.auth.Devices()
	list := func(scope service.Scope) []string {
		t.Helper()
		devices, err := svc.List(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(devices))
		for i, d := range devices {
			out[i] = string(d.ID)
		}
		return out
	}
	if got := list(service.Scope{Principal: root}); len(got) != 3 {
		t.Fatalf("root = %v", got)
	}
	if got := list(service.Scope{Tenant: "acme", Principal: acmeAdmin}); len(got) != 3 {
		t.Fatalf("acme admin = %v", got)
	}
	if got := list(service.Scope{Tenant: "beta", Principal: betaAdmin}); len(got) != 0 {
		t.Fatalf("beta admin = %v", got)
	}
	if got := list(service.Scope{Tenant: "acme", Principal: editor}); len(got) != 1 || got[0] != string(editorCred.Device.ID) {
		t.Fatalf("editor = %v", got)
	}
	if _, err := svc.List(ctx, service.Scope{Principal: domain.Principal{Method: domain.AuthSystem}}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("anonymous list: %v", err)
	}
}

func TestPeerIdentity(t *testing.T) {
	f := setup(t, Options{LocalUsers: []config.LocalUser{{UID: 1000, Email: "ops@example.com"}, {UID: 1001, Email: "ghost@example.com"}}})
	f.user(t, "ops@example.com", false, "acme", domain.RoleAdmin)
	uid := 1000
	p, err := f.auth.Authenticate(ctx, security.Credentials{PeerUID: &uid})
	if err != nil {
		t.Fatal(err)
	}
	if p.Email != "ops@example.com" || p.Method != domain.AuthPeer || p.DeviceID != "" {
		t.Fatalf("principal = %+v", p)
	}
	for _, unknown := range []int{0, 1001, 9999} {
		u := unknown
		if _, err := f.auth.Authenticate(ctx, security.Credentials{PeerUID: &u}); !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Errorf("uid %d: %v", u, err)
		}
	}
	// A bad token never falls back to the peer identity.
	if _, err := f.auth.Authenticate(ctx, security.Credentials{PeerUID: &uid, BearerToken: "vdm_bad"}); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("bad token with a good peer: %v", err)
	}
	// A reload replaces the map.
	f.auth.SetLocalUsers(nil)
	if _, err := f.auth.Authenticate(ctx, security.Credentials{PeerUID: &uid}); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("after reload: %v", err)
	}
}

func TestBadCredentials(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", true, "", "")
	cred := f.pair(t, admin, "admin@example.com")
	other := f.pair(t, admin, "admin@example.com")
	_, secret, _ := strings.Cut(strings.TrimPrefix(cred.Token, "vdm_"), "_")
	cases := []string{
		"",
		"nonsense",
		"vdm_",
		"vdm_nothing",
		"vdm_" + string(cred.Device.ULID),
		"vdm_" + string(cred.Device.ULID) + "_",
		"vdm_" + string(cred.Device.ULID) + "_wrong",
		"vdm_notaulid_" + secret,
		// A valid secret under another device's ID.
		"vdm_" + string(other.Device.ULID) + "_" + secret,
		"vdw_" + secret,
		strings.ToUpper(cred.Token),
	}
	for _, token := range cases {
		if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: token}); !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Errorf("token %q: %v", token, err)
		}
	}
	// Errors say nothing about why.
	_, a := f.auth.Authenticate(ctx, security.Credentials{BearerToken: "vdm_nothing"})
	_, b := f.auth.Authenticate(ctx, security.Credentials{BearerToken: "vdm_" + string(cred.Device.ULID) + "_wrong"})
	if errs.Public(a).Message != errs.Public(b).Message {
		t.Fatalf("messages differ: %q, %q", a, b)
	}
	// The lower-case ULID form of the same token still works: it is the same ID.
	lower := "vdm_" + strings.ToLower(string(cred.Device.ULID)) + "_" + secret
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: lower}); err != nil {
		t.Fatalf("lower-case ULID: %v", err)
	}
}

func TestLastSeenIsThrottled(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", true, "", "")
	cred := f.pair(t, admin, "admin@example.com")
	first := *f.clock
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token}); err != nil {
		t.Fatal(err)
	}
	seen := func() time.Time {
		d, err := f.sys.Devices().Get(ctx, id.Ref(cred.Device.ID))
		if err != nil {
			t.Fatal(err)
		}
		if d.LastSeenAt == nil {
			t.Fatal("last_seen_at not set")
		}
		return *d.LastSeenAt
	}
	if seen().Sub(first).Abs() > time.Second {
		t.Fatalf("last seen = %v, want %v", seen(), first)
	}
	*f.clock = f.clock.Add(30 * time.Second)
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token}); err != nil {
		t.Fatal(err)
	}
	if seen().Sub(first).Abs() > time.Second {
		t.Fatalf("updated too soon: %v", seen())
	}
	*f.clock = f.clock.Add(31 * time.Second)
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token}); err != nil {
		t.Fatal(err)
	}
	if seen().Sub(first) < time.Minute {
		t.Fatalf("not updated: %v", seen())
	}
	// Last-seen bookkeeping never resurrects a revoked device.
	if err := f.auth.Revoke(ctx, admin, id.Ref(cred.Device.ID)); err != nil {
		t.Fatal(err)
	}
	d, _ := f.sys.Devices().Get(ctx, id.Ref(cred.Device.ID))
	if d.Status != domain.DeviceRevoked {
		t.Fatalf("device = %+v", d)
	}
}

func TestWorkerTokens(t *testing.T) {
	f := setup(t)
	token, err := f.auth.IssueWorkerToken(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "vdw_") {
		t.Fatalf("token = %s", token)
	}
	name, err := f.auth.AuthenticateWorker(ctx, security.Credentials{BearerToken: token})
	if err != nil || name != "default" {
		t.Fatalf("worker = %q, %v", name, err)
	}
	for _, bad := range []string{"", "vdw_", "vdw_wrong", "vdm_x_y", "nonsense"} {
		if _, err := f.auth.AuthenticateWorker(ctx, security.Credentials{BearerToken: bad}); !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
	// A worker token is not a user credential, and vice versa.
	if _, err := f.auth.Authenticate(ctx, security.Credentials{BearerToken: token}); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("worker token as a user: %v", err)
	}
	// Issuing again replaces.
	replacement, err := f.auth.IssueWorkerToken(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.AuthenticateWorker(ctx, security.Credentials{BearerToken: token}); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("replaced token: %v", err)
	}
	if _, err := f.auth.AuthenticateWorker(ctx, security.Credentials{BearerToken: replacement}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.IssueWorkerToken(ctx, ""); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no name: %v", err)
	}
}

// D12: only hashes reach disk.
func TestSecretsNeverOnDisk(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", true, "", "")
	code, err := f.auth.CreateCode(ctx, admin, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	unused, err := f.auth.CreateCode(ctx, admin, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := f.auth.Pair(ctx, code, "x")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := f.auth.IssueWorkerToken(ctx, "w")
	if err != nil {
		t.Fatal(err)
	}
	_, deviceSecret, _ := strings.Cut(strings.TrimPrefix(cred.Token, "vdm_"), "_")
	needles := []string{deviceSecret, strings.TrimPrefix(worker, "vdw_"), code, unused, normalizeCode(code), normalizeCode(unused)}
	if err := f.stores.Close(); err != nil {
		t.Fatal(err)
	}
	err = filepath.Walk(f.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, n := range needles {
			if strings.Contains(string(data), n) {
				t.Errorf("%s contains a secret %q in plaintext", path, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// JSON responses carry the device, never its hash.
	data, err := json.Marshal(cred.Device)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "credential") {
		t.Fatalf("device JSON = %s", data)
	}
}

func TestDevicesService(t *testing.T) {
	f := setup(t, Options{TLSFingerprint: func() string { return "sha256:fictional" }})
	admin := f.user(t, "admin@example.com", false, "acme", domain.RoleAdmin)
	svc := f.auth.Devices()
	scope := service.Scope{Tenant: "acme", Principal: admin, RequestID: "req-1"}
	resp, err := svc.CreatePairingCode(ctx, scope, api.CreatePairingCodeRequest{TTLSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code == "" || resp.TLSFingerprint != "sha256:fictional" || !resp.ExpiresAt.After(*f.clock) {
		t.Fatalf("resp = %+v", resp)
	}
	cred, err := svc.Pair(ctx, api.PairDeviceRequest{Code: resp.Code, DeviceName: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token == "" || cred.Device.ID != "CLIENT-001" {
		t.Fatalf("cred = %+v", cred)
	}
	rotated, err := svc.Rotate(ctx, scope, id.Ref(cred.Device.ID))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Token == cred.Token {
		t.Fatal("rotation must change the token")
	}
	device, err := svc.Revoke(ctx, scope, id.Ref(cred.Device.ID))
	if err != nil {
		t.Fatal(err)
	}
	if device.Status != domain.DeviceRevoked {
		t.Fatalf("device = %+v", device)
	}
	devices, err := svc.List(ctx, service.Scope{Principal: domain.Principal{UserULID: admin.UserULID, Email: admin.Email}})
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("own devices = %+v", devices)
	}

	// Every step is audited in both chains, with no secret anywhere.
	sys, err := f.auditor.Verify(ctx, "")
	if err != nil || !sys.OK || sys.Records < 4 {
		t.Fatalf("system chain = %+v, %v", sys, err)
	}
	st, _ := f.stores.Tenants.ForTenant(ctx, "acme")
	records, _, err := st.Audit().List(ctx, repo.AuditFilter{}, repo.Page{})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, r := range records {
		actions[r.Action] = true
		blob, _ := json.Marshal(r)
		for _, secret := range []string{resp.Code, cred.Token, rotated.Token} {
			if strings.Contains(string(blob), secret) {
				t.Fatalf("the audit leaks a secret: %s", blob)
			}
		}
	}
	for _, want := range []string{"device.pairing_code.create", "device.rotate", "device.revoke"} {
		if !actions[want] {
			t.Errorf("no %s in the tenant chain: %v", want, actions)
		}
	}
}

// A failed audit write in the system chain rolls the change back with it.
func TestAuditIsInTheSameTransaction(t *testing.T) {
	f := setup(t)
	admin := f.user(t, "admin@example.com", true, "", "")
	cred := f.pair(t, admin, "admin@example.com")
	db, err := sqlitedriver.Open(filepath.Join(f.dir, "system.db"), sqlitedriver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.Revoke(ctx, admin, id.Ref(cred.Device.ID)); err == nil {
		t.Fatal("no audit, no revocation")
	}
	d, err := f.sys.Devices().Get(ctx, id.Ref(cred.Device.ID))
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != domain.DeviceActive {
		t.Fatalf("the change must roll back: %+v", d)
	}
}
