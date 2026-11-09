package vault

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/persistence"
)

var ctx = context.Background()

// staticKey is a provider for tests.
type staticKey struct {
	key []byte
	err error
}

func (s staticKey) Name() string                   { return "static" }
func (s staticKey) Sealing() security.SealingLevel { return security.SealingDev }
func (s staticKey) Unseal(context.Context) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]byte(nil), s.key...), nil
}

func key(b byte) []byte { return bytes.Repeat([]byte{b}, MasterKeySize) }

// spyAuditor records entries and can fail on demand.
type spyAuditor struct {
	entries []security.AuditEntry
	err     error
}

func (a *spyAuditor) Record(_ context.Context, e security.AuditEntry) error {
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, e)
	return nil
}
func (a *spyAuditor) Verify(context.Context, domain.TenantID) (domain.AuditVerification, error) {
	return domain.AuditVerification{}, nil
}
func (a *spyAuditor) Export(context.Context, domain.TenantID) (string, error) { return "", nil }

type fixture struct {
	stores *persistence.Stores
	dir    string
	vault  *Vault
	audit  *spyAuditor
}

func setup(t *testing.T, tenants ...domain.TenantID) *fixture {
	t.Helper()
	dir := t.TempDir()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stores.Close() })
	for _, tn := range tenants {
		if _, err := stores.Tenants.Create(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	spy := &spyAuditor{}
	v, err := New(stores.System, stores.Tenants, staticKey{key: key(1)}, Options{Auditor: spy})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{stores, dir, v, spy}
}

func server(name string) domain.SecretMeta {
	return domain.SecretMeta{Name: name, Location: domain.SecretServer, CreatedBy: "admin@example.com"}
}

func TestRoundTrip(t *testing.T) {
	f := setup(t, "acme")
	secret := []byte("fictional-secret-value")
	if err := f.vault.Put(ctx, "acme", server("gcs-main"), secret); err != nil {
		t.Fatal(err)
	}
	got, err := f.vault.Get(ctx, "acme", "gcs-main")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("got %q", got)
	}
	list, err := f.vault.List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "gcs-main" || list[0].CreatedBy != "admin@example.com" || list[0].LastUsedAt == nil {
		t.Fatalf("list = %+v", list)
	}
	if err := f.vault.Delete(ctx, "acme", "gcs-main"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Get(ctx, "acme", "gcs-main"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := f.vault.Delete(ctx, "acme", "gcs-main"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

// D13: only ciphertext reaches disk.
func TestPlaintextNeverOnDisk(t *testing.T) {
	f := setup(t, "acme")
	secret := "fictional-needle-3f9a1c"
	if err := f.vault.Put(ctx, "acme", server("needle"), []byte(secret)); err != nil {
		t.Fatal(err)
	}
	// Force the WAL into the main files too, then scan everything in the data dir.
	if err := f.stores.Close(); err != nil {
		t.Fatal(err)
	}
	err := filepath.Walk(f.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("%s contains the secret in plaintext", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPutValidation(t *testing.T) {
	f := setup(t, "acme")
	future := time.Now().Add(time.Hour)
	cases := map[string]struct {
		meta  domain.SecretMeta
		value []byte
	}{
		"bad name":         {domain.SecretMeta{Name: "../etc", Location: domain.SecretServer}, []byte("x")},
		"space in name":    {domain.SecretMeta{Name: "a b", Location: domain.SecretServer}, []byte("x")},
		"client location":  {domain.SecretMeta{Name: "a", Location: domain.SecretClient}, []byte("x")},
		"no location":      {domain.SecretMeta{Name: "a"}, []byte("x")},
		"delegated no ttl": {domain.SecretMeta{Name: "a", Location: domain.SecretDelegated}, []byte("x")},
		"empty":            {domain.SecretMeta{Name: "a", Location: domain.SecretServer}, nil},
		"too large":        {domain.SecretMeta{Name: "a", Location: domain.SecretServer}, make([]byte, MaxSecretBytes+1)},
	}
	for name, c := range cases {
		if err := f.vault.Put(ctx, "acme", c.meta, c.value); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := f.vault.Put(ctx, "acme", domain.SecretMeta{Name: "ok", Location: domain.SecretDelegated, ExpiresAt: &future}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := f.vault.Put(ctx, "missing-tenant", server("a"), []byte("x")); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func TestExpiredSecretDoesNotOpen(t *testing.T) {
	f := setup(t, "acme")
	past := time.Now().Add(-time.Minute)
	if err := f.vault.Put(ctx, "acme", domain.SecretMeta{Name: "old", Location: domain.SecretDelegated, ExpiresAt: &past}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Get(ctx, "acme", "old"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("expired: %v", err)
	}
}

// D13: a wrong master key fails closed; it never yields garbage.
func TestWrongMasterKeyFailsClosed(t *testing.T) {
	f := setup(t, "acme")
	if err := f.vault.Put(ctx, "acme", server("x"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	other, err := New(f.stores.System, f.stores.Tenants, staticKey{key: key(2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get(ctx, "acme", "x"); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("wrong master: %v", err)
	}
	if err := other.Put(ctx, "acme", server("y"), []byte("v")); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("wrong master put: %v", err)
	}
	if _, err := other.CacheKey(ctx, "acme"); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("wrong master derive: %v", err)
	}
}

func TestProviderFailureMeansNoVault(t *testing.T) {
	f := setup(t, "acme")
	boom := errs.New(errs.CodeSealed, "boom")
	if _, err := New(f.stores.System, f.stores.Tenants, staticKey{err: boom}); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("failing provider: %v", err)
	}
	if _, err := New(f.stores.System, f.stores.Tenants, staticKey{key: []byte("short")}); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("short key: %v", err)
	}
	if _, err := New(f.stores.System, f.stores.Tenants, nil); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("no provider: %v", err)
	}
}

func TestCloseSeals(t *testing.T) {
	f := setup(t, "acme")
	if err := f.vault.Put(ctx, "acme", server("x"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	f.vault.Close()
	if _, err := f.vault.Get(ctx, "acme", "x"); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("closed get: %v", err)
	}
	if err := f.vault.Put(ctx, "acme", server("y"), []byte("v")); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("closed put: %v", err)
	}
	if err := f.vault.RewrapAll(ctx, staticKey{key: key(3)}); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("closed rewrap: %v", err)
	}
}

// A ciphertext does not open under another name or another tenant, so nobody with
// database access can swap secrets around (I109).
func TestCiphertextIsBoundToNameAndTenant(t *testing.T) {
	f := setup(t, "acme", "beta")
	if err := f.vault.Put(ctx, "acme", server("gcs"), []byte("acme-value")); err != nil {
		t.Fatal(err)
	}
	if err := f.vault.Put(ctx, "acme", server("other"), []byte("other-value")); err != nil {
		t.Fatal(err)
	}
	acme, _ := f.stores.Tenants.ForTenant(ctx, "acme")
	beta, _ := f.stores.Tenants.ForTenant(ctx, "beta")
	rec, err := acme.Secrets().Get(ctx, "gcs")
	if err != nil {
		t.Fatal(err)
	}
	// Swapped name in the same tenant.
	swapped := rec
	swapped.Meta.Name = "other"
	if err := acme.Secrets().Put(ctx, swapped); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Get(ctx, "acme", "other"); err == nil {
		t.Fatal("a ciphertext opened under another name")
	}
	// Copied into another tenant's database.
	if err := f.vault.Put(ctx, "beta", server("seed"), []byte("beta-value")); err != nil {
		t.Fatal(err)
	}
	moved := rec
	moved.Meta.TenantID = "beta"
	if err := beta.Secrets().Put(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Get(ctx, "beta", "gcs"); err == nil {
		t.Fatal("a ciphertext opened in another tenant")
	}
	// Tampered byte.
	rec2, _ := acme.Secrets().Get(ctx, "gcs")
	rec2.Ciphertext[len(rec2.Ciphertext)-1] ^= 1
	if err := acme.Secrets().Put(ctx, rec2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Get(ctx, "acme", "gcs"); !errs.IsCode(err, errs.CodeInternal) {
		t.Fatalf("tampered: %v", err)
	}
}

func TestDataKeysArePerTenant(t *testing.T) {
	f := setup(t, "acme", "beta")
	a, err := f.vault.CacheKey(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.vault.CacheKey(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || bytes.Equal(a, b) {
		t.Fatal("cache keys must be 32 bytes and differ per tenant")
	}
	again, err := f.vault.CacheKey(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, again) {
		t.Fatal("the cache key must be stable without a rotation")
	}
	pa, err := f.vault.PublicIDKey(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(pa, a) {
		t.Fatal("derived keys must be separated by purpose")
	}
	if _, err := f.vault.CacheKey(ctx, "unknown"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	keys, err := f.stores.System.WrappedKeys().List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Version != 1 || !keys[0].Active {
		t.Fatalf("keys = %+v", keys)
	}
	// The wrapped key is not the key.
	if bytes.Contains(keys[0].Wrapped, a) {
		t.Fatal("the wrapped blob holds a derived key")
	}
}

func TestRotateDataKey(t *testing.T) {
	f := setup(t, "acme")
	values := map[string]string{"a": "value-a", "b": "value-b", "c": "value-c"}
	for name, value := range values {
		if err := f.vault.Put(ctx, "acme", server(name), []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := f.stores.Tenants.ForTenant(ctx, "acme")
	before, _ := store.Secrets().Get(ctx, "a")
	cacheBefore, _ := f.vault.CacheKey(ctx, "acme")
	publicBefore, _ := f.vault.PublicIDKey(ctx, "acme")

	if err := f.vault.RotateDataKey(ctx, "acme"); err != nil {
		t.Fatal(err)
	}

	keys, err := f.stores.System.WrappedKeys().List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Active || !keys[1].Active || keys[1].Version != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	for name, value := range values {
		rec, err := store.Secrets().Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if rec.KeyVersion != 2 {
			t.Fatalf("%s still has key version %d", name, rec.KeyVersion)
		}
		got, err := f.vault.Get(ctx, "acme", name)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != value {
			t.Fatalf("%s = %q", name, got)
		}
	}
	after, _ := store.Secrets().Get(ctx, "a")
	if bytes.Equal(before.Ciphertext, after.Ciphertext) {
		t.Fatal("rotation must re-encrypt")
	}
	if !after.Meta.CreatedAt.Equal(before.Meta.CreatedAt) || after.Meta.CreatedBy != before.Meta.CreatedBy {
		t.Fatalf("rotation changed metadata: %+v", after.Meta)
	}
	// New values use the new version.
	if err := f.vault.Put(ctx, "acme", server("d"), []byte("value-d")); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.Secrets().Get(ctx, "d"); rec.KeyVersion != 2 {
		t.Fatalf("new secret has key version %d", rec.KeyVersion)
	}
	// The cache key follows the active key; public IDs do not change (I109).
	cacheAfter, _ := f.vault.CacheKey(ctx, "acme")
	publicAfter, _ := f.vault.PublicIDKey(ctx, "acme")
	if bytes.Equal(cacheBefore, cacheAfter) {
		t.Fatal("the cache key must follow the data key")
	}
	if !bytes.Equal(publicBefore, publicAfter) {
		t.Fatal("public IDs must survive a rotation")
	}
	// Again, to prove versions keep growing.
	if err := f.vault.RotateDataKey(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.Secrets().Get(ctx, "b"); rec.KeyVersion != 3 {
		t.Fatalf("key version = %d", rec.KeyVersion)
	}
	found := false
	for _, e := range f.audit.entries {
		if e.Action == "vault.rotate_data_key" {
			found = true
		}
	}
	if !found {
		t.Fatal("a rotation must be audited")
	}
}

// I109: the state a crash leaves between the two steps of a rotation stays readable.
func TestRotationCrashLeavesSecretsReadable(t *testing.T) {
	f := setup(t, "acme")
	if err := f.vault.Put(ctx, "acme", server("a"), []byte("value-a")); err != nil {
		t.Fatal(err)
	}
	// Step one of a rotation only: the new key is active, records are not moved.
	if _, err := f.vault.newDataKey(ctx, "acme", 2); err != nil {
		t.Fatal(err)
	}
	got, err := f.vault.Get(ctx, "acme", "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value-a" {
		t.Fatalf("got %q", got)
	}
	// A new write uses the active version; the next rotation moves the rest.
	if err := f.vault.RotateDataKey(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	store, _ := f.stores.Tenants.ForTenant(ctx, "acme")
	rec, _ := store.Secrets().Get(ctx, "a")
	if rec.KeyVersion != 3 {
		t.Fatalf("key version = %d", rec.KeyVersion)
	}
	got, err = f.vault.Get(ctx, "acme", "a")
	if err != nil || string(got) != "value-a" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRewrapAll(t *testing.T) {
	f := setup(t, "acme", "beta")
	if err := f.vault.Put(ctx, "acme", server("a"), []byte("value-a")); err != nil {
		t.Fatal(err)
	}
	if err := f.vault.Put(ctx, "beta", server("b"), []byte("value-b")); err != nil {
		t.Fatal(err)
	}
	if err := f.vault.RotateDataKey(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	wrappedBefore, _ := f.stores.System.WrappedKeys().List(ctx, "acme")
	cacheBefore, _ := f.vault.CacheKey(ctx, "acme")

	if err := f.vault.RewrapAll(ctx, staticKey{err: errors.New("no")}); err == nil {
		t.Fatal("a failing new provider must abort")
	}
	if err := f.vault.RewrapAll(ctx, staticKey{key: key(9)}); err != nil {
		t.Fatal(err)
	}
	wrappedAfter, _ := f.stores.System.WrappedKeys().List(ctx, "acme")
	if bytes.Equal(wrappedBefore[0].Wrapped, wrappedAfter[0].Wrapped) || bytes.Equal(wrappedBefore[1].Wrapped, wrappedAfter[1].Wrapped) {
		t.Fatal("every version must be re-wrapped")
	}
	if !wrappedAfter[1].Active {
		t.Fatal("re-wrapping must keep the active version")
	}
	// The running vault keeps working, on the new key.
	if got, err := f.vault.Get(ctx, "acme", "a"); err != nil || string(got) != "value-a" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := f.vault.Get(ctx, "beta", "b"); err != nil || string(got) != "value-b" {
		t.Fatalf("got %q, %v", got, err)
	}
	cacheAfter, _ := f.vault.CacheKey(ctx, "acme")
	if !bytes.Equal(cacheBefore, cacheAfter) {
		t.Fatal("re-wrapping must not change data keys")
	}
	// A restart with the new key works, and the old key no longer opens anything.
	restarted, err := New(f.stores.System, f.stores.Tenants, staticKey{key: key(9)})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := restarted.Get(ctx, "acme", "a"); err != nil || string(got) != "value-a" {
		t.Fatalf("got %q, %v", got, err)
	}
	old, err := New(f.stores.System, f.stores.Tenants, staticKey{key: key(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Get(ctx, "acme", "a"); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("old master: %v", err)
	}
}

func TestGetAuditsUseAndFailsClosed(t *testing.T) {
	f := setup(t, "acme")
	if err := f.vault.Put(ctx, "acme", server("gcs"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	r := f.vault.Resolver("acme")
	got, err := r.ResolveSecret(ctx, "gcs")
	if err != nil || string(got) != "value" {
		t.Fatalf("got %q, %v", got, err)
	}
	if len(f.audit.entries) != 1 || f.audit.entries[0].Action != "secret.used" || f.audit.entries[0].Resource != "secret:gcs" {
		t.Fatalf("entries = %+v", f.audit.entries)
	}
	f.audit.err = errors.New("audit is down")
	if _, err := r.ResolveSecret(ctx, "gcs"); err == nil {
		t.Fatal("no audit, no value")
	}
	f.audit.err = nil
	if _, err := r.ResolveSecret(ctx, "../gcs"); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad ref: %v", err)
	}
	if _, err := r.ResolveSecret(ctx, "missing"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSecretsService(t *testing.T) {
	f := setup(t, "acme")
	svc := f.vault.Secrets()
	scope := service.Scope{Tenant: "acme", Principal: domain.Principal{Email: "admin@example.com", Method: domain.AuthDevice}, RequestID: "r1"}
	value := "fictional-service-value"

	meta, err := svc.Put(ctx, scope, api.PutSecretRequest{Name: "youtube-oauth", Location: domain.SecretServer, Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "youtube-oauth" || meta.CreatedBy != "admin@example.com" || meta.TenantID != "acme" || meta.Location != domain.SecretServer {
		t.Fatalf("meta = %+v", meta)
	}
	if _, err := svc.Put(ctx, scope, api.PutSecretRequest{Name: "youtube-oauth", Location: domain.SecretServer, Value: "x"}); !errs.IsCode(err, errs.CodeConflict) {
		t.Fatalf("second add: %v", err)
	}
	if _, err := svc.Put(ctx, scope, api.PutSecretRequest{Name: "c", Location: domain.SecretClient, Value: "x"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("client: %v", err)
	}
	// Delegated: limited duration, default and maximum (I110).
	del, err := svc.Put(ctx, scope, api.PutSecretRequest{Name: "vimeo-oauth", Location: domain.SecretDelegated, Value: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if del.ExpiresAt == nil || time.Until(*del.ExpiresAt) > DefaultDelegatedTTL {
		t.Fatalf("delegated = %+v", del)
	}
	if _, err := svc.Put(ctx, scope, api.PutSecretRequest{Name: "too-long", Location: domain.SecretDelegated, Value: "x", TTLSeconds: int(2 * MaxDelegatedTTL / time.Second)}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("long ttl: %v", err)
	}

	// §12.1, R41: nothing the API receives carries the value.
	list, err := svc.List(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(list)
	if strings.Contains(string(data), value) || strings.Contains(string(data), "ciphertext") {
		t.Fatalf("list leaks: %s", data)
	}

	res, err := svc.Test(ctx, scope, "youtube-oauth")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || strings.Contains(res.Detail, value) {
		t.Fatalf("test = %+v", res)
	}
	if _, err := svc.Test(ctx, scope, "missing"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("test missing: %v", err)
	}
	// A test is not a use.
	list, _ = svc.List(ctx, scope)
	for _, m := range list {
		if m.LastUsedAt != nil {
			t.Fatalf("a test counted as a use: %+v", m)
		}
	}

	rotated, err := svc.Rotate(ctx, scope, "youtube-oauth", api.RotateSecretRequest{Value: "fictional-new-value"})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.RotatedAt == nil || rotated.CreatedBy != "admin@example.com" {
		t.Fatalf("rotated = %+v", rotated)
	}
	got, err := f.vault.Get(ctx, "acme", "youtube-oauth")
	if err != nil || string(got) != "fictional-new-value" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := svc.Rotate(ctx, scope, "missing", api.RotateSecretRequest{Value: "x"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("rotate missing: %v", err)
	}
	if _, err := svc.Rotate(ctx, scope, "youtube-oauth", api.RotateSecretRequest{}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("rotate empty: %v", err)
	}
	if err := svc.Delete(ctx, scope, "youtube-oauth"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, scope, "youtube-oauth"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("delete twice: %v", err)
	}

	// The audit names actions and permissions, never the value.
	var actions []string
	for _, e := range f.audit.entries {
		actions = append(actions, e.Action)
		blob, _ := json.Marshal(e)
		if strings.Contains(string(blob), value) || strings.Contains(string(blob), "fictional-new-value") {
			t.Fatalf("the audit leaks a value: %s", blob)
		}
	}
	for _, want := range []string{"secret.add", "secret.test", "secret.rotate", "secret.delete", "secret.used"} {
		found := false
		for _, a := range actions {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s in %v", want, actions)
		}
	}
	for _, e := range f.audit.entries {
		if e.Action == "secret.add" && (e.Permission != domain.PermSecretWrite || e.Principal.Email != "admin@example.com" || e.RequestID != "r1") {
			t.Fatalf("entry = %+v", e)
		}
	}
}

func TestSecretTestReportsAnExpiredOne(t *testing.T) {
	f := setup(t, "acme")
	svc := f.vault.Secrets()
	scope := service.Scope{Tenant: "acme", Principal: domain.Principal{Email: "a@example.com"}}
	now := time.Now()
	f.vault.clock = func() time.Time { return now }
	if _, err := svc.Put(ctx, scope, api.PutSecretRequest{Name: "soon", Location: domain.SecretServer, Value: "x", TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	f.vault.clock = func() time.Time { return now.Add(2 * time.Minute) }
	res, err := svc.Test(ctx, scope, "soon")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Detail, "expired") {
		t.Fatalf("test = %+v", res)
	}
}

func TestAuditBoot(t *testing.T) {
	f := setup(t)
	if err := f.vault.AuditBoot(ctx, f.audit); err != nil {
		t.Fatal(err)
	}
	e := f.audit.entries[0]
	if e.TenantID != "" || e.Action != "daemon.unseal" || e.Details["sealing"] != string(security.SealingDev) || e.Details["provider"] != "static" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestConcurrentFirstUseCreatesOneKey(t *testing.T) {
	f := setup(t, "acme")
	const n = 16
	keys := make(chan []byte, n)
	errc := make(chan error, n)
	for range n {
		go func() {
			k, err := f.vault.CacheKey(ctx, "acme")
			errc <- err
			keys <- k
		}()
	}
	var first []byte
	for range n {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
		k := <-keys
		if first == nil {
			first = k
		} else if !bytes.Equal(first, k) {
			t.Fatal("concurrent first uses created different keys")
		}
	}
	list, _ := f.stores.System.WrappedKeys().List(ctx, "acme")
	if len(list) != 1 {
		t.Fatalf("keys = %+v", list)
	}
}

// ---- Providers ----

func writeKeyFile(t *testing.T, mode os.FileMode, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProviderForHasNoDefault(t *testing.T) {
	if _, err := ProviderFor(config.Unseal{}); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("no provider: %v", err)
	}
	if _, err := ProviderFor(config.Unseal{Provider: "default"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("unknown provider: %v", err)
	}
	cases := map[config.UnsealProvider]security.SealingLevel{
		config.UnsealSystemdCreds:  security.SealingHardware,
		config.UnsealMacOSKeychain: security.SealingOS,
		config.UnsealEnv:           security.SealingDev,
		config.UnsealGCPKMS:        security.SealingKMS,
	}
	for name, level := range cases {
		p, err := ProviderFor(config.Unseal{Provider: name})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Sealing() != level || p.Name() != string(name) {
			t.Errorf("%s: sealing %s, name %s", name, p.Sealing(), p.Name())
		}
	}
}

func TestFileProvider(t *testing.T) {
	raw := key(7)
	encoded := []byte(base64.StdEncoding.EncodeToString(raw) + "\n")
	if _, err := ProviderFor(config.Unseal{Provider: config.UnsealFile, AcknowledgeFileSealed: true}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no path: %v", err)
	}
	path := writeKeyFile(t, 0o400, encoded)
	if _, err := ProviderFor(config.Unseal{Provider: config.UnsealFile, Path: path}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("no acknowledgment: %v", err)
	}
	for _, content := range [][]byte{encoded, raw} {
		for _, mode := range []os.FileMode{0o400, 0o440} {
			p, err := ProviderFor(config.Unseal{Provider: config.UnsealFile, Path: writeKeyFile(t, mode, content), AcknowledgeFileSealed: true})
			if err != nil {
				t.Fatal(err)
			}
			if p.Sealing() != security.SealingFile || p.Name() != "file" {
				t.Fatalf("provider = %s %s", p.Name(), p.Sealing())
			}
			got, err := p.Unseal(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatal("wrong key")
			}
		}
	}
	bad := map[string]func() string{
		"world readable": func() string { return writeKeyFile(t, 0o644, encoded) },
		"owner writable": func() string { return writeKeyFile(t, 0o600, encoded) },
		"short":          func() string { return writeKeyFile(t, 0o400, []byte("c2hvcnQ=")) },
		"not base64":     func() string { return writeKeyFile(t, 0o400, []byte("this is not a key at all")) },
		"missing":        func() string { return filepath.Join(t.TempDir(), "absent") },
		"directory":      func() string { return t.TempDir() },
	}
	for name, makePath := range bad {
		p, err := ProviderFor(config.Unseal{Provider: config.UnsealFile, Path: makePath(), AcknowledgeFileSealed: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A bad key file's error never echoes its content.
	secretish := "this-looks-like-a-passphrase"
	p, err := ProviderFor(config.Unseal{Provider: config.UnsealFile, Path: writeKeyFile(t, 0o400, []byte(secretish)), AcknowledgeFileSealed: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Unseal(ctx); err == nil || strings.Contains(err.Error(), secretish) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvProvider(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(key(5))
	t.Setenv(EnvMasterKey, encoded)
	t.Setenv(EnvInsecureDev, "")
	p, err := ProviderFor(config.Unseal{Provider: config.UnsealEnv})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("without the second switch: %v", err)
	}
	t.Setenv(EnvInsecureDev, "1")
	got, err := p.Unseal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key(5)) {
		t.Fatal("wrong key")
	}
	// The daemon checks a provider again after the vault has used it.
	if again, err := p.Unseal(ctx); err != nil || !bytes.Equal(again, key(5)) {
		t.Fatalf("second unseal: %v", err)
	}
	t.Setenv(EnvMasterKey, "bad")
	if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("bad key: %v", err)
	}
}

// Stubbed: this host has no systemd (I12).
func TestSystemdProvider(t *testing.T) {
	p, err := ProviderFor(config.Unseal{Provider: config.UnsealSystemdCreds})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCredentialsDir, "")
	if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("no directory: %v", err)
	}
	dir := t.TempDir()
	t.Setenv(EnvCredentialsDir, dir)
	if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("no credential: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DefaultItemName), key(6), 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := p.Unseal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key(6)) {
		t.Fatal("wrong key")
	}
	named, err := ProviderFor(config.Unseal{Provider: config.UnsealSystemdCreds, ItemName: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := named.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("other item: %v", err)
	}
	if _, err := ProviderFor(config.Unseal{Provider: config.UnsealSystemdCreds, ItemName: "../x"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("traversal: %v", err)
	}
}

// Stubbed by default, so tests never touch the maintainer's Keychain (I12).
func TestKeychainProvider(t *testing.T) {
	savedOS, savedRead := currentOS, keychainRead
	t.Cleanup(func() { currentOS, keychainRead = savedOS, savedRead })

	p, err := ProviderFor(config.Unseal{Provider: config.UnsealMacOSKeychain})
	if err != nil {
		t.Fatal(err)
	}
	currentOS = "linux"
	if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("wrong platform: %v", err)
	}
	currentOS = "darwin"
	keychainRead = func(service, account string) (string, error) {
		if service != "vidyom" || account != DefaultItemName {
			t.Errorf("read %s/%s", service, account)
		}
		return base64.StdEncoding.EncodeToString(key(8)), nil
	}
	got, err := p.Unseal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key(8)) {
		t.Fatal("wrong key")
	}
	keychainRead = func(string, string) (string, error) { return "", errors.New("no item") }
	if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("no item: %v", err)
	}
	keychainRead = func(string, string) (string, error) { return "short", nil }
	if _, err := p.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("bad item: %v", err)
	}
}

func TestDPAPIAndKMSFailClosedHere(t *testing.T) {
	if _, err := ProviderFor(config.Unseal{Provider: config.UnsealDPAPI}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("dpapi no path: %v", err)
	}
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, []byte("blob"), 0o400); err != nil {
		t.Fatal(err)
	}
	dp, err := ProviderFor(config.Unseal{Provider: config.UnsealDPAPI, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if dp.Sealing() != security.SealingOS {
		t.Fatalf("sealing = %s", dp.Sealing())
	}
	if currentOS != "windows" {
		if _, err := dp.Unseal(ctx); !errs.IsCode(err, errs.CodeSealed) {
			t.Fatalf("dpapi here: %v", err)
		}
	}
	km, err := ProviderFor(config.Unseal{Provider: config.UnsealGCPKMS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := km.Unseal(ctx); !errs.IsCode(err, errs.CodeNotImplemented) {
		t.Fatalf("kms: %v", err)
	}
}

// A file written the way `vidyomd init --generate-master-key` writes it opens (I111,
// I138): standard base64, a trailing newline, mode 0400.
func TestFileProviderReadsTheGeneratedFormat(t *testing.T) {
	path := writeKeyFile(t, 0o400, []byte(base64.StdEncoding.EncodeToString(key(4))+"\n"))
	p, err := ProviderFor(config.Unseal{Provider: config.UnsealFile, Path: path, AcknowledgeFileSealed: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Unseal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key(4)) {
		t.Fatal("wrong key")
	}
}

// The real Keychain runs only when asked, so a plain `go test` never touches the
// maintainer's keychain (I12).
func TestRealKeychain(t *testing.T) {
	if os.Getenv("VIDYOM_TEST_REAL_KEYCHAIN") != "1" {
		t.Skip("set VIDYOM_TEST_REAL_KEYCHAIN=1 to use the real macOS Keychain")
	}
	const item = "vidyom-master-key-test"
	encoded := base64.StdEncoding.EncodeToString(key(3))
	if err := gokeyring.Set(keychainService, item, encoded); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gokeyring.Delete(keychainService, item) })
	p, err := ProviderFor(config.Unseal{Provider: config.UnsealMacOSKeychain, ItemName: item})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Unseal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key(3)) {
		t.Fatal("wrong key")
	}
}
