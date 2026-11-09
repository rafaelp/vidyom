-- tenant.db: the canonical catalog (D5, D24) and everything else of one tenant.
-- Foreign keys use ULIDs; friendly IDs are aliases (I13). Timestamps are fixed-width
-- UTC text, so comparisons in SQL are correct (I102).

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

INSERT INTO meta (key, value) VALUES ('kind', 'tenant');

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

-- Friendly ID sequences per prefix (R5, D46). A row only ever grows.
CREATE TABLE sequences (
  prefix TEXT PRIMARY KEY,
  last   INTEGER NOT NULL
) STRICT;

-- ---- Catalog ----

CREATE TABLE assets (
  ulid               TEXT PRIMARY KEY,
  friendly_id        TEXT NOT NULL UNIQUE,
  kind               TEXT NOT NULL CHECK (kind IN ('original', 'clip', 'edited')),
  category           TEXT NOT NULL DEFAULT '',
  title              TEXT NOT NULL,
  description        TEXT NOT NULL DEFAULT '',
  tags               TEXT NOT NULL DEFAULT '[]',
  collection         TEXT NOT NULL DEFAULT '',
  visibility         TEXT NOT NULL,
  ai_processing      TEXT NOT NULL CHECK (ai_processing IN ('local_only', 'external_allowed')),
  ai_consent         TEXT,
  ai_policy_source   TEXT NOT NULL DEFAULT 'default',
  status             TEXT NOT NULL,
  current_version    TEXT NOT NULL,
  master_available   INTEGER NOT NULL DEFAULT 0,
  backup_verified    INTEGER NOT NULL DEFAULT 0,
  proxy_available    INTEGER NOT NULL DEFAULT 0,
  timeline_validated INTEGER NOT NULL DEFAULT 0,
  catalog_sync       TEXT NOT NULL DEFAULT 'none',
  duration_seconds   REAL NOT NULL DEFAULT 0,
  language           TEXT NOT NULL DEFAULT '',
  parent_ulid        TEXT REFERENCES assets (ulid),
  lineage            TEXT,
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  deleted_at         TEXT,
  -- D8: external processing always has a consent record.
  CHECK (ai_processing = 'local_only' OR ai_consent IS NOT NULL)
) STRICT;

CREATE INDEX assets_kind ON assets (kind);
CREATE INDEX assets_status ON assets (status);
CREATE INDEX assets_collection ON assets (collection);
CREATE INDEX assets_parent ON assets (parent_ulid);

CREATE TABLE asset_history (
  id         INTEGER PRIMARY KEY,
  asset_ulid TEXT NOT NULL REFERENCES assets (ulid),
  at         TEXT NOT NULL,
  actor      TEXT NOT NULL,
  action     TEXT NOT NULL,
  changes    TEXT
) STRICT;

CREATE INDEX asset_history_asset ON asset_history (asset_ulid);

-- Lineage intervals, for the D47 overlap warning.
CREATE TABLE lineage_segments (
  asset_ulid      TEXT NOT NULL REFERENCES assets (ulid),
  source_asset_id TEXT NOT NULL,
  source_version  TEXT NOT NULL,
  start_seconds   REAL NOT NULL,
  end_seconds     REAL NOT NULL
) STRICT;

CREATE INDEX lineage_segments_source ON lineage_segments (source_asset_id, start_seconds);
CREATE INDEX lineage_segments_asset ON lineage_segments (asset_ulid);

CREATE TABLE asset_versions (
  asset_ulid TEXT NOT NULL REFERENCES assets (ulid),
  version    TEXT NOT NULL,
  immutable  INTEGER NOT NULL DEFAULT 0,
  provenance TEXT,
  created_at TEXT NOT NULL,
  PRIMARY KEY (asset_ulid, version)
) STRICT;

CREATE TABLE version_files (
  asset_ulid TEXT NOT NULL,
  version    TEXT NOT NULL,
  purpose    TEXT NOT NULL,
  name       TEXT NOT NULL DEFAULT '',
  file       TEXT NOT NULL,
  PRIMARY KEY (asset_ulid, version, purpose, name),
  FOREIGN KEY (asset_ulid, version) REFERENCES asset_versions (asset_ulid, version)
) STRICT;

CREATE TABLE collections (
  name          TEXT PRIMARY KEY,
  ai_processing TEXT NOT NULL CHECK (ai_processing IN ('local_only', 'external_allowed')),
  ai_consent    TEXT,
  CHECK (ai_processing = 'local_only' OR ai_consent IS NOT NULL)
) STRICT;

-- ---- Transcripts and analysis (private by default, D30) ----

CREATE TABLE transcripts (
  asset_ulid      TEXT NOT NULL REFERENCES assets (ulid),
  version         TEXT NOT NULL,
  engine          TEXT NOT NULL DEFAULT '',
  timestamp_basis TEXT NOT NULL DEFAULT '',
  document        TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (asset_ulid, version)
) STRICT;

