// Package repotest is the conformance suite for internal/contract/repo. A backend
// passes when these tests pass against its real database. It lives apart from the
// SQLite code so a PostgreSQL backend (§5.1) can run the same suite.
package repotest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// Factory opens a fresh backend. Close happens through t.Cleanup in the factory.
type Factory func(t *testing.T) (repo.SystemStore, repo.TenantStores)

func ctx() context.Context { return context.Background() }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	if !errs.IsCode(err, code) {
		t.Fatalf("got %v, want code %s", err, code)
	}
}

// fixedTime has a sub-second part, which reveals any lossy time encoding, and lies
// in the past of any real run, so records stamped by the wall clock sort after it.
var fixedTime = time.Date(2026, 1, 15, 12, 30, 15, 123456789, time.UTC)

// Run exercises every repository of both databases.
func Run(t *testing.T, open Factory) {
	t.Run("system", func(t *testing.T) { runSystem(t, open) })
	t.Run("tenant", func(t *testing.T) { runTenant(t, open) })
}

func tenantStore(t *testing.T, open Factory, name domain.TenantID) repo.TenantStore {
	t.Helper()
	_, stores := open(t)
	st, err := stores.Create(ctx(), name)
	must(t, err)
	return st
}

// ---- System ----

func runSystem(t *testing.T, open Factory) {
	t.Run("tenants", func(t *testing.T) {
		sys, _ := open(t)
		r := sys.Tenants()
		_, err := r.Get(ctx(), "acme")
		wantCode(t, err, errs.CodeNotFound)
		must(t, r.Create(ctx(), domain.Tenant{ID: "acme", ULID: id.New(), Name: "Acme", StorageRoot: "acme/", CreatedAt: fixedTime}))
		wantCode(t, r.Create(ctx(), domain.Tenant{ID: "acme", ULID: id.New()}), errs.CodeConflict)
		wantCode(t, r.Create(ctx(), domain.Tenant{ID: "../evil", ULID: id.New()}), errs.CodeInvalidArgument)
		got, err := r.Get(ctx(), "acme")
		must(t, err)
		if got.Name != "Acme" || got.StorageRoot != "acme/" || !got.CreatedAt.Equal(fixedTime) {
			t.Fatalf("tenant = %+v", got)
		}
		must(t, r.Create(ctx(), domain.Tenant{ID: "beta", ULID: id.New()}))
		list, err := r.List(ctx())
		must(t, err)
		if len(list) != 2 || list[0].ID != "acme" || list[1].ID != "beta" {
			t.Fatalf("list = %+v", list)
		}
	})

	t.Run("users and memberships", func(t *testing.T) {
		sys, _ := open(t)
		must(t, sys.Tenants().Create(ctx(), domain.Tenant{ID: "acme", ULID: id.New()}))
		r := sys.Users()
		u := domain.User{ULID: id.New(), Email: "Maria@Example.com", SystemAdmin: true}
		must(t, r.Create(ctx(), u))
		wantCode(t, r.Create(ctx(), domain.User{ULID: id.New(), Email: "maria@example.com"}), errs.CodeConflict)
		got, err := r.GetByEmail(ctx(), "MARIA@example.com")
		must(t, err)
		if got.ULID != u.ULID || !got.SystemAdmin || got.Email != "maria@example.com" {
			t.Fatalf("user = %+v", got)
		}
		byID, err := r.Get(ctx(), u.ULID)
		must(t, err)
		if byID.Email != got.Email {
			t.Fatalf("user = %+v", byID)
		}
		_, err = r.GetByEmail(ctx(), "nobody@example.com")
		wantCode(t, err, errs.CodeNotFound)

		_, err = r.Membership(ctx(), u.ULID, "acme")
		wantCode(t, err, errs.CodeNotFound)
		m := domain.Membership{UserULID: u.ULID, TenantID: "acme", Role: domain.RoleEditor, Grants: []domain.Permission{domain.PermAssetDelete}}
		must(t, r.SetMembership(ctx(), m))
		got2, err := r.Membership(ctx(), u.ULID, "acme")
		must(t, err)
		if got2.Role != domain.RoleEditor || len(got2.Grants) != 1 || got2.Grants[0] != domain.PermAssetDelete || got2.Email != "maria@example.com" {
			t.Fatalf("membership = %+v", got2)
		}
		m.Role = domain.RoleAdmin
		m.Grants = nil
		must(t, r.SetMembership(ctx(), m))
		members, err := r.ListMembers(ctx(), "acme")
		must(t, err)
		if len(members) != 1 || members[0].Role != domain.RoleAdmin || len(members[0].Grants) != 0 {
			t.Fatalf("members = %+v", members)
		}
		all, err := r.Memberships(ctx(), u.ULID)
		must(t, err)
		if len(all) != 1 {
			t.Fatalf("memberships = %+v", all)
		}
		wantCode(t, r.SetMembership(ctx(), domain.Membership{UserULID: u.ULID, TenantID: "acme", Role: "owner"}), errs.CodeInvalidArgument)
		wantCode(t, r.SetMembership(ctx(), domain.Membership{UserULID: u.ULID, TenantID: "missing", Role: domain.RoleViewer}), errs.CodeNotFound)
		must(t, r.RemoveMembership(ctx(), u.ULID, "acme"))
		wantCode(t, r.RemoveMembership(ctx(), u.ULID, "acme"), errs.CodeNotFound)
	})

	t.Run("devices", func(t *testing.T) {
		sys, _ := open(t)
		u := domain.User{ULID: id.New(), Email: "a@example.com"}
		must(t, sys.Users().Create(ctx(), u))
		r := sys.Devices()
		first, err := r.NextID(ctx())
		must(t, err)
		second, err := r.NextID(ctx())
		must(t, err)
		if first != "CLIENT-001" || second != "CLIENT-002" {
			t.Fatalf("IDs = %s, %s", first, second)
		}
		d := domain.Device{ID: first, ULID: id.New(), UserULID: u.ULID, Name: "laptop", CredentialHash: "hash-1", CreatedAt: fixedTime}
		must(t, r.Create(ctx(), d))
		for _, ref := range []id.Ref{id.Ref(first), id.Ref(d.ULID)} {
			got, err := r.Get(ctx(), ref)
			must(t, err)
			if got.Email != "a@example.com" || got.Status != domain.DeviceActive || got.CredentialHash != "hash-1" {
				t.Fatalf("device = %+v", got)
			}
		}
		got, err := r.GetByCredentialHash(ctx(), "hash-1")
		must(t, err)
		if got.ID != first {
			t.Fatalf("device = %+v", got)
		}
		_, err = r.GetByCredentialHash(ctx(), "other")
		wantCode(t, err, errs.CodeNotFound)
		_, err = r.Get(ctx(), "ORI-001")
		wantCode(t, err, errs.CodeNotFound)
		now := fixedTime
		got.Status = domain.DeviceRevoked
		got.RevokedAt = &now
		must(t, r.Update(ctx(), got))
		updated, err := r.Get(ctx(), id.Ref(first))
		must(t, err)
		if updated.Status != domain.DeviceRevoked || updated.RevokedAt == nil || !updated.RevokedAt.Equal(now) {
			t.Fatalf("device = %+v", updated)
		}
		list, err := r.List(ctx(), "")
		must(t, err)
		mine, err := r.List(ctx(), u.ULID)
		must(t, err)
		none, err := r.List(ctx(), id.New())
		must(t, err)
		if len(list) != 1 || len(mine) != 1 || len(none) != 0 {
			t.Fatalf("list = %d, mine = %d, none = %d", len(list), len(mine), len(none))
		}
		// JSON never carries the hash.
		data, err := json.Marshal(updated)
		must(t, err)
		if strings.Contains(string(data), "hash-1") {
			t.Fatal("a device must never serialize its credential hash")
		}
	})

	t.Run("pairing codes are one-time and expire", func(t *testing.T) {
		sys, _ := open(t)
		u := domain.User{ULID: id.New(), Email: "a@example.com"}
		must(t, sys.Users().Create(ctx(), u))
		r := sys.Pairing()
		must(t, r.Create(ctx(), repo.PairingCode{Hash: "code", UserULID: u.ULID, CreatedBy: "admin", ExpiresAt: fixedTime.Add(time.Hour)}))
		got, err := r.Consume(ctx(), "code", fixedTime)
		must(t, err)
		if got.UserULID != u.ULID || got.UsedAt == nil || got.CreatedBy != "admin" {
			t.Fatalf("code = %+v", got)
		}
		_, err = r.Consume(ctx(), "code", fixedTime)
		wantCode(t, err, errs.CodeNotFound)
		must(t, r.Create(ctx(), repo.PairingCode{Hash: "old", UserULID: u.ULID, ExpiresAt: fixedTime.Add(-time.Second)}))
		_, err = r.Consume(ctx(), "old", fixedTime)
		wantCode(t, err, errs.CodeNotFound)
		_, err = r.Consume(ctx(), "missing", fixedTime)
		wantCode(t, err, errs.CodeNotFound)
	})

	t.Run("plugins", func(t *testing.T) {
		sys, _ := open(t)
		r := sys.Plugins()
		_, err := r.Get(ctx(), "notifications")
		wantCode(t, err, errs.CodeNotFound)
		p := domain.PluginRecord{ID: "notifications", Version: "1.0.0", Manifest: json.RawMessage(`{"id":"notifications"}`), SHA256: "abc", Enabled: true, InstalledBy: "admin"}
		must(t, r.Upsert(ctx(), p))
		p.Version = "1.1.0"
		p.Enabled = false
		must(t, r.Upsert(ctx(), p))
		got, err := r.Get(ctx(), "notifications")
		must(t, err)
		if got.Version != "1.1.0" || got.Enabled || string(got.Manifest) != `{"id":"notifications"}` {
			t.Fatalf("plugin = %+v", got)
		}
		list, err := r.List(ctx())
		must(t, err)
		if len(list) != 1 {
			t.Fatalf("list = %+v", list)
		}
		must(t, r.Delete(ctx(), "notifications"))
		wantCode(t, r.Delete(ctx(), "notifications"), errs.CodeNotFound)
	})

	t.Run("wrapped keys keep one active", func(t *testing.T) {
		sys, _ := open(t)
		r := sys.WrappedKeys()
		_, err := r.Active(ctx(), "acme")
		wantCode(t, err, errs.CodeNotFound)
		must(t, r.Put(ctx(), repo.WrappedKey{TenantID: "acme", Version: 1, Wrapped: []byte{1}, Active: true}))
		must(t, r.Put(ctx(), repo.WrappedKey{TenantID: "acme", Version: 2, Wrapped: []byte{2}, Active: true}))
		must(t, r.Put(ctx(), repo.WrappedKey{TenantID: "beta", Version: 1, Wrapped: []byte{3}, Active: true}))
		active, err := r.Active(ctx(), "acme")
		must(t, err)
		if active.Version != 2 || active.Wrapped[0] != 2 {
			t.Fatalf("active = %+v", active)
		}
		old, err := r.Get(ctx(), "acme", 1)
		must(t, err)
		if old.Active || old.Wrapped[0] != 1 {
			t.Fatalf("old = %+v", old)
		}
		// A re-wrap replaces the bytes of an existing version.
		must(t, r.Put(ctx(), repo.WrappedKey{TenantID: "acme", Version: 1, Wrapped: []byte{9}, Active: false}))
		list, err := r.List(ctx(), "acme")
		must(t, err)
		if len(list) != 2 || list[0].Version != 1 || list[0].Wrapped[0] != 9 || list[1].Version != 2 {
			t.Fatalf("list = %+v", list)
		}
		_, err = r.Get(ctx(), "acme", 3)
		wantCode(t, err, errs.CodeNotFound)
	})

	t.Run("workers", func(t *testing.T) {
		sys, _ := open(t)
		r := sys.Workers()
		ok, err := r.HasTokenHash(ctx(), "h1")
		must(t, err)
		if ok {
			t.Fatal("unknown token hash")
		}
		must(t, r.PutTokenHash(ctx(), "default", "h1"))
		ok, err = r.HasTokenHash(ctx(), "h1")
		must(t, err)
		if !ok {
			t.Fatal("token hash not found")
		}
		if l, ok := r.(repo.WorkerTokenLookup); ok {
			name, found, err := l.TokenName(ctx(), "h1")
			must(t, err)
			if !found || name != "default" {
				t.Fatalf("name = %q, %v", name, found)
			}
		}
		// Replacing a named token retires the old hash.
		must(t, r.PutTokenHash(ctx(), "default", "h2"))
		ok, err = r.HasTokenHash(ctx(), "h1")
		must(t, err)
		if ok {
			t.Fatal("a replaced token must stop working")
		}
		w := domain.Worker{ID: "w1", Pools: []string{"media"}, Resources: domain.WorkerResources{CPUCores: 4, MaxConcurrency: 2}, Version: "0.1", LastSeenAt: fixedTime}
		must(t, r.Upsert(ctx(), w))
		w.Running = 1
		must(t, r.Upsert(ctx(), w))
		list, err := r.List(ctx())
		must(t, err)
		if len(list) != 1 || list[0].Running != 1 || list[0].Pools[0] != "media" || list[0].Resources.CPUCores != 4 {
			t.Fatalf("workers = %+v", list)
		}
		must(t, r.Delete(ctx(), "w1"))
		wantCode(t, r.Delete(ctx(), "w1"), errs.CodeNotFound)
	})

	t.Run("transaction rolls back", func(t *testing.T) {
		sys, _ := open(t)
		err := sys.InTx(ctx(), func(tx repo.SystemRepos) error {
			must(t, tx.Tenants().Create(ctx(), domain.Tenant{ID: "acme", ULID: id.New()}))
			return errs.New(errs.CodeConflict, "abort")
		})
		wantCode(t, err, errs.CodeConflict)
		_, err = sys.Tenants().Get(ctx(), "acme")
		wantCode(t, err, errs.CodeNotFound)
	})

	t.Run("system audit chain", func(t *testing.T) {
		sys, _ := open(t)
		provider, ok := sys.(repo.SystemAuditProvider)
		if !ok {
			t.Skip("backend has no system chain")
		}
		runAuditChain(t, provider.SystemAudit(), "")
	})
}

// ---- Tenant ----

func newAsset(t *testing.T, st repo.TenantRepos, kind domain.AssetKind) domain.Asset {
	t.Helper()
	friendly, err := st.IDs().Next(ctx(), kind.IDPrefix())
	must(t, err)
	a := domain.Asset{ID: friendly, ULID: id.New(), Kind: kind, Title: "Title of " + string(friendly), CreatedAt: fixedTime}
	must(t, st.Assets().Create(ctx(), a))
	got, err := st.Assets().Get(ctx(), id.Ref(friendly))
	must(t, err)
	return got
}

func newVersion(t *testing.T, st repo.TenantRepos, a domain.Asset, version string) {
	t.Helper()
	must(t, st.Versions().Create(ctx(), domain.AssetVersion{AssetID: a.ID, Version: version}))
}

