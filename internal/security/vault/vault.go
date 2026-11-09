package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

// Domain separation labels (I39, I109). They are part of the storage format.
const (
	labelSecret   = "vidyom-secret-v1"
	labelCache    = "vidyom-cache-v1"
	labelPublicID = "vidyom-public-id-v1"
)

// Format version byte of everything this package seals.
const sealedV1 = 1

// MaxSecretBytes bounds a secret's value (I110).
const MaxSecretBytes = 1 << 20

var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Options configures a vault.
type Options struct {
	// Auditor receives secret.used and key events. A nil auditor records nothing,
	// which only tests should do: the daemon must wire it.
	Auditor security.Auditor
	// Clock is used for timestamps. Nil means time.Now.
	Clock func() time.Time
	// UnsealTimeout bounds the provider call. Zero means 30 s.
	UnsealTimeout time.Duration
}

// Vault implements security.Vault and security.DerivedKeys. It always holds a master
// key until Close; there is no sealed Vault value to forget to check.
type Vault struct {
	system   repo.SystemStore
	tenants  repo.TenantStores
	auditor  security.Auditor
	clock    func() time.Time
	mu       sync.RWMutex
	master   []byte // nil after Close
	provider security.MasterKeyProvider
	locks    sync.Map // domain.TenantID -> *sync.Mutex
}

var (
	_ security.Vault           = (*Vault)(nil)
	_ security.DerivedKeys     = (*Vault)(nil)
	_ connector.SecretResolver = resolver{}
)

// New unseals the master key and returns the vault. A provider failure is returned
// as is: the daemon exits with no listener (D13).
func New(system repo.SystemStore, tenants repo.TenantStores, key security.MasterKeyProvider, opts ...Options) (*Vault, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if key == nil {
		return nil, errs.New(errs.CodeSealed, "no master key provider")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.UnsealTimeout <= 0 {
		o.UnsealTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.UnsealTimeout)
	defer cancel()
	master, err := unsealKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &Vault{system: system, tenants: tenants, auditor: o.Auditor, clock: o.Clock, master: master, provider: key}, nil
}

func unsealKey(ctx context.Context, p security.MasterKeyProvider) ([]byte, error) {
	key, err := p.Unseal(ctx)
	if err != nil {
		return nil, err
	}
	if len(key) != MasterKeySize {
		wipe(key)
		return nil, errs.Newf(errs.CodeSealed, "provider %s returned a key of the wrong size", p.Name())
	}
	return key, nil
}

// Close wipes the master key. Everything afterwards answers errs.CodeSealed.
func (v *Vault) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	wipe(v.master)
	v.master = nil
}

// Sealing states how the master key is protected (I10), for doctor and /health.
func (v *Vault) Sealing() security.SealingLevel {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.provider.Sealing()
}

// ProviderName names the provider in use.
func (v *Vault) ProviderName() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.provider.Name()
}

// AuditBoot records how the master key is sealed in the system chain (I10). The
// daemon calls it once at startup and exits if it fails (§10.1).
func (v *Vault) AuditBoot(ctx context.Context, auditor security.Auditor) error {
	return auditor.Record(ctx, security.AuditEntry{
		Principal: domain.Principal{Method: domain.AuthSystem},
		Action:    "daemon.unseal",
		Result:    "allowed",
		Details:   map[string]any{"provider": v.ProviderName(), "sealing": string(v.Sealing())},
	})
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (v *Vault) masterKey() ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.master == nil {
		return nil, errs.New(errs.CodeSealed, "the vault is sealed")
	}
	return append([]byte(nil), v.master...), nil
}

func (v *Vault) lock(tenant domain.TenantID) func() {
	m, _ := v.locks.LoadOrStore(tenant, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ---- Sealing primitives ----

func seal(key, plaintext []byte, aad string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+gcm.NonceSize(), 1+gcm.NonceSize()+len(plaintext)+gcm.Overhead())
	out[0] = sealedV1
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, err
	}
	return gcm.Seal(out, out[1:], plaintext, []byte(aad)), nil
}

func unseal(key, sealed []byte, aad string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 1+gcm.NonceSize() || sealed[0] != sealedV1 {
		return nil, errors.New("unknown sealed format")
	}
	nonce := sealed[1 : 1+gcm.NonceSize()]
	return gcm.Open(nil, nonce, sealed[1+gcm.NonceSize():], []byte(aad))
}

func derive(dataKey []byte, label string) ([]byte, error) {
	return hkdf.Key(sha256.New, dataKey, nil, label, 32)
}

func dekAAD(tenant domain.TenantID, version int) string {
	return fmt.Sprintf("vidyom-dek-v1|%s|%d", tenant, version)
}

func secretAAD(tenant domain.TenantID, name string, keyVersion int) string {
	return fmt.Sprintf("%s|%s|%d", tenant, name, keyVersion)
}

