-- system.db: tenants, users, roles, device credentials, plugin registry, wrapped
-- tenant data keys, worker tokens and the system audit chain (D5, D6, D12, D13, I108).

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

INSERT INTO meta (key, value) VALUES ('kind', 'system');

CREATE TABLE tenants (
  id           TEXT PRIMARY KEY,
  ulid         TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  storage_root TEXT NOT NULL,
  created_at   TEXT NOT NULL
) STRICT;

CREATE TABLE users (
  ulid         TEXT PRIMARY KEY,
  email        TEXT NOT NULL UNIQUE,
  system_admin INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL
) STRICT;

CREATE TABLE memberships (
  user_ulid TEXT NOT NULL REFERENCES users (ulid) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
  role      TEXT NOT NULL,
  grants    TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (user_ulid, tenant_id)
) STRICT;

CREATE INDEX memberships_tenant ON memberships (tenant_id);

-- CLIENT-nnn is system-wide (I14).
CREATE TABLE sequences (
  prefix TEXT PRIMARY KEY,
  last   INTEGER NOT NULL
) STRICT;

CREATE TABLE devices (
  ulid            TEXT PRIMARY KEY,
  friendly_id     TEXT NOT NULL UNIQUE,
  user_ulid       TEXT NOT NULL REFERENCES users (ulid),
  name            TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL,
  credential_hash TEXT NOT NULL UNIQUE,
  last_seen_at    TEXT,
  rotated_at      TEXT,
  revoked_at      TEXT,
  created_at      TEXT NOT NULL
) STRICT;

CREATE INDEX devices_user ON devices (user_ulid);

CREATE TABLE pairing_codes (
  hash       TEXT PRIMARY KEY,
  user_ulid  TEXT NOT NULL REFERENCES users (ulid),
  created_by TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at    TEXT
) STRICT;

CREATE TABLE plugins (
  id           TEXT PRIMARY KEY,
  version      TEXT NOT NULL,
  manifest     TEXT NOT NULL,
  sha256       TEXT NOT NULL,
  enabled      INTEGER NOT NULL,
  installed_by TEXT NOT NULL DEFAULT '',
  installed_at TEXT NOT NULL
) STRICT;

CREATE TABLE wrapped_keys (
  tenant_id  TEXT NOT NULL,
  version    INTEGER NOT NULL,
  wrapped    BLOB NOT NULL,
  active     INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (tenant_id, version)
) STRICT;

CREATE UNIQUE INDEX wrapped_keys_one_active ON wrapped_keys (tenant_id) WHERE active = 1;

CREATE TABLE worker_tokens (
  name       TEXT PRIMARY KEY,
  hash       TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
) STRICT;

CREATE TABLE workers (
  id           TEXT PRIMARY KEY,
  pools        TEXT NOT NULL,
  resources    TEXT NOT NULL,
  version      TEXT NOT NULL DEFAULT '',
  running      INTEGER NOT NULL DEFAULT 0,
  last_seen_at TEXT NOT NULL
) STRICT;

-- Same shape and rules as the tenant chain (I107, I108).
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
