package authentication

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/security/audit"
)

// Token prefixes and limits (I17, I112).
const (
	devicePrefix = "vdm_"
	workerPrefix = "vdw_"
	secretBytes  = 32

	DefaultPairingTTL = 15 * time.Minute
	MaxPairingTTL     = 24 * time.Hour

	lastSeenInterval = time.Minute
)

// Options configures the service.
type Options struct {
	// LocalUsers maps Unix peer UIDs to users (I17).
	LocalUsers []config.LocalUser
	// Auditor receives tenant-scoped records of device actions. The system chain is
	// written in the same transaction as each change.
	Auditor security.Auditor
	// Metrics counts failures (§10.3). Nil discards them.
	Metrics telemetry.Metrics
	// Clock is used for timestamps. Nil means time.Now.
	Clock func() time.Time
	// TLSFingerprint supplies the pin for a self-signed listener (I18). Nil or empty
	// leaves it out of pairing code responses.
	TLSFingerprint func() string
}

// Service implements security.Authenticator and security.Pairing.
type Service struct {
	system  repo.SystemStore
	opts    Options
	metrics telemetry.Metrics
	clock   func() time.Time

	mu     sync.RWMutex
	byUID  map[int]string
	seenMu sync.Mutex
	seen   map[id.ULID]time.Time
}

var (
	_ security.Authenticator = (*Service)(nil)
	_ security.Pairing       = (*Service)(nil)
)

// New returns the service.
func New(system repo.SystemStore, opts Options) *Service {
	if opts.Metrics == nil {
		opts.Metrics = telemetry.Noop{}
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	s := &Service{system: system, opts: opts, metrics: opts.Metrics, clock: opts.Clock, seen: map[id.ULID]time.Time{}}
	s.SetLocalUsers(opts.LocalUsers)
	return s
}

// SetLocalUsers replaces the Unix peer map, for a configuration reload.
func (s *Service) SetLocalUsers(users []config.LocalUser) {
	m := make(map[int]string, len(users))
	for _, u := range users {
		m[u.UID] = u.Email
	}
	s.mu.Lock()
	s.byUID = m
	s.mu.Unlock()
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var errUnauthenticated = errs.New(errs.CodeUnauthenticated, "authentication required")

func (s *Service) fail(reason string) error {
	s.metrics.Add(telemetry.AuthFailures, 1, telemetry.Labels{"reason": reason})
	return errUnauthenticated
}

// Authenticate resolves a device credential, or a Unix peer identity when no token
// came. A bad token never falls back to the peer identity (I112).
func (s *Service) Authenticate(ctx context.Context, c security.Credentials) (domain.Principal, error) {
	if c.BearerToken != "" {
		return s.authenticateDevice(ctx, c.BearerToken)
	}
	if c.PeerUID != nil {
		return s.authenticatePeer(ctx, *c.PeerUID)
	}
	return domain.Principal{}, s.fail("no_credentials")
}

func (s *Service) authenticateDevice(ctx context.Context, token string) (domain.Principal, error) {
	rest, ok := strings.CutPrefix(token, devicePrefix)
	if !ok {
		return domain.Principal{}, s.fail("malformed_token")
	}
	ulidPart, secret, ok := strings.Cut(rest, "_")
	if !ok || secret == "" || !id.IsULID(ulidPart) {
		return domain.Principal{}, s.fail("malformed_token")
	}
	hash := hashSecret(secret)
	device, err := s.system.Devices().GetByCredentialHash(ctx, hash)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return domain.Principal{}, s.fail("unknown_token")
		}
		return domain.Principal{}, err
	}
	// The lookup found the hash; the comparison ties the token's ID to that device.
	if subtle.ConstantTimeCompare([]byte(strings.ToUpper(ulidPart)), []byte(device.ULID)) != 1 {
		return domain.Principal{}, s.fail("unknown_token")
	}
	if device.Status != domain.DeviceActive {
		return domain.Principal{}, s.fail("revoked_device")
	}
	user, err := s.system.Users().Get(ctx, device.UserULID)
	if err != nil {
		return domain.Principal{}, err
	}
	s.touchLastSeen(ctx, device.ULID)
	return domain.Principal{UserULID: user.ULID, Email: user.Email, DeviceID: device.ID, Method: domain.AuthDevice, SystemAdmin: user.SystemAdmin}, nil
}