func newJob(t *testing.T, st repo.TenantRepos) domain.Job {
	t.Helper()
	friendly, err := st.IDs().Next(ctx(), id.PrefixJob)
	must(t, err)
	j := domain.Job{ID: friendly, ULID: id.New(), WorkflowID: "ingest-original", WorkflowVersion: "1.0.0", WorkflowSHA256: "abc",
		Snapshot: json.RawMessage(`{"id":"ingest-original",  "spaces":   "kept"}`), Inputs: json.RawMessage(`{"a":1}`),
		Trigger: domain.JobTrigger{Type: "cli"}, Permissions: []domain.Permission{domain.PermStorageArchiveRestore}, CreatedAt: fixedTime}
	must(t, st.Jobs().Create(ctx(), j))
	got, err := st.Jobs().GetByULID(ctx(), j.ULID)
	must(t, err)
	return got
}

func runTenant(t *testing.T, open Factory) {
	t.Run("stores", func(t *testing.T) {
		_, stores := open(t)
		_, err := stores.ForTenant(ctx(), "acme")
		wantCode(t, err, errs.CodeNotFound)
		_, err = stores.ForTenant(ctx(), "../../etc")
		wantCode(t, err, errs.CodeNotFound)
		_, err = stores.Create(ctx(), "../../etc")
		wantCode(t, err, errs.CodeInvalidArgument)
		st, err := stores.Create(ctx(), "acme")
		must(t, err)
		if st.Tenant() != "acme" {
			t.Fatalf("tenant = %s", st.Tenant())
		}
		_, err = stores.Create(ctx(), "acme")
		wantCode(t, err, errs.CodeConflict)
		again, err := stores.ForTenant(ctx(), "acme")
		must(t, err)
		if again.Tenant() != "acme" {
			t.Fatalf("tenant = %s", again.Tenant())
		}
		_, err = stores.Create(ctx(), "beta")
		must(t, err)
		list, err := stores.Tenants(ctx())
		must(t, err)
		if len(list) != 2 || list[0] != "acme" || list[1] != "beta" {
			t.Fatalf("tenants = %v", list)
		}
	})

	t.Run("friendly IDs", func(t *testing.T) { testIDs(t, open) })
	t.Run("assets", func(t *testing.T) { testAssets(t, open) })
	t.Run("versions", func(t *testing.T) { testVersions(t, open) })
	t.Run("collections and AI policy", func(t *testing.T) { testAIPolicy(t, open) })
	t.Run("transcripts", func(t *testing.T) { testTranscripts(t, open) })
	t.Run("search", func(t *testing.T) { testSearch(t, open) })
	t.Run("jobs", func(t *testing.T) { testJobs(t, open) })
	t.Run("approvals", func(t *testing.T) { testApprovals(t, open) })
	t.Run("publications", func(t *testing.T) { testPublications(t, open) })
	t.Run("uploads", func(t *testing.T) { testUploads(t, open) })
	t.Run("audit", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		runAuditChain(t, st.Audit(), "acme")
	})
	t.Run("audit in a transaction", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		boom := errs.New(errs.CodeConflict, "abort")
		err := st.InTx(ctx(), func(tx repo.TenantRepos) error {
			_, err := tx.Audit().Append(ctx(), domain.AuditRecord{Actor: "a", Action: "x", Result: "allowed"})
			must(t, err)
			return boom
		})
		wantCode(t, err, errs.CodeConflict)
		rec, err := st.Audit().Append(ctx(), domain.AuditRecord{Actor: "a", Action: "y", Result: "allowed"})
		must(t, err)
		if rec.Seq != 1 || rec.PrevHash != domain.AuditGenesisHash {
			t.Fatalf("a rolled back append must leave no trace: %+v", rec)
		}
	})
	t.Run("outbox", func(t *testing.T) { testOutbox(t, open) })
	t.Run("secrets", func(t *testing.T) { testSecrets(t, open) })
	t.Run("budgets", func(t *testing.T) { testBudgets(t, open) })
	t.Run("idempotency", func(t *testing.T) { testIdempotency(t, open) })
	t.Run("isolation between tenants", func(t *testing.T) { testIsolation(t, open) })
}

func testIDs(t *testing.T, open Factory) {
	t.Run("sequential per prefix", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		for prefix, want := range map[id.Prefix]string{id.PrefixOriginal: "ORI-001", id.PrefixClip: "CLIP-001", id.PrefixEdited: "EDT-001", id.PrefixJob: "JOB-001", id.PrefixApproval: "APR-001", id.PrefixPublication: "PUB-001", id.PrefixUpload: "UPL-001"} {
			got, err := st.IDs().Next(ctx(), prefix)
			must(t, err)
			if string(got) != want {
				t.Errorf("%s: got %s, want %s", prefix, got, want)
			}
		}
		next, err := st.IDs().Next(ctx(), id.PrefixOriginal)
		must(t, err)
		if next != "ORI-002" {
			t.Fatalf("next = %s", next)
		}
		// CLIENT numbering is system-wide (I14).
		_, err = st.IDs().Next(ctx(), id.PrefixClient)
		wantCode(t, err, errs.CodeInvalidArgument)
		_, err = st.IDs().Next(ctx(), "BAD")
		wantCode(t, err, errs.CodeInvalidArgument)
	})

	t.Run("no collisions under concurrency", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		const workers, each = 16, 25
		var (
			mu   sync.Mutex
			seen = map[id.Friendly]bool{}
			wg   sync.WaitGroup
		)
		errc := make(chan error, workers*each)
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range each {
					// Half allocate in their own transaction, as a real create does.
					var got id.Friendly
					var err error
					if w%2 == 0 {
						got, err = st.IDs().Next(ctx(), id.PrefixClip)
					} else {
						err = st.InTx(ctx(), func(tx repo.TenantRepos) error {
							var err error
							got, err = tx.IDs().Next(ctx(), id.PrefixClip)
							return err
						})
					}
					if err != nil {
						errc <- err
						return
					}
					mu.Lock()
					if seen[got] {
						errc <- fmt.Errorf("duplicate %s", got)
					}
					seen[got] = true
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		close(errc)
		for err := range errc {
			t.Fatal(err)
		}
		if len(seen) != workers*each {
			t.Fatalf("got %d IDs", len(seen))
		}
		next, err := st.IDs().Next(ctx(), id.PrefixClip)
		must(t, err)
		if next.Number() != workers*each+1 {
			t.Fatalf("next = %s: numbering must have no gaps from commits", next)
		}
	})

	t.Run("never reused", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindClip)
		must(t, st.Assets().SoftDelete(ctx(), id.Ref(a.ID), fixedTime))
		next, err := st.IDs().Next(ctx(), id.PrefixClip)
		must(t, err)
		if next == a.ID || next != "CLIP-002" {
			t.Fatalf("after deleting %s got %s", a.ID, next)
		}
	})

	t.Run("a rollback does not burn a number", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		err := st.InTx(ctx(), func(tx repo.TenantRepos) error {
			_, err := tx.IDs().Next(ctx(), id.PrefixClip)
			must(t, err)
			return errs.New(errs.CodeConflict, "abort")
		})
		wantCode(t, err, errs.CodeConflict)
		next, err := st.IDs().Next(ctx(), id.PrefixClip)
		must(t, err)
		if next != "CLIP-001" {
			t.Fatalf("next = %s", next)
		}
	})
}

