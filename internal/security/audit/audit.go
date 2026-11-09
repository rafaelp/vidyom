package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// scanBatch is how many records Verify and Export read at a time.
const scanBatch = 500

// Options configures the auditor.
type Options struct {
	// System provides the chain for actions with no tenant (I108). Nil refuses them.
	System repo.SystemStore
	// Storage opens the archive role for exports (D23). Nil makes Export fail.
	Storage connector.StorageResolver
	// Clock is used for timestamps. Nil means time.Now.
	Clock func() time.Time
}

// Auditor implements security.Auditor.
type Auditor struct {
	tenants repo.TenantStores
	system  repo.SystemStore
	storage atomic.Pointer[connector.StorageResolver]
	clock   func() time.Time
}

var _ security.Auditor = (*Auditor)(nil)

// New returns an auditor over the tenant databases.
func New(tenants repo.TenantStores, opts Options) *Auditor {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	a := &Auditor{tenants: tenants, system: opts.System, clock: opts.Clock}
	if opts.Storage != nil {
		a.SetStorage(opts.Storage)
	}
	return a
}

// SetStorage sets the archive resolver after construction. It breaks a wiring cycle:
// the storage resolver needs the vault for credentials, and the vault needs this
// auditor for secret.used.
func (a *Auditor) SetStorage(s connector.StorageResolver) { a.storage.Store(&s) }

var secretKey = regexp.MustCompile(`(?i)(secret|token|password|passwd|api[-_]?key|private[-_]?key|credential|authorization)`)

// Redacted replaces a detail whose key looks like a credential (I107).
const Redacted = "[redacted]"

func sanitize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKey.MatchString(k) {
				out[k] = Redacted
			} else {
				out[k] = sanitize(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = sanitize(val)
		}
		return out
	default:
		return v
	}
}

// BuildRecord turns an entry into a record for AuditRepo.Append.
func BuildRecord(e security.AuditEntry, at time.Time) (domain.AuditRecord, error) {
	rec := domain.AuditRecord{
		TenantID:   e.TenantID,
		At:         at.UTC(),
		Actor:      e.Principal.Actor(),
		Method:     e.Principal.Method,
		Action:     e.Action,
		Permission: e.Permission,
		Resource:   e.Resource,
		Result:     e.Result,
		RequestID:  e.RequestID,
	}
	if rec.Result == "" {
		rec.Result = "allowed"
	}
	if len(e.Details) > 0 {
		data, err := json.Marshal(sanitize(e.Details))
		if err != nil {
			return domain.AuditRecord{}, errs.Wrap(errs.CodeInvalidArgument, err, "audit details do not encode")
		}
		rec.Details = data
	}
	return rec, nil
}

// AppendTx writes an entry through a repository inside the caller's transaction, so
// a state change and its audit commit together. Calling Auditor.Record from inside a
// transaction on the same database would wait on the writer gate instead (I102).
func AppendTx(ctx context.Context, r repo.AuditRepo, e security.AuditEntry, at time.Time) error {
	rec, err := BuildRecord(e, at)
	if err != nil {
		return err
	}
	_, err = r.Append(ctx, rec)
	return err
}

// chain returns the repository of a tenant's chain, or of the system chain for an
// empty tenant (I108).
func (a *Auditor) chain(ctx context.Context, tenant domain.TenantID) (repo.AuditRepo, error) {
	if tenant == "" {
		if a.system == nil {
			return nil, errs.New(errs.CodeFailedPrecondition, "no system audit chain is configured")
		}
		provider, ok := a.system.(repo.SystemAuditProvider)
		if !ok {
			return nil, errs.New(errs.CodeFailedPrecondition, "this store has no system audit chain")
		}
		return provider.SystemAudit(), nil
	}
	store, err := a.tenants.ForTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return store.Audit(), nil
}

// Record writes an entry. A failure returns to the caller: critical audit events are
// never silently dropped (§10.1).
func (a *Auditor) Record(ctx context.Context, e security.AuditEntry) error {
	r, err := a.chain(ctx, e.TenantID)
	if err != nil {
		return err
	}
	return AppendTx(ctx, r, e, a.clock())
}

