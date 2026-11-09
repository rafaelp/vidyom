# Plan: component `core`

Owner of `internal/persistence/**` (including `sqlitedriver`), `internal/tenants/**`, `internal/catalog/**` and `internal/security/**`. Normative inputs: `spec/vidyom-specification-v1.2.md`, `docs/plans/00-master-plan.md` (sections 3, 5–10), `docs/decisions.md`, `internal/contract/**`.

Status vocabulary: everything below is a **target** until its commit lands. Decisions taken while reviewing this plan are in [`docs/decisions/core.md`](../decisions/core.md) as `I100–I119`.

## 1. Deliverables

| Package | Entry point (master plan §9) | Implements |
|---|---|---|
| `internal/persistence` | `persistence.Open(ctx, persistence.Options) (*persistence.Stores, error)` | `repo.SystemStore`, `repo.TenantStores` and every repository of `internal/contract/repo` |
| `internal/persistence/sqlitedriver` | `sqlitedriver.Open(path, Options)` | present from foundation; gains error classification helpers |
| `internal/persistence/repotest` | `repotest.System`, `repotest.Tenant` | conformance suite, run against the real driver, reusable by a future PostgreSQL backend |
| `internal/security/keyring` | `keyring.New() security.Keyring` | client OS keyring, no plaintext fallback |
| `internal/security/vault` | `vault.New(system, tenants, key, opts...)`, `vault.ProviderFor(config.Unseal)` | `security.Vault`, `security.DerivedKeys`, `service.Secrets`, `connector.SecretResolver` per tenant, all `security.MasterKeyProvider`s of I10 |
| `internal/security/audit` | `audit.New(...)` | `security.Auditor`, `service.Audit` |
| `internal/security/authentication` | `authentication.New(...)` | `security.Authenticator`, `security.Pairing`, `service.Devices` |
| `internal/security/authorization` | `authorization.New(...)` | `security.Authorizer` |
| `internal/tenants` | `tenants.New(...)` | `service.Tenants`, `service.Users`, `service.Budgets`, `config.TenantSource`, bootstrap for `vidyomd init`, retention sweeps (I45) |
| `internal/catalog` | `catalog.New(...)` | `service.Assets` |

Import rules: `keyring` stays cgo-free and database-free because `cli` imports it. `vault`, `audit`, `authentication`, `authorization`, `tenants` and `catalog` depend on `internal/contract/**` only, so they can be tested with the real store from `_test.go` files without a compile-time dependency on SQLite in non-test code.

## 2. Phases

### Phase 1: persistence

1. `sqlitedriver`: add `IsConstraint`, `IsBusy`, `IsForeignKey` so nothing else imports the driver.
2. Migrator: embedded numbered SQL per database kind, applied in one immediate transaction, tracked in `schema_migrations` with checksum and `PRAGMA user_version`. Refuses a database newer than the binary and a changed migration.
3. `system.db` schema: tenants, users, memberships, sequences, devices, pairing codes, plugins, wrapped keys, worker tokens, workers, system audit.
4. `tenant.db` schema: sequences, assets, asset history, lineage segments, versions and files, collections, transcripts, analysis, sensitive segments, search chunks with FTS5, embedding models and per-model `vec0` tables, jobs, node executions, job events, approvals, publications, uploads, audit log with triggers, outbox, deliveries, secrets, budgets, usage, idempotency.
5. Store layer: `querier`-based repositories usable inside and outside `InTx`; a per-database writer gate on top of `BEGIN IMMEDIATE`; multi-statement reads in a deferred transaction; a single error mapper to `errs`.
6. Asset denormalization on read: files of `CurrentVersion`, transcript info, publications, provenance.
7. Friendly ID allocation with `INSERT … ON CONFLICT … RETURNING`.
8. Search: FTS5 with sanitized queries, per-model vectors, side-by-side reindexing, tenant-wide scan cap (I43).
9. `repotest`: every method of every repository interface, friendly and ULID refs, transitions, cross-tenant negatives, concurrency.

Landed first: `internal/persistence/...` because `api`, `engine`, `ai`, `publishing` and `integration` replace fakes with it.

### Phase 2: keyring