// ---- Data keys ----

// newDataKey wraps a fresh data key as the tenant's active version.
func (v *Vault) newDataKey(ctx context.Context, tenant domain.TenantID, version int) ([]byte, error) {
	master, err := v.masterKey()
	if err != nil {
		return nil, err
	}
	defer wipe(master)
	dataKey := make([]byte, MasterKeySize)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("generate data key: %w", err)
	}
	wrapped, err := seal(master, dataKey, dekAAD(tenant, version))
	if err != nil {
		return nil, fmt.Errorf("wrap data key: %w", err)
	}
	if err := v.system.WrappedKeys().Put(ctx, repo.WrappedKey{TenantID: tenant, Version: version, Wrapped: wrapped, Active: true, CreatedAt: v.clock()}); err != nil {
		return nil, err
	}
	return dataKey, nil
}

func (v *Vault) unwrap(tenant domain.TenantID, k repo.WrappedKey) ([]byte, error) {
	master, err := v.masterKey()
	if err != nil {
		return nil, err
	}
	defer wipe(master)
	dataKey, err := unseal(master, k.Wrapped, dekAAD(tenant, k.Version))
	if err != nil {
		// A wrong master key lands here. The cause says nothing about key material.
		return nil, errs.Wrap(errs.CodeSealed, err, "the tenant data key does not open with this master key")
	}
	return dataKey, nil
}

// activeKey returns the active data key, creating version 1 on first use. Only a
// tenant with a database gets a key.
func (v *Vault) activeKey(ctx context.Context, tenant domain.TenantID) (int, []byte, error) {
	k, err := v.system.WrappedKeys().Active(ctx, tenant)
	if err == nil {
		dataKey, err := v.unwrap(tenant, k)
		return k.Version, dataKey, err
	}
	if !errs.IsCode(err, errs.CodeNotFound) {
		return 0, nil, err
	}
	if _, err := v.tenants.ForTenant(ctx, tenant); err != nil {
		return 0, nil, err
	}
	unlock := v.lock(tenant)
	defer unlock()
	k, err = v.system.WrappedKeys().Active(ctx, tenant)
	if err == nil {
		dataKey, err := v.unwrap(tenant, k)
		return k.Version, dataKey, err
	}
	if !errs.IsCode(err, errs.CodeNotFound) {
		return 0, nil, err
	}
	dataKey, err := v.newDataKey(ctx, tenant, 1)
	return 1, dataKey, err
}

func (v *Vault) keyAt(ctx context.Context, tenant domain.TenantID, version int) ([]byte, error) {
	k, err := v.system.WrappedKeys().Get(ctx, tenant, version)
	if err != nil {
		return nil, err
	}
	return v.unwrap(tenant, k)
}

// EnsureDataKey creates the tenant's data key now rather than at first use, so
// creating a tenant fails while the vault is sealed (I114).
func (v *Vault) EnsureDataKey(ctx context.Context, tenant domain.TenantID) error {
	_, dataKey, err := v.activeKey(ctx, tenant)
	wipe(dataKey)
	return err
}

// ---- security.Vault ----

func validateSecretName(name string) error {
	if !secretNamePattern.MatchString(name) {
		return errs.Newf(errs.CodeInvalidArgument, "invalid secret name %q: letters, digits, '.', '_' and '-' only", name)
	}
	return nil
}

func (v *Vault) encrypt(tenant domain.TenantID, name string, version int, dataKey, value []byte) ([]byte, error) {
	key, err := derive(dataKey, labelSecret)
	if err != nil {
		return nil, err
	}
	defer wipe(key)
	return seal(key, value, secretAAD(tenant, name, version))
}

func (v *Vault) decrypt(tenant domain.TenantID, name string, version int, dataKey, ciphertext []byte) ([]byte, error) {
	key, err := derive(dataKey, labelSecret)
	if err != nil {
		return nil, err
	}
	defer wipe(key)
	plain, err := unseal(key, ciphertext, secretAAD(tenant, name, version))
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "a secret does not open: it was changed or moved")
	}
	return plain, nil
}

