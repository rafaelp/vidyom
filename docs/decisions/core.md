# Implementation decisions: component `core` (I100–I119)

Same format as [`docs/decisions.md`](../decisions.md). What these say a version does is a **target** until the corresponding commit lands; `docs/plans/core.md` tracks it. These never change or relabel the maintainer's `Dn` decisions.

---

## Wiring

### I100. One type per contract where method sets collide

- **Choice.** `security.Vault` and `service.Secrets`, `security.Auditor` and `service.Audit`, `security.Pairing` and `service.Devices`, `service.Tenants` and `service.Users` have methods with the same name and different signatures, so one Go type cannot satisfy both. Each constructor returns the richer type and exposes the second through a method: `vault.New(...).Secrets()`, `.Resolver(tenant)`; `audit.New(...).Service()`; `authentication.New(...).Devices()`; `tenants.New(...).Users()`, `.Budgets()`. `vault.New` returns `(*Vault, error)` because it unseals at construction: an unsealed vault cannot exist without a key, and a failing provider is the error that stops the daemon (D13).
- **Why.** Master plan §9 names the entry points and what they provide, not that one value must provide all of it. Changing the contract signatures would be breaking.
- **Revisit if.** `integration` finds a cleaner seam; the contract can gain additive adapters.

### I101. Optional repository capabilities live beside the contract

- **Choice.** Additive interfaces in `internal/contract/repo`, discovered by type assertion: `SystemAuditProvider` (the system chain, I106), `WorkerTokenLookup` (the name for `AuthenticateWorker`, since `HasTokenHash` returns only a bool), `AuditMaintainer` (export watermark and retention prune, I106), `AssetAIPolicyWriter` (I104), `OutboxMaintainer` (I118). The hash function is additive in `domain` as `AuditRecord.ComputeHash`, because the repository writes the chain and the auditor verifies it.
- **Why.** Master plan §7 forbids adding a method to an interface others may fake.
- **Revisit if.** After the parallel stage these can merge into the main interfaces.

---

## Persistence

### I102. Store mechanics

- **Choice.** Writes pass a per-database in-process writer gate and then `BEGIN IMMEDIATE`, so writers queue fairly instead of spinning on SQLite's busy handler; acquiring the gate times out after 30 s with `errs.CodeUnavailable`. Inside `InTx` only the supplied repositories may be used: a write through the outer store from inside the callback would wait on the gate and fail at that timeout rather than hang forever. Multi-statement reads run in a deferred transaction, so an `Asset` and its denormalized parts are one consistent view. Timestamps are `TEXT` in a fixed-width UTC format with nanoseconds, so comparisons in SQL are correct. Lists order newest first by insertion (`rowid`) with an opaque cursor, default limit 50, maximum 500; `Scan` and `Stream` are ascending. `JobRepo.List` omits `Snapshot` to keep lists light; `Get` and `ListActive` include it. Data directories are created `0700` and database files `0600` before SQLite opens them, because they hold transcripts and vault ciphertext. `persistence.Options.RequireExisting` makes opening fail when `system.db` is absent, so a normal start never initializes a data directory by accident; only `vidyomd init` leaves it false.
- **Why.** Only the daemon opens these files (D5), so one process owns all writers. Durable `synchronous=FULL` commits make unfair busy waiting visible under load.
- **Revisit if.** A PostgreSQL backend appears; these are SQLite details behind `repo`.

### I103. Migrations and file identity

- **Choice.** Numbered SQL files embedded per database kind, applied in one immediate transaction, recorded with a SHA-256 in `schema_migrations`, mirrored in `PRAGMA user_version`. Opening fails on a database newer than the binary or on an applied migration whose text changed. A `meta` table records `kind` (`system` or `tenant`) and, for tenant databases, `tenant_id`, in the first migration's transaction; `ForTenant` refuses a file whose recorded tenant differs from the requested one, so a misplaced or copied file fails closed rather than serving another tenant's data (§15.6). Handles stay cached for the process lifetime with idle connections expiring.
- **Why.** D5 needs migrations and D6 needs isolation that survives operator mistakes.
- **Revisit if.** Tenant counts make cached handles a problem; an LRU can close idle ones.

### I104. External AI policy is not an ordinary field

