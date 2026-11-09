package persistence

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// systemStore is system.db.
type systemStore struct {
	systemRepos
}

// systemRepos binds the repositories to a connection or a transaction.
type systemRepos struct {
	conn
}

var (
	_ repo.SystemStore         = (*systemStore)(nil)
	_ repo.SystemAuditProvider = systemRepos{}
	_ repo.WorkerTokenLookup   = workerRepo{}
)

func newSystemStore(d *database) *systemStore {
	return &systemStore{systemRepos{conn{d: d}}}
}

func (s *systemStore) InTx(ctx context.Context, fn func(repo.SystemRepos) error) error {
	return s.d.inTx(ctx, func(q querier) error {
		return fn(systemRepos{conn{d: s.d, tx: q}})
	})
}

func (s *systemStore) Close() error { return s.d.close() }

func (r systemRepos) Tenants() repo.TenantRepo         { return tenantRepo{r.conn} }
func (r systemRepos) Users() repo.UserRepo             { return userRepo{r.conn} }
func (r systemRepos) Devices() repo.DeviceRepo         { return deviceRepo{r.conn} }
func (r systemRepos) Pairing() repo.PairingRepo        { return pairingRepo{r.conn} }
func (r systemRepos) Plugins() repo.PluginRepo         { return pluginRepo{r.conn} }
func (r systemRepos) WrappedKeys() repo.WrappedKeyRepo { return wrappedKeyRepo{r.conn} }
func (r systemRepos) Workers() repo.WorkerRepo         { return workerRepo{r.conn} }

// SystemAudit is the chain for actions with no tenant (I108).
func (r systemRepos) SystemAudit() repo.AuditRepo { return auditRepo{r.conn, ""} }

// ---- Tenants ----

type tenantRepo struct{ conn }

func (r tenantRepo) Create(ctx context.Context, t domain.Tenant) error {
	if err := t.ID.Validate(); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant ID")
	}
	if t.ULID == "" {
		t.ULID = id.New()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO tenants (id, ulid, name, storage_root, created_at) VALUES (?, ?, ?, ?, ?)`,
			string(t.ID), string(t.ULID), t.Name, t.StorageRoot, timeText(t.CreatedAt))
		return mapErr(err)
	})
}

func scanTenant(s scanner) (domain.Tenant, error) {
	var t domain.Tenant
	err := s.Scan(&t.ID, &t.ULID, &t.Name, &t.StorageRoot, reqTime{&t.CreatedAt})
	return t, err
}

const tenantColumns = `id, ulid, name, storage_root, created_at`

func (r tenantRepo) Get(ctx context.Context, tenant domain.TenantID) (domain.Tenant, error) {
	t, err := scanTenant(r.q().QueryRowContext(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id = ?`, string(tenant)))
	if err != nil {
		return domain.Tenant{}, notFoundIfNone(err, "tenant")
	}
	return t, nil
}

func (r tenantRepo) List(ctx context.Context) ([]domain.Tenant, error) {
	return queryAll(ctx, r.q(), `SELECT `+tenantColumns+` FROM tenants ORDER BY id`, nil, scanTenant)
}

// ---- Users ----

type userRepo struct{ conn }

// normalizeEmail makes email comparison case-insensitive everywhere.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func (r userRepo) Create(ctx context.Context, u domain.User) error {
	u.Email = normalizeEmail(u.Email)
	if u.Email == "" {
		return invalidArg("a user needs an email")
	}
	if u.ULID == "" {
		return invalidArg("a user needs a ULID")
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO users (ulid, email, system_admin, created_at) VALUES (?, ?, ?, ?)`,
			string(u.ULID), u.Email, boolInt(u.SystemAdmin), timeText(u.CreatedAt))
		return mapErr(err)
	})
}

func scanUser(s scanner) (domain.User, error) {
	var u domain.User
	var admin int
	err := s.Scan(&u.ULID, &u.Email, &admin, reqTime{&u.CreatedAt})
	u.SystemAdmin = admin != 0
	return u, err
}

const userColumns = `ulid, email, system_admin, created_at`

func (r userRepo) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := scanUser(r.q().QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, normalizeEmail(email)))
	if err != nil {
		return domain.User{}, notFoundIfNone(err, "user")
	}
	return u, nil
}

func (r userRepo) Get(ctx context.Context, user id.ULID) (domain.User, error) {
	u, err := scanUser(r.q().QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE ulid = ?`, string(user)))
	if err != nil {
		return domain.User{}, notFoundIfNone(err, "user")
	}
	return u, nil
}

func scanMembership(s scanner) (domain.Membership, error) {
	var m domain.Membership
	err := s.Scan(&m.UserULID, &m.Email, &m.TenantID, &m.Role, jsonCol{&m.Grants})
	return m, err
}