CREATE TABLE analyses (
  asset_ulid TEXT NOT NULL REFERENCES assets (ulid),
  version    TEXT NOT NULL,
  document   TEXT NOT NULL,
  PRIMARY KEY (asset_ulid, version)
) STRICT;

CREATE TABLE sensitive_segments (
  asset_ulid    TEXT NOT NULL REFERENCES assets (ulid),
  version       TEXT NOT NULL,
  start_seconds REAL NOT NULL,
  end_seconds   REAL NOT NULL,
  reason        TEXT NOT NULL DEFAULT '',
  marked_by     TEXT NOT NULL DEFAULT '',
  marked_at     TEXT NOT NULL,
  PRIMARY KEY (asset_ulid, version, start_seconds, end_seconds)
) STRICT;

-- ---- Search (D5, D15, R18) ----

CREATE TABLE search_chunks (
  id            INTEGER PRIMARY KEY,
  asset_ulid    TEXT NOT NULL REFERENCES assets (ulid),
  version       TEXT NOT NULL,
  level         TEXT NOT NULL,
  start_seconds REAL NOT NULL,
  end_seconds   REAL NOT NULL,
  text          TEXT NOT NULL
) STRICT;

CREATE INDEX search_chunks_asset ON search_chunks (asset_ulid, version);

