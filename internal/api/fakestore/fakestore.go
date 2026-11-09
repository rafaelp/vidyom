// Package fakestore is an in-memory repo.TenantStores for tests of this component.
// It implements only what the API layer touches: uploads, idempotency records and
// friendly ID allocation. Anything else panics on the nil embedded interface, which
// is the point: this layer must not reach other repositories.
package fakestore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// Stores is a set of tenant stores. Only the tenants given to New exist.
type Stores struct {
	repo.TenantStores // nil: Create and Close are not used by this layer

	mu      sync.Mutex
	tenants map[domain.TenantID]*Store
}

// New returns stores for the given tenants.
func New(tenants ...domain.TenantID) *Stores {
	s := &Stores{tenants: map[domain.TenantID]*Store{}}
	for _, t := range tenants {
		s.tenants[t] = &Store{
			tenant:      t,
			uploads:     map[id.ULID]domain.Upload{},
			idempotency: map[string]repo.IdempotentResponse{},
			seq:         map[id.Prefix]uint64{},
		}
	}
	return s
}

// ForTenant returns a tenant's store; unknown tenants answer not_found.
func (s *Stores) ForTenant(_ context.Context, t domain.TenantID) (repo.TenantStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tenants[t]
	if !ok {
		return nil, errs.NotFound("tenant")
	}
	return st, nil
}

// Tenants lists the tenants, sorted.
func (s *Stores) Tenants(context.Context) ([]domain.TenantID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.TenantID, 0, len(s.tenants))
	for t := range s.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Store is one tenant.
type Store struct {
	repo.TenantStore // nil: other repositories are out of reach on purpose

	mu          sync.Mutex
	tenant      domain.TenantID
	uploads     map[id.ULID]domain.Upload
	idempotency map[string]repo.IdempotentResponse
	seq         map[id.Prefix]uint64
	// FailUploadUpdate makes the next UploadRepo.Update fail, to simulate a crash
	// between the file write and the commit.
	FailUploadUpdate error
}

// Store returns a tenant's store, for assertions.
func (s *Stores) Store(t domain.TenantID) *Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenants[t]
}

func (s *Store) Tenant() domain.TenantID { return s.tenant }

func (s *Store) InTx(_ context.Context, fn func(repo.TenantRepos) error) error { return fn(s) }

func (s *Store) IDs() repo.IDAllocator { return allocator{s} }

func (s *Store) Uploads() repo.UploadRepo { return uploadRepo{s} }

func (s *Store) Idempotency() repo.IdempotencyRepo { return idempotencyRepo{s} }

// Records returns how many idempotency records exist.
func (s *Store) Records() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.idempotency)
}

// Record returns one idempotency record by stored key.
func (s *Store) Record(key string) (repo.IdempotentResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.idempotency[key]
	return r, ok
}

type allocator struct{ s *Store }

func (a allocator) Next(_ context.Context, p id.Prefix) (id.Friendly, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.seq[p]++
	return id.Format(p, a.s.seq[p]), nil
}

type uploadRepo struct{ s *Store }

func (r uploadRepo) find(ref id.Ref) (domain.Upload, bool) {
	for _, u := range r.s.uploads {
		if string(u.ULID) == string(ref) || string(u.ID) == string(ref) {
			return u, true
		}
	}
	return domain.Upload{}, false
}

func (r uploadRepo) Create(_ context.Context, u domain.Upload) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if u.ULID == "" || u.ID == "" {
		return fmt.Errorf("fakestore: upload without an ID")
	}
	r.s.uploads[u.ULID] = u
	return nil
}

func (r uploadRepo) Get(_ context.Context, ref id.Ref) (domain.Upload, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	u, ok := r.find(ref)
	if !ok {
		return domain.Upload{}, errs.NotFound("upload")
	}
	return u, nil
}

func (r uploadRepo) Update(_ context.Context, u domain.Upload) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.FailUploadUpdate; err != nil {
		r.s.FailUploadUpdate = nil
		return err
	}
	if _, ok := r.s.uploads[u.ULID]; !ok {
		return errs.NotFound("upload")
	}
	r.s.uploads[u.ULID] = u
	return nil
}

func (r uploadRepo) ListExpired(_ context.Context, t time.Time) ([]domain.Upload, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []domain.Upload
	for _, u := range r.s.uploads {
		if !u.ExpiresAt.IsZero() && u.ExpiresAt.Before(t) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (r uploadRepo) Delete(_ context.Context, ref id.Ref) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	u, ok := r.find(ref)
	if !ok {
		return errs.NotFound("upload")
	}
	delete(r.s.uploads, u.ULID)
	return nil
}

type idempotencyRepo struct{ s *Store }

func (r idempotencyRepo) Get(_ context.Context, key string) (repo.IdempotentResponse, bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	rec, ok := r.s.idempotency[key]
	return rec, ok, nil
}

func (r idempotencyRepo) Put(_ context.Context, rec repo.IdempotentResponse) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.idempotency[rec.Key] = rec
	return nil
}

func (r idempotencyRepo) DeleteBefore(context.Context, time.Time) (int64, error) { return 0, nil }