// Put encrypts and stores a value. Replacing an existing secret keeps its creation
// metadata and sets rotated_at.
func (v *Vault) Put(ctx context.Context, tenant domain.TenantID, meta domain.SecretMeta, value []byte) error {
	if err := validateSecretName(meta.Name); err != nil {
		return err
	}
	switch meta.Location {
	case domain.SecretServer:
	case domain.SecretDelegated:
		if meta.ExpiresAt == nil {
			return errs.New(errs.CodeInvalidArgument, "a delegated secret needs an expiry (§9.1)")
		}
	case domain.SecretClient:
		return errs.New(errs.CodeInvalidArgument, "client secrets stay in the client's keyring and never reach the vault (§9.1)")
	default:
		return errs.Newf(errs.CodeInvalidArgument, "unknown secret location %q", meta.Location)
	}
	if len(value) == 0 || len(value) > MaxSecretBytes {
		return errs.Newf(errs.CodeInvalidArgument, "a secret's value must be 1 to %d bytes", MaxSecretBytes)
	}
	store, err := v.tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	version, dataKey, err := v.activeKey(ctx, tenant)
	if err != nil {
		return err
	}
	defer wipe(dataKey)
	ciphertext, err := v.encrypt(tenant, meta.Name, version, dataKey, value)
	if err != nil {
		return fmt.Errorf("encrypt secret: %w", err)
	}
	meta.TenantID = tenant
	now := v.clock().UTC()
	return store.InTx(ctx, func(tx repo.TenantRepos) error {
		existing, err := tx.Secrets().Get(ctx, meta.Name)
		switch {
		case err == nil:
			meta.CreatedAt = existing.Meta.CreatedAt
			if meta.CreatedBy == "" {
				meta.CreatedBy = existing.Meta.CreatedBy
			}
			if meta.RotatedAt == nil {
				meta.RotatedAt = &now
			}
			if meta.LastUsedAt == nil {
				meta.LastUsedAt = existing.Meta.LastUsedAt
			}
		case errs.IsCode(err, errs.CodeNotFound):
			if meta.CreatedAt.IsZero() {
				meta.CreatedAt = now
			}
		default:
			return err
		}
		return tx.Secrets().Put(ctx, repo.SecretRecord{Meta: meta, Ciphertext: ciphertext, KeyVersion: version})
	})
}

// open decrypts a record, refusing an expired one. Nothing here counts as a use.
func (v *Vault) open(ctx context.Context, store repo.TenantRepos, tenant domain.TenantID, name string) ([]byte, repo.SecretRecord, error) {
	rec, err := store.Secrets().Get(ctx, name)
	if err != nil {
		return nil, repo.SecretRecord{}, err
	}
	if rec.Meta.ExpiresAt != nil && !v.clock().Before(*rec.Meta.ExpiresAt) {
		return nil, rec, errs.Newf(errs.CodeFailedPrecondition, "secret %s has expired", name)
	}
	dataKey, err := v.keyAt(ctx, tenant, rec.KeyVersion)
	if err != nil {
		return nil, rec, err
	}
	defer wipe(dataKey)
	value, err := v.decrypt(tenant, name, rec.KeyVersion, dataKey, rec.Ciphertext)
	if err != nil {
		return nil, rec, err
	}
	return value, rec, nil
}

// Get returns a value for use inside the daemon or a lease. Each use updates
// last_used_at and writes the audit; a failed audit write withholds the value (I110).
func (v *Vault) Get(ctx context.Context, tenant domain.TenantID, name string) ([]byte, error) {
	store, err := v.tenants.ForTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	value, _, err := v.open(ctx, store, tenant, name)
	if err != nil {
		return nil, err
	}
	if v.auditor != nil {
		err := v.auditor.Record(ctx, security.AuditEntry{
			TenantID:  tenant,
			Principal: domain.Principal{Method: domain.AuthSystem},
			Action:    "secret.used",
			Resource:  "secret:" + name,
			Result:    "allowed",
		})
		if err != nil {
			wipe(value)
			return nil, err
		}
	}
	if err := store.Secrets().TouchUsed(ctx, name, v.clock()); err != nil {
		wipe(value)
		return nil, err
	}
	return value, nil
}

func (v *Vault) List(ctx context.Context, tenant domain.TenantID) ([]domain.SecretMeta, error) {
	store, err := v.tenants.ForTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return store.Secrets().List(ctx)
}

func (v *Vault) Delete(ctx context.Context, tenant domain.TenantID, name string) error {
	store, err := v.tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	return store.Secrets().Delete(ctx, name)
}