func testAssets(t *testing.T, open Factory) {
	t.Run("create, resolve and default", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindOriginal)
		if a.ID != "ORI-001" || a.TenantID != "acme" || a.Status != domain.AssetRegistered || a.Visibility != domain.VisibilityPrivate ||
			a.AIProcessing != domain.AILocalOnly || a.AIConsent != nil || a.CatalogSync != domain.CatalogSyncNone || a.CurrentVersion != "v1" {
			t.Fatalf("asset = %+v", a)
		}
		if !a.CreatedAt.Equal(fixedTime) {
			t.Fatalf("created_at = %v", a.CreatedAt)
		}
		byULID, err := st.Assets().Get(ctx(), id.Ref(a.ULID))
		must(t, err)
		if byULID.ID != a.ID {
			t.Fatalf("by ULID = %+v", byULID)
		}
		// A lower-case ULID is the same ULID.
		lower, err := st.Assets().Get(ctx(), id.Ref(strings.ToLower(string(a.ULID))))
		must(t, err)
		if lower.ID != a.ID {
			t.Fatalf("by lower ULID = %+v", lower)
		}
		for _, ref := range []id.Ref{"ORI-999", "JOB-001", "garbage", ""} {
			_, err := st.Assets().Get(ctx(), ref)
			wantCode(t, err, errs.CodeNotFound)
		}
	})

	t.Run("create rejects bad input", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		r := st.Assets()
		cases := map[string]domain.Asset{
			"no id":         {ULID: id.New(), Kind: domain.KindClip, Title: "x"},
			"no ulid":       {ID: "CLIP-001", Kind: domain.KindClip, Title: "x"},
			"wrong prefix":  {ID: "ORI-001", ULID: id.New(), Kind: domain.KindClip, Title: "x"},
			"no title":      {ID: "CLIP-001", ULID: id.New(), Kind: domain.KindClip},
			"bad status":    {ID: "CLIP-001", ULID: id.New(), Kind: domain.KindClip, Title: "x", Status: "nope"},
			"other tenant":  {ID: "CLIP-001", ULID: id.New(), Kind: domain.KindClip, Title: "x", TenantID: "beta"},
			"bad kind":      {ID: "CLIP-001", ULID: id.New(), Kind: "lecture", Title: "x"},
			"bad visiblity": {ID: "CLIP-001", ULID: id.New(), Kind: domain.KindClip, Title: "x", Visibility: "open"},
		}
		for name, a := range cases {
			if err := r.Create(ctx(), a); !errs.IsCode(err, errs.CodeInvalidArgument) {
				t.Errorf("%s: %v", name, err)
			}
		}
		good := domain.Asset{ID: "CLIP-001", ULID: id.New(), Kind: domain.KindClip, Title: "x"}
		must(t, r.Create(ctx(), good))
		wantCode(t, r.Create(ctx(), good), errs.CodeConflict)
		// A parent that does not resolve in this tenant is not found.
		wantCode(t, r.Create(ctx(), domain.Asset{ID: "CLIP-002", ULID: id.New(), Kind: domain.KindClip, Title: "y", ParentAssetID: "ORI-404"}), errs.CodeNotFound)
	})

	t.Run("update", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindOriginal)
		a.Title = "New title"
		a.Tags = []string{"yoga", "pranayama"}
		a.Status = domain.AssetIngesting
		a.MasterAvailable = true
		a.CatalogSync = domain.CatalogSyncFailed
		a.DurationSeconds = 123.456
		must(t, st.Assets().Update(ctx(), a))
		got, err := st.Assets().Get(ctx(), id.Ref(a.ID))
		must(t, err)
		if got.Title != "New title" || len(got.Tags) != 2 || got.Status != domain.AssetIngesting || !got.MasterAvailable ||
			got.CatalogSync != domain.CatalogSyncFailed || got.DurationSeconds != 123.456 {
			t.Fatalf("asset = %+v", got)
		}
		if !got.UpdatedAt.After(got.CreatedAt) {
			t.Fatalf("updated_at = %v, created_at = %v", got.UpdatedAt, got.CreatedAt)
		}
		// Repeating a state is allowed so retried nodes stay idempotent.
		must(t, st.Assets().Update(ctx(), got))
		// A jump the state machine forbids.
		got.Status = domain.AssetPublished
		wantCode(t, st.Assets().Update(ctx(), got), errs.CodeFailedPrecondition)
		missing := a
		missing.ULID = id.New()
		missing.ID = "ORI-404"
		wantCode(t, st.Assets().Update(ctx(), missing), errs.CodeNotFound)
	})

	t.Run("list filters and pages", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		ori := newAsset(t, st, domain.KindOriginal)
		var clips []domain.Asset
		for range 5 {
			c := newAsset(t, st, domain.KindClip)
			c.ParentAssetID = ori.ID
			must(t, st.Assets().Update(ctx(), c))
			clips = append(clips, c)
		}
		items, next, err := st.Assets().List(ctx(), repo.AssetFilter{Kind: domain.KindClip}, repo.Page{Limit: 2})
		must(t, err)
		if len(items) != 2 || next == "" || items[0].ID != "CLIP-005" || items[1].ID != "CLIP-004" {
			t.Fatalf("page 1 = %v %q", ids(items), next)
		}
		items, next, err = st.Assets().List(ctx(), repo.AssetFilter{Kind: domain.KindClip}, repo.Page{Limit: 2, Cursor: next})
		must(t, err)
		if len(items) != 2 || next == "" || items[0].ID != "CLIP-003" {
			t.Fatalf("page 2 = %v %q", ids(items), next)
		}
		items, next, err = st.Assets().List(ctx(), repo.AssetFilter{Kind: domain.KindClip}, repo.Page{Limit: 2, Cursor: next})
		must(t, err)
		if len(items) != 1 || next != "" || items[0].ID != "CLIP-001" {
			t.Fatalf("page 3 = %v %q", ids(items), next)
		}
		_, _, err = st.Assets().List(ctx(), repo.AssetFilter{}, repo.Page{Cursor: "%%%"})
		wantCode(t, err, errs.CodeInvalidArgument)

		children, _, err := st.Assets().List(ctx(), repo.AssetFilter{SourceID: id.Ref(ori.ID)}, repo.Page{})
		must(t, err)
		if len(children) != 5 {
			t.Fatalf("children = %v", ids(children))
		}
		_, _, err = st.Assets().List(ctx(), repo.AssetFilter{SourceID: "ORI-404"}, repo.Page{})
		wantCode(t, err, errs.CodeNotFound)

		must(t, st.Assets().SoftDelete(ctx(), id.Ref(clips[0].ID), fixedTime))
		visible, _, err := st.Assets().List(ctx(), repo.AssetFilter{Kind: domain.KindClip}, repo.Page{})
		must(t, err)
		withDeleted, _, err := st.Assets().List(ctx(), repo.AssetFilter{Kind: domain.KindClip, IncludeDeleted: true}, repo.Page{})
		must(t, err)
		if len(visible) != 4 || len(withDeleted) != 5 {
			t.Fatalf("visible = %d, with deleted = %d", len(visible), len(withDeleted))
		}
		got, err := st.Assets().Get(ctx(), id.Ref(clips[0].ID))
		must(t, err)
		if got.DeletedAt == nil || !got.DeletedAt.Equal(fixedTime) {
			t.Fatalf("deleted_at = %v", got.DeletedAt)
		}
		must(t, st.Assets().SoftDelete(ctx(), id.Ref(clips[0].ID), fixedTime.Add(time.Hour)))
		got, err = st.Assets().Get(ctx(), id.Ref(clips[0].ID))
		must(t, err)
		if !got.DeletedAt.Equal(fixedTime) {
			t.Fatal("a repeated delete must keep the first time")
		}
	})

	t.Run("history", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindOriginal)
		must(t, st.Assets().AppendHistory(ctx(), domain.HistoryEntry{AssetID: a.ID, Actor: "a@example.com", Action: "metadata.update", Changes: json.RawMessage(`{"title":"x"}`)}))
		must(t, st.Assets().AppendHistory(ctx(), domain.HistoryEntry{AssetID: a.ID, Actor: "b@example.com", Action: "ai_processing.set"}))
		items, _, err := st.Assets().History(ctx(), id.Ref(a.ID), repo.Page{})
		must(t, err)
		if len(items) != 2 || items[0].Actor != "b@example.com" || items[1].AssetID != a.ID || string(items[1].Changes) != `{"title":"x"}` {
			t.Fatalf("history = %+v", items)
		}
		wantCode(t, st.Assets().AppendHistory(ctx(), domain.HistoryEntry{AssetID: "ORI-404", Action: "x"}), errs.CodeNotFound)
	})

	t.Run("overlapping clips", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		ori := newAsset(t, st, domain.KindOriginal)
		make := func(start, end float64) domain.Asset {
			friendly, err := st.IDs().Next(ctx(), id.PrefixClip)
			must(t, err)
			c := domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindClip, Title: "c", Lineage: &domain.Lineage{
				Method: domain.LineageComputed, SourceAssetID: ori.ID, RootAssetID: ori.ID,
				Segments: []domain.Segment{{SourceAssetID: ori.ID, SourceVersion: "v1", StartSeconds: start, EndSeconds: end}},
			}}
			must(t, st.Assets().Create(ctx(), c))
			return c
		}
		a := make(10, 20)
		b := make(30, 40)
		c := make(100, 110)
		must(t, st.Assets().SoftDelete(ctx(), id.Ref(c.ID), fixedTime))
		got, err := st.Assets().ClipsOverlapping(ctx(), id.Ref(ori.ID), []domain.Segment{{StartSeconds: 15, EndSeconds: 35}})
		must(t, err)
		if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
			t.Fatalf("overlaps = %v", ids(got))
		}
		// Touching at the boundary is not an overlap.
		got, err = st.Assets().ClipsOverlapping(ctx(), id.Ref(ori.ID), []domain.Segment{{StartSeconds: 20, EndSeconds: 30}})
		must(t, err)
		if len(got) != 0 {
			t.Fatalf("overlaps = %v", ids(got))
		}
		// A deleted clip does not warn.
		got, err = st.Assets().ClipsOverlapping(ctx(), id.Ref(ori.ID), []domain.Segment{{StartSeconds: 100, EndSeconds: 105}})
		must(t, err)
		if len(got) != 0 {
			t.Fatalf("overlaps = %v", ids(got))
		}
		// The lineage round-trips, and the parent follows the lineage's source.
		stored, err := st.Assets().Get(ctx(), id.Ref(a.ID))
		must(t, err)
		if stored.Lineage == nil || stored.Lineage.Segments[0].EndSeconds != 20 || stored.ParentAssetID != ori.ID {
			t.Fatalf("asset = %+v", stored)
		}
		_, err = st.Assets().ClipsOverlapping(ctx(), "ORI-404", nil)
		wantCode(t, err, errs.CodeNotFound)
	})

	t.Run("reads derive files, transcript and publications", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindOriginal)
		newVersion(t, st, a, "v1")
		put := func(p domain.FilePurpose, name string, role domain.StorageRole, key string) {
			must(t, st.Versions().PutFile(ctx(), id.Ref(a.ID), "v1", domain.VersionFile{Purpose: p, Name: name, File: domain.MediaFile{Storage: role, Key: key, Verified: true}}))
		}
		put(domain.FileSourceOriginal, "", domain.RoleArchive, "originals/ORI-001/v1/original.mov")
		put(domain.FileWorkingMaster, "", domain.RoleMaster, "originals/ORI-001/v1/master.mp4")
		put(domain.FileProxy, "", domain.RoleProxy, "originals/ORI-001/v1/proxy.mp4")
		put(domain.FileBackup, "", domain.RoleArchive, "originals/ORI-001/v1/backup.mov")
		put(domain.FileThumbnail, "", domain.RoleProxy, "originals/ORI-001/v1/thumbnail.jpg")
		must(t, st.Versions().SetProvenance(ctx(), id.Ref(a.ID), "v1", domain.Provenance{JobID: "JOB-001", Workflow: "ingest-original@1.0.0"}))

		got, err := st.Assets().Get(ctx(), id.Ref(a.ID))
		must(t, err)
		if got.WorkingMaster == nil || got.WorkingMaster.Key != "originals/ORI-001/v1/master.mp4" || got.SourceOriginal == nil || got.Proxy == nil || got.Backup == nil || got.Thumbnail == nil {
			t.Fatalf("files = %+v", got)
		}
		if got.Provenance == nil || got.Provenance.Workflow != "ingest-original@1.0.0" {
			t.Fatalf("provenance = %+v", got.Provenance)
		}
		if got.Transcript != nil {
			t.Fatalf("transcript before saving = %+v", got.Transcript)
		}

		// Writers cannot set readiness or files by hand (I105).
		forged := got
		forged.Transcript = &domain.TranscriptInfo{Indexed: true, EmbeddingModel: "forged@1"}
		forged.WorkingMaster = &domain.MediaFile{Key: "forged"}
		must(t, st.Assets().Update(ctx(), forged))
		got, err = st.Assets().Get(ctx(), id.Ref(a.ID))
		must(t, err)
		if got.Transcript != nil || got.WorkingMaster.Key == "forged" {
			t.Fatalf("derived fields were written: %+v", got)
		}

		must(t, st.Transcripts().Save(ctx(), domain.Transcript{AssetID: a.ID, Version: "v1", Language: "pt", TimestampBasis: "working_master", Engine: "fake", Segments: []domain.TranscriptSegment{{Start: 0, End: 1, Text: "olá"}}}))
		got, err = st.Assets().Get(ctx(), id.Ref(a.ID))
		must(t, err)
		if got.Transcript == nil || got.Transcript.Indexed || got.Transcript.Format != "json" || got.Transcript.Engine != "fake" || got.Transcript.TimestampBasis != "working_master" {
			t.Fatalf("transcript = %+v", got.Transcript)
		}
		must(t, st.Search().IndexLexical(ctx(), id.Ref(a.ID), "v1", []domain.SearchChunk{{Level: domain.ChunkWindow, EndSeconds: 1, Text: "olá"}}))
		got, _ = st.Assets().Get(ctx(), id.Ref(a.ID))
		if got.Transcript.Indexed {
			t.Fatal("lexical alone is not indexed (R11)")
		}
		must(t, st.Search().IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "fake@1", Chunking: "d15", Chunks: []domain.EmbeddedChunk{{SearchChunk: domain.SearchChunk{EndSeconds: 1, Text: "olá"}, Vector: []float32{1, 0}}}}))
		got, _ = st.Assets().Get(ctx(), id.Ref(a.ID))
		if !got.Transcript.Indexed || got.Transcript.EmbeddingModel != "fake@1" {
			t.Fatalf("transcript = %+v", got.Transcript)
		}
		put(domain.FileCaptions, "pt", domain.RoleProxy, "originals/ORI-001/v1/captions.pt.srt")
		got, _ = st.Assets().Get(ctx(), id.Ref(a.ID))
		if got.Transcript.Format != "json+srt" {
			t.Fatalf("format = %s", got.Transcript.Format)
		}
		// All flags plus derived info make it ready.
		got.BackupVerified, got.MasterAvailable, got.ProxyAvailable, got.TimelineValidated = true, true, true, true
		must(t, st.Assets().Update(ctx(), got))
		ready, _ := st.Assets().Get(ctx(), id.Ref(a.ID))
		if !ready.ReadinessSatisfied() {
			t.Fatalf("readiness = %+v", ready)
		}

		pubID, err := st.IDs().Next(ctx(), id.PrefixPublication)
		must(t, err)
		must(t, st.Publications().Create(ctx(), domain.Publication{ID: pubID, ULID: id.New(), AssetID: a.ID, Destination: "mock-main", AssetVersion: "v1", Status: domain.PubUploading}))
		got, _ = st.Assets().Get(ctx(), id.Ref(a.ID))
		if len(got.Publications) != 1 || got.Publications[0].ID != pubID || got.Publications[0].AssetID != a.ID {
			t.Fatalf("publications = %+v", got.Publications)
		}
		// Lists hydrate too.
		items, _, err := st.Assets().List(ctx(), repo.AssetFilter{}, repo.Page{})
		must(t, err)
		if len(items) != 1 || items[0].WorkingMaster == nil || len(items[0].Publications) != 1 {
			t.Fatalf("list = %+v", items)
		}
	})

	t.Run("a transaction commits or rolls back everything", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		err := st.InTx(ctx(), func(tx repo.TenantRepos) error {
			a := newAsset(t, tx, domain.KindOriginal)
			_, err := tx.Outbox().Append(ctx(), event.Envelope{Type: event.AssetCreated, AssetID: string(a.ID)})
			must(t, err)
			return errs.New(errs.CodeConflict, "abort")
		})
		wantCode(t, err, errs.CodeConflict)
		items, _, err := st.Assets().List(ctx(), repo.AssetFilter{IncludeDeleted: true}, repo.Page{})
		must(t, err)
		events, err := st.Outbox().Stream(ctx(), 0, 10)
		must(t, err)
		if len(items) != 0 || len(events) != 0 {
			t.Fatalf("rollback left %d assets and %d events", len(items), len(events))
		}
		var friendly id.Friendly
		must(t, st.InTx(ctx(), func(tx repo.TenantRepos) error {
			a := newAsset(t, tx, domain.KindOriginal)
			friendly = a.ID
			_, err := tx.Outbox().Append(ctx(), event.Envelope{Type: event.AssetCreated, AssetID: string(a.ID)})
			return err
		}))
		events, err = st.Outbox().Stream(ctx(), 0, 10)
		must(t, err)
		if len(events) != 1 || events[0].Event.AssetID != string(friendly) {
			t.Fatalf("events = %+v", events)
		}
	})
}

func ids(items []domain.Asset) []string {
	out := make([]string, len(items))
	for i, a := range items {
		out[i] = string(a.ID)
	}
	return out
}

func testVersions(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	a := newAsset(t, st, domain.KindClip)
	r := st.Versions()
	ref := id.Ref(a.ID)
	_, err := r.Get(ctx(), ref, "v1")
	wantCode(t, err, errs.CodeNotFound)
	must(t, r.Create(ctx(), domain.AssetVersion{AssetID: a.ID, Version: "v1", Files: []domain.VersionFile{{Purpose: domain.FileWorkingMaster, File: domain.MediaFile{Storage: domain.RoleMaster, Key: "clips/CLIP-001/v1/master.mp4", SHA256: "aa"}}}}))
	wantCode(t, r.Create(ctx(), domain.AssetVersion{AssetID: a.ID, Version: "v1"}), errs.CodeConflict)
	wantCode(t, r.Create(ctx(), domain.AssetVersion{AssetID: "CLIP-404", Version: "v1"}), errs.CodeNotFound)

	// Before publication a file is replaced in place (D35).
	file := domain.VersionFile{Purpose: domain.FileWorkingMaster, File: domain.MediaFile{Storage: domain.RoleMaster, Key: "clips/CLIP-001/v1/master.mp4", SHA256: "bb", Location: &domain.Location{Driver: "local"}}}
	must(t, r.PutFile(ctx(), ref, "v1", file))
	must(t, r.PutFile(ctx(), ref, "v1", domain.VersionFile{Purpose: domain.FilePublishedFile, Name: "youtube-main", File: domain.MediaFile{Storage: domain.RoleMaster, Key: "clips/CLIP-001/v1/published-youtube-main.mp4"}}))
	must(t, r.PutFile(ctx(), ref, "v1", domain.VersionFile{Purpose: domain.FilePublishedFile, Name: "vimeo-main", File: domain.MediaFile{Storage: domain.RoleMaster, Key: "clips/CLIP-001/v1/published-vimeo-main.mp4"}}))
	got, err := r.Get(ctx(), ref, "v1")
	must(t, err)
	if got.Immutable || len(got.Files) != 3 || got.AssetID != a.ID {
		t.Fatalf("version = %+v", got)
	}
	var master domain.VersionFile
	for _, f := range got.Files {
		if f.Purpose == domain.FileWorkingMaster {
			master = f
		}
	}
	if master.File.SHA256 != "bb" || master.File.Location == nil || master.File.Location.Driver != "local" {
		t.Fatalf("master = %+v", master)
	}
	wantCode(t, r.PutFile(ctx(), ref, "v9", file), errs.CodeNotFound)
	wantCode(t, r.PutFile(ctx(), "CLIP-404", "v1", file), errs.CodeNotFound)

	must(t, r.SetProvenance(ctx(), ref, "v1", domain.Provenance{JobID: "JOB-001", Workflow: "create-clip@1.0.0", WorkflowSHA256: "cc", NodeTypes: map[string]int{"video.cut": 1}, Tools: map[string]string{"ffmpeg": "7"}}))
	wantCode(t, r.SetProvenance(ctx(), ref, "v9", domain.Provenance{}), errs.CodeNotFound)

	must(t, r.MarkImmutable(ctx(), ref, "v1"))
	must(t, r.MarkImmutable(ctx(), ref, "v1"))
	wantCode(t, r.PutFile(ctx(), ref, "v1", file), errs.CodeFailedPrecondition)
	wantCode(t, r.MarkImmutable(ctx(), ref, "v9"), errs.CodeNotFound)

	must(t, r.Create(ctx(), domain.AssetVersion{AssetID: a.ID, Version: "v2"}))
	list, err := r.List(ctx(), ref)
	must(t, err)
	if len(list) != 2 || list[0].Version != "v1" || !list[0].Immutable || list[0].Provenance == nil || list[0].Provenance.NodeTypes["video.cut"] != 1 || list[1].Version != "v2" || list[1].Immutable {
		t.Fatalf("list = %+v", list)
	}
	_, err = r.List(ctx(), "CLIP-404")
	wantCode(t, err, errs.CodeNotFound)
}

