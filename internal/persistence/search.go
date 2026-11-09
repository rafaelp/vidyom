package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
)

// searchRepo is FTS5 and sqlite-vec (D5, I106).
type searchRepo struct{ conn }

var _ repo.SearchRepo = searchRepo{}

const (
	activeModelKey    = "active_embedding_model"
	defaultSearchHits = 20
	maxSearchHits     = 1000
	// maxKNN is the largest k a vec0 table accepts by default.
	maxKNN = 4096
)

func hasLexical(ctx context.Context, q querier, assetULID, version string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM search_chunks WHERE asset_ulid = ? AND version = ? LIMIT 1`, assetULID, version).Scan(&one)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, mapErr(err)
	}
	return true, nil
}

func activeModel(ctx context.Context, q querier) (string, error) {
	var model string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, activeModelKey).Scan(&model)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", mapErr(err)
	}
	return model, nil
}

// vectorModelFor returns the model with vectors for this version: the active one if
// it qualifies, otherwise the most recently registered (I105).
func vectorModelFor(ctx context.Context, q querier, assetULID, version string) (string, error) {
	models, err := queryAll(ctx, q, `SELECT m.model FROM embedding_models m
		WHERE EXISTS (SELECT 1 FROM vector_chunks c WHERE c.model = m.model AND c.asset_ulid = ? AND c.version = ?)
		ORDER BY m.rowid DESC`, []any{assetULID, version},
		func(s scanner) (string, error) {
			var m string
			err := s.Scan(&m)
			return m, err
		})
	if err != nil || len(models) == 0 {
		return "", err
	}
	active, err := activeModel(ctx, q)
	if err != nil {
		return "", err
	}
	for _, m := range models {
		if m == active {
			return m, nil
		}
	}
	return models[0], nil
}

// IndexLexical replaces the lexical chunks of one asset version.
func (r searchRepo) IndexLexical(ctx context.Context, asset id.Ref, version string, chunks []domain.SearchChunk) error {
	if version == "" {
		return invalidArg("lexical indexing needs a version")
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM search_chunks WHERE asset_ulid = ? AND version = ?`, key.ulid, version); err != nil {
			return mapErr(err)
		}
		for _, c := range chunks {
			level := c.Level
			if level == "" {
				level = domain.ChunkWindow
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO search_chunks (asset_ulid, version, level, start_seconds, end_seconds, text) VALUES (?, ?, ?, ?, ?, ?)`,
				key.ulid, version, string(level), c.StartSeconds, c.EndSeconds, c.Text); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// vectorTable names a model's vec0 table. The name comes from a hash, so nothing a
// model string contains can reach SQL.
func vectorTable(model string) string {
	sum := sha256.Sum256([]byte(model))
	return "vec_" + hex.EncodeToString(sum[:8])
}

type modelRow struct {
	model      string
	dimensions int
	table      string
}

func getModel(ctx context.Context, q querier, model string) (modelRow, bool, error) {
	m := modelRow{model: model}
	err := q.QueryRowContext(ctx, `SELECT dimensions, table_name FROM embedding_models WHERE model = ?`, model).Scan(&m.dimensions, &m.table)
	if err != nil {
		if isNoRows(err) {
			return modelRow{}, false, nil
		}
		return modelRow{}, false, mapErr(err)
	}
	return m, true, nil
}

// ensureModel registers a model and creates its vec0 table on first use. A model's
// dimensions never change (R18).
func (r searchRepo) ensureModel(ctx context.Context, q querier, model string, dimensions int) (modelRow, error) {
	m, found, err := getModel(ctx, q, model)
	if err != nil {
		return modelRow{}, err
	}
	if found {
		if m.dimensions != dimensions {
			return modelRow{}, invalidArg("model %s has %d dimensions, got %d", model, m.dimensions, dimensions)
		}
		return m, nil
	}
	m = modelRow{model: model, dimensions: dimensions, table: vectorTable(model)}
	// Partition per asset for asset-scoped search (D5); a small chunk size keeps the
	// allocation per partition near 0.5 MiB at 1024 dimensions (I106). Only integers
	// and our hashed table name reach this statement.
	ddl := fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING vec0(asset TEXT PARTITION KEY, embedding FLOAT[%d] distance_metric=cosine, chunk_size=128)`, m.table, dimensions)
	if _, err := q.ExecContext(ctx, ddl); err != nil {
		return modelRow{}, mapErr(err)
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO embedding_models (model, dimensions, table_name, created_at) VALUES (?, ?, ?, ?)`,
		model, dimensions, m.table, timeText(r.now())); err != nil {
		return modelRow{}, mapErr(err)
	}
	return m, nil
}

// IndexVectors replaces the vectors of one asset version for one model. The first
// model written to a tenant becomes active; later ones wait for SetActiveModel, so
// searches keep using the old model until reindexing completes (D15).
func (r searchRepo) IndexVectors(ctx context.Context, batch repo.EmbeddingBatch) error {
	if batch.Model == "" {
		return invalidArg("vectors need an embedding model (R18)")
	}
	if batch.Version == "" {
		return invalidArg("vectors need a version")
	}
	dimensions := 0
	for _, c := range batch.Chunks {
		if len(c.Vector) == 0 {
			return invalidArg("a chunk has an empty vector")
		}
		if dimensions == 0 {
			dimensions = len(c.Vector)
		} else if len(c.Vector) != dimensions {
			return invalidArg("vectors of one batch must share their dimensions")
		}
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, batch.AssetID)
		if err != nil {
			return err
		}
		if dimensions == 0 {
			// Nothing to write except removing what a previous attempt left.
			m, found, err := getModel(ctx, q, batch.Model)
			if err != nil || !found {
				return err
			}
			return deleteVectors(ctx, q, m, key.ulid, batch.Version)
		}
		m, err := r.ensureModel(ctx, q, batch.Model, dimensions)
		if err != nil {
			return err
		}
		if err := deleteVectors(ctx, q, m, key.ulid, batch.Version); err != nil {
			return err
		}
		for _, c := range batch.Chunks {
			level := c.Level
			if level == "" {
				level = domain.ChunkWindow
			}
			res, err := q.ExecContext(ctx, `INSERT INTO vector_chunks (model, asset_ulid, version, chunking, level, start_seconds, end_seconds, text) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				batch.Model, key.ulid, batch.Version, batch.Chunking, string(level), c.StartSeconds, c.EndSeconds, c.Text)
			if err != nil {
				return mapErr(err)
			}
			chunkID, err := res.LastInsertId()
			if err != nil {
				return mapErr(err)
			}
			blob, err := sqlitedriver.SerializeVector(c.Vector)
			if err != nil {
				return fmt.Errorf("serialize vector: %w", err)
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO `+m.table+` (rowid, asset, embedding) VALUES (?, ?, ?)`, chunkID, key.ulid, blob); err != nil {
				return mapErr(err)
			}
		}
		active, err := activeModel(ctx, q)
		if err != nil {
			return err
		}
		if active == "" {
			return setActiveModel(ctx, q, batch.Model)
		}
		return nil
	})
}

func deleteVectors(ctx context.Context, q querier, m modelRow, assetULID, version string) error {
	ids, err := queryAll(ctx, q, `SELECT id FROM vector_chunks WHERE model = ? AND asset_ulid = ? AND version = ?`, []any{m.model, assetULID, version}, scanInt64)
	if err != nil {
		return err
	}
	for _, chunkID := range ids {
		if _, err := q.ExecContext(ctx, `DELETE FROM `+m.table+` WHERE rowid = ?`, chunkID); err != nil {
			return mapErr(err)
		}
	}
	_, err = q.ExecContext(ctx, `DELETE FROM vector_chunks WHERE model = ? AND asset_ulid = ? AND version = ?`, m.model, assetULID, version)
	return mapErr(err)
}

func scanInt64(s scanner) (int64, error) {
	var v int64
	err := s.Scan(&v)
	return v, err
}

// ftsQuery turns user text into a safe FTS5 expression: each token quoted, joined
// by OR, so nothing typed can be read as FTS5 syntax (I106).
func ftsQuery(text string) string {
	fields := strings.FieldsFunc(text, func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsDigit(c)
	})
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+f+`"`)
	}
	return strings.Join(quoted, " OR ")
}

func hitLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultSearchHits
	case limit > maxSearchHits:
		return maxSearchHits
	default:
		return limit
	}
}

// SearchLexical runs FTS5 over each asset's current version.
func (r searchRepo) SearchLexical(ctx context.Context, lq repo.LexicalQuery) ([]domain.SearchHit, error) {
	expr := ftsQuery(lq.Text)
	if expr == "" {
		return nil, nil
	}
	var out []domain.SearchHit
	err := r.read(ctx, func(q querier) error {
		query := `SELECT a.friendly_id, c.version, c.level, c.start_seconds, c.end_seconds, c.text, -bm25(search_fts)
			FROM search_fts
			JOIN search_chunks c ON c.id = search_fts.rowid
			JOIN assets a ON a.ulid = c.asset_ulid
			WHERE search_fts MATCH ? AND a.deleted_at IS NULL AND c.version = a.current_version`
		args := []any{expr}
		if lq.AssetID != "" {
			key, err := resolveAsset(ctx, q, lq.AssetID)
			if err != nil {
				return err
			}
			query += ` AND a.ulid = ?`
			args = append(args, key.ulid)
		}
		query += ` ORDER BY bm25(search_fts) LIMIT ?`
		args = append(args, hitLimit(lq.Limit))
		var err error
		out, err = queryAll(ctx, q, query, args, func(s scanner) (domain.SearchHit, error) {
			h := domain.SearchHit{Source: "lexical"}
			err := s.Scan(&h.AssetID, &h.Version, &h.Level, &h.StartSeconds, &h.EndSeconds, &h.Text, &h.Score)
			return h, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type knnResult struct {
	chunkID  int64
	distance float64
}

// knn asks one vec0 table for the k nearest neighbors, in one partition when asset
// is set.
func knn(ctx context.Context, q querier, m modelRow, blob []byte, k int, asset string) ([]knnResult, error) {
	query := `SELECT rowid, distance FROM ` + m.table + ` WHERE embedding MATCH ? AND k = ?`
	args := []any{blob, k}
	if asset != "" {
		query += ` AND asset = ?`
		args = append(args, asset)
	}
	query += ` ORDER BY distance`
	return queryAll(ctx, q, query, args, func(s scanner) (knnResult, error) {
		var v knnResult
		err := s.Scan(&v.chunkID, &v.distance)
		return v, err
	})
}

// scanTargets returns the partitions a query must visit. Asset-scoped is one; tenant
// wide is all of them, unless the model exceeds the scan cap, when only the newest
// assets up to the cap are visited, one partition at a time (I43, I106).
func (r searchRepo) scanTargets(ctx context.Context, q querier, m modelRow, asset string) ([]string, error) {
	if asset != "" {
		return []string{asset}, nil
	}
	var total int64
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM vector_chunks WHERE model = ?`, m.model).Scan(&total); err != nil {
		return nil, mapErr(err)
	}
	if total <= int64(r.d.opts.VectorScanCap) {
		return []string{""}, nil
	}
	type partition struct {
		asset string
		count int64
	}
	partitions, err := queryAll(ctx, q, `SELECT c.asset_ulid, count(*) FROM vector_chunks c JOIN assets a ON a.ulid = c.asset_ulid
		WHERE c.model = ? AND a.deleted_at IS NULL GROUP BY c.asset_ulid ORDER BY a.rowid DESC`, []any{m.model},
		func(s scanner) (partition, error) {
			var p partition
			err := s.Scan(&p.asset, &p.count)
			return p, err
		})
	if err != nil {
		return nil, err
	}
	var (
		targets []string
		scanned int64
	)
	for _, p := range partitions {
		if scanned+p.count > int64(r.d.opts.VectorScanCap) && len(targets) > 0 {
			break
		}
		targets = append(targets, p.asset)
		scanned += p.count
	}
	return targets, nil
}

// SearchVector runs KNN over one model only (R18). Hits are limited to each asset's
// current version and exclude deleted assets; because that filter runs after KNN, it
// over-fetches until it has enough hits or the table has no more.
func (r searchRepo) SearchVector(ctx context.Context, vq repo.VectorQuery) ([]domain.SearchHit, error) {
	if len(vq.Vector) == 0 {
		return nil, invalidArg("vector search needs a query vector")
	}
	if vq.Model == "" {
		return nil, invalidArg("vector search must name the embedding model (R18)")
	}
	limit := hitLimit(vq.Limit)
	var out []domain.SearchHit
	err := r.read(ctx, func(q querier) error {
		asset := ""
		if vq.AssetID != "" {
			key, err := resolveAsset(ctx, q, vq.AssetID)
			if err != nil {
				return err
			}
			asset = key.ulid
		}
		m, found, err := getModel(ctx, q, vq.Model)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if m.dimensions != len(vq.Vector) {
			return invalidArg("model %s has %d dimensions, the query has %d", vq.Model, m.dimensions, len(vq.Vector))
		}
		blob, err := sqlitedriver.SerializeVector(vq.Vector)
		if err != nil {
			return fmt.Errorf("serialize vector: %w", err)
		}
		targets, err := r.scanTargets(ctx, q, m, asset)
		if err != nil {
			return err
		}
		for k := min(limit*2, maxKNN); ; k = min(k*4, maxKNN) {
			var results []knnResult
			exhausted := true
			for _, target := range targets {
				part, err := knn(ctx, q, m, blob, k, target)
				if err != nil {
					return err
				}
				if len(part) == k {
					exhausted = false
				}
				results = append(results, part...)
			}
			sort.Slice(results, func(i, j int) bool { return results[i].distance < results[j].distance })
			out, err = vectorHits(ctx, q, results, limit)
			if err != nil {
				return err
			}
			if len(out) >= limit || exhausted || k == maxKNN {
				return nil
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// vectorHits loads chunk metadata for KNN results, in distance order.
func vectorHits(ctx context.Context, q querier, results []knnResult, limit int) ([]domain.SearchHit, error) {
	var out []domain.SearchHit
	for _, res := range results {
		if len(out) == limit {
			break
		}
		h := domain.SearchHit{Source: "vector", Score: 1 - res.distance}
		err := q.QueryRowContext(ctx, `SELECT a.friendly_id, c.version, c.level, c.start_seconds, c.end_seconds, c.text
			FROM vector_chunks c JOIN assets a ON a.ulid = c.asset_ulid
			WHERE c.id = ? AND a.deleted_at IS NULL AND c.version = a.current_version`, res.chunkID).
			Scan(&h.AssetID, &h.Version, &h.Level, &h.StartSeconds, &h.EndSeconds, &h.Text)
		if err != nil {
			if isNoRows(err) {
				continue
			}
			return nil, mapErr(err)
		}
		out = append(out, h)
	}
	return out, nil
}

// DeleteAsset removes an asset's chunks and vectors of every model and version.
func (r searchRepo) DeleteAsset(ctx context.Context, asset id.Ref) error {
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM search_chunks WHERE asset_ulid = ?`, key.ulid); err != nil {
			return mapErr(err)
		}
		models, err := queryAll(ctx, q, `SELECT DISTINCT c.model, m.dimensions, m.table_name FROM vector_chunks c JOIN embedding_models m ON m.model = c.model WHERE c.asset_ulid = ?`,
			[]any{key.ulid}, func(s scanner) (modelRow, error) {
				var m modelRow
				err := s.Scan(&m.model, &m.dimensions, &m.table)
				return m, err
			})
		if err != nil {
			return err
		}
		for _, m := range models {
			ids, err := queryAll(ctx, q, `SELECT id FROM vector_chunks WHERE model = ? AND asset_ulid = ?`, []any{m.model, key.ulid}, scanInt64)
			if err != nil {
				return err
			}
			for _, chunkID := range ids {
				if _, err := q.ExecContext(ctx, `DELETE FROM `+m.table+` WHERE rowid = ?`, chunkID); err != nil {
					return mapErr(err)
				}
			}
		}
		_, err = q.ExecContext(ctx, `DELETE FROM vector_chunks WHERE asset_ulid = ?`, key.ulid)
		return mapErr(err)
	})
}

// DeleteModel drops a model's vectors. The active model cannot be dropped: searches
// would have nothing to use (D15).
func (r searchRepo) DeleteModel(ctx context.Context, model string) error {
	return r.write(ctx, func(q querier) error {
		active, err := activeModel(ctx, q)
		if err != nil {
			return err
		}
		if active == model {
			return errs.Newf(errs.CodeFailedPrecondition, "model %s is active: switch to another before deleting it", model)
		}
		m, found, err := getModel(ctx, q, model)
		if err != nil {
			return err
		}
		if !found {
			return errs.NotFound("embedding model")
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM vector_chunks WHERE model = ?`, model); err != nil {
			return mapErr(err)
		}
		if _, err := q.ExecContext(ctx, `DROP TABLE `+m.table); err != nil {
			return mapErr(err)
		}
		_, err = q.ExecContext(ctx, `DELETE FROM embedding_models WHERE model = ?`, model)
		return mapErr(err)
	})
}

func (r searchRepo) ActiveModel(ctx context.Context) (string, error) {
	return activeModel(ctx, r.q())
}

func setActiveModel(ctx context.Context, q querier, model string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, activeModelKey, model)
	return mapErr(err)
}

// SetActiveModel swaps searches over to a model that has vectors (D15).
func (r searchRepo) SetActiveModel(ctx context.Context, model string) error {
	return r.write(ctx, func(q querier) error {
		_, found, err := getModel(ctx, q, model)
		if err != nil {
			return err
		}
		if !found {
			return errs.NotFound("embedding model")
		}
		return setActiveModel(ctx, q, model)
	})
}

func (r searchRepo) HasIndex(ctx context.Context, asset id.Ref, version, model string) (lexical, vector bool, err error) {
	err = r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		lexical, err = hasLexical(ctx, q, key.ulid, version)
		if err != nil {
			return err
		}
		var one int
		err = q.QueryRowContext(ctx, `SELECT 1 FROM vector_chunks WHERE model = ? AND asset_ulid = ? AND version = ? LIMIT 1`, model, key.ulid, version).Scan(&one)
		switch {
		case err == nil:
			vector = true
			return nil
		case isNoRows(err):
			return nil
		default:
			return mapErr(err)
		}
	})
	return lexical, vector, err
}