const membershipSelect = `SELECT m.user_ulid, u.email, m.tenant_id, m.role, m.grants FROM memberships m JOIN users u ON u.ulid = m.user_ulid`

func (r userRepo) ListMembers(ctx context.Context, tenant domain.TenantID) ([]domain.Membership, error) {
	return queryAll(ctx, r.q(), membershipSelect+` WHERE m.tenant_id = ? ORDER BY u.email`, []any{string(tenant)}, scanMembership)
}

func (r userRepo) SetMembership(ctx context.Context, m domain.Membership) error {
	if !m.Role.Valid() {
		return invalidArg("unknown role %q", m.Role)
	}
	grants, err := jsonList(m.Grants)
	if err != nil {
		return err
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO memberships (user_ulid, tenant_id, role, grants) VALUES (?, ?, ?, ?)
			ON CONFLICT (user_ulid, tenant_id) DO UPDATE SET role = excluded.role, grants = excluded.grants`,
			string(m.UserULID), string(m.TenantID), string(m.Role), grants)
		if err != nil && errs.IsCode(mapErr(err), errs.CodeFailedPrecondition) {
			return errs.NotFound("user or tenant")
		}
		return mapErr(err)
	})
}

func (r userRepo) RemoveMembership(ctx context.Context, user id.ULID, tenant domain.TenantID) error {
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `DELETE FROM memberships WHERE user_ulid = ? AND tenant_id = ?`, string(user), string(tenant))
		return requireAffected(res, err, "membership")
	})
}

func (r userRepo) Membership(ctx context.Context, user id.ULID, tenant domain.TenantID) (domain.Membership, error) {
	m, err := scanMembership(r.q().QueryRowContext(ctx, membershipSelect+` WHERE m.user_ulid = ? AND m.tenant_id = ?`, string(user), string(tenant)))
	if err != nil {
		return domain.Membership{}, notFoundIfNone(err, "membership")
	}
	return m, nil
}

func (r userRepo) Memberships(ctx context.Context, user id.ULID) ([]domain.Membership, error) {
	return queryAll(ctx, r.q(), membershipSelect+` WHERE m.user_ulid = ? ORDER BY m.tenant_id`, []any{string(user)}, scanMembership)
}

// ---- Devices ----

type deviceRepo struct{ conn }

func (r deviceRepo) NextID(ctx context.Context) (id.Friendly, error) {
	var out id.Friendly
	err := r.write(ctx, func(q querier) error {
		var err error
		out, err = nextSequence(ctx, q, id.PrefixClient)
		return err
	})
	return out, err
}

func (r deviceRepo) Create(ctx context.Context, d domain.Device) error {
	if d.ID == "" || d.ULID == "" {
		return invalidArg("a device needs an ID and a ULID")
	}
	if d.CredentialHash == "" {
		return invalidArg("a device needs a credential hash")
	}
	if d.Status == "" {
		d.Status = domain.DeviceActive
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO devices (ulid, friendly_id, user_ulid, name, status, credential_hash, last_seen_at, rotated_at, revoked_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(d.ULID), string(d.ID), string(d.UserULID), d.Name, string(d.Status), d.CredentialHash,
			timeArg(d.LastSeenAt), timeArg(d.RotatedAt), timeArg(d.RevokedAt), timeText(d.CreatedAt))
		return mapErr(err)
	})
}

const deviceSelect = `SELECT d.friendly_id, d.ulid, d.user_ulid, u.email, d.name, d.status, d.last_seen_at, d.rotated_at, d.revoked_at, d.created_at, d.credential_hash
	FROM devices d JOIN users u ON u.ulid = d.user_ulid`

func scanDevice(s scanner) (domain.Device, error) {
	var d domain.Device
	err := s.Scan(&d.ID, &d.ULID, &d.UserULID, &d.Email, &d.Name, &d.Status,
		nullTime{&d.LastSeenAt}, nullTime{&d.RotatedAt}, nullTime{&d.RevokedAt}, reqTime{&d.CreatedAt}, &d.CredentialHash)
	return d, err
}

func (r deviceRepo) Get(ctx context.Context, ref id.Ref) (domain.Device, error) {
	column, value, ok := refColumn(ref, id.PrefixClient)
	if !ok {
		return domain.Device{}, errs.NotFound("device")
	}
	d, err := scanDevice(r.q().QueryRowContext(ctx, deviceSelect+` WHERE d.`+column+` = ?`, value))
	if err != nil {
		return domain.Device{}, notFoundIfNone(err, "device")
	}
	return d, nil
}

func (r deviceRepo) GetByCredentialHash(ctx context.Context, hash string) (domain.Device, error) {
	d, err := scanDevice(r.q().QueryRowContext(ctx, deviceSelect+` WHERE d.credential_hash = ?`, hash))
	if err != nil {
		return domain.Device{}, notFoundIfNone(err, "device")
	}
	return d, nil
}

func (r deviceRepo) List(ctx context.Context, user id.ULID) ([]domain.Device, error) {
	if user == "" {
		return queryAll(ctx, r.q(), deviceSelect+` ORDER BY d.rowid`, nil, scanDevice)
	}
	return queryAll(ctx, r.q(), deviceSelect+` WHERE d.user_ulid = ? ORDER BY d.rowid`, []any{string(user)}, scanDevice)
}

func (r deviceRepo) Update(ctx context.Context, d domain.Device) error {
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `UPDATE devices SET name = ?, status = ?, credential_hash = ?, last_seen_at = ?, rotated_at = ?, revoked_at = ? WHERE ulid = ?`,
			d.Name, string(d.Status), d.CredentialHash, timeArg(d.LastSeenAt), timeArg(d.RotatedAt), timeArg(d.RevokedAt), string(d.ULID))
		return requireAffected(res, err, "device")
	})
}

// ---- Pairing codes ----

type pairingRepo struct{ conn }

func (r pairingRepo) Create(ctx context.Context, c repo.PairingCode) error {
	if c.Hash == "" {
		return invalidArg("a pairing code needs a hash")
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO pairing_codes (hash, user_ulid, created_by, expires_at, used_at) VALUES (?, ?, ?, ?, ?)`,
			c.Hash, string(c.UserULID), c.CreatedBy, timeText(c.ExpiresAt), timeArg(c.UsedAt))
		return mapErr(err)
	})
}