func testAIPolicy(t *testing.T, open Factory) {
	consent := &domain.AIConsent{AuthorizedBy: "admin@example.com", AuthorizedAt: fixedTime, Scope: "collection"}
	assetConsent := &domain.AIConsent{AuthorizedBy: "admin@example.com", AuthorizedAt: fixedTime, Scope: "asset"}

	t.Run("collections", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		r := st.Collections()
		_, err := r.Get(ctx(), "yoga")
		wantCode(t, err, errs.CodeNotFound)
		wantCode(t, r.Upsert(ctx(), domain.Collection{Name: "yoga", AIProcessing: domain.AIExternalAllowed}), errs.CodeFailedPrecondition)
		must(t, r.Upsert(ctx(), domain.Collection{Name: "yoga", AIProcessing: domain.AIExternalAllowed, AIConsent: consent}))
		must(t, r.Upsert(ctx(), domain.Collection{Name: "talks"}))
		got, err := r.Get(ctx(), "yoga")
		must(t, err)
		if got.TenantID != "acme" || got.AIProcessing != domain.AIExternalAllowed || got.AIConsent == nil || got.AIConsent.AuthorizedBy != "admin@example.com" {
			t.Fatalf("collection = %+v", got)
		}
		list, err := r.List(ctx())
		must(t, err)
		if len(list) != 2 || list[0].Name != "talks" || list[0].AIProcessing != domain.AILocalOnly {
			t.Fatalf("list = %+v", list)
		}
	})

	t.Run("external needs consent at create", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		base := domain.Asset{ID: "ORI-001", ULID: id.New(), Kind: domain.KindOriginal, Title: "x", AIProcessing: domain.AIExternalAllowed}
		wantCode(t, st.Assets().Create(ctx(), base), errs.CodeFailedPrecondition)
		base.AIConsent = assetConsent
		must(t, st.Assets().Create(ctx(), base))
		got, err := st.Assets().Get(ctx(), "ORI-001")
		must(t, err)
		if got.AIProcessing != domain.AIExternalAllowed || got.AIConsent == nil {
			t.Fatalf("asset = %+v", got)
		}
	})

	t.Run("update cannot change it", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindOriginal)
		a.AIProcessing = domain.AIExternalAllowed
		a.AIConsent = assetConsent
		must(t, st.Assets().Update(ctx(), a))
		got, err := st.Assets().Get(ctx(), id.Ref(a.ID))
		must(t, err)
		if got.AIProcessing != domain.AILocalOnly || got.AIConsent != nil {
			t.Fatalf("Update changed the AI policy: %+v", got)
		}
	})

	t.Run("policy writer and collection propagation", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		w, ok := st.Assets().(repo.AssetAIPolicyWriter)
		if !ok {
			t.Skip("backend has no policy writer")
		}
		must(t, st.Collections().Upsert(ctx(), domain.Collection{Name: "yoga", AIProcessing: domain.AIExternalAllowed, AIConsent: consent}))
		create := func(collection string, processing domain.AIProcessing) domain.Asset {
			friendly, err := st.IDs().Next(ctx(), id.PrefixOriginal)
			must(t, err)
			must(t, st.Assets().Create(ctx(), domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindOriginal, Title: "x", Collection: collection, AIProcessing: processing}))
			got, err := st.Assets().Get(ctx(), id.Ref(friendly))
			must(t, err)
			return got
		}
		inherited := create("yoga", "")
		if inherited.AIProcessing != domain.AIExternalAllowed || inherited.AIConsent == nil || inherited.AIConsent.Scope != "collection" {
			t.Fatalf("inherited = %+v", inherited)
		}
		explicit := create("yoga", domain.AILocalOnly)
		if explicit.AIProcessing != domain.AILocalOnly {
			t.Fatalf("explicit = %+v", explicit)
		}
		newName := create("new-collection", "")
		if newName.AIProcessing != domain.AILocalOnly {
			t.Fatalf("new collection = %+v", newName)
		}
		if _, err := st.Collections().Get(ctx(), "new-collection"); err != nil {
			t.Fatalf("a named collection must exist: %v", err)
		}
		source, err := w.AIPolicySource(ctx(), id.Ref(explicit.ID))
		must(t, err)
		if source != repo.AIPolicyAsset {
			t.Fatalf("source = %s", source)
		}

		// A collection change does not touch an asset-level decision.
		n, err := w.ApplyCollectionPolicy(ctx(), "yoga", domain.AILocalOnly, nil)
		must(t, err)
		if n != 1 {
			t.Fatalf("changed = %d", n)
		}
		got, _ := st.Assets().Get(ctx(), id.Ref(inherited.ID))
		if got.AIProcessing != domain.AILocalOnly || got.AIConsent != nil {
			t.Fatalf("member = %+v", got)
		}
		_, err = w.ApplyCollectionPolicy(ctx(), "yoga", domain.AIExternalAllowed, nil)
		wantCode(t, err, errs.CodeFailedPrecondition)
		_, err = w.ApplyCollectionPolicy(ctx(), "yoga", domain.AIExternalAllowed, consent)
		must(t, err)
		got, _ = st.Assets().Get(ctx(), id.Ref(explicit.ID))
		if got.AIProcessing != domain.AILocalOnly {
			t.Fatalf("an asset-level decision must win: %+v", got)
		}
		got, _ = st.Assets().Get(ctx(), id.Ref(inherited.ID))
		if got.AIProcessing != domain.AIExternalAllowed || got.AIConsent == nil {
			t.Fatalf("member = %+v", got)
		}

		// Moving between collections re-evaluates a policy not decided on the asset.
		got.Collection = "new-collection"
		must(t, st.Assets().Update(ctx(), got))
		moved, _ := st.Assets().Get(ctx(), id.Ref(inherited.ID))
		if moved.AIProcessing != domain.AILocalOnly || moved.AIConsent != nil {
			t.Fatalf("moved = %+v", moved)
		}
		moved.Collection = "yoga"
		must(t, st.Assets().Update(ctx(), moved))
		back, _ := st.Assets().Get(ctx(), id.Ref(inherited.ID))
		if back.AIProcessing != domain.AIExternalAllowed {
			t.Fatalf("back = %+v", back)
		}
		explicit.Collection = "new-collection"
		must(t, st.Assets().Update(ctx(), explicit))
		stays, _ := st.Assets().Get(ctx(), id.Ref(explicit.ID))
		if stays.AIProcessing != domain.AILocalOnly {
			t.Fatalf("stays = %+v", stays)
		}

		wantCode(t, w.SetAIPolicy(ctx(), id.Ref(explicit.ID), domain.AIExternalAllowed, nil, repo.AIPolicyAsset), errs.CodeFailedPrecondition)
		wantCode(t, w.SetAIPolicy(ctx(), id.Ref(explicit.ID), "open", nil, repo.AIPolicyAsset), errs.CodeInvalidArgument)
		wantCode(t, w.SetAIPolicy(ctx(), "ORI-404", domain.AILocalOnly, nil, repo.AIPolicyAsset), errs.CodeNotFound)
		must(t, w.SetAIPolicy(ctx(), id.Ref(explicit.ID), domain.AIExternalAllowed, assetConsent, repo.AIPolicyAsset))
		got, _ = st.Assets().Get(ctx(), id.Ref(explicit.ID))
		if got.AIProcessing != domain.AIExternalAllowed || got.AIConsent.Scope != "asset" {
			t.Fatalf("set = %+v", got)
		}
	})
}

func testTranscripts(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	a := newAsset(t, st, domain.KindOriginal)
	ref := id.Ref(a.ID)
	r := st.Transcripts()
	_, err := r.Get(ctx(), ref, "v1")
	wantCode(t, err, errs.CodeNotFound)
	tr := domain.Transcript{AssetID: a.ID, Version: "v1", Language: "pt", DurationSeconds: 60, TimestampBasis: "working_master", Engine: "fake",
		Segments: []domain.TranscriptSegment{{Start: 0.5, End: 2.25, Text: "respiração consciente", Words: []domain.Word{{Start: 0.5, End: 1, Text: "respiração"}}, Speaker: "S1"}}}
	must(t, r.Save(ctx(), tr))
	tr.Language = "en"
	must(t, r.Save(ctx(), tr))
	got, err := r.Get(ctx(), ref, "v1")
	must(t, err)
	if got.Language != "en" || got.Segments[0].Words[0].Text != "respiração" || got.AssetID != a.ID || got.CreatedAt.IsZero() {
		t.Fatalf("transcript = %+v", got)
	}
	wantCode(t, r.Save(ctx(), domain.Transcript{AssetID: "ORI-404", Version: "v1"}), errs.CodeNotFound)
	wantCode(t, r.Save(ctx(), domain.Transcript{AssetID: a.ID}), errs.CodeInvalidArgument)

	_, err = r.Analysis(ctx(), ref, "v1")
	wantCode(t, err, errs.CodeNotFound)
	must(t, r.SaveAnalysis(ctx(), domain.Analysis{AssetID: a.ID, Version: "v1", Available: true, Summary: "s", Stories: []domain.Story{{Title: "t", StartSeconds: 1, EndSeconds: 2, Confidence: 0.5}}, Model: "m", Endpoint: "local"}))
	an, err := r.Analysis(ctx(), ref, "v1")
	must(t, err)
	if !an.Available || an.Stories[0].Confidence != 0.5 {
		t.Fatalf("analysis = %+v", an)
	}

	must(t, r.MarkSensitive(ctx(), domain.SensitiveSegment{AssetID: a.ID, Version: "v1", StartSeconds: 10.5, EndSeconds: 20, Reason: "minor", MarkedBy: "e@example.com"}))
	must(t, r.MarkSensitive(ctx(), domain.SensitiveSegment{AssetID: a.ID, Version: "v1", StartSeconds: 1, EndSeconds: 2}))
	wantCode(t, r.MarkSensitive(ctx(), domain.SensitiveSegment{AssetID: a.ID, Version: "v1", StartSeconds: 5, EndSeconds: 5}), errs.CodeInvalidArgument)
	wantCode(t, r.MarkSensitive(ctx(), domain.SensitiveSegment{AssetID: a.ID, Version: "v1", StartSeconds: -1, EndSeconds: 5}), errs.CodeInvalidArgument)
	segs, err := r.Sensitive(ctx(), ref, "v1")
	must(t, err)
	if len(segs) != 2 || segs[0].StartSeconds != 1 || segs[1].Reason != "minor" || segs[1].AssetID != a.ID || segs[1].MarkedAt.IsZero() {
		t.Fatalf("segments = %+v", segs)
	}
	must(t, r.UnmarkSensitive(ctx(), ref, "v1", 10.5, 20))
	wantCode(t, r.UnmarkSensitive(ctx(), ref, "v1", 10.5, 20), errs.CodeNotFound)
	segs, err = r.Sensitive(ctx(), ref, "v1")
	must(t, err)
	if len(segs) != 1 {
		t.Fatalf("segments = %+v", segs)
	}
}

func vec(dim int, hot ...int) []float32 {
	v := make([]float32, dim)
	for _, i := range hot {
		v[i] = 1
	}
	return v
}