// Verify recomputes the chain from its anchor (I107): every hash, every link, sequence
// continuity, and each recorded export's head hash. It reports the first bad record.
func (a *Auditor) Verify(ctx context.Context, tenant domain.TenantID) (domain.AuditVerification, error) {
	r, err := a.chain(ctx, tenant)
	if err != nil {
		return domain.AuditVerification{}, err
	}
	var (
		anchor  repo.AuditAnchor
		exports []repo.AuditExport
	)
	if m, ok := r.(repo.AuditMaintainer); ok {
		if anchor, err = m.Anchor(ctx); err != nil {
			return domain.AuditVerification{}, err
		}
		if exports, err = m.ListExports(ctx); err != nil {
			return domain.AuditVerification{}, err
		}
	}
	heads := make(map[int64]string, len(exports))
	var lastExported int64
	for _, e := range exports {
		heads[e.ToSeq] = e.HeadHash
		if e.ToSeq > lastExported {
			lastExported = e.ToSeq
		}
	}

	out := domain.AuditVerification{OK: true}
	fail := func(seq int64, reason string) (domain.AuditVerification, error) {
		out.OK = false
		out.FirstBadSeq = seq
		out.Reason = reason
		return out, nil
	}
	expectedSeq := anchor.Seq + 1
	prevHash := anchor.Hash
	if anchor.Seq == 0 {
		prevHash = domain.AuditGenesisHash
	}
	cursor := anchor.Seq
	for {
		batch, err := r.Scan(ctx, cursor, scanBatch)
		if err != nil {
			return domain.AuditVerification{}, err
		}
		if len(batch) == 0 {
			break
		}
		for _, rec := range batch {
			if rec.Seq != expectedSeq {
				return fail(expectedSeq, fmt.Sprintf("record %d is missing", expectedSeq))
			}
			if rec.PrevHash != prevHash {
				return fail(rec.Seq, "the link to the previous record is broken")
			}
			want, err := rec.ComputeHash()
			if err != nil {
				return fail(rec.Seq, "the record does not encode")
			}
			if want != rec.Hash {
				return fail(rec.Seq, "the record does not match its hash")
			}
			if head, ok := heads[rec.Seq]; ok && head != rec.Hash {
				return fail(rec.Seq, "the record differs from what was exported")
			}
			out.Records++
			expectedSeq = rec.Seq + 1
			prevHash = rec.Hash
			cursor = rec.Seq
		}
	}
	if lastExported >= expectedSeq {
		return fail(expectedSeq, fmt.Sprintf("records up to %d were exported but are missing here", lastExported))
	}
	return out, nil
}

// exportKey is the role-relative key of one export (§14.1).
func exportKey(at time.Time, from, to int64) string {
	return fmt.Sprintf("audit/%s/%08d-%08d.jsonl", at.UTC().Format("2006/01/02"), from, to)
}

// Export writes the chain since the last export to the tenant's archive role, one
// record per line, and returns the key. The watermark moves only after the driver
// accepted the file with its checksum. Nothing new returns the last key. The system
// chain has no archive role and answers not_implemented (I108).
func (a *Auditor) Export(ctx context.Context, tenant domain.TenantID) (string, error) {
	if tenant == "" {
		return "", errs.NotImplemented("exporting the system audit chain")
	}
	resolver := a.storage.Load()
	if resolver == nil {
		return "", errs.New(errs.CodeFailedPrecondition, "no archive storage is configured for audit export")
	}
	r, err := a.chain(ctx, tenant)
	if err != nil {
		return "", err
	}
	m, ok := r.(repo.AuditMaintainer)
	if !ok {
		return "", errs.New(errs.CodeFailedPrecondition, "this store cannot track audit exports")
	}
	last, hadExport, err := m.LastExport(ctx)
	if err != nil {
		return "", err
	}
	after := last.ToSeq
	if !hadExport {
		anchor, err := m.Anchor(ctx)
		if err != nil {
			return "", err
		}
		after = anchor.Seq
	}

	var (
		buf     bytes.Buffer
		encoder = json.NewEncoder(&buf)
		from    int64
		to      int64
		head    string
		cursor  = after
	)
	for {
		batch, err := r.Scan(ctx, cursor, scanBatch)
		if err != nil {
			return "", err
		}
		if len(batch) == 0 {
			break
		}
		for _, rec := range batch {
			if from == 0 {
				from = rec.Seq
			}
			if err := encoder.Encode(rec); err != nil {
				return "", fmt.Errorf("encode audit record: %w", err)
			}
			to = rec.Seq
			head = rec.Hash
			cursor = rec.Seq
		}
	}
	if from == 0 {
		return last.Key, nil
	}

	now := a.clock()
	key := exportKey(now, from, to)
	if err := domain.ValidateStorageKey(key); err != nil {
		return "", err
	}
	storage, err := (*resolver).Storage(ctx, tenant, domain.RoleArchive)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	digest := hex.EncodeToString(sum[:])
	_, err = storage.Put(ctx, key, bytes.NewReader(buf.Bytes()), connector.PutOptions{
		SHA256:         digest,
		SizeBytes:      int64(buf.Len()),
		ContentType:    "application/x-ndjson",
		IdempotencyKey: digest,
	})
	if err != nil {
		return "", err
	}
	err = m.RecordExport(ctx, repo.AuditExport{FromSeq: from, ToSeq: to, HeadHash: head, Key: key, SHA256: digest, At: now})
	if err != nil {
		return "", err
	}
	return key, nil
}