// RotateDataKey creates a new data key and re-encrypts the tenant's secrets (D13). The
// order is what makes it crash safe, because one transaction cannot span both
// databases: the new version becomes active first, then every record moves in one
// tenant transaction. A crash in between leaves records under the old version, which
// stays readable because each record names its version; the next rotation finishes
// them (I109).
func (v *Vault) RotateDataKey(ctx context.Context, tenant domain.TenantID) error {
	store, err := v.tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	// Make sure version 1 exists before taking the lock activeKey also takes.
	if err := v.EnsureDataKey(ctx, tenant); err != nil {
		return err
	}
	unlock := v.lock(tenant)
	defer unlock()
	keys, err := v.system.WrappedKeys().List(ctx, tenant)
	if err != nil {
		return err
	}
	next := 1
	for _, k := range keys {
		if k.Version >= next {
			next = k.Version + 1
		}
	}
	dataKey, err := v.newDataKey(ctx, tenant, next)
	if err != nil {
		return err
	}
	defer wipe(dataKey)
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		metas, err := tx.Secrets().List(ctx)
		if err != nil {
			return err
		}
		for _, meta := range metas {
			rec, err := tx.Secrets().Get(ctx, meta.Name)
			if err != nil {
				return err
			}
			if rec.KeyVersion == next {
				continue
			}
			old, err := v.keyAt(ctx, tenant, rec.KeyVersion)
			if err != nil {
				return err
			}
			value, err := v.decrypt(tenant, meta.Name, rec.KeyVersion, old, rec.Ciphertext)
			wipe(old)
			if err != nil {
				return err
			}
			ciphertext, err := v.encrypt(tenant, meta.Name, next, dataKey, value)
			wipe(value)
			if err != nil {
				return fmt.Errorf("re-encrypt secret: %w", err)
			}
			rec.Ciphertext, rec.KeyVersion = ciphertext, next
			if err := tx.Secrets().Put(ctx, rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if v.auditor != nil {
		return v.auditor.Record(ctx, security.AuditEntry{
			TenantID:  tenant,
			Principal: domain.Principal{Method: domain.AuthSystem},
			Action:    "vault.rotate_data_key",
			Result:    "allowed",
			Details:   map[string]any{"key_version": next},
		})
	}
	return nil
}

// RewrapAll re-wraps every tenant's data keys with a new master key, in one system.db
// transaction, then switches to it in memory. The operator changes the configured
// provider before the next start (D13).
func (v *Vault) RewrapAll(ctx context.Context, newMaster security.MasterKeyProvider) error {
	if newMaster == nil {
		return errs.New(errs.CodeInvalidArgument, "no new master key provider")
	}
	next, err := unsealKey(ctx, newMaster)
	if err != nil {
		return err
	}
	defer wipe(next)
	current, err := v.masterKey()
	if err != nil {
		return err
	}
	defer wipe(current)

	tenants := map[domain.TenantID]bool{}
	withDB, err := v.tenants.Tenants(ctx)
	if err != nil {
		return err
	}
	for _, t := range withDB {
		tenants[t] = true
	}
	registered, err := v.system.Tenants().List(ctx)
	if err != nil {
		return err
	}
	for _, t := range registered {
		tenants[t.ID] = true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.master == nil {
		return errs.New(errs.CodeSealed, "the vault is sealed")
	}
	err = v.system.InTx(ctx, func(tx repo.SystemRepos) error {
		for tenant := range tenants {
			keys, err := tx.WrappedKeys().List(ctx, tenant)
			if err != nil {
				return err
			}
			for _, k := range keys {
				dataKey, err := unseal(current, k.Wrapped, dekAAD(tenant, k.Version))
				if err != nil {
					return errs.Wrap(errs.CodeSealed, err, "a tenant data key does not open with the current master key")
				}
				wrapped, err := seal(next, dataKey, dekAAD(tenant, k.Version))
				wipe(dataKey)
				if err != nil {
					return fmt.Errorf("re-wrap data key: %w", err)
				}
				k.Wrapped = wrapped
				if err := tx.WrappedKeys().Put(ctx, k); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	wipe(v.master)
	v.master = append([]byte(nil), next...)
	v.provider = newMaster
	return nil
}

// ---- security.DerivedKeys ----

// CacheKey follows the active data key, so a rotation makes old cache chunks
// undecryptable; they count as misses (I39, I109).
func (v *Vault) CacheKey(ctx context.Context, tenant domain.TenantID) ([]byte, error) {
	_, dataKey, err := v.activeKey(ctx, tenant)
	if err != nil {
		return nil, err
	}
	defer wipe(dataKey)
	return derive(dataKey, labelCache)
}

// PublicIDKey derives from data key version 1, which is never deleted, so public IDs
// and public URLs survive a rotation (I29, I109).
func (v *Vault) PublicIDKey(ctx context.Context, tenant domain.TenantID) ([]byte, error) {
	if err := v.EnsureDataKey(ctx, tenant); err != nil {
		return nil, err
	}
	dataKey, err := v.keyAt(ctx, tenant, 1)
	if err != nil {
		return nil, err
	}
	defer wipe(dataKey)
	return derive(dataKey, labelPublicID)
}

// ---- connector.SecretResolver ----

// Resolver returns the tenant's connector.SecretResolver.
func (v *Vault) Resolver(tenant domain.TenantID) connector.SecretResolver {
	return resolver{v, tenant}
}

type resolver struct {
	v      *Vault
	tenant domain.TenantID
}

// ResolveSecret reads a credentialsRef at the moment of use.
func (r resolver) ResolveSecret(ctx context.Context, ref string) ([]byte, error) {
	if err := validateSecretName(ref); err != nil {
		return nil, err
	}
	return r.v.Get(ctx, r.tenant, ref)
}