func testSearch(t *testing.T, open Factory) {
	chunk := func(start, end float64, text string, v []float32) domain.EmbeddedChunk {
		return domain.EmbeddedChunk{SearchChunk: domain.SearchChunk{Level: domain.ChunkWindow, StartSeconds: start, EndSeconds: end, Text: text}, Vector: v}
	}

	t.Run("lexical", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		r := st.Search()
		a := newAsset(t, st, domain.KindOriginal)
		b := newAsset(t, st, domain.KindOriginal)
		must(t, r.IndexLexical(ctx(), id.Ref(a.ID), "v1", []domain.SearchChunk{
			{Level: domain.ChunkWindow, StartSeconds: 0, EndSeconds: 30, Text: "explanation of Ujjayi breathing"},
			{Level: domain.ChunkChapter, StartSeconds: 30, EndSeconds: 60, Text: "conscious breathing and respiração consciente"},
		}))
		must(t, r.IndexLexical(ctx(), id.Ref(b.ID), "v1", []domain.SearchChunk{{StartSeconds: 5, EndSeconds: 6, Text: "unrelated breathing note"}}))

		hits, err := r.SearchLexical(ctx(), repo.LexicalQuery{Text: "conscious breathing"})
		must(t, err)
		if len(hits) != 3 || hits[0].StartSeconds != 30 || hits[0].Level != domain.ChunkChapter || hits[0].Source != "lexical" || hits[0].Score <= hits[1].Score || hits[0].AssetID != a.ID || hits[0].Version != "v1" {
			t.Fatalf("hits = %+v", hits)
		}
		// Diacritics fold, and an asset filter applies (D5).
		hits, err = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "respiracao", AssetID: id.Ref(a.ID)})
		must(t, err)
		if len(hits) != 1 {
			t.Fatalf("hits = %+v", hits)
		}
		hits, err = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "respiracao", AssetID: id.Ref(b.ID)})
		must(t, err)
		if len(hits) != 0 {
			t.Fatalf("hits = %+v", hits)
		}
		_, err = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "x", AssetID: "ORI-404"})
		wantCode(t, err, errs.CodeNotFound)

		// User text is never FTS5 syntax.
		for _, nasty := range []string{`"`, `breathing"`, `NEAR(a b)`, `text:breathing`, `*`, `(`, `a AND`, `- OR`, `'; DROP TABLE assets; --`} {
			if _, err := r.SearchLexical(ctx(), repo.LexicalQuery{Text: nasty}); err != nil {
				t.Errorf("query %q: %v", nasty, err)
			}
		}
		hits, err = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "   "})
		must(t, err)
		if len(hits) != 0 {
			t.Fatalf("hits = %+v", hits)
		}

		// Indexing again replaces, so a retry never duplicates.
		must(t, r.IndexLexical(ctx(), id.Ref(a.ID), "v1", []domain.SearchChunk{{StartSeconds: 1, EndSeconds: 2, Text: "replaced text"}}))
		hits, err = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "ujjayi"})
		must(t, err)
		if len(hits) != 0 {
			t.Fatalf("hits = %+v", hits)
		}
		hits, err = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "replaced", Limit: 1})
		must(t, err)
		if len(hits) != 1 {
			t.Fatalf("hits = %+v", hits)
		}

		lexical, vector, err := r.HasIndex(ctx(), id.Ref(a.ID), "v1", "any@1")
		must(t, err)
		if !lexical || vector {
			t.Fatalf("has index = %v, %v", lexical, vector)
		}
		lexical, _, err = r.HasIndex(ctx(), id.Ref(a.ID), "v9", "any@1")
		must(t, err)
		if lexical {
			t.Fatal("no chunks for v9")
		}
	})

	t.Run("only the current version and live assets are found", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		r := st.Search()
		a := newAsset(t, st, domain.KindOriginal)
		must(t, r.IndexLexical(ctx(), id.Ref(a.ID), "v1", []domain.SearchChunk{{EndSeconds: 1, Text: "old word"}}))
		must(t, r.IndexLexical(ctx(), id.Ref(a.ID), "v2", []domain.SearchChunk{{EndSeconds: 1, Text: "new word"}}))
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "m@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "old word", vec(4, 0))}}))
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v2", Model: "m@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "new word", vec(4, 1))}}))
		hits, err := r.SearchLexical(ctx(), repo.LexicalQuery{Text: "word"})
		must(t, err)
		if len(hits) != 1 || hits[0].Version != "v1" {
			t.Fatalf("hits = %+v", hits)
		}
		vhits, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 1), Model: "m@1"})
		must(t, err)
		if len(vhits) != 1 || vhits[0].Version != "v1" {
			t.Fatalf("hits = %+v", vhits)
		}
		a.CurrentVersion = "v2"
		must(t, st.Assets().Update(ctx(), a))
		hits, _ = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "word"})
		if len(hits) != 1 || hits[0].Version != "v2" {
			t.Fatalf("hits = %+v", hits)
		}
		must(t, st.Assets().SoftDelete(ctx(), id.Ref(a.ID), fixedTime))
		hits, _ = r.SearchLexical(ctx(), repo.LexicalQuery{Text: "word"})
		vhits, _ = r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 1), Model: "m@1"})
		if len(hits) != 0 || len(vhits) != 0 {
			t.Fatalf("deleted asset found: %+v %+v", hits, vhits)
		}
	})

	t.Run("vectors", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		r := st.Search()
		a := newAsset(t, st, domain.KindOriginal)
		b := newAsset(t, st, domain.KindOriginal)
		active, err := r.ActiveModel(ctx())
		must(t, err)
		if active != "" {
			t.Fatalf("active = %q", active)
		}
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "m@1", Chunking: "d15", Chunks: []domain.EmbeddedChunk{
			chunk(0, 30, "first", vec(4, 0)),
			chunk(30, 60, "second", vec(4, 1)),
		}}))
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(b.ID), Version: "v1", Model: "m@1", Chunking: "d15", Chunks: []domain.EmbeddedChunk{
			chunk(0, 30, "other", vec(4, 0, 1)),
		}}))
		// The first model written becomes active.
		active, err = r.ActiveModel(ctx())
		must(t, err)
		if active != "m@1" {
			t.Fatalf("active = %q", active)
		}
		// Asset-scoped is the main path (D5).
		hits, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "m@1", AssetID: id.Ref(a.ID), Limit: 5})
		must(t, err)
		if len(hits) != 2 || hits[0].Text != "first" || hits[0].Source != "vector" || hits[0].AssetID != a.ID || hits[0].Score <= hits[1].Score || hits[0].Score < 0.99 {
			t.Fatalf("hits = %+v", hits)
		}
		all, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "m@1", Limit: 5})
		must(t, err)
		if len(all) != 3 || all[0].Text != "first" || all[1].Text != "other" {
			t.Fatalf("all = %+v", all)
		}
		one, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "m@1", Limit: 1})
		must(t, err)
		if len(one) != 1 {
			t.Fatalf("one = %+v", one)
		}
		_, err = r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(3, 0), Model: "m@1"})
		wantCode(t, err, errs.CodeInvalidArgument)
		_, err = r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0)})
		wantCode(t, err, errs.CodeInvalidArgument)
		_, err = r.SearchVector(ctx(), repo.VectorQuery{Model: "m@1"})
		wantCode(t, err, errs.CodeInvalidArgument)
		_, err = r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "m@1", AssetID: "ORI-404"})
		wantCode(t, err, errs.CodeNotFound)
		none, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "unknown@1"})
		must(t, err)
		if len(none) != 0 {
			t.Fatalf("hits for an unknown model = %+v", none)
		}

		// Indexing again replaces.
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "m@1", Chunks: []domain.EmbeddedChunk{chunk(0, 30, "only", vec(4, 2))}}))
		hits, err = r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "m@1", AssetID: id.Ref(a.ID)})
		must(t, err)
		if len(hits) != 1 || hits[0].Text != "only" {
			t.Fatalf("hits = %+v", hits)
		}
		// Mismatched dimensions never reach the table.
		wantCode(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "m@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "x", vec(3, 0))}}), errs.CodeInvalidArgument)
		wantCode(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "n@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "x", vec(3, 0)), chunk(1, 2, "y", vec(4, 0))}}), errs.CodeInvalidArgument)
		wantCode(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "x", vec(4, 0))}}), errs.CodeInvalidArgument)
		wantCode(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: "ORI-404", Version: "v1", Model: "m@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "x", vec(4, 0))}}), errs.CodeNotFound)
		// An empty batch clears what a previous attempt left.
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "m@1"}))
		_, vector, err := r.HasIndex(ctx(), id.Ref(a.ID), "v1", "m@1")
		must(t, err)
		if vector {
			t.Fatal("an empty batch must clear the vectors")
		}
	})

	t.Run("side by side reindexing", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		r := st.Search()
		a := newAsset(t, st, domain.KindOriginal)
		must(t, st.Transcripts().Save(ctx(), domain.Transcript{AssetID: a.ID, Version: "v1", Engine: "fake"}))
		must(t, r.IndexLexical(ctx(), id.Ref(a.ID), "v1", []domain.SearchChunk{{EndSeconds: 1, Text: "t"}}))
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "old@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "old model", vec(4, 0))}}))
		// A new model with other dimensions coexists (D15, R18).
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "new@2", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "new model", vec(8, 0))}}))
		active, err := r.ActiveModel(ctx())
		must(t, err)
		if active != "old@1" {
			t.Fatalf("searches must stay on the old model until the swap: %q", active)
		}
		oldHits, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "old@1"})
		must(t, err)
		newHits, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(8, 0), Model: "new@2"})
		must(t, err)
		if len(oldHits) != 1 || oldHits[0].Text != "old model" || len(newHits) != 1 || newHits[0].Text != "new model" {
			t.Fatalf("models mixed: %+v %+v", oldHits, newHits)
		}
		info, _ := st.Assets().Get(ctx(), id.Ref(a.ID))
		if info.Transcript.EmbeddingModel != "old@1" {
			t.Fatalf("model = %s", info.Transcript.EmbeddingModel)
		}
		wantCode(t, r.DeleteModel(ctx(), "old@1"), errs.CodeFailedPrecondition)
		wantCode(t, r.SetActiveModel(ctx(), "missing@1"), errs.CodeNotFound)
		must(t, r.SetActiveModel(ctx(), "new@2"))
		info, _ = st.Assets().Get(ctx(), id.Ref(a.ID))
		if info.Transcript.EmbeddingModel != "new@2" {
			t.Fatalf("model = %s", info.Transcript.EmbeddingModel)
		}
		must(t, r.DeleteModel(ctx(), "old@1"))
		wantCode(t, r.DeleteModel(ctx(), "old@1"), errs.CodeNotFound)
		gone, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "old@1"})
		must(t, err)
		if len(gone) != 0 {
			t.Fatalf("hits after delete = %+v", gone)
		}
		_, vector, err := r.HasIndex(ctx(), id.Ref(a.ID), "v1", "old@1")
		must(t, err)
		if vector {
			t.Fatal("the old model's vectors must be gone")
		}
		// The same model can come back with other dimensions after a full delete.
		must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "old@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "again", vec(2, 0))}}))
	})

	t.Run("delete asset", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		r := st.Search()
		a := newAsset(t, st, domain.KindOriginal)
		b := newAsset(t, st, domain.KindOriginal)
		for _, x := range []domain.Asset{a, b} {
			must(t, r.IndexLexical(ctx(), id.Ref(x.ID), "v1", []domain.SearchChunk{{EndSeconds: 1, Text: "word"}}))
			must(t, r.IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(x.ID), Version: "v1", Model: "m@1", Chunks: []domain.EmbeddedChunk{chunk(0, 1, "word", vec(4, 0))}}))
		}
		must(t, r.DeleteAsset(ctx(), id.Ref(a.ID)))
		must(t, r.DeleteAsset(ctx(), id.Ref(a.ID)))
		hits, err := r.SearchLexical(ctx(), repo.LexicalQuery{Text: "word"})
		must(t, err)
		vhits, err := r.SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "m@1"})
		must(t, err)
		if len(hits) != 1 || hits[0].AssetID != b.ID || len(vhits) != 1 || vhits[0].AssetID != b.ID {
			t.Fatalf("hits = %+v %+v", hits, vhits)
		}
		wantCode(t, r.DeleteAsset(ctx(), "ORI-404"), errs.CodeNotFound)
	})
}