// Prune removes exported records past retention, never below the 1-year floor
// (D23, I107). It returns how many it removed.
func (a *Auditor) Prune(ctx context.Context, tenant domain.TenantID, retentionDays int) (int64, error) {
	if retentionDays < config.MinAuditDays {
		retentionDays = config.MinAuditDays
	}
	r, err := a.chain(ctx, tenant)
	if err != nil {
		return 0, err
	}
	m, ok := r.(repo.AuditMaintainer)
	if !ok {
		return 0, nil
	}
	return m.PruneExportedBefore(ctx, a.clock().AddDate(0, 0, -retentionDays))
}

// Service returns what the API calls. Only a tenant's chain is routed; the system
// chain is reached through Verify on the Auditor (I108).
func (a *Auditor) Service() service.Audit { return auditService{a} }

type auditService struct{ a *Auditor }

// ParseSince accepts an RFC 3339 time or a Go duration such as "24h", counted back
// from now, as `vidyom security audit --since 24h` (§11.5) sends it.
func ParseSince(since string, now time.Time) (*time.Time, error) {
	if since == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(since); err == nil {
		if d < 0 {
			return nil, errs.New(errs.CodeInvalidArgument, "since must not be negative")
		}
		t := now.Add(-d)
		return &t, nil
	}
	t, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return nil, errs.New(errs.CodeInvalidArgument, "since must be an RFC 3339 time or a duration such as 24h")
	}
	return &t, nil
}

func (s auditService) List(ctx context.Context, scope service.Scope, q api.AuditQuery) (api.List[domain.AuditRecord], error) {
	store, err := s.a.tenants.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return api.List[domain.AuditRecord]{}, err
	}
	since, err := ParseSince(q.Since, s.a.clock())
	if err != nil {
		return api.List[domain.AuditRecord]{}, err
	}
	items, next, err := store.Audit().List(ctx, repo.AuditFilter{Since: since, Actor: q.Actor, Action: q.Action, Resource: q.Resource}, repo.Page{Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return api.List[domain.AuditRecord]{}, err
	}
	if items == nil {
		items = []domain.AuditRecord{}
	}
	return api.List[domain.AuditRecord]{Items: items, NextCursor: next}, nil
}

func (s auditService) Verify(ctx context.Context, scope service.Scope) (domain.AuditVerification, error) {
	if scope.Tenant == "" {
		return domain.AuditVerification{}, errs.New(errs.CodeTenantRequired, "audit verification needs a tenant")
	}
	return s.a.Verify(ctx, scope.Tenant)
}

func (s auditService) Export(ctx context.Context, scope service.Scope) (api.AuditExportResponse, error) {
	if scope.Tenant == "" {
		return api.AuditExportResponse{}, errs.New(errs.CodeTenantRequired, "audit export needs a tenant")
	}
	key, err := s.a.Export(ctx, scope.Tenant)
	if err != nil {
		return api.AuditExportResponse{}, err
	}
	err = s.a.Record(ctx, security.AuditEntry{
		TenantID:   scope.Tenant,
		Principal:  scope.Principal,
		Action:     "audit.export",
		Permission: domain.PermAuditExport,
		Resource:   key,
		Result:     "allowed",
		RequestID:  scope.RequestID,
	})
	if err != nil {
		return api.AuditExportResponse{}, errors.Join(errs.New(errs.CodeInternal, "the export succeeded but recording it failed"), err)
	}
	return api.AuditExportResponse{Key: key}, nil
}