- **Choice.** `AssetRepo.Update` never writes `ai_processing` or `ai_consent`. Only the optional `AssetAIPolicyWriter` does, called by the catalog service, which writes the consent record and the audit in one transaction (D8). A stale read-modify-write of an asset elsewhere therefore cannot re-enable external processing an admin has just turned off. The stored value is the effective one, plus a source (`default`, `asset`, `collection`):
  - `AssetRepo.Create` with a blank policy takes the collection's policy and consent when the asset names a collection, otherwise `local_only`. An explicit `local_only` is honored and sourced `asset`. `external_allowed` with no consent record fails with `errs.CodeFailedPrecondition`: nobody authorized it. A clip does not inherit from its source.
  - An asset-level decision is sourced `asset` and wins over later collection changes. A collection change rewrites every member not sourced `asset`. Moving an asset between collections re-evaluates it the same way.
  - Returning to `local_only` clears `ai_consent`; the audit and the asset history keep who decided what and when.
- **Why.** D8 says `local_only` content may only use `local` endpoints; every path reads `asset.ai_processing` through `config.AllowedEndpoints`, so the column must always be the effective answer, and privacy must fail closed.
- **Wiring note.** `catalog.register` (engine) should leave `aiProcessing` blank unless the workflow sets it, and pass a consent record when it is `external_allowed`, or the create fails.
- **Revisit if.** The maintainer wants clips to inherit their source's consent.

### I105. Asset reads and references

- **Choice.** Following I13, foreign keys are ULIDs and JSON carries friendly IDs. `AssetRepo.Create` and `Update` ignore `SourceOriginal`, `WorkingMaster`, `Proxy`, `Backup`, `Thumbnail`, `Transcript`, `Provenance` and `Publications`, which are derived on read for `CurrentVersion` from version files (purpose with an empty `name`), the transcript row, the search indexes and publications. `Transcript.Indexed` is true only when that version has lexical chunks and vectors; `EmbeddingModel` is the active model when it has vectors for the version, otherwise the most recent model that does; `Format` is `json`, or `json+srt` when a `captions` file exists. Friendly IDs and ULIDs are always caller-supplied on create (`IDs().Next`, `id.New`), since `Create` returns neither. Defaults on create: status `registered`, visibility `private`, `catalog_sync` `none`, version `v1`. A version row is explicit through `VersionRepo.Create`; `PutFile` on a missing version is `not_found`. `PublicationRepo.Update` to `published` marks the recorded version immutable in the same transaction, so D35 holds whoever writes it; `MarkImmutable` stays idempotent. `JobRepo.Update` never rewrites the snapshot, workflow identity or trigger (§6.5). `PutNode` refuses output above `domain.MaxNodeOutputBytes` (I35). A wrongly typed or unparsable `id.Ref` resolves to `not_found`, which keeps cross-tenant and garbage IDs indistinguishable. Approvals keep the job's friendly ID as text, so job retention (§10.4) never deletes the record of who approved what (R13); they are not cascaded.
- **Why.** The contract says these are denormalized; one rule stops writers from setting readiness by hand, which is what R11 exists to prevent.
- **Revisit if.** Hydrating lists gets slow at scale; it is per asset within one read transaction.

### I106. Search and vectors

- **Choice.** Lexical: one FTS5 external-content table with `unicode61 remove_diacritics 2`. User text is tokenized and each token quoted, joined by `OR`, so no query can inject FTS5 syntax and BM25 ranks documents with more terms first; `Score` is `-bm25`. Vectors: one `vec0` table per model (`asset` partition key, cosine distance, `chunk_size=128` to keep the per-partition allocation near 0.5 MiB), named from a hash of the model, registered in `embedding_models` with dimensions and chunking strategy; `Score` is `1 - distance`. That gives the D15 side-by-side property and makes mixing models or dimensions impossible at the schema level (R18). `IndexLexical` and `IndexVectors` replace what exists for that asset, version and model, so retries are idempotent; `IndexVectors` is one call per asset version. The first model written to a tenant becomes active automatically; later ones need `SetActiveModel`. `DeleteModel` refuses the active model. Hits are filtered to the asset's `current_version` and exclude soft-deleted assets; vector search over-fetches until it has enough hits after that filter. Tenant-wide KNN over a model above the scan cap (default 50 000, I43) searches only the newest assets up to the cap, one partition at a time. `ListEmbeddings` implements `repo.EmbeddingLister` for the `public.db` export (I232).
- **Why.** D5 and I43. A `vec0` table has one fixed dimension, and BGE-M3 and test models need not share it.
- **Revisit if.** sqlite-vec gains an approximate index, or D5's load test sets another cap.

---

## Audit

### I107. Audit chain details