func testJobs(t *testing.T, open Factory) {
	t.Run("create, read and snapshot", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		j := newJob(t, st)
		if j.ID != "JOB-001" || j.TenantID != "acme" || j.Status != domain.JobQueued || j.Trigger.Type != "cli" || len(j.Permissions) != 1 {
			t.Fatalf("job = %+v", j)
		}
		// The snapshot keeps its exact bytes, which its hash covers (§6.5).
		if string(j.Snapshot) != `{"id":"ingest-original",  "spaces":   "kept"}` {
			t.Fatalf("snapshot = %s", j.Snapshot)
		}
		for _, ref := range []id.Ref{id.Ref(j.ID), id.Ref(j.ULID)} {
			got, err := st.Jobs().Get(ctx(), ref)
			must(t, err)
			if got.ULID != j.ULID {
				t.Fatalf("job = %+v", got)
			}
		}
		_, err := st.Jobs().Get(ctx(), "ORI-001")
		wantCode(t, err, errs.CodeNotFound)
		_, err = st.Jobs().GetByULID(ctx(), id.New())
		wantCode(t, err, errs.CodeNotFound)
		wantCode(t, st.Jobs().Create(ctx(), domain.Job{ID: "JOB-009", ULID: id.New(), WorkflowID: "x", AssetID: "ORI-404"}), errs.CodeNotFound)
		wantCode(t, st.Jobs().Create(ctx(), domain.Job{ID: "ORI-009", ULID: id.New(), WorkflowID: "x"}), errs.CodeInvalidArgument)
	})

	t.Run("transitions and immutable identity", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		j := newJob(t, st)
		j.Status = domain.JobRunning
		now := fixedTime
		j.StartedAt = &now
		j.ProgressPercent = 12.5
		// None of these may change through Update.
		j.WorkflowID = "other"
		j.Snapshot = json.RawMessage(`{"changed":true}`)
		must(t, st.Jobs().Update(ctx(), j))
		got, err := st.Jobs().Get(ctx(), id.Ref(j.ID))
		must(t, err)
		if got.Status != domain.JobRunning || got.ProgressPercent != 12.5 || got.StartedAt == nil || got.WorkflowID != "ingest-original" || !strings.Contains(string(got.Snapshot), "kept") {
			t.Fatalf("job = %+v", got)
		}
		// Progress in the same state is allowed.
		got.ProgressPercent = 50
		must(t, st.Jobs().Update(ctx(), got))
		got.Status = domain.JobCompleted
		must(t, st.Jobs().Update(ctx(), got))
		got.Status = domain.JobRunning
		wantCode(t, st.Jobs().Update(ctx(), got), errs.CodeFailedPrecondition)
		// D11: needs_attention leaves only through resolve.
		other := newJob(t, st)
		other.Status = domain.JobRunning
		must(t, st.Jobs().Update(ctx(), other))
		other.Status = domain.JobNeedsAttention
		other.Attention = &domain.AttentionInfo{NodeID: "n", NodeType: "publish.upload", Reason: "unknown", OutcomeUnknown: true, Since: fixedTime}
		must(t, st.Jobs().Update(ctx(), other))
		stored, err := st.Jobs().Get(ctx(), id.Ref(other.ID))
		must(t, err)
		if stored.Attention == nil || !stored.Attention.OutcomeUnknown {
			t.Fatalf("attention = %+v", stored.Attention)
		}
		stored.Status = domain.JobCompleted
		wantCode(t, st.Jobs().Update(ctx(), stored), errs.CodeFailedPrecondition)
	})

	t.Run("idempotency key", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		_, found, err := st.Jobs().FindByIdempotencyKey(ctx(), "k1")
		must(t, err)
		if found {
			t.Fatal("no job yet")
		}
		must(t, st.Jobs().Create(ctx(), domain.Job{ID: "JOB-001", ULID: id.New(), WorkflowID: "x", IdempotencyKey: "k1"}))
		got, found, err := st.Jobs().FindByIdempotencyKey(ctx(), "k1")
		must(t, err)
		if !found || got.ID != "JOB-001" {
			t.Fatalf("job = %+v", got)
		}
		wantCode(t, st.Jobs().Create(ctx(), domain.Job{ID: "JOB-002", ULID: id.New(), WorkflowID: "x", IdempotencyKey: "k1"}), errs.CodeConflict)
		// An empty key is not a key.
		must(t, st.Jobs().Create(ctx(), domain.Job{ID: "JOB-003", ULID: id.New(), WorkflowID: "x"}))
		must(t, st.Jobs().Create(ctx(), domain.Job{ID: "JOB-004", ULID: id.New(), WorkflowID: "x"}))
		_, found, err = st.Jobs().FindByIdempotencyKey(ctx(), "")
		must(t, err)
		if found {
			t.Fatal("an empty key must find nothing")
		}
	})

	t.Run("list and active", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindOriginal)
		first := newJob(t, st)
		second := newJob(t, st)
		second.Status = domain.JobRunning
		second.AssetID = a.ID
		must(t, st.Jobs().Update(ctx(), second))
		child := domain.Job{ID: "JOB-003", ULID: id.New(), WorkflowID: "child", Trigger: domain.JobTrigger{Type: "subworkflow", ParentJobID: string(first.ID)}}
		must(t, st.Jobs().Create(ctx(), child))

		items, _, err := st.Jobs().List(ctx(), repo.JobFilter{}, repo.Page{})
		must(t, err)
		if len(items) != 3 || items[0].ID != "JOB-003" || len(items[0].Snapshot) != 0 {
			t.Fatalf("items = %+v", items)
		}
		items, _, err = st.Jobs().List(ctx(), repo.JobFilter{Status: domain.JobRunning}, repo.Page{})
		must(t, err)
		if len(items) != 1 || items[0].ID != second.ID || items[0].AssetID != a.ID {
			t.Fatalf("items = %+v", items)
		}
		items, _, err = st.Jobs().List(ctx(), repo.JobFilter{WorkflowID: "child"}, repo.Page{})
		must(t, err)
		if len(items) != 1 {
			t.Fatalf("items = %+v", items)
		}
		items, _, err = st.Jobs().List(ctx(), repo.JobFilter{AssetID: id.Ref(a.ID)}, repo.Page{})
		must(t, err)
		if len(items) != 1 {
			t.Fatalf("items = %+v", items)
		}
		for _, ref := range []id.Ref{id.Ref(first.ID), id.Ref(first.ULID)} {
			items, _, err = st.Jobs().List(ctx(), repo.JobFilter{ParentJob: ref}, repo.Page{})
			must(t, err)
			if len(items) != 1 || items[0].ID != "JOB-003" {
				t.Fatalf("children = %+v", items)
			}
		}
		_, _, err = st.Jobs().List(ctx(), repo.JobFilter{ParentJob: "JOB-404"}, repo.Page{})
		wantCode(t, err, errs.CodeNotFound)

		second.Status = domain.JobCompleted
		must(t, st.Jobs().Update(ctx(), second))
		active, err := st.Jobs().ListActive(ctx())
		must(t, err)
		if len(active) != 2 || active[0].ID != first.ID || len(active[0].Snapshot) == 0 {
			t.Fatalf("active = %+v", active)
		}
	})

	t.Run("node executions", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		j := newJob(t, st)
		r := st.Jobs()
		_, err := r.GetNode(ctx(), j.ULID, "transcode")
		wantCode(t, err, errs.CodeNotFound)
		lease := fixedTime
		n := domain.NodeExecution{JobULID: j.ULID, NodeID: "transcode", NodeType: "video.transcode", TypeVersion: 1, Status: domain.NodeQueued, Attempt: 1,
			Parameters: json.RawMessage(`{"profile":"auto:master"}`), IdempotencyKey: "ik", TaskID: "task-1", LeaseExpiresAt: &lease}
		must(t, r.PutNode(ctx(), n))
		n.Status = domain.NodeCompleted
		n.Output = json.RawMessage(`{"uri":"scratch://x"}`)
		n.Error = &domain.JobError{Code: "e", Message: "m"}
		must(t, r.PutNode(ctx(), n))
		got, err := r.GetNode(ctx(), j.ULID, "transcode")
		must(t, err)
		if got.Status != domain.NodeCompleted || string(got.Output) != `{"uri":"scratch://x"}` || got.Error == nil || got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(lease) {
			t.Fatalf("node = %+v", got)
		}
		byTask, err := r.GetNodeByTask(ctx(), "task-1")
		must(t, err)
		if byTask.NodeID != "transcode" {
			t.Fatalf("by task = %+v", byTask)
		}
		_, err = r.GetNodeByTask(ctx(), "task-404")
		wantCode(t, err, errs.CodeNotFound)
		_, err = r.GetNodeByTask(ctx(), "")
		wantCode(t, err, errs.CodeNotFound)
		must(t, r.PutNode(ctx(), domain.NodeExecution{JobULID: j.ULID, NodeID: "other", NodeType: "x", Status: domain.NodePending}))
		nodes, err := r.ListNodes(ctx(), j.ULID)
		must(t, err)
		if len(nodes) != 2 || nodes[0].NodeID != "transcode" {
			t.Fatalf("nodes = %+v", nodes)
		}
		wantCode(t, r.PutNode(ctx(), domain.NodeExecution{JobULID: id.New(), NodeID: "x", NodeType: "x", Status: domain.NodePending}), errs.CodeNotFound)
		// I35: the output cap holds at the store.
		big := domain.NodeExecution{JobULID: j.ULID, NodeID: "big", NodeType: "x", Status: domain.NodeCompleted, Output: make(json.RawMessage, domain.MaxNodeOutputBytes+1)}
		wantCode(t, r.PutNode(ctx(), big), errs.CodeOutputTooLarge)
	})

	t.Run("events are sequential per job", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newJob(t, st)
		b := newJob(t, st)
		r := st.Jobs()
		for i := int64(1); i <= 3; i++ {
			seq, err := r.AppendEvent(ctx(), domain.JobEvent{JobID: a.ID, Kind: domain.JobEventLog, Message: fmt.Sprint(i), Data: json.RawMessage(`{"x":1}`)})
			must(t, err)
			if seq != i {
				t.Fatalf("seq = %d, want %d", seq, i)
			}
		}
		seq, err := r.AppendEvent(ctx(), domain.JobEvent{JobID: b.ID, Kind: domain.JobEventState})
		must(t, err)
		if seq != 1 {
			t.Fatalf("seq = %d", seq)
		}
		events, err := r.ListEvents(ctx(), id.Ref(a.ID), 1, 10)
		must(t, err)
		if len(events) != 2 || events[0].Seq != 2 || events[0].Message != "2" || events[0].JobID != a.ID || string(events[0].Data) != `{"x":1}` {
			t.Fatalf("events = %+v", events)
		}
		limited, err := r.ListEvents(ctx(), id.Ref(a.ULID), 0, 1)
		must(t, err)
		if len(limited) != 1 || limited[0].Seq != 1 {
			t.Fatalf("events = %+v", limited)
		}
		_, err = r.AppendEvent(ctx(), domain.JobEvent{JobID: "JOB-404", Kind: domain.JobEventLog})
		wantCode(t, err, errs.CodeNotFound)
		_, err = r.ListEvents(ctx(), "JOB-404", 0, 1)
		wantCode(t, err, errs.CodeNotFound)

		// Concurrent appends never collide.
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 10 {
					if _, err := r.AppendEvent(ctx(), domain.JobEvent{JobID: b.ID, Kind: domain.JobEventLog}); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()
		all, err := r.ListEvents(ctx(), id.Ref(b.ID), 0, 500)
		must(t, err)
		if len(all) != 81 {
			t.Fatalf("events = %d", len(all))
		}
		for i, e := range all {
			if e.Seq != int64(i+1) {
				t.Fatalf("event %d has seq %d", i, e.Seq)
			}
		}
	})

	t.Run("retention keeps provenance and approvals", func(t *testing.T) {
		st := tenantStore(t, open, "acme")
		a := newAsset(t, st, domain.KindOriginal)
		newVersion(t, st, a, "v1")
		must(t, st.Versions().SetProvenance(ctx(), id.Ref(a.ID), "v1", domain.Provenance{JobID: "JOB-001", Workflow: "w@1"}))
		old := newJob(t, st)
		active := newJob(t, st)
		recent := newJob(t, st)
		finish := func(j domain.Job, at time.Time) {
			j.Status = domain.JobRunning
			must(t, st.Jobs().Update(ctx(), j))
			j.Status = domain.JobCompleted
			j.FinishedAt = &at
			must(t, st.Jobs().Update(ctx(), j))
		}
		finish(old, fixedTime.Add(-100*24*time.Hour))
		finish(recent, fixedTime)
		must(t, st.Jobs().PutNode(ctx(), domain.NodeExecution{JobULID: old.ULID, NodeID: "n", NodeType: "x", Status: domain.NodeCompleted, TaskID: "t-old"}))
		_, err := st.Jobs().AppendEvent(ctx(), domain.JobEvent{JobID: old.ID, Kind: domain.JobEventLog})
		must(t, err)
		apr, err := st.IDs().Next(ctx(), id.PrefixApproval)
		must(t, err)
		must(t, st.Approvals().Create(ctx(), domain.Approval{ID: apr, ULID: id.New(), Kind: domain.ApprovalClipSelection, AssetID: a.ID, JobID: old.ID, NodeID: "n"}))

		removed, err := st.Jobs().DeleteFinishedBefore(ctx(), fixedTime.Add(-90*24*time.Hour))
		must(t, err)
		if removed != 1 {
			t.Fatalf("removed = %d", removed)
		}
		_, err = st.Jobs().Get(ctx(), id.Ref(old.ID))
		wantCode(t, err, errs.CodeNotFound)
		_, err = st.Jobs().GetNodeByTask(ctx(), "t-old")
		wantCode(t, err, errs.CodeNotFound)
		if _, err := st.Jobs().Get(ctx(), id.Ref(active.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Jobs().Get(ctx(), id.Ref(recent.ID)); err != nil {
			t.Fatal(err)
		}
		// R21: provenance belongs to the asset. R13: the approval stays.
		ver, err := st.Versions().Get(ctx(), id.Ref(a.ID), "v1")
		must(t, err)
		if ver.Provenance == nil || ver.Provenance.JobID != "JOB-001" {
			t.Fatalf("provenance = %+v", ver.Provenance)
		}
		kept, err := st.Approvals().Get(ctx(), id.Ref(apr))
		must(t, err)
		if kept.JobID != old.ID {
			t.Fatalf("approval = %+v", kept)
		}
		// The friendly ID stays burned after retention.
		next, err := st.IDs().Next(ctx(), id.PrefixJob)
		must(t, err)
		if next == old.ID {
			t.Fatal("a job ID must never be reused")
		}
	})
}

func testApprovals(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	a := newAsset(t, st, domain.KindOriginal)
	j := newJob(t, st)
	r := st.Approvals()
	expires := fixedTime.Add(time.Hour)
	create := func(kind domain.ApprovalKind) domain.Approval {
		friendly, err := st.IDs().Next(ctx(), id.PrefixApproval)
		must(t, err)
		ap := domain.Approval{ID: friendly, ULID: id.New(), Kind: kind, AssetID: a.ID, AssetVersion: "v1", JobID: j.ID, NodeID: "gate", Approvers: []string{"editor"},
			Evidence: json.RawMessage(`{"excerpt":"text"}`), AllowAdjust: true, Segments: []domain.Segment{{SourceAssetID: a.ID, StartSeconds: 1, EndSeconds: 2}}, ExpiresAt: &expires, OnTimeout: domain.OnTimeoutFail}
		must(t, r.Create(ctx(), ap))
		return ap
	}
	first := create(domain.ApprovalClipSelection)
	got, err := r.Get(ctx(), id.Ref(first.ID))
	must(t, err)
	if got.Status != domain.ApprovalPending || got.TenantID != "acme" || got.AssetID != a.ID || got.JobID != j.ID || !got.AllowAdjust ||
		got.Approvers[0] != "editor" || string(got.Evidence) != `{"excerpt":"text"}` || got.Segments[0].EndSeconds != 2 || got.OnTimeout != domain.OnTimeoutFail {
		t.Fatalf("approval = %+v", got)
	}
	byULID, err := r.Get(ctx(), id.Ref(first.ULID))
	must(t, err)
	if byULID.ID != first.ID {
		t.Fatalf("by ULID = %+v", byULID)
	}
	_, err = r.Get(ctx(), "APR-404")
	wantCode(t, err, errs.CodeNotFound)
	wantCode(t, r.Create(ctx(), domain.Approval{ID: "APR-900", ULID: id.New(), Kind: domain.ApprovalOther, JobID: "JOB-404"}), errs.CodeNotFound)
	wantCode(t, r.Create(ctx(), domain.Approval{ID: "APR-901", ULID: id.New(), Kind: domain.ApprovalOther, AssetID: "ORI-404"}), errs.CodeNotFound)
	wantCode(t, r.Create(ctx(), domain.Approval{ID: "JOB-901", ULID: id.New(), Kind: domain.ApprovalOther}), errs.CodeInvalidArgument)

	node, found, err := r.GetForNode(ctx(), j.ULID, "gate")
	must(t, err)
	if !found || node.ID != first.ID {
		t.Fatalf("for node = %+v, %v", node, found)
	}
	_, found, err = r.GetForNode(ctx(), j.ULID, "other")
	must(t, err)
	if found {
		t.Fatal("no approval for that node")
	}

	second := create(domain.ApprovalPublication)
	third := create(domain.ApprovalPublication)
	items, _, err := r.List(ctx(), repo.ApprovalFilter{Kind: domain.ApprovalPublication}, repo.Page{})
	must(t, err)
	if len(items) != 2 || items[0].ID != third.ID {
		t.Fatalf("items = %+v", items)
	}
	items, _, err = r.List(ctx(), repo.ApprovalFilter{AssetID: id.Ref(a.ID), Status: domain.ApprovalPending}, repo.Page{Limit: 1})
	must(t, err)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	_, _, err = r.List(ctx(), repo.ApprovalFilter{AssetID: "ORI-404"}, repo.Page{})
	wantCode(t, err, errs.CodeNotFound)

	expiring, err := r.ExpiringBefore(ctx(), expires)
	must(t, err)
	if len(expiring) != 3 {
		t.Fatalf("expiring = %d", len(expiring))
	}
	none, err := r.ExpiringBefore(ctx(), fixedTime)
	must(t, err)
	if len(none) != 0 {
		t.Fatalf("expiring = %d", len(none))
	}

	// One decision only, and it may adjust the segments.
	adjusted := []domain.Segment{{SourceAssetID: a.ID, StartSeconds: 5, EndSeconds: 6}}
	decided, err := r.Decide(ctx(), id.Ref(first.ID), repo.ApprovalDecision{Status: domain.ApprovalApproved, DecidedBy: "e@example.com", DecidedAt: fixedTime, Segments: adjusted})
	must(t, err)
	if decided.Status != domain.ApprovalApproved || decided.DecidedBy != "e@example.com" || decided.DecidedAt == nil || decided.Segments[0].StartSeconds != 5 {
		t.Fatalf("decided = %+v", decided)
	}
	_, err = r.Decide(ctx(), id.Ref(first.ID), repo.ApprovalDecision{Status: domain.ApprovalRejected, DecidedBy: "x"})
	wantCode(t, err, errs.CodeFailedPrecondition)
	again, _ := r.Get(ctx(), id.Ref(first.ID))
	if again.Status != domain.ApprovalApproved || again.DecidedBy != "e@example.com" {
		t.Fatalf("a second decision must not change the record: %+v", again)
	}
	rejected, err := r.Decide(ctx(), id.Ref(second.ULID), repo.ApprovalDecision{Status: domain.ApprovalRejected, DecidedBy: "p@example.com", Reason: "no"})
	must(t, err)
	if rejected.Reason != "no" || len(rejected.Segments) != 1 || rejected.Segments[0].StartSeconds != 1 {
		t.Fatalf("rejected = %+v", rejected)
	}
	_, err = r.Decide(ctx(), id.Ref(third.ID), repo.ApprovalDecision{Status: domain.ApprovalPending})
	wantCode(t, err, errs.CodeInvalidArgument)
	_, err = r.Decide(ctx(), "APR-404", repo.ApprovalDecision{Status: domain.ApprovalApproved})
	wantCode(t, err, errs.CodeNotFound)

	// Two simultaneous decisions: exactly one wins.
	race := create(domain.ApprovalPublication)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Decide(ctx(), id.Ref(race.ID), repo.ApprovalDecision{Status: domain.ApprovalApproved, DecidedBy: "p"})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errs.IsCode(err, errs.CodeFailedPrecondition) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d decisions won", wins)
	}
}

