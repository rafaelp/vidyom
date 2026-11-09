package publicexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
	"github.com/rafaelp/vidyom/schemas"
)

const (
	finalName   = "public.db"
	tempPattern = "public.db.tmp-*"
)

type meta struct {
	generatedAt    time.Time
	embeddingModel string
	dimensions     int
}

// writeFile builds the database in a temporary file beside the target and renames
// it into place. The result is one self-contained file: checkpointed, in rollback
// journal mode, with no side files, so a read-only reader needs nothing else.
func writeFile(ctx context.Context, dir string, tenant domain.TenantID, videos []*video, m meta) (connector.PublicArtifact, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return connector.PublicArtifact{}, errs.Wrap(errs.CodeUnavailable, err, "cannot create the tenant's data directory")
	}
	removeStaleTemp(dir)
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return connector.PublicArtifact{}, errs.Wrap(errs.CodeUnavailable, err, "cannot create the public.db temporary file")
	}
	path := tmp.Name()
	tmp.Close()
	defer removeTemp(path)

	if err := build(ctx, path, videos, m); err != nil {
		return connector.PublicArtifact{}, err
	}
	sum, size, err := seal(path)
	if err != nil {
		return connector.PublicArtifact{}, err
	}
	target := filepath.Join(dir, finalName)
	if err := os.Rename(path, target); err != nil {
		return connector.PublicArtifact{}, errs.Wrap(errs.CodeUnavailable, err, "cannot replace public.db")
	}
	if err := syncDir(dir); err != nil {
		return connector.PublicArtifact{}, errs.Wrap(errs.CodeUnavailable, err, "cannot sync the tenant's data directory")
	}
	return connector.PublicArtifact{
		Tenant:        tenant,
		Path:          target,
		SHA256:        sum,
		SizeBytes:     size,
		SchemaVersion: schemas.PublicDBSchemaVersion,
	}, nil
}

func removeTemp(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}
}

func build(ctx context.Context, path string, videos []*video, m meta) (err error) {
	db, err := sqlitedriver.Open(path, sqlitedriver.Options{MaxOpenConns: 1})
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot open the public.db temporary file")
	}
	defer func() {
		if closeErr := db.Close(); err == nil && closeErr != nil {
			err = errs.Wrap(errs.CodeUnavailable, closeErr, "cannot close public.db")
		}
	}()

	schema, err := schemas.PublicDBSchema()
	if err != nil {
		return fmt.Errorf("read the public.db schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply the public.db schema: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin writing public.db: %w", err)
	}
	defer tx.Rollback()
	if err := insertMeta(ctx, tx, m); err != nil {
		return err
	}
	w, err := newWriter(ctx, tx)
	if err != nil {
		return err
	}
	defer w.close()
	for _, v := range videos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.video(ctx, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit public.db: %w", err)
	}

	// One self-contained file: nothing left in a WAL, and a header a reader on a
	// read-only mount can use. sqlitedriver opens in WAL, so this has to be undone.
	for _, stmt := range []string{
		"INSERT INTO search_fts(search_fts) VALUES('optimize')",
		"PRAGMA wal_checkpoint(TRUNCATE)",
		"PRAGMA journal_mode = DELETE",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("finish public.db: %s: %w", stmt, err)
		}
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("check public.db: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("public.db failed its integrity check: %s", integrity)
	}
	return nil
}