// touchLastSeen updates last_seen_at at most once a minute per device. It is
// best-effort: a request must not fail because this bookkeeping did.
func (s *Service) touchLastSeen(ctx context.Context, device id.ULID) {
	now := s.clock()
	s.seenMu.Lock()
	last, ok := s.seen[device]
	if ok && now.Sub(last) < lastSeenInterval {
		s.seenMu.Unlock()
		return
	}
	s.seen[device] = now
	s.seenMu.Unlock()
	_ = s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		d, err := tx.Devices().Get(ctx, id.Ref(device))
		if err != nil {
			return err
		}
		d.LastSeenAt = &now
		return tx.Devices().Update(ctx, d)
	})
}

func (s *Service) authenticatePeer(ctx context.Context, uid int) (domain.Principal, error) {
	s.mu.RLock()
	email, ok := s.byUID[uid]
	s.mu.RUnlock()
	if !ok {
		return domain.Principal{}, s.fail("unmapped_peer")
	}
	user, err := s.system.Users().GetByEmail(ctx, email)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return domain.Principal{}, s.fail("unmapped_peer")
		}
		return domain.Principal{}, err
	}
	return domain.Principal{UserULID: user.ULID, Email: user.Email, Method: domain.AuthPeer, SystemAdmin: user.SystemAdmin}, nil
}

// AuthenticateWorker resolves a worker token (I4).
func (s *Service) AuthenticateWorker(ctx context.Context, c security.Credentials) (string, error) {
	secret, ok := strings.CutPrefix(c.BearerToken, workerPrefix)
	if !ok || secret == "" {
		return "", s.fail("malformed_worker_token")
	}
	hash := hashSecret(secret)
	r := s.system.Workers()
	if lookup, ok := r.(repo.WorkerTokenLookup); ok {
		name, found, err := lookup.TokenName(ctx, hash)
		if err != nil {
			return "", err
		}
		if !found {
			return "", s.fail("unknown_worker_token")
		}
		return name, nil
	}
	found, err := r.HasTokenHash(ctx, hash)
	if err != nil {
		return "", err
	}
	if !found {
		return "", s.fail("unknown_worker_token")
	}
	return "worker", nil
}

// IssueWorkerToken creates or replaces a named worker token and returns it once. It
// backs `vidyomd init` (I17).
func (s *Service) IssueWorkerToken(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errs.New(errs.CodeInvalidArgument, "a worker token needs a name")
	}
	secret, err := randomSecret()
	if err != nil {
		return "", err
	}
	err = s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		if err := tx.Workers().PutTokenHash(ctx, name, hashSecret(secret)); err != nil {
			return err
		}
		return s.systemAudit(ctx, tx, security.AuditEntry{Principal: domain.Principal{Method: domain.AuthSystem}, Action: "worker.token.issue", Resource: "worker:" + name})
	})
	if err != nil {
		return "", err
	}
	return workerPrefix + secret, nil
}

// systemAudit writes to the system chain inside the transaction, when the store has
// one (I108).
func (s *Service) systemAudit(ctx context.Context, tx repo.SystemRepos, e security.AuditEntry) error {
	provider, ok := tx.(repo.SystemAuditProvider)
	if !ok {
		return nil
	}
	e.TenantID = ""
	return audit.AppendTx(ctx, provider.SystemAudit(), e, s.clock())
}

// tenantAudit writes to the tenant chain as well when the request has a tenant.
func (s *Service) tenantAudit(ctx context.Context, tenant domain.TenantID, e security.AuditEntry) error {
	if tenant == "" || s.opts.Auditor == nil {
		return nil
	}
	e.TenantID = tenant
	return s.opts.Auditor.Record(ctx, e)
}

// ---- Pairing codes ----