- **Choice.** `Hash` is SHA-256 of a versioned canonical JSON of every field except `Hash`, including `TenantID` and `PrevHash`; the first record's `PrevHash` is 64 zeros (`domain.AuditGenesisHash`). SQLite triggers refuse `UPDATE` and `DELETE` on `audit_log`, which stops application bugs; the chain is what detects a host-level edit, so a tamper test must drop the trigger first, as an attacker would (`DROP TRIGGER audit_log_no_update`). Verify recomputes every hash and checks sequence continuity and each export's recorded head hash, so truncating exported history is detected as well; truncating records appended after the last export is not detectable from the database alone, which is why the daily export exists (D23). Export writes `audit/YYYY/MM/DD/<from>-<to>.jsonl` to the tenant's `archive` role, one record per line, verifiable offline with the same function, with the SHA-256 checked by the driver; the watermark moves only after the put succeeds. Nothing to export returns the last key and writes nothing. Prune removes only records already exported and older than retention, with `config.MinAuditDays` as a hard floor, and keeps an anchor with the last pruned hash. `Auditor.Record` refuses nothing silently: a failed write returns an error (§10.1). A detail whose key looks like a credential is replaced with `[redacted]`.
- **Why.** D23 and I8. Spec §10.1 says critical audit events are never silently dropped.
- **Revisit if.** A signed, externally anchored chain is required; an HMAC with a tenant-derived key could be added beside the hash.

### I108. A system chain for actions without a tenant

- **Choice.** `system.db` has its own `audit_log`, with the same shape and rules. `Auditor.Record` with an empty `TenantID` writes there: pairing, device changes with no tenant in scope, tenant creation, the boot record of how the master key is sealed (I10). `Verify` with an empty tenant checks it. Only system admins reach it, and `Export` with an empty tenant answers `not_implemented`, because storage roles are per tenant and the system database has no `archive` of its own. Stated gap against D23's daily export; Litestream (I31) is the backup path for it.
- **Why.** D23 describes a per-tenant table, but I10 requires auditing the boot and D12 requires auditing devices, and neither belongs to a tenant.
- **Revisit if.** A deployment-level archive exists.

---

## Vault

### I109. Envelope details and rotation order

- **Choice.** Master key wraps each 256-bit data key with AES-256-GCM, AAD `vidyom-dek-v1|<tenant>|<version>`. A secret is sealed with AES-256-GCM under `HKDF-SHA256(data key, info="vidyom-secret-v1")`, AAD `<tenant>|<name>|<key version>`, so a ciphertext copied to another name or tenant does not open. Data keys are unwrapped per operation and not cached. `RotateDataKey`: under a per-tenant lock, store the new version as active in `system.db`, then re-encrypt every secret in one tenant transaction; a crash between the two leaves old records under the old version, still readable because every record names its version and old versions are kept, and the next rotation finishes them. `RewrapAll` is one `system.db` transaction, then the operator changes the provider. `CacheKey` is `HKDF(active data key, "vidyom-cache-v1")` (I39), so rotation makes old cache chunks undecryptable and they count as misses. `PublicIDKey` is `HKDF(data key version 1, "vidyom-public-id-v1")`, because public IDs must survive a rotation or every public URL would change (I29). Data keys are created on first use and eagerly at tenant creation.
- **Wiring note.** `media` must treat a cache chunk that fails to decrypt as a miss, not an error.
- **Why.** D13 requires rotation and re-wrap; one database transaction cannot span `system.db` and `tenant.db`, so the order is what provides crash safety.
- **Revisit if.** Keeping old data keys forever is judged unnecessary; only version 1 is needed once all secrets moved.

### I110. Secret rules

- **Choice.** Names match `^[A-Za-z0-9][A-Za-z0-9._-]*$`, the same pattern as `credentialsRef` in `schemas/connector-config.v1.json`, up to 128 bytes. `client` location is refused (§9.1: those stay on the client). `delegated` requires a duration: `ttl_seconds`, default 1 hour, maximum 24 hours. Past `expires_at` a value does not open (`failed_precondition`). `ResolveSecret` updates `last_used_at` and writes `secret.used` to the audit, and the value is not returned if that write fails. `Test` decrypts and reports OK or why not, never the value. Value size limit is 1 MiB.
- **Why.** §9.1: rotation, revocation, access tests and usage auditing are requirements; delegated credentials have limited duration.
- **Revisit if.** Audit volume from `secret.used` becomes a problem; it can be throttled per secret per hour.

### I111. Master key formats and provider details