func (r pairingRepo) Consume(ctx context.Context, hash string, at time.Time) (repo.PairingCode, error) {
	var out repo.PairingCode
	err := r.write(ctx, func(q querier) error {
		row := q.QueryRowContext(ctx, `UPDATE pairing_codes SET used_at = ?
			WHERE hash = ? AND used_at IS NULL AND expires_at > ?
			RETURNING hash, user_ulid, created_by, expires_at, used_at`,
			timeText(at), hash, timeText(at))
		err := row.Scan(&out.Hash, &out.UserULID, &out.CreatedBy, reqTime{&out.ExpiresAt}, nullTime{&out.UsedAt})
		return notFoundIfNone(err, "pairing code")
	})
	return out, err
}

// ---- Plugins ----

type pluginRepo struct{ conn }

func (r pluginRepo) Upsert(ctx context.Context, p domain.PluginRecord) error {
	if p.ID == "" {
		return invalidArg("a plugin needs an ID")
	}
	if p.InstalledAt.IsZero() {
		p.InstalledAt = r.now()
	}
	manifest := string(p.Manifest)
	if manifest == "" {
		manifest = "{}"
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO plugins (id, version, manifest, sha256, enabled, installed_by, installed_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET version = excluded.version, manifest = excluded.manifest, sha256 = excluded.sha256,
				enabled = excluded.enabled, installed_by = excluded.installed_by, installed_at = excluded.installed_at`,
			p.ID, p.Version, manifest, p.SHA256, boolInt(p.Enabled), p.InstalledBy, timeText(p.InstalledAt))
		return mapErr(err)
	})
}

const pluginColumns = `id, version, manifest, sha256, enabled, installed_by, installed_at`

func scanPlugin(s scanner) (domain.PluginRecord, error) {
	var p domain.PluginRecord
	var enabled int
	err := s.Scan(&p.ID, &p.Version, rawCol{&p.Manifest}, &p.SHA256, &enabled, &p.InstalledBy, reqTime{&p.InstalledAt})
	p.Enabled = enabled != 0
	return p, err
}

func (r pluginRepo) Get(ctx context.Context, pluginID string) (domain.PluginRecord, error) {
	p, err := scanPlugin(r.q().QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugins WHERE id = ?`, pluginID))
	if err != nil {
		return domain.PluginRecord{}, notFoundIfNone(err, "plugin")
	}
	return p, nil
}

func (r pluginRepo) List(ctx context.Context) ([]domain.PluginRecord, error) {
	return queryAll(ctx, r.q(), `SELECT `+pluginColumns+` FROM plugins ORDER BY id`, nil, scanPlugin)
}

func (r pluginRepo) Delete(ctx context.Context, pluginID string) error {
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `DELETE FROM plugins WHERE id = ?`, pluginID)
		return requireAffected(res, err, "plugin")
	})
}

// ---- Wrapped keys ----

type wrappedKeyRepo struct{ conn }