// crockford is Crockford's base32 alphabet: no I, L, O or U, so codes survive being
// read aloud or typed.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// newPairingCode returns 100 random bits in four groups of five characters.
func newPairingCode() (string, error) {
	b := make([]byte, 13)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	text := crockford.EncodeToString(b)[:20]
	return text[:5] + "-" + text[5:10] + "-" + text[10:15] + "-" + text[15:20], nil
}

// normalizeCode applies Crockford's reading rules, so case and look-alikes don't matter.
func normalizeCode(code string) string {
	r := strings.NewReplacer("-", "", " ", "", "I", "1", "L", "1", "O", "0")
	return r.Replace(strings.ToUpper(strings.TrimSpace(code)))
}

func hashCode(code string) string { return hashSecret("pairing:" + normalizeCode(code)) }

// CreateCode implements security.Pairing.
func (s *Service) CreateCode(ctx context.Context, by domain.Principal, userEmail string, ttlSeconds int) (string, error) {
	code, _, err := s.createCode(ctx, by, by.TenantID, userEmail, ttlSeconds, "")
	return code, err
}

func (s *Service) createCode(ctx context.Context, by domain.Principal, tenant domain.TenantID, userEmail string, ttlSeconds int, requestID string) (string, time.Time, error) {
	if userEmail == "" {
		userEmail = by.Email
	}
	if userEmail == "" {
		return "", time.Time{}, errs.New(errs.CodeInvalidArgument, "a pairing code needs a user")
	}
	ttl := DefaultPairingTTL
	if ttlSeconds > 0 {
		ttl = time.Duration(ttlSeconds) * time.Second
	}
	if ttl > MaxPairingTTL {
		return "", time.Time{}, errs.Newf(errs.CodeInvalidArgument, "a pairing code lasts at most %s", MaxPairingTTL)
	}
	target, err := s.system.Users().GetByEmail(ctx, userEmail)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := s.mayIssueFor(ctx, by, tenant, target); err != nil {
		return "", time.Time{}, err
	}
	code, err := newPairingCode()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.clock().UTC().Add(ttl)
	entry := security.AuditEntry{
		Principal:  by,
		Action:     "device.pairing_code.create",
		Permission: domain.PermDeviceManage,
		Resource:   "user:" + target.Email,
		RequestID:  requestID,
		Details:    map[string]any{"ttl_seconds": int(ttl / time.Second)},
	}
	err = s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		if err := tx.Pairing().Create(ctx, repo.PairingCode{Hash: hashCode(code), UserULID: target.ULID, CreatedBy: by.Actor(), ExpiresAt: expires}); err != nil {
			return err
		}
		return s.systemAudit(ctx, tx, entry)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	if err := s.tenantAudit(ctx, tenant, entry); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// mayIssueFor applies I112: a system admin for anyone; a tenant admin for members of
// that tenant, never for a system admin.
func (s *Service) mayIssueFor(ctx context.Context, by domain.Principal, tenant domain.TenantID, target domain.User) error {
	if by.SystemAdmin {
		return nil
	}
	if tenant == "" || !by.Can(domain.PermDeviceManage) {
		return errs.Forbidden(string(domain.PermDeviceManage))
	}
	if target.SystemAdmin {
		return errs.Forbidden(string(domain.PermSystemAdmin))
	}
	if _, err := s.system.Users().Membership(ctx, target.ULID, tenant); err != nil {
		return err
	}
	return nil
}

// Pair exchanges a code for a device credential. It is the only unauthenticated
// mutating call. Consuming the code and creating the device are one transaction.
func (s *Service) Pair(ctx context.Context, code, deviceName string) (security.IssuedCredential, error) {
	secret, err := randomSecret()
	if err != nil {
		return security.IssuedCredential{}, err
	}
	now := s.clock().UTC()
	var device domain.Device
	err = s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		pc, err := tx.Pairing().Consume(ctx, hashCode(code), now)
		if err != nil {
			if errs.IsCode(err, errs.CodeNotFound) {
				s.metrics.Add(telemetry.AuthFailures, 1, telemetry.Labels{"reason": "bad_pairing_code"})
				return errs.New(errs.CodeUnauthenticated, "the pairing code is invalid, expired or already used")
			}
			return err
		}
		friendly, err := tx.Devices().NextID(ctx)
		if err != nil {
			return err
		}
		device = domain.Device{ID: friendly, ULID: id.New(), UserULID: pc.UserULID, Name: deviceName, Status: domain.DeviceActive, CredentialHash: hashSecret(secret), CreatedAt: now}
		if err := tx.Devices().Create(ctx, device); err != nil {
			return err
		}
		stored, err := tx.Devices().Get(ctx, id.Ref(device.ULID))
		if err != nil {
			return err
		}
		device = stored
		return s.systemAudit(ctx, tx, security.AuditEntry{
			Principal: domain.Principal{Email: stored.Email, DeviceID: stored.ID, Method: domain.AuthDevice},
			Action:    "device.pair",
			Resource:  string(stored.ID),
			Details:   map[string]any{"created_by": pc.CreatedBy, "name": deviceName},
		})
	})
	if err != nil {
		return security.IssuedCredential{}, err
	}
	return security.IssuedCredential{Device: device, Token: deviceToken(device.ULID, secret)}, nil
}