func insertMeta(ctx context.Context, tx *sql.Tx, m meta) error {
	rows := [][2]string{
		{"schema_version", strconv.Itoa(schemas.PublicDBSchemaVersion)},
		{"generated_at", m.generatedAt.UTC().Format(time.RFC3339)},
		{"embedding_model", m.embeddingModel},
		{"embedding_dimensions", strconv.Itoa(m.dimensions)},
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)`, r[0], r[1]); err != nil {
			return fmt.Errorf("write public.db meta: %w", err)
		}
	}
	return nil
}

// writer holds prepared statements for one transaction.
type writer struct {
	video_, destination, chapter, segment, embedding, fts *sql.Stmt
}

func newWriter(ctx context.Context, tx *sql.Tx) (*writer, error) {
	w := &writer{}
	for _, s := range []struct {
		dst **sql.Stmt
		sql string
	}{
		{&w.video_, `INSERT INTO videos (public_id, title, description, kind, category, language, speaker, tags, duration_seconds, published_at, thumbnail_url, source_group)
			VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?)`},
		{&w.destination, `INSERT OR IGNORE INTO destinations (public_id, destination, url, embed_url, supports_timestamps) VALUES (?, ?, ?, ?, ?)`},
		{&w.chapter, `INSERT INTO chapters (public_id, position, title, start_seconds, end_seconds, summary) VALUES (?, ?, ?, ?, ?, ?)`},
		{&w.segment, `INSERT INTO transcript_segments (public_id, position, start_seconds, end_seconds, text) VALUES (?, ?, ?, ?, ?)`},
		{&w.embedding, `INSERT INTO embeddings (public_id, position, level, start_seconds, end_seconds, text, vector) VALUES (?, ?, ?, ?, ?, ?, ?)`},
		{&w.fts, `INSERT INTO search_fts (public_id, source, start_seconds, end_seconds, text) VALUES (?, ?, ?, ?, ?)`},
	} {
		stmt, err := tx.PrepareContext(ctx, s.sql)
		if err != nil {
			w.close()
			return nil, fmt.Errorf("prepare public.db statement: %w", err)
		}
		*s.dst = stmt
	}
	return w, nil
}

func (w *writer) close() {
	for _, s := range []*sql.Stmt{w.video_, w.destination, w.chapter, w.segment, w.embedding, w.fts} {
		if s != nil {
			s.Close()
		}
	}
}

func (w *writer) video(ctx context.Context, v *video) error {
	tags, err := tagsJSON(v.tags)
	if err != nil {
		return fmt.Errorf("encode tags: %w", err)
	}
	a := v.asset
	if _, err := w.video_.ExecContext(ctx, v.publicID, v.title, v.description, string(a.Kind), a.Category, a.Language,
		tags, a.DurationSeconds, v.publishedAt.UTC().Format(time.RFC3339), v.thumbnail, v.sourceGroup); err != nil {
		return fmt.Errorf("write a public.db video: %w", err)
	}
	for _, d := range v.links {
		ts := 0
		if d.timestamps {
			ts = 1
		}
		if _, err := w.destination.ExecContext(ctx, v.publicID, d.driver, d.url, d.embed, ts); err != nil {
			return fmt.Errorf("write a public.db destination: %w", err)
		}
	}
	if err := w.search(ctx, v.publicID, "title", v.title, nil, nil); err != nil {
		return err
	}
	if v.description != "" {
		if err := w.search(ctx, v.publicID, "description", v.description, nil, nil); err != nil {
			return err
		}
	}
	for i, c := range v.chapters {
		if _, err := w.chapter.ExecContext(ctx, v.publicID, i, c.Title, c.StartSeconds, c.EndSeconds, c.Summary); err != nil {
			return fmt.Errorf("write a public.db chapter: %w", err)
		}
		text := strings.TrimSpace(c.Title + ". " + c.Summary)
		if err := w.search(ctx, v.publicID, "chapter", text, c.StartSeconds, c.EndSeconds); err != nil {
			return err
		}
	}
	for i, s := range v.segments {
		if _, err := w.segment.ExecContext(ctx, v.publicID, i, s.Start, s.End, s.Text); err != nil {
			return fmt.Errorf("write a public.db transcript segment: %w", err)
		}
		if err := w.search(ctx, v.publicID, "transcript", s.Text, s.Start, s.End); err != nil {
			return err
		}
	}
	for i, c := range v.vectors {
		blob, err := sqlitedriver.SerializeVector(c.Vector)
		if err != nil {
			return fmt.Errorf("encode a vector: %w", err)
		}
		level := c.Level
		if level == "" {
			level = domain.ChunkWindow
		}
		if _, err := w.embedding.ExecContext(ctx, v.publicID, i, string(level), c.StartSeconds, c.EndSeconds, c.Text, blob); err != nil {
			return fmt.Errorf("write a public.db embedding: %w", err)
		}
	}
	return nil
}

func (w *writer) search(ctx context.Context, publicID, source, text string, start, end any) error {
	if _, err := w.fts.ExecContext(ctx, publicID, source, start, end, text); err != nil {
		return fmt.Errorf("write a public.db search row: %w", err)
	}
	return nil
}

// seal proves the file is one self-contained database, flushes it and hashes it.
func seal(path string) (string, int64, error) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return "", 0, fmt.Errorf("public.db left a side file %s", suffix)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", 0, errs.Wrap(errs.CodeUnavailable, err, "cannot reopen public.db")
	}
	defer f.Close()

	// Bytes 18 and 19 of the header are the read and write versions; 1 is rollback
	// journal mode, 2 would be WAL.
	header := make([]byte, 20)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", 0, fmt.Errorf("read the public.db header: %w", err)
	}
	if !bytes.Equal(header[18:20], []byte{1, 1}) {
		return "", 0, errors.New("public.db is not in rollback journal mode")
	}
	if err := f.Chmod(0o600); err != nil {
		return "", 0, errs.Wrap(errs.CodeUnavailable, err, "cannot set the mode of public.db")
	}
	if err := f.Sync(); err != nil {
		return "", 0, errs.Wrap(errs.CodeUnavailable, err, "cannot flush public.db")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, fmt.Errorf("rewind public.db: %w", err)
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash public.db: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// syncDir makes a rename durable. Windows cannot flush a directory handle.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