// Put stores one wrapped key. Active true also demotes the tenant's other versions,
// in the same write. An existing version is replaced, which is how a re-wrap works.
func (r wrappedKeyRepo) Put(ctx context.Context, k repo.WrappedKey) error {
	if len(k.Wrapped) == 0 || k.Version <= 0 {
		return invalidArg("a wrapped key needs bytes and a positive version")
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		if k.Active {
			if _, err := q.ExecContext(ctx, `UPDATE wrapped_keys SET active = 0 WHERE tenant_id = ? AND version != ?`, string(k.TenantID), k.Version); err != nil {
				return mapErr(err)
			}
		}
		_, err := q.ExecContext(ctx, `INSERT INTO wrapped_keys (tenant_id, version, wrapped, active, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (tenant_id, version) DO UPDATE SET wrapped = excluded.wrapped, active = excluded.active`,
			string(k.TenantID), k.Version, k.Wrapped, boolInt(k.Active), timeText(k.CreatedAt))
		return mapErr(err)
	})
}

const wrappedKeyColumns = `tenant_id, version, wrapped, active, created_at`

func scanWrappedKey(s scanner) (repo.WrappedKey, error) {
	var k repo.WrappedKey
	var active int
	err := s.Scan(&k.TenantID, &k.Version, &k.Wrapped, &active, reqTime{&k.CreatedAt})
	k.Active = active != 0
	return k, err
}

func (r wrappedKeyRepo) Active(ctx context.Context, tenant domain.TenantID) (repo.WrappedKey, error) {
	k, err := scanWrappedKey(r.q().QueryRowContext(ctx, `SELECT `+wrappedKeyColumns+` FROM wrapped_keys WHERE tenant_id = ? AND active = 1`, string(tenant)))
	if err != nil {
		return repo.WrappedKey{}, notFoundIfNone(err, "tenant data key")
	}
	return k, nil
}

func (r wrappedKeyRepo) Get(ctx context.Context, tenant domain.TenantID, version int) (repo.WrappedKey, error) {
	k, err := scanWrappedKey(r.q().QueryRowContext(ctx, `SELECT `+wrappedKeyColumns+` FROM wrapped_keys WHERE tenant_id = ? AND version = ?`, string(tenant), version))
	if err != nil {
		return repo.WrappedKey{}, notFoundIfNone(err, "tenant data key")
	}
	return k, nil
}

func (r wrappedKeyRepo) List(ctx context.Context, tenant domain.TenantID) ([]repo.WrappedKey, error) {
	return queryAll(ctx, r.q(), `SELECT `+wrappedKeyColumns+` FROM wrapped_keys WHERE tenant_id = ? ORDER BY version`, []any{string(tenant)}, scanWrappedKey)
}

// ---- Workers ----

type workerRepo struct{ conn }

func (r workerRepo) PutTokenHash(ctx context.Context, name, hash string) error {
	if name == "" || hash == "" {
		return invalidArg("a worker token needs a name and a hash")
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO worker_tokens (name, hash, created_at) VALUES (?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET hash = excluded.hash, created_at = excluded.created_at`,
			name, hash, timeText(r.now()))
		return mapErr(err)
	})
}

func (r workerRepo) HasTokenHash(ctx context.Context, hash string) (bool, error) {
	_, ok, err := r.TokenName(ctx, hash)
	return ok, err
}

// TokenName names a token's owner (I101).
func (r workerRepo) TokenName(ctx context.Context, hash string) (string, bool, error) {
	var name string
	err := r.q().QueryRowContext(ctx, `SELECT name FROM worker_tokens WHERE hash = ?`, hash).Scan(&name)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, mapErr(err)
	}
	return name, true, nil
}

func (r workerRepo) Upsert(ctx context.Context, w domain.Worker) error {
	if w.ID == "" {
		return invalidArg("a worker needs an ID")
	}
	if w.LastSeenAt.IsZero() {
		w.LastSeenAt = r.now()
	}
	pools, err := jsonList(w.Pools)
	if err != nil {
		return err
	}
	resources, err := json.Marshal(w.Resources)
	if err != nil {
		return err
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO workers (id, pools, resources, version, running, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET pools = excluded.pools, resources = excluded.resources, version = excluded.version,
				running = excluded.running, last_seen_at = excluded.last_seen_at`,
			w.ID, pools, string(resources), w.Version, w.Running, timeText(w.LastSeenAt))
		return mapErr(err)
	})
}

func scanWorker(s scanner) (domain.Worker, error) {
	var w domain.Worker
	err := s.Scan(&w.ID, jsonCol{&w.Pools}, jsonCol{&w.Resources}, &w.Version, &w.Running, reqTime{&w.LastSeenAt})
	return w, err
}

func (r workerRepo) List(ctx context.Context) ([]domain.Worker, error) {
	return queryAll(ctx, r.q(), `SELECT id, pools, resources, version, running, last_seen_at FROM workers ORDER BY id`, nil, scanWorker)
}

func (r workerRepo) Delete(ctx context.Context, workerID string) error {
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `DELETE FROM workers WHERE id = ?`, workerID)
		return requireAffected(res, err, "worker")
	})
}
