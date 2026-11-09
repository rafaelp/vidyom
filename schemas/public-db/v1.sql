-- public.db, schema version 1: the read-only public dataset of one tenant (D18).
--
-- Written by internal/publicexport, read by library/. The writer rebuilds the
-- whole file on each sync and replaces it atomically, so there are no migrations:
-- a new schema version is a new file here, and meta.schema_version tells readers.
--
-- It holds only approved, published, public records (§13.2). It must never hold a
-- storage key, bucket, ULID, friendly ID, or anything from a private original
-- (R44, §15.6). `public_id` and `source_group` are opaque, derived with an HMAC
-- over the tenant's key (I29). Segments marked sensitive are absent (D30).

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) WITHOUT ROWID;
-- Required keys: schema_version ('1'), generated_at (RFC 3339, UTC),
-- embedding_model ('bge-m3@1', 'fake-hash-embed@1', or '' when there are no
-- vectors), embedding_dimensions.

CREATE TABLE videos (
  public_id        TEXT PRIMARY KEY,
  title            TEXT NOT NULL,
  description      TEXT NOT NULL DEFAULT '',
  kind             TEXT NOT NULL CHECK (kind IN ('clip', 'edited', 'original')),
  category         TEXT NOT NULL DEFAULT '',
  language         TEXT NOT NULL DEFAULT '',
  speaker          TEXT NOT NULL DEFAULT '',
  tags             TEXT NOT NULL DEFAULT '[]', -- JSON array of strings
  duration_seconds REAL NOT NULL DEFAULT 0,
  published_at     TEXT NOT NULL,              -- RFC 3339, UTC
  thumbnail_url    TEXT NOT NULL DEFAULT '',   -- from the destination, never a bucket
  -- Clips of the same source share a group, so the site can relate them without
  -- exposing the private original (§13.2).
  source_group     TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- One row per public publication. URLs come only from confirmed API responses (D45).
CREATE TABLE destinations (
  public_id           TEXT NOT NULL REFERENCES videos(public_id) ON DELETE CASCADE,
  destination         TEXT NOT NULL, -- driver name, e.g. 'youtube' or 'mock'
  url                 TEXT NOT NULL,
  embed_url           TEXT NOT NULL DEFAULT '',
  supports_timestamps INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (public_id, destination, url)
) WITHOUT ROWID;

CREATE TABLE chapters (
  public_id     TEXT NOT NULL REFERENCES videos(public_id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,
  title         TEXT NOT NULL,
  start_seconds REAL NOT NULL,
  end_seconds   REAL NOT NULL,
  summary       TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (public_id, position)
) WITHOUT ROWID;

-- Only for approved public videos, without sensitive segments (D30).
CREATE TABLE transcript_segments (
  public_id     TEXT NOT NULL REFERENCES videos(public_id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,
  start_seconds REAL NOT NULL,
  end_seconds   REAL NOT NULL,
  text          TEXT NOT NULL,
  PRIMARY KEY (public_id, position)
) WITHOUT ROWID;

-- Vectors are little-endian float32 BLOBs of meta.embedding_dimensions, so the
-- site needs no SQLite extension and can scan with cosine similarity (I29).
CREATE TABLE embeddings (
  public_id     TEXT NOT NULL REFERENCES videos(public_id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,
  level         TEXT NOT NULL CHECK (level IN ('window', 'chapter')),
  start_seconds REAL NOT NULL,
  end_seconds   REAL NOT NULL,
  text          TEXT NOT NULL,
  vector        BLOB NOT NULL,
  PRIMARY KEY (public_id, position)
) WITHOUT ROWID;

-- Lexical search over titles, descriptions, chapters and released transcript text.
CREATE VIRTUAL TABLE search_fts USING fts5(
  public_id UNINDEXED,
  source UNINDEXED,        -- 'title', 'description', 'chapter' or 'transcript'
  start_seconds UNINDEXED,
  end_seconds UNINDEXED,
  text,
  tokenize = 'unicode61 remove_diacritics 2'
);

CREATE INDEX videos_published_at ON videos(published_at DESC);
CREATE INDEX videos_source_group ON videos(source_group);