- **Choice.** A key source holds exactly 32 raw bytes, or their standard base64 with optional trailing whitespace. Anything else fails closed. `file` requires `path`, `acknowledgeFileSealed: true` and mode `0400` or `0440` (not checked on Windows, where mode bits do not exist). `env` requires both `VIDYOM_MASTER_KEY_B64` and `VIDYOM_INSECURE_DEV_UNSEAL=1`; it stays readable, because the daemon checks a provider again before opening a listener (I138), and processes it starts get a minimal environment. `systemd-creds` reads `$CREDENTIALS_DIRECTORY/<itemName>`, default `vidyom-master-key`, and reports `hardware`; a credential not sealed with a TPM is the operator's choice and is not detectable from inside the service. **Validate.** `macos-keychain` reads a generic password with service `vidyom` and account `<itemName>` through `/usr/bin/security`; refused on other platforms. **Validate.** `dpapi` decrypts the file at `path` with `CryptUnprotectData`; compile-checked only. **Validate.** `gcp-kms` validates and answers `not_implemented` at unseal. `vidyomd init --generate-master-key` (`api`, I138) writes the base64 form at mode `0400`. `Vault.AuditBoot` records the provider and sealing level in the system chain.
- **Why.** I10 states the providers but not formats. `openssl rand -base64 32` and raw bytes are both common.
- **Revisit if.** A hardware-backed provider gets validated; the sealing report stays what callers see.

---

## Identity

### I112. Device credentials and pairing

- **Choice.** Pairing codes are 100 random bits in four groups of five Crockford base32 characters, stored as SHA-256; default TTL 15 minutes, maximum 24 hours. Tokens are `vdm_<device ulid>_<base64url of 32 random bytes>`, stored as SHA-256 of the secret part; the token's ULID must match the device found. Worker tokens are `vdw_<base64url of 32 random bytes>`, also hashed. On a Unix socket a bearer token takes precedence over peer identity, and a bad token never falls back to it. `last_seen_at` updates at most once a minute per device. Scope rules: a system admin may act on any device and create a code for any user; an admin of the request's tenant (`device.manage`) may create codes for members of that tenant and act on their devices, but not create a code for a system admin; anyone may list, rotate and revoke their own devices. `Devices.List` returns what the caller may act on. Everything is audited in the system chain, in the same transaction as the change, and also in the tenant chain when the request has a tenant. `Pair` is unauthenticated, as the route table says; the 100-bit code makes guessing impractical, and rate limiting is the API's.
- **Why.** D12, I17. The route table marks these `Dynamic`, leaving scope to the service.
- **Revisit if.** Codes must be typed on phones; they are already case-insensitive.

### I113. Authorization edge cases

- **Choice.** `Resolve` looks up the membership; none, or an unknown tenant, is `not_found` (§12.1). A system admin with no membership gets `not_found` too, since `Principal.Can` says tenant access still needs a membership; system-level permissions pass with no tenant. `AuthSystem` principals (event and schedule triggers) resolve with no role and only explicit grants, and worker principals are refused: they use `AuthenticateWorker`. `Require` denies through `errs.Forbidden` naming the permission and counts `vidyom_authz_denials_total`.
- **Why.** D6, D48. Fail closed by default.
- **Revisit if.** System admins should see tenant metadata without membership.

---

## Tenants, budgets and catalog

### I114. Tenant lifecycle and bootstrap

- **Choice.** Creation order: refuse if `system.db` has the tenant; create or open the tenant database; create the data key; write the system row, the creator's `admin` membership and the audit last. A crash before the last step leaves an orphan file that the next attempt adopts, and no request can reach it. The creating system admin becomes the tenant's `admin`, otherwise they could not use it. `Service.Bootstrap` backs `vidyomd init` (I17): it refuses if any tenant exists, adopts a system admin left by an attempt that crashed part way, then creates the tenant, the first user (system admin and tenant admin), a worker token and one pairing code lasting 1 hour, returning the last two once.
- **Why.** I17. Two databases, no shared transaction, so the system row is the commit point.
- **Revisit if.** Tenants can be renamed or deleted; neither exists in §11.

### I115. Tenant configuration files