func testPublications(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	a := newAsset(t, st, domain.KindClip)
	newVersion(t, st, a, "v1")
	r := st.Publications()
	create := func(dest string) domain.Publication {
		friendly, err := st.IDs().Next(ctx(), id.PrefixPublication)
		must(t, err)
		p := domain.Publication{ID: friendly, ULID: id.New(), AssetID: a.ID, Destination: dest, AssetVersion: "v1", IdempotencyKey: "idem-" + dest, Title: "t", ApprovalID: "APR-002"}
		must(t, r.Create(ctx(), p))
		return p
	}
	first := create("mock-main")
	got, err := r.Get(ctx(), id.Ref(first.ID))
	must(t, err)
	if got.Status != domain.PubPendingApproval || got.Visibility != domain.VisibilityPrivate || got.AssetID != a.ID || got.TenantID != "acme" || got.ApprovalID != "APR-002" {
		t.Fatalf("publication = %+v", got)
	}
	_, err = r.Get(ctx(), "PUB-404")
	wantCode(t, err, errs.CodeNotFound)
	wantCode(t, r.Create(ctx(), domain.Publication{ID: "PUB-900", ULID: id.New(), AssetID: "CLIP-404", AssetVersion: "v1"}), errs.CodeNotFound)
	wantCode(t, r.Create(ctx(), domain.Publication{ID: "PUB-901", ULID: id.New(), AssetID: a.ID}), errs.CodeFailedPrecondition)
	wantCode(t, r.Create(ctx(), domain.Publication{ID: "PUB-902", ULID: id.New(), AssetID: a.ID, AssetVersion: "v1", IdempotencyKey: "idem-mock-main"}), errs.CodeConflict)

	found, ok, err := r.FindByIdempotencyKey(ctx(), "idem-mock-main")
	must(t, err)
	if !ok || found.ID != first.ID {
		t.Fatalf("found = %+v, %v", found, ok)
	}
	_, ok, err = r.FindByIdempotencyKey(ctx(), "other")
	must(t, err)
	if ok {
		t.Fatal("no publication with that key")
	}

	// R43: published needs a remote ID and URL.
	got.Status = domain.PubPublished
	got.Visibility = domain.VisibilityPublic
	wantCode(t, r.Update(ctx(), got), errs.CodeFailedPrecondition)
	got.RemoteID = "r-1"
	wantCode(t, r.Update(ctx(), got), errs.CodeFailedPrecondition)
	before, err := st.Versions().Get(ctx(), id.Ref(a.ID), "v1")
	must(t, err)
	if before.Immutable {
		t.Fatal("a version is not immutable before publication")
	}
	got.URL = "https://media.example.internal/watch/r-1"
	now := fixedTime
	got.PublishedAt = &now
	must(t, r.Update(ctx(), got))

	// D35: the first confirmed publication makes the version immutable.
	after, err := st.Versions().Get(ctx(), id.Ref(a.ID), "v1")
	must(t, err)
	if !after.Immutable {
		t.Fatal("a published version must be immutable")
	}
	wantCode(t, st.Versions().PutFile(ctx(), id.Ref(a.ID), "v1", domain.VersionFile{Purpose: domain.FileProxy, File: domain.MediaFile{Key: "x"}}), errs.CodeFailedPrecondition)

	public, err := r.ListPublic(ctx())
	must(t, err)
	if len(public) != 1 || public[0].ID != first.ID || public[0].URL == "" || public[0].PublishedAt == nil {
		t.Fatalf("public = %+v", public)
	}
	private := create("other")
	private.Status = domain.PubProcessing
	must(t, r.Update(ctx(), private))
	items, _, err := r.List(ctx(), repo.PublicationFilter{AssetID: id.Ref(a.ID)}, repo.Page{})
	must(t, err)
	if len(items) != 2 || items[0].ID != private.ID {
		t.Fatalf("items = %+v", items)
	}
	items, _, err = r.List(ctx(), repo.PublicationFilter{Destination: "mock-main", Status: domain.PubPublished}, repo.Page{})
	must(t, err)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	_, _, err = r.List(ctx(), repo.PublicationFilter{AssetID: "CLIP-404"}, repo.Page{})
	wantCode(t, err, errs.CodeNotFound)
	missing := got
	missing.ULID = id.New()
	missing.ID = "PUB-404"
	wantCode(t, r.Update(ctx(), missing), errs.CodeNotFound)

	// Withdrawal: gone from the public source.
	got.Status = domain.PubWithdrawn
	got.WithdrawnAt = &now
	must(t, r.Update(ctx(), got))
	public, err = r.ListPublic(ctx())
	must(t, err)
	if len(public) != 0 {
		t.Fatalf("public = %+v", public)
	}
}

func testUploads(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	r := st.Uploads()
	friendly, err := st.IDs().Next(ctx(), id.PrefixUpload)
	must(t, err)
	u := domain.Upload{ID: friendly, ULID: id.New(), Filename: "class.mp4", SizeBytes: 1000, SHA256: "aa", ChunkBytes: 100, CreatedBy: "a@example.com", ExpiresAt: fixedTime.Add(time.Hour)}
	must(t, r.Create(ctx(), u))
	wantCode(t, r.Create(ctx(), u), errs.CodeConflict)
	wantCode(t, r.Create(ctx(), domain.Upload{ID: "JOB-001", ULID: id.New()}), errs.CodeInvalidArgument)
	got, err := r.Get(ctx(), id.Ref(friendly))
	must(t, err)
	if got.Status != domain.UploadPending || got.TenantID != "acme" || got.Filename != "class.mp4" {
		t.Fatalf("upload = %+v", got)
	}
	got.ReceivedBytes = 500
	must(t, r.Update(ctx(), got))
	back, err := r.Get(ctx(), id.Ref(u.ULID))
	must(t, err)
	if back.ReceivedBytes != 500 {
		t.Fatalf("upload = %+v", back)
	}
	expired, err := r.ListExpired(ctx(), fixedTime.Add(2*time.Hour))
	must(t, err)
	if len(expired) != 1 {
		t.Fatalf("expired = %+v", expired)
	}
	none, err := r.ListExpired(ctx(), fixedTime)
	must(t, err)
	if len(none) != 0 {
		t.Fatalf("expired = %+v", none)
	}
	back.Status = domain.UploadComplete
	must(t, r.Update(ctx(), back))
	done, err := r.ListExpired(ctx(), fixedTime.Add(2*time.Hour))
	must(t, err)
	if len(done) != 0 {
		t.Fatalf("a completed upload must not expire: %+v", done)
	}
	must(t, r.Delete(ctx(), id.Ref(friendly)))
	wantCode(t, r.Delete(ctx(), id.Ref(friendly)), errs.CodeNotFound)
	_, err = r.Get(ctx(), id.Ref(friendly))
	wantCode(t, err, errs.CodeNotFound)
	missing := back
	wantCode(t, r.Update(ctx(), missing), errs.CodeNotFound)
}