CREATE VIRTUAL TABLE search_fts USING fts5 (
  text,
  content = 'search_chunks',
  content_rowid = 'id',
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER search_chunks_ai AFTER INSERT ON search_chunks BEGIN
  INSERT INTO search_fts (rowid, text) VALUES (new.id, new.text);
END;

CREATE TRIGGER search_chunks_ad AFTER DELETE ON search_chunks BEGIN
  INSERT INTO search_fts (search_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;

-- One vec0 table per model, created on first use (I106). Every vector records its
-- model and segmentation strategy (R18).
CREATE TABLE embedding_models (
  model      TEXT PRIMARY KEY,
  dimensions INTEGER NOT NULL,
  table_name TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
) STRICT;

CREATE TABLE vector_chunks (
  id            INTEGER PRIMARY KEY,
  model         TEXT NOT NULL REFERENCES embedding_models (model),
  asset_ulid    TEXT NOT NULL REFERENCES assets (ulid),
  version       TEXT NOT NULL,
  chunking      TEXT NOT NULL DEFAULT '',
  level         TEXT NOT NULL,
  start_seconds REAL NOT NULL,
  end_seconds   REAL NOT NULL,
  text          TEXT NOT NULL
) STRICT;

CREATE INDEX vector_chunks_model_asset ON vector_chunks (model, asset_ulid, version);
CREATE INDEX vector_chunks_asset ON vector_chunks (asset_ulid, version);

-- ---- Jobs (source of truth, D4; checkpoints, D11) ----

CREATE TABLE jobs (
  ulid             TEXT PRIMARY KEY,
  friendly_id      TEXT NOT NULL UNIQUE,
  workflow_id      TEXT NOT NULL,
  workflow_version TEXT NOT NULL DEFAULT '',
  workflow_sha256  TEXT NOT NULL DEFAULT '',
  snapshot         TEXT,
  status           TEXT NOT NULL,
  status_reason    TEXT NOT NULL DEFAULT '',
  inputs           TEXT,
  outputs          TEXT,
  trigger_info     TEXT NOT NULL,
  parent_job_id    TEXT NOT NULL DEFAULT '',
  asset_ulid       TEXT REFERENCES assets (ulid),
  submitted_by     TEXT NOT NULL DEFAULT '',
  permissions      TEXT NOT NULL DEFAULT '[]',
  idempotency_key  TEXT NOT NULL DEFAULT '',
  trace_id         TEXT NOT NULL DEFAULT '',
  progress_percent REAL NOT NULL DEFAULT 0,
  attention        TEXT,
  error            TEXT,
  created_at       TEXT NOT NULL,
  started_at       TEXT,
  finished_at      TEXT
) STRICT;

CREATE UNIQUE INDEX jobs_idempotency ON jobs (idempotency_key) WHERE idempotency_key != '';
CREATE INDEX jobs_status ON jobs (status);
CREATE INDEX jobs_workflow ON jobs (workflow_id);
CREATE INDEX jobs_asset ON jobs (asset_ulid);
CREATE INDEX jobs_parent ON jobs (parent_job_id) WHERE parent_job_id != '';
CREATE INDEX jobs_finished ON jobs (finished_at) WHERE finished_at IS NOT NULL;

CREATE TABLE node_executions (
  job_ulid         TEXT NOT NULL REFERENCES jobs (ulid) ON DELETE CASCADE,
  node_id          TEXT NOT NULL,
  node_type        TEXT NOT NULL,
  type_version     INTEGER NOT NULL DEFAULT 0,
  status           TEXT NOT NULL,
  attempt          INTEGER NOT NULL DEFAULT 0,
  parameters       TEXT,
  output           TEXT,
  error            TEXT,
  idempotency_key  TEXT NOT NULL DEFAULT '',
  task_id          TEXT NOT NULL DEFAULT '',
  worker_id        TEXT NOT NULL DEFAULT '',
  lease_id         TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  progress_percent REAL NOT NULL DEFAULT 0,
  started_at       TEXT,
  finished_at      TEXT,
  PRIMARY KEY (job_ulid, node_id)
) STRICT;

CREATE UNIQUE INDEX node_executions_task ON node_executions (task_id) WHERE task_id != '';

CREATE TABLE job_events (
  job_ulid TEXT NOT NULL REFERENCES jobs (ulid) ON DELETE CASCADE,
  seq      INTEGER NOT NULL,
  at       TEXT NOT NULL,
  kind     TEXT NOT NULL,
  node_id  TEXT NOT NULL DEFAULT '',
  level    TEXT NOT NULL DEFAULT '',
  message  TEXT NOT NULL DEFAULT '',
  data     TEXT,
  PRIMARY KEY (job_ulid, seq)
) STRICT;

-- ---- Approvals (R13, R25). Kept past job retention (I105). ----

CREATE TABLE approvals (
  ulid            TEXT PRIMARY KEY,
  friendly_id     TEXT NOT NULL UNIQUE,
  kind            TEXT NOT NULL,
  status          TEXT NOT NULL,
  asset_ulid      TEXT REFERENCES assets (ulid),
  asset_version   TEXT NOT NULL DEFAULT '',
  job_id          TEXT NOT NULL DEFAULT '',
  job_ulid        TEXT NOT NULL DEFAULT '',
  node_id         TEXT NOT NULL DEFAULT '',
  approvers       TEXT NOT NULL DEFAULT '[]',
  evidence        TEXT,
  preview         TEXT NOT NULL DEFAULT '',
  allow_adjust    INTEGER NOT NULL DEFAULT 0,
  segments        TEXT NOT NULL DEFAULT '[]',
  timeout_seconds INTEGER NOT NULL DEFAULT 0,
  on_timeout      TEXT NOT NULL DEFAULT '',
  expires_at      TEXT,
  decided_by      TEXT NOT NULL DEFAULT '',
  decided_at      TEXT,
  reason          TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL
) STRICT;

CREATE INDEX approvals_job_node ON approvals (job_ulid, node_id);
CREATE INDEX approvals_asset ON approvals (asset_ulid);
CREATE INDEX approvals_status ON approvals (status);
CREATE INDEX approvals_expiring ON approvals (expires_at) WHERE status = 'pending' AND expires_at IS NOT NULL;

-- ---- Publications (§2.1, §12.4) ----

CREATE TABLE publications (
  ulid                  TEXT PRIMARY KEY,
  friendly_id           TEXT NOT NULL UNIQUE,
  asset_ulid            TEXT NOT NULL REFERENCES assets (ulid),
  destination           TEXT NOT NULL,
  asset_version         TEXT NOT NULL,
  published_file_sha256 TEXT NOT NULL DEFAULT '',
  published_file        TEXT,
  remote_id             TEXT NOT NULL DEFAULT '',
  url                   TEXT NOT NULL DEFAULT '',
  status                TEXT NOT NULL,
  visibility            TEXT NOT NULL,
  approval_id           TEXT NOT NULL DEFAULT '',
  title                 TEXT NOT NULL DEFAULT '',
  description           TEXT NOT NULL DEFAULT '',
  idempotency_key       TEXT NOT NULL DEFAULT '',
  status_reason         TEXT NOT NULL DEFAULT '',
  created_at            TEXT NOT NULL,
  updated_at            TEXT NOT NULL,
  published_at          TEXT,
  withdrawn_at          TEXT,
  -- R43: published only after the destination returned an ID and a URL.
  CHECK (status != 'published' OR (remote_id != '' AND url != ''))
) STRICT;

CREATE UNIQUE INDEX publications_idempotency ON publications (idempotency_key) WHERE idempotency_key != '';
CREATE INDEX publications_asset ON publications (asset_ulid);
CREATE INDEX publications_status ON publications (status, visibility);
CREATE INDEX publications_destination ON publications (destination);

-- ---- Uploads (§5.4, I20) ----

CREATE TABLE uploads (
  ulid           TEXT PRIMARY KEY,
  friendly_id    TEXT NOT NULL UNIQUE,
  filename       TEXT NOT NULL,
  size_bytes     INTEGER NOT NULL,
  sha256         TEXT NOT NULL,
  chunk_bytes    INTEGER NOT NULL DEFAULT 0,
  received_bytes INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL,
  created_by     TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  expires_at     TEXT NOT NULL
) STRICT;

CREATE INDEX uploads_expires ON uploads (expires_at);

-- ---- Audit (D23, I107) ----

CREATE TABLE audit_log (
  seq        INTEGER PRIMARY KEY,
  ulid       TEXT NOT NULL UNIQUE,
  at         TEXT NOT NULL,
  actor      TEXT NOT NULL,
  method     TEXT NOT NULL DEFAULT '',
  action     TEXT NOT NULL,
  permission TEXT NOT NULL DEFAULT '',
  resource   TEXT NOT NULL DEFAULT '',
  result     TEXT NOT NULL,
  request_id TEXT NOT NULL DEFAULT '',
  details    TEXT,
  prev_hash  TEXT NOT NULL,
  hash       TEXT NOT NULL
) STRICT;

CREATE INDEX audit_log_at ON audit_log (at);
CREATE INDEX audit_log_actor ON audit_log (actor);
CREATE INDEX audit_log_action ON audit_log (action);
CREATE INDEX audit_log_resource ON audit_log (resource);

-- The prune of whole, exported prefixes opens this guard inside its transaction.
-- Everything else hits the triggers, which stop application bugs; the chain itself
-- is what detects an edit made by someone who removed them.
CREATE TABLE audit_prune_guard (allowed INTEGER NOT NULL) STRICT;
INSERT INTO audit_prune_guard (allowed) VALUES (0);

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN
  SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
WHEN (SELECT allowed FROM audit_prune_guard) = 0
BEGIN
  SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TABLE audit_anchor (
  id   INTEGER PRIMARY KEY CHECK (id = 1),
  seq  INTEGER NOT NULL,
  hash TEXT NOT NULL
) STRICT;

CREATE TABLE audit_exports (
  to_seq    INTEGER PRIMARY KEY,
  from_seq  INTEGER NOT NULL,
  head_hash TEXT NOT NULL,
  key       TEXT NOT NULL,
  sha256    TEXT NOT NULL,
  at        TEXT NOT NULL
) STRICT;

-- ---- Outbox and deliveries (§8.2, D5) ----

CREATE TABLE outbox (
  seq           INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id      TEXT NOT NULL UNIQUE,
  type          TEXT NOT NULL,
  occurred_at   TEXT NOT NULL,
  envelope      TEXT NOT NULL,
  dispatched_at TEXT
) STRICT;

CREATE INDEX outbox_pending ON outbox (seq) WHERE dispatched_at IS NULL;
CREATE INDEX outbox_type ON outbox (type);
CREATE INDEX outbox_occurred ON outbox (occurred_at);

CREATE TABLE deliveries (
  event_id     TEXT NOT NULL,
  subscriber   TEXT NOT NULL,
  status       TEXT NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT NOT NULL DEFAULT '',
  next_attempt TEXT,
  delivered_at TEXT,
  PRIMARY KEY (event_id, subscriber)
) STRICT;

CREATE INDEX deliveries_status ON deliveries (status);
CREATE INDEX deliveries_due ON deliveries (next_attempt) WHERE next_attempt IS NOT NULL;

-- ---- Vault (D13). Ciphertext only. ----

CREATE TABLE secrets (
  name         TEXT PRIMARY KEY,
  location     TEXT NOT NULL,
  created_by   TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  rotated_at   TEXT,
  expires_at   TEXT,
  last_used_at TEXT,
  ciphertext   BLOB NOT NULL,
  key_version  INTEGER NOT NULL
) STRICT;

-- ---- Budgets (D19) ----

CREATE TABLE budget (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  monthly_minor   INTEGER,
  currency        TEXT,
  override_until  TEXT,
  override_by     TEXT,
  override_reason TEXT
) STRICT;

CREATE TABLE usage_records (
  id              INTEGER PRIMARY KEY,
  period          TEXT NOT NULL,
  operation       TEXT NOT NULL,
  provider        TEXT NOT NULL DEFAULT '',
  quantity        REAL NOT NULL DEFAULT 0,
  unit            TEXT NOT NULL DEFAULT '',
  estimated_minor INTEGER NOT NULL DEFAULT 0,
  job_id          TEXT NOT NULL DEFAULT '',
  node_id         TEXT NOT NULL DEFAULT '',
  at              TEXT NOT NULL
) STRICT;

CREATE INDEX usage_records_period ON usage_records (period);

-- ---- API idempotency (§12.1) ----

CREATE TABLE idempotency_keys (
  key          TEXT PRIMARY KEY,
  request_hash TEXT NOT NULL,
  status       INTEGER NOT NULL,
  body         TEXT,
  created_at   TEXT NOT NULL
) STRICT;

CREATE INDEX idempotency_keys_created ON idempotency_keys (created_at);