func deviceToken(device id.ULID, secret string) string {
	return devicePrefix + string(device) + "_" + secret
}

// mayManage applies I112 to an existing device.
func (s *Service) mayManage(ctx context.Context, by domain.Principal, d domain.Device) error {
	if by.SystemAdmin {
		return nil
	}
	if by.UserULID != "" && by.UserULID == d.UserULID {
		return nil
	}
	if by.TenantID == "" || !by.Can(domain.PermDeviceManage) {
		return errs.Forbidden(string(domain.PermDeviceManage))
	}
	if _, err := s.system.Users().Membership(ctx, d.UserULID, by.TenantID); err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return errs.NotFound("device")
		}
		return err
	}
	user, err := s.system.Users().Get(ctx, d.UserULID)
	if err != nil {
		return err
	}
	if user.SystemAdmin {
		return errs.Forbidden(string(domain.PermSystemAdmin))
	}
	return nil
}

// Revoke implements security.Pairing.
func (s *Service) Revoke(ctx context.Context, by domain.Principal, device id.Ref) error {
	_, err := s.revoke(ctx, by, device, "")
	return err
}

func (s *Service) revoke(ctx context.Context, by domain.Principal, ref id.Ref, requestID string) (domain.Device, error) {
	d, err := s.system.Devices().Get(ctx, ref)
	if err != nil {
		return domain.Device{}, err
	}
	if err := s.mayManage(ctx, by, d); err != nil {
		return domain.Device{}, err
	}
	now := s.clock().UTC()
	entry := security.AuditEntry{Principal: by, Action: "device.revoke", Permission: domain.PermDeviceManage, Resource: string(d.ID), RequestID: requestID}
	err = s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		current, err := tx.Devices().Get(ctx, ref)
		if err != nil {
			return err
		}
		if current.Status == domain.DeviceRevoked {
			d = current
			return nil
		}
		current.Status = domain.DeviceRevoked
		current.RevokedAt = &now
		if err := tx.Devices().Update(ctx, current); err != nil {
			return err
		}
		d = current
		return s.systemAudit(ctx, tx, entry)
	})
	if err != nil {
		return domain.Device{}, err
	}
	if err := s.tenantAudit(ctx, by.TenantID, entry); err != nil {
		return domain.Device{}, err
	}
	return d, nil
}

// Rotate implements security.Pairing. The old credential stops working in the same
// transaction that creates the new one; a revoked device cannot rotate back to life.
func (s *Service) Rotate(ctx context.Context, by domain.Principal, ref id.Ref) (security.IssuedCredential, error) {
	return s.rotate(ctx, by, ref, "")
}