// runAuditChain checks the append and read contract on one chain. Verify lives in
// package audit; here each link is checked by hand.
func runAuditChain(t *testing.T, r repo.AuditRepo, tenant domain.TenantID) {
	t.Helper()
	first, err := r.Append(ctx(), domain.AuditRecord{TenantID: tenant, Actor: "a@example.com", Method: domain.AuthDevice, Action: "asset.update", Permission: domain.PermAssetWrite, Resource: "ORI-001", Result: "allowed", RequestID: "r1", Details: json.RawMessage(`{"z":1,"a":"<&>"}`), At: fixedTime})
	must(t, err)
	if first.Seq != 1 || first.PrevHash != domain.AuditGenesisHash || first.Hash == "" || first.ULID == "" {
		t.Fatalf("first = %+v", first)
	}
	second, err := r.Append(ctx(), domain.AuditRecord{Actor: "b@example.com", Action: "secret.used", Result: "denied"})
	must(t, err)
	if second.Seq != 2 || second.PrevHash != first.Hash || second.Hash == first.Hash {
		t.Fatalf("second = %+v", second)
	}
	if _, err := r.Append(ctx(), domain.AuditRecord{Actor: "a", Result: "allowed"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no action: %v", err)
	}
	if _, err := r.Append(ctx(), domain.AuditRecord{Actor: "a", Action: "x"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no result: %v", err)
	}
	if _, err := r.Append(ctx(), domain.AuditRecord{Actor: "a", Action: "x", Result: "allowed", Details: json.RawMessage(`{bad`)}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad details: %v", err)
	}
	if _, err := r.Append(ctx(), domain.AuditRecord{TenantID: "someone-else", Actor: "a", Action: "x", Result: "allowed"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("other tenant: %v", err)
	}

	// Stored records hash to what was returned, in every field.
	scanned, err := r.Scan(ctx(), 0, 10)
	must(t, err)
	if len(scanned) != 2 {
		t.Fatalf("scanned = %+v", scanned)
	}
	for i, rec := range scanned {
		want, err := rec.ComputeHash()
		must(t, err)
		if want != rec.Hash {
			t.Fatalf("record %d does not hash to its stored value", i)
		}
		if rec.TenantID != tenant {
			t.Fatalf("record tenant = %q", rec.TenantID)
		}
	}
	if scanned[0].Hash != first.Hash || !scanned[0].At.Equal(fixedTime) || scanned[0].Permission != domain.PermAssetWrite {
		t.Fatalf("scanned = %+v", scanned[0])
	}
	after, err := r.Scan(ctx(), 1, 10)
	must(t, err)
	if len(after) != 1 || after[0].Seq != 2 {
		t.Fatalf("after = %+v", after)
	}

	items, _, err := r.List(ctx(), repo.AuditFilter{}, repo.Page{})
	must(t, err)
	if len(items) != 2 || items[0].Seq != 2 {
		t.Fatalf("items = %+v", items)
	}
	since := fixedTime.Add(time.Hour)
	items, _, err = r.List(ctx(), repo.AuditFilter{Since: &since}, repo.Page{})
	must(t, err)
	if len(items) != 1 || items[0].Seq != 2 {
		t.Fatalf("since = %+v", items)
	}
	items, _, err = r.List(ctx(), repo.AuditFilter{Actor: "a@example.com"}, repo.Page{})
	must(t, err)
	if len(items) != 1 || items[0].Seq != 1 {
		t.Fatalf("actor = %+v", items)
	}
	items, _, err = r.List(ctx(), repo.AuditFilter{Action: "secret.used"}, repo.Page{})
	must(t, err)
	if len(items) != 1 {
		t.Fatalf("action = %+v", items)
	}
	items, _, err = r.List(ctx(), repo.AuditFilter{Resource: "ORI-001"}, repo.Page{Limit: 1})
	must(t, err)
	if len(items) != 1 {
		t.Fatalf("resource = %+v", items)
	}

	// Concurrent appends keep one straight chain.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := r.Append(ctx(), domain.AuditRecord{Actor: "c", Action: "x", Result: "allowed"}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	all, err := r.Scan(ctx(), 0, 500)
	must(t, err)
	if len(all) != 82 {
		t.Fatalf("records = %d", len(all))
	}
	for i := range all {
		if all[i].Seq != int64(i+1) {
			t.Fatalf("record %d has seq %d", i, all[i].Seq)
		}
		if i > 0 && all[i].PrevHash != all[i-1].Hash {
			t.Fatalf("the chain forks at %d", i)
		}
	}

	m, ok := r.(repo.AuditMaintainer)
	if !ok {
		return
	}
	anchor, err := m.Anchor(ctx())
	must(t, err)
	if anchor.Seq != 0 {
		t.Fatalf("anchor = %+v", anchor)
	}
	_, found, err := m.LastExport(ctx())
	must(t, err)
	if found {
		t.Fatal("no export yet")
	}
	// Nothing exported, nothing pruned, however old.
	removed, err := m.PruneExportedBefore(ctx(), fixedTime.Add(100*365*24*time.Hour))
	must(t, err)
	if removed != 0 {
		t.Fatalf("pruned %d unexported records", removed)
	}
	must(t, m.RecordExport(ctx(), repo.AuditExport{FromSeq: 1, ToSeq: 2, HeadHash: second.Hash, Key: "audit/x.jsonl", SHA256: "aa"}))
	last, found, err := m.LastExport(ctx())
	must(t, err)
	if !found || last.ToSeq != 2 || last.HeadHash != second.Hash || last.Key != "audit/x.jsonl" {
		t.Fatalf("last = %+v", last)
	}
	exports, err := m.ListExports(ctx())
	must(t, err)
	if len(exports) != 1 {
		t.Fatalf("exports = %+v", exports)
	}
	// Only the exported prefix goes, and only what is old enough: record 1 is at
	// fixedTime, record 2 is at the wall clock's now.
	removed, err = m.PruneExportedBefore(ctx(), fixedTime.Add(time.Second))
	must(t, err)
	if removed != 1 {
		t.Fatalf("removed = %d", removed)
	}
	anchor, err = m.Anchor(ctx())
	must(t, err)
	if anchor.Seq != 1 || anchor.Hash != first.Hash {
		t.Fatalf("anchor = %+v", anchor)
	}
	rest, err := r.Scan(ctx(), 0, 500)
	must(t, err)
	if len(rest) != 81 || rest[0].Seq != 2 || rest[0].PrevHash != anchor.Hash {
		t.Fatalf("rest = %d, first = %+v", len(rest), rest[0])
	}
	// The chain continues after a prune.
	next, err := r.Append(ctx(), domain.AuditRecord{Actor: "c", Action: "x", Result: "allowed"})
	must(t, err)
	if next.Seq != 83 {
		t.Fatalf("next = %+v", next)
	}
}

func testOutbox(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	r := st.Outbox()
	seq1, err := r.Append(ctx(), event.Envelope{Type: event.AssetCreated, AssetID: "ORI-001", JobID: "JOB-001", Payload: json.RawMessage(`{"kind":"original"}`)})
	must(t, err)
	seq2, err := r.Append(ctx(), event.Envelope{Type: event.ClipCreated, EventID: id.New(), OccurredAt: fixedTime, TenantID: "acme", CausationID: "x", CausationDepth: 2})
	must(t, err)
	if seq2 <= seq1 {
		t.Fatalf("seq = %d, %d", seq1, seq2)
	}
	_, err = r.Append(ctx(), event.Envelope{})
	wantCode(t, err, errs.CodeInvalidArgument)
	_, err = r.Append(ctx(), event.Envelope{Type: event.AssetCreated, TenantID: "beta"})
	wantCode(t, err, errs.CodeInvalidArgument)

	pending, err := r.Pending(ctx(), 10)
	must(t, err)
	if len(pending) != 2 || pending[0].Seq != seq1 || pending[0].Event.TenantID != "acme" || pending[0].Event.EventID == "" || pending[0].Event.SchemaVersion != event.SchemaVersion || string(pending[0].Event.Payload) != `{"kind":"original"}` {
		t.Fatalf("pending = %+v", pending)
	}
	must(t, r.MarkDispatched(ctx(), seq1))
	must(t, r.MarkDispatched(ctx(), seq1))
	wantCode(t, r.MarkDispatched(ctx(), 9999), errs.CodeNotFound)
	pending, err = r.Pending(ctx(), 10)
	must(t, err)
	if len(pending) != 1 || pending[0].Seq != seq2 {
		t.Fatalf("pending = %+v", pending)
	}

	got, err := r.GetEvent(ctx(), pending[0].Event.EventID)
	must(t, err)
	if got.CausationDepth != 2 || got.Type != event.ClipCreated {
		t.Fatalf("event = %+v", got)
	}
	_, err = r.GetEvent(ctx(), id.New())
	wantCode(t, err, errs.CodeNotFound)

	stream, err := r.Stream(ctx(), seq1, 10)
	must(t, err)
	if len(stream) != 1 || stream[0].Seq != seq2 {
		t.Fatalf("stream = %+v", stream)
	}
	events, _, err := r.ListEvents(ctx(), repo.OutboxFilter{}, repo.Page{})
	must(t, err)
	if len(events) != 2 || events[0].Type != event.ClipCreated {
		t.Fatalf("events = %+v", events)
	}
	events, _, err = r.ListEvents(ctx(), repo.OutboxFilter{Type: event.AssetCreated}, repo.Page{})
	must(t, err)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	since := fixedTime.Add(time.Hour)
	events, _, err = r.ListEvents(ctx(), repo.OutboxFilter{Since: &since}, repo.Page{Limit: 1})
	must(t, err)
	if len(events) != 1 || events[0].Type != event.AssetCreated {
		t.Fatalf("since = %+v", events)
	}

	// Deliveries are upserted per subscriber and come due by time.
	evID := got.EventID
	later := fixedTime.Add(time.Hour)
	must(t, r.PutDelivery(ctx(), event.Delivery{EventID: evID, Subscriber: "workflow:a", Status: event.DeliveryPending}))
	must(t, r.PutDelivery(ctx(), event.Delivery{EventID: evID, Subscriber: "plugin:b", Status: event.DeliveryFailed, Attempts: 1, LastError: "boom", NextAttempt: &later}))
	must(t, r.PutDelivery(ctx(), event.Delivery{EventID: evID, Subscriber: "plugin:c", Status: event.DeliveryFailed, Attempts: 5}))
	must(t, r.PutDelivery(ctx(), event.Delivery{EventID: evID, Subscriber: "plugin:d", Status: event.DeliveryDelivered, DeliveredAt: &later}))
	due, err := r.DueDeliveries(ctx(), fixedTime, 10)
	must(t, err)
	if len(due) != 1 || due[0].Subscriber != "workflow:a" {
		t.Fatalf("due = %+v", due)
	}
	due, err = r.DueDeliveries(ctx(), later, 10)
	must(t, err)
	if len(due) != 2 {
		t.Fatalf("due = %+v", due)
	}
	must(t, r.PutDelivery(ctx(), event.Delivery{EventID: evID, Subscriber: "workflow:a", Status: event.DeliveryDelivered, Attempts: 1, DeliveredAt: &later}))
	deliveries, _, err := r.ListDeliveries(ctx(), repo.DeliveryFilter{Status: event.DeliveryFailed}, repo.Page{})
	must(t, err)
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	deliveries, _, err = r.ListDeliveries(ctx(), repo.DeliveryFilter{Subscriber: "workflow:a"}, repo.Page{})
	must(t, err)
	if len(deliveries) != 1 || deliveries[0].Status != event.DeliveryDelivered || deliveries[0].DeliveredAt == nil {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	wantCode(t, r.PutDelivery(ctx(), event.Delivery{Subscriber: "x"}), errs.CodeInvalidArgument)

	if m, ok := r.(repo.OutboxMaintainer); ok {
		must(t, r.MarkDispatched(ctx(), seq2))
		removed, err := m.DeleteDispatchedBefore(ctx(), fixedTime.Add(time.Second))
		must(t, err)
		if removed != 1 {
			t.Fatalf("removed = %d", removed)
		}
		deliveries, _, err = r.ListDeliveries(ctx(), repo.DeliveryFilter{}, repo.Page{})
		must(t, err)
		if len(deliveries) != 0 {
			t.Fatalf("deliveries of a removed event must go: %+v", deliveries)
		}
		// Sequences keep growing after a removal, so SSE resume stays correct.
		seq3, err := r.Append(ctx(), event.Envelope{Type: event.AssetCreated})
		must(t, err)
		if seq3 <= seq2 {
			t.Fatalf("seq = %d after %d", seq3, seq2)
		}
	}
}

func testSecrets(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	r := st.Secrets()
	_, err := r.Get(ctx(), "gcs-main")
	wantCode(t, err, errs.CodeNotFound)
	rec := repo.SecretRecord{Meta: domain.SecretMeta{Name: "gcs-main", Location: domain.SecretServer, CreatedBy: "a@example.com", CreatedAt: fixedTime}, Ciphertext: []byte{1, 2, 0, 3}, KeyVersion: 1}
	must(t, r.Put(ctx(), rec))
	wantCode(t, r.Put(ctx(), repo.SecretRecord{Meta: domain.SecretMeta{Name: "x"}}), errs.CodeInvalidArgument)
	wantCode(t, r.Put(ctx(), repo.SecretRecord{Meta: domain.SecretMeta{Name: "x", TenantID: "beta"}, Ciphertext: []byte{1}, KeyVersion: 1}), errs.CodeInvalidArgument)
	got, err := r.Get(ctx(), "gcs-main")
	must(t, err)
	if string(got.Ciphertext) != string([]byte{1, 2, 0, 3}) || got.KeyVersion != 1 || got.Meta.TenantID != "acme" || got.Meta.Location != domain.SecretServer {
		t.Fatalf("secret = %+v", got)
	}
	rec.Ciphertext = []byte{9}
	rec.KeyVersion = 2
	must(t, r.Put(ctx(), rec))
	got, _ = r.Get(ctx(), "gcs-main")
	if got.KeyVersion != 2 || got.Ciphertext[0] != 9 {
		t.Fatalf("secret = %+v", got)
	}
	must(t, r.TouchUsed(ctx(), "gcs-main", fixedTime))
	wantCode(t, r.TouchUsed(ctx(), "missing", fixedTime), errs.CodeNotFound)
	list, err := r.List(ctx())
	must(t, err)
	if len(list) != 1 || list[0].LastUsedAt == nil || !list[0].LastUsedAt.Equal(fixedTime) {
		t.Fatalf("list = %+v", list)
	}
	must(t, r.Delete(ctx(), "gcs-main"))
	wantCode(t, r.Delete(ctx(), "gcs-main"), errs.CodeNotFound)
}

func testBudgets(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	r := st.Budgets()
	_, found, err := r.Get(ctx())
	must(t, err)
	if found {
		t.Fatal("no budget yet")
	}
	until, err := r.Override(ctx())
	must(t, err)
	if until != nil {
		t.Fatalf("override = %v", until)
	}
	must(t, r.Set(ctx(), domain.Budget{MonthlyMinor: 50000, Currency: "USD"}))
	got, found, err := r.Get(ctx())
	must(t, err)
	if !found || got.MonthlyMinor != 50000 || got.Currency != "USD" || got.TenantID != "acme" {
		t.Fatalf("budget = %+v", got)
	}
	wantCode(t, r.Set(ctx(), domain.Budget{MonthlyMinor: -1}), errs.CodeInvalidArgument)

	nov := time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC)
	dec := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	must(t, r.RecordUsage(ctx(), domain.UsageRecord{Operation: "ai.external", Provider: "x", Quantity: 1.5, Unit: "minute", EstimatedMinor: 100, JobID: "JOB-001", NodeID: "n", At: nov}))
	must(t, r.RecordUsage(ctx(), domain.UsageRecord{Operation: "storage.egress", EstimatedMinor: 50, At: nov}))
	must(t, r.RecordUsage(ctx(), domain.UsageRecord{Operation: "ai.external", EstimatedMinor: 7, At: dec}))
	wantCode(t, r.RecordUsage(ctx(), domain.UsageRecord{EstimatedMinor: 7}), errs.CodeInvalidArgument)
	wantCode(t, r.RecordUsage(ctx(), domain.UsageRecord{TenantID: "beta", Operation: "x"}), errs.CodeInvalidArgument)
	spent, err := r.SpentMinor(ctx(), "2026-11")
	must(t, err)
	if spent != 150 {
		t.Fatalf("spent = %d", spent)
	}
	spent, err = r.SpentMinor(ctx(), "2027-01")
	must(t, err)
	if spent != 0 {
		t.Fatalf("spent = %d", spent)
	}
	usage, _, err := r.ListUsage(ctx(), "2026-11", repo.Page{Limit: 1})
	must(t, err)
	if len(usage) != 1 || usage[0].Operation != "storage.egress" {
		t.Fatalf("usage = %+v", usage)
	}

	must(t, r.SetOverride(ctx(), fixedTime, "admin", "release"))
	until, err = r.Override(ctx())
	must(t, err)
	if until == nil || !until.Equal(fixedTime) {
		t.Fatalf("override = %v", until)
	}
	// An override does not disturb the budget, and vice versa.
	got, _, err = r.Get(ctx())
	must(t, err)
	if got.MonthlyMinor != 50000 {
		t.Fatalf("budget = %+v", got)
	}
	must(t, r.Set(ctx(), domain.Budget{MonthlyMinor: 1, Currency: "EUR"}))
	until, err = r.Override(ctx())
	must(t, err)
	if until == nil {
		t.Fatal("setting a budget must keep the override")
	}
}

func testIdempotency(t *testing.T, open Factory) {
	st := tenantStore(t, open, "acme")
	r := st.Idempotency()
	_, found, err := r.Get(ctx(), "k")
	must(t, err)
	if found {
		t.Fatal("no record yet")
	}
	must(t, r.Put(ctx(), repo.IdempotentResponse{Key: "k", RequestHash: "h", Status: 201, Body: json.RawMessage(`{"id":"ORI-001"}`), CreatedAt: fixedTime}))
	must(t, r.Put(ctx(), repo.IdempotentResponse{Key: "k", RequestHash: "h", Status: 500, Body: json.RawMessage(`{}`)}))
	got, found, err := r.Get(ctx(), "k")
	must(t, err)
	if !found || got.Status != 201 || string(got.Body) != `{"id":"ORI-001"}` || !got.CreatedAt.Equal(fixedTime) {
		t.Fatalf("record = %+v", got)
	}
	wantCode(t, r.Put(ctx(), repo.IdempotentResponse{Key: "k", RequestHash: "other", Status: 200}), errs.CodeIdempotencyMismatch)
	wantCode(t, r.Put(ctx(), repo.IdempotentResponse{RequestHash: "h"}), errs.CodeInvalidArgument)
	must(t, r.Put(ctx(), repo.IdempotentResponse{Key: "new", RequestHash: "h", Status: 200}))
	removed, err := r.DeleteBefore(ctx(), fixedTime.Add(time.Second))
	must(t, err)
	if removed != 1 {
		t.Fatalf("removed = %d", removed)
	}
	_, found, err = r.Get(ctx(), "k")
	must(t, err)
	if found {
		t.Fatal("the old record must be gone")
	}
}

// testIsolation is the repository half of §15.6: IDs of one tenant mean nothing in
// another, and sequences are independent (D46).
func testIsolation(t *testing.T, open Factory) {
	_, stores := open(t)
	acme, err := stores.Create(ctx(), "acme")
	must(t, err)
	beta, err := stores.Create(ctx(), "beta")
	must(t, err)

	a := newAsset(t, acme, domain.KindOriginal)
	newVersion(t, acme, a, "v1")
	job := newJob(t, acme)
	apr := domain.Approval{ID: "APR-001", ULID: id.New(), Kind: domain.ApprovalOther, Approvers: []string{"admin"}}
	must(t, acme.Approvals().Create(ctx(), apr))
	pub := domain.Publication{ID: "PUB-001", ULID: id.New(), AssetID: a.ID, AssetVersion: "v1"}
	must(t, acme.Publications().Create(ctx(), pub))
	up := domain.Upload{ID: "UPL-001", ULID: id.New(), ExpiresAt: fixedTime}
	must(t, acme.Uploads().Create(ctx(), up))
	must(t, acme.Transcripts().Save(ctx(), domain.Transcript{AssetID: a.ID, Version: "v1"}))
	must(t, acme.Search().IndexLexical(ctx(), id.Ref(a.ID), "v1", []domain.SearchChunk{{EndSeconds: 1, Text: "private word"}}))
	must(t, acme.Search().IndexVectors(ctx(), repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: "v1", Model: "m@1", Chunks: []domain.EmbeddedChunk{{SearchChunk: domain.SearchChunk{EndSeconds: 1, Text: "private word"}, Vector: vec(4, 0)}}}))
	must(t, acme.Secrets().Put(ctx(), repo.SecretRecord{Meta: domain.SecretMeta{Name: "gcs", Location: domain.SecretServer}, Ciphertext: []byte{1}, KeyVersion: 1}))
	must(t, acme.Budgets().Set(ctx(), domain.Budget{MonthlyMinor: 5, Currency: "USD"}))
	_, err = acme.Audit().Append(ctx(), domain.AuditRecord{Actor: "a", Action: "x", Result: "allowed"})
	must(t, err)
	_, err = acme.Outbox().Append(ctx(), event.Envelope{Type: event.AssetCreated})
	must(t, err)

	// IDs do not resolve across tenants, friendly or ULID.
	for _, ref := range []id.Ref{id.Ref(a.ID), id.Ref(a.ULID)} {
		_, err := beta.Assets().Get(ctx(), ref)
		wantCode(t, err, errs.CodeNotFound)
		_, err = beta.Versions().Get(ctx(), ref, "v1")
		wantCode(t, err, errs.CodeNotFound)
		_, err = beta.Transcripts().Get(ctx(), ref, "v1")
		wantCode(t, err, errs.CodeNotFound)
		_, err = beta.Search().SearchLexical(ctx(), repo.LexicalQuery{Text: "private", AssetID: ref})
		wantCode(t, err, errs.CodeNotFound)
	}
	for _, ref := range []id.Ref{id.Ref(job.ID), id.Ref(job.ULID)} {
		_, err := beta.Jobs().Get(ctx(), ref)
		wantCode(t, err, errs.CodeNotFound)
	}
	_, err = beta.Jobs().GetByULID(ctx(), job.ULID)
	wantCode(t, err, errs.CodeNotFound)
	_, err = beta.Approvals().Get(ctx(), id.Ref(apr.ULID))
	wantCode(t, err, errs.CodeNotFound)
	_, err = beta.Publications().Get(ctx(), id.Ref(pub.ULID))
	wantCode(t, err, errs.CodeNotFound)
	_, err = beta.Uploads().Get(ctx(), id.Ref(up.ULID))
	wantCode(t, err, errs.CodeNotFound)
	_, err = beta.Secrets().Get(ctx(), "gcs")
	wantCode(t, err, errs.CodeNotFound)

	// Tenant-wide searches see only their own tenant.
	hits, err := beta.Search().SearchLexical(ctx(), repo.LexicalQuery{Text: "private"})
	must(t, err)
	vhits, err := beta.Search().SearchVector(ctx(), repo.VectorQuery{Vector: vec(4, 0), Model: "m@1"})
	must(t, err)
	if len(hits) != 0 || len(vhits) != 0 {
		t.Fatalf("cross-tenant hits: %+v %+v", hits, vhits)
	}

	// Lists are empty, and budget and audit are separate.
	assets, _, err := beta.Assets().List(ctx(), repo.AssetFilter{IncludeDeleted: true}, repo.Page{})
	must(t, err)
	jobs, _, err := beta.Jobs().List(ctx(), repo.JobFilter{}, repo.Page{})
	must(t, err)
	audit, err := beta.Audit().Scan(ctx(), 0, 10)
	must(t, err)
	events, err := beta.Outbox().Stream(ctx(), 0, 10)
	must(t, err)
	_, hasBudget, err := beta.Budgets().Get(ctx())
	must(t, err)
	if len(assets) != 0 || len(jobs) != 0 || len(audit) != 0 || len(events) != 0 || hasBudget {
		t.Fatalf("cross-tenant data: %d assets, %d jobs, %d audit, %d events, budget %v", len(assets), len(jobs), len(audit), len(events), hasBudget)
	}

	// A record cannot be written to another tenant's database by naming it.
	wantCode(t, beta.Assets().Create(ctx(), domain.Asset{ID: "ORI-001", ULID: id.New(), Kind: domain.KindOriginal, Title: "x", TenantID: "acme"}), errs.CodeInvalidArgument)
	wantCode(t, beta.Jobs().Create(ctx(), domain.Job{ID: "JOB-001", ULID: id.New(), WorkflowID: "x", TenantID: "acme"}), errs.CodeInvalidArgument)

	// Sequences are per tenant (D46): beta's first ID is its own 001.
	next, err := beta.IDs().Next(ctx(), id.PrefixOriginal)
	must(t, err)
	if next != "ORI-001" {
		t.Fatalf("next = %s", next)
	}
	// And a reference inside beta to acme's asset fails: beta has no such asset yet.
	wantCode(t, beta.Jobs().Create(ctx(), domain.Job{ID: "JOB-002", ULID: id.New(), WorkflowID: "x", AssetID: a.ID}), errs.CodeNotFound)
}