- **Choice.** `tenants.Options.ConfigDir` is `<config-dir>`, which `wire.go` takes from `config.Daemon.ConfigDir` (I138). Files per tenant: `storages.json`, `publishing.json`, `ai.json`, `observability.json`, `hooks.json` (I34) plus `catalogs.json` and `budget.json`, which `config.Tenant` needs and I34 omits. `ai.json` and `catalogs.json` accept both the wrapped form the schema validates (`{"ai": {...}}`) and the bare form `config.AI`'s comment implies; the wrapped form is canonical. `budget.json` is `{"perUnitMinor": {...}}`. A missing file means an empty section; `ai.defaultProcessing` defaults to `local_only`. A present file must pass `schemas/connector-config.v1.json`, `config.ValidateStorages` and strict decoding, otherwise that tenant's configuration fails to load. `Reload` (for `SIGHUP`) swaps only tenants that load cleanly and returns the failures, so a bad edit never removes working configuration. An unknown tenant is `not_found`.
- **Why.** I34, §7.1. The disagreement between `config.AI`'s comment and the schema is pointed out here and accepted in both forms.
- **Revisit if.** Tenant admins need to change connectors through the API (I34).

### I116. Budget details

- **Choice.** Periods are UTC calendar months. No budget set means nothing is blocked and `Percent` is 0. `Check` returns `errs.CodeBudgetExceeded` at `spent >= monthly` with no active override. `Record` computes `EstimatedMinor` from `config.Tenant.Budget.PerUnitMinor[operation]` when the caller left it at 0, rounding to the nearest minor unit, and fires `budget.threshold_reached` and `budget.exceeded` once per crossing, computed in the same transaction as the usage row, so concurrent records cannot both or neither fire. Override requires a reason, defaults to the end of the current month, and is audited. Setting a new budget clears nothing already recorded.
- **Why.** D19 and I33: the engine pauses a job on `budget_exceeded`.
- **Revisit if.** Tenants need calendars other than UTC months.

### I117. Metadata edits on public videos

- **Choice.** D7 requires a new publisher approval to change public metadata of an already public video. `catalog.Update` on an asset with a publication that `IsPublic` requires `approval.decide.publication` on the caller and treats that as the approval: an `editor` gets `403`, a `publisher` edits, the audit records that permission. It also writes `publication.updated` to the outbox per public publication in the same transaction, so `public-library-on-updated` refreshes `public.db`. No staged "pending metadata" exists in this version. Soft-deleted assets answer `404` through the service, and stay visible to `AssetRepo.Get` so workflows and retries can reason about them. Changing a public video's collection is not public metadata and needs no approval.
- **Why.** The API offers a direct `PATCH` and no route to stage an edit.
- **Revisit if.** Publishers want a review step separate from editing; a `metadata_change` approval kind already exists for a workflow to use.

### I118. Sensitive segments and transcript reads

- **Choice.** `service.Assets.Transcript` returns the full transcript to callers holding `asset.sensitive` (editors and above) and a redacted one to others, with each overlapped segment's text replaced and its words removed, through `domain.Transcript.Redacted`. D30 lists exclusions from public and external paths, and does not name private reads; a viewer has no need for that text, and the private default should fail closed. Marks default to the asset's current version, need `0 <= start < end`, and `RemoveSensitive` matches the exact interval (`not_found` otherwise). Both write history and audit, without the segment's text. Retention: `tenants.Maintenance` runs daily per tenant: job records past `JobDays` (with their nodes and events), dispatched outbox events past `JobDays`, idempotency records past 7 days, delegated secrets past their expiry (§9.1), audit export, then audit prune past `max(AuditDays, 365)`. Logs live in the `file` sink's JSONL, whose rotation belongs to `extensibility`; expired uploads belong to `api`, which owns the spool.
- **Why.** D30, I45, §10.4.
- **Revisit if.** Viewers need the whole text; a grant of `asset.sensitive` already gives it to one user.

---

## Disagreements found

| # | Where | Disagreement | This implementation |
|---|---|---|---|
| 1 | `config.AI` comment vs `schemas/connector-config.v1.json` | "AI is ai.json" with no wrapper vs a required top-level `ai` key | Both accepted (I115) |
| 2 | I34 vs `config.Tenant` | I34 lists five files; `config.Tenant` also has catalogs and budget costs | Two additional optional files (I115) |
| 3 | `security.Authenticator` vs `repo.WorkerRepo` | The first returns a worker name, the second only answers yes or no | Optional `WorkerTokenLookup` (I101) |
| 4 | Master plan §9 vs contract method sets | One entry point "provides" types whose methods collide | Adapter methods (I100) |
| 5 | D23 vs D12, I10 | Audit is per tenant, yet device and boot events have no tenant | System chain (I108) |
| 6 | `catalog.register` parameter `aiProcessing` vs D8 | A workflow parameter could set `external_allowed` with no admin consent | Refused without a consent record (I104) |