`keyring.New()` over `zalando/go-keyring`. Maps "not found" to `security.ErrKeyringNotFound`, anything else to `security.ErrKeyringUnavailable` with the OS reason appended. No file fallback exists in the code. `make check-static` must stay green.

### Phase 3: vault

Providers per I10 (`systemd-creds`, `macos-keychain`, `dpapi`, `gcp-kms` not implemented, `file`, `env`), no default, no fall-through. Envelope encryption: master key wraps per-tenant data keys (AES-256-GCM with AAD binding tenant and version) in `system.db`; secrets are sealed with a key derived by HKDF from the data key (AES-256-GCM with AAD binding tenant, name and key version) in `tenant.db`. `RotateDataKey`, `RewrapAll`, `CacheKey` (I39), `PublicIDKey` (I29). `Close` wipes the master key; everything then answers `errs.CodeSealed`.

### Phase 4: audit (D23, I8)

Hash chain computed by a shared `domain.AuditRecord.ComputeHash`. Append-only enforced by triggers, verify recomputes the chain and checks export anchors, export writes JSONL to the `archive` role under `audit/`, prune only removes exported records past retention (minimum 365 days) and leaves an anchor. Secret-like detail keys are refused from the record. A system chain in `system.db` covers actions with no tenant (pairing, tenant creation, boot).

### Phase 5: authentication and authorization (D12, D48, R19, I17)

`vdm_<ulid>_<secret>` credentials, SHA-256 of a 256-bit secret, one-time pairing codes stored as hashes, rotation, revocation, last-seen with throttling, Unix peer identity through `localUsers`, worker tokens. Authorization: membership resolution with `404` across tenants, role bundles plus grants, `system.*` only for system admins.

### Phase 6: tenants, users, budgets, configuration

Tenant creation (database, system row, data key, creator membership) with crash-safe ordering. Users and grants. Budgets per D19 with crossing detection, events in the outbox, audited override. `TenantConfig` loaded from `<config-dir>/tenants/<tenant>/*.json`, validated against `schemas/connector-config.v1.json` and `config.ValidateStorages`, reloadable with the last good version kept. `Bootstrap` for `vidyomd init`. Daily maintenance: job and idempotency retention, audit export and prune.

### Phase 7: catalog (D8, D24, D30, D35)

`service.Assets`: list, get, metadata update with history and the D7 rule for public videos, lineage with children, versions, AI policy with consent record for assets and collections, transcript with sensitive redaction for readers without `asset.sensitive`, sensitive segment management. Every mutation audited in the same transaction.

### Phase 8: isolation and final checks

`§15.6` isolation test crossing all the above with two real tenants. `go vet`, `go test -race` on own paths, `make check-static`, `GOOS=windows` compile check of `vault` and `keyring`.

## 3. Test strategy

- Real SQLite, FTS5 and sqlite-vec through `sqlitedriver` in all persistence tests. No mock database.
- Services are tested against the real store from `_test.go` files, with a small fake `connector.StorageResolver` for audit export.
- Provider tests use `t.TempDir` files and stubs for the OS calls; the real Keychain path runs only with `VIDYOM_TEST_REAL_KEYCHAIN=1`, so tests never touch the maintainer's keychain by default. `dpapi` is compile-checked only (I10, I12).
- `-race` on all own packages. Temporary files only under `t.TempDir`.

## 4. Definition of done

Master plan §10 `core`, repeated as a checklist:

- [ ] Per-tenant `tenant.db` and `system.db` created and migrated through `sqlitedriver`, WAL, `synchronous=FULL`, autocheckpoint on (I1).
- [ ] FTS5 and sqlite-vec indexes with `embedding_model` per vector and side-by-side reindexing (D15).
- [ ] Transactional friendly ID allocation, no collisions under concurrency, never reused (R5, D46).
- [ ] All `repo` interfaces implemented with a conformance test against the real driver.
- [ ] Envelope-encrypted vault with per-tenant data key rotation; `security.MasterKeyProvider` implementations per I10, fail closed.
- [ ] Hash-chained audit with verify and export (D23).
- [ ] Authentication (hashing, pairing, rotation, revocation, peer credentials), authorization with role bundles and grants (D48), budgets (D19).
- [ ] Asset service with consent record (D8), sensitive segments (D30), versions (D35).
- [ ] Tenant isolation test matching §15.6.