func (s *Service) rotate(ctx context.Context, by domain.Principal, ref id.Ref, requestID string) (security.IssuedCredential, error) {
	d, err := s.system.Devices().Get(ctx, ref)
	if err != nil {
		return security.IssuedCredential{}, err
	}
	if err := s.mayManage(ctx, by, d); err != nil {
		return security.IssuedCredential{}, err
	}
	secret, err := randomSecret()
	if err != nil {
		return security.IssuedCredential{}, err
	}
	now := s.clock().UTC()
	entry := security.AuditEntry{Principal: by, Action: "device.rotate", Permission: domain.PermDeviceManage, Resource: string(d.ID), RequestID: requestID}
	err = s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		current, err := tx.Devices().Get(ctx, ref)
		if err != nil {
			return err
		}
		if current.Status != domain.DeviceActive {
			return errs.Newf(errs.CodeFailedPrecondition, "device %s is revoked and cannot be rotated", current.ID)
		}
		current.CredentialHash = hashSecret(secret)
		current.RotatedAt = &now
		if err := tx.Devices().Update(ctx, current); err != nil {
			return err
		}
		d = current
		return s.systemAudit(ctx, tx, entry)
	})
	if err != nil {
		return security.IssuedCredential{}, err
	}
	if err := s.tenantAudit(ctx, by.TenantID, entry); err != nil {
		return security.IssuedCredential{}, err
	}
	return security.IssuedCredential{Device: d, Token: deviceToken(d.ULID, secret)}, nil
}

// ---- service.Devices ----

// Devices returns what the API calls.
func (s *Service) Devices() service.Devices { return deviceService{s} }

type deviceService struct{ s *Service }

func (d deviceService) CreatePairingCode(ctx context.Context, scope service.Scope, req api.CreatePairingCodeRequest) (api.PairingCodeResponse, error) {
	code, expires, err := d.s.createCode(ctx, scope.Principal, scope.Tenant, req.UserEmail, req.TTLSeconds, scope.RequestID)
	if err != nil {
		return api.PairingCodeResponse{}, err
	}
	resp := api.PairingCodeResponse{Code: code, ExpiresAt: expires}
	if d.s.opts.TLSFingerprint != nil {
		resp.TLSFingerprint = d.s.opts.TLSFingerprint()
	}
	return resp, nil
}

func (d deviceService) Pair(ctx context.Context, req api.PairDeviceRequest) (api.DeviceCredentialResponse, error) {
	cred, err := d.s.Pair(ctx, req.Code, req.DeviceName)
	if err != nil {
		return api.DeviceCredentialResponse{}, err
	}
	return api.DeviceCredentialResponse{Device: cred.Device, Token: cred.Token}, nil
}

// List returns what the caller may act on (I112).
func (d deviceService) List(ctx context.Context, scope service.Scope) ([]domain.Device, error) {
	p := scope.Principal
	if p.SystemAdmin {
		return d.s.system.Devices().List(ctx, "")
	}
	if scope.Tenant != "" && p.Can(domain.PermDeviceRead) {
		members, err := d.s.system.Users().ListMembers(ctx, scope.Tenant)
		if err != nil {
			return nil, err
		}
		out := []domain.Device{}
		for _, m := range members {
			devices, err := d.s.system.Devices().List(ctx, m.UserULID)
			if err != nil {
				return nil, err
			}
			out = append(out, devices...)
		}
		return out, nil
	}
	if p.UserULID == "" {
		return nil, errs.Forbidden(string(domain.PermDeviceRead))
	}
	out, err := d.s.system.Devices().List(ctx, p.UserULID)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.Device{}
	}
	return out, nil
}

func (d deviceService) Revoke(ctx context.Context, scope service.Scope, ref id.Ref) (domain.Device, error) {
	return d.s.revoke(ctx, withTenant(scope), ref, scope.RequestID)
}

func (d deviceService) Rotate(ctx context.Context, scope service.Scope, ref id.Ref) (api.DeviceCredentialResponse, error) {
	cred, err := d.s.rotate(ctx, withTenant(scope), ref, scope.RequestID)
	if err != nil {
		return api.DeviceCredentialResponse{}, err
	}
	return api.DeviceCredentialResponse{Device: cred.Device, Token: cred.Token}, nil
}

// withTenant keeps the principal's tenant in step with the scope's.
func withTenant(scope service.Scope) domain.Principal {
	p := scope.Principal
	if p.TenantID == "" {
		p.TenantID = scope.Tenant
	}
	return p
}
