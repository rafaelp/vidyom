import { readConfig, type LibraryConfig } from '../config';
import { getDataset, type DatasetManager, type Generation } from '../dataset';
import { resolveEmbedder } from '../embedders/registry';
import type { EmbedderResolution, QueryEmbedder } from '../embedders/types';
import { destinationUrlAt, pickPrimary, videoPath } from '../links';
import { filterClause, getDestinations, getVideosByIds, hasFilters } from '../queries';
import type { VideoFilters, VideoSummary } from '../types';
import { parseQuery } from './fts-query';
import { reciprocalRankFusion } from './fusion';
import { lexicalSearch, type LexicalHit } from './lexical';
import { metadataSearch, type MetadataMatches } from './metadata';
import { scanVectors, vectorIndexFor, type ScanHit } from './semantic';
import type {
  MatchedBy,
  MatchedIn,
  SearchMoment,
  SearchRequest,
  SearchResponse,
  SearchResult,
  SemanticInfo,
  SemanticReason,
  SnippetSegment,
} from './types';

/**
 * The single search entry point, used by the API route and the SSR page. Lexical
 * always; semantic when an embedder for the file's `embedding_model` is available;
 * the active mode and the reason it is not hybrid are always reported (I250).
 */

const MOMENTS_PER_RESULT = 3;
const SEMANTIC_TEXT_LIMIT = 220;

export interface SearchDependencies {
  dataset?: DatasetManager;
  config?: LibraryConfig;
  resolve?: typeof resolveEmbedder;
}

interface SemanticPlan {
  info: SemanticInfo;
  embedder?: QueryEmbedder;
  vector?: Float32Array;
}

function unavailable(reason: SemanticReason, model: string | null): SemanticPlan {
  return { info: { available: false, model, test_only: false, reason } };
}

async function embedWithTimeout(
  embedder: QueryEmbedder,
  text: string,
  timeoutMs: number,
): Promise<Float32Array> {
  const controller = new AbortController();
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      controller.abort();
      reject(new Error('query embedding timed out'));
    }, timeoutMs);
  });
  try {
    return await Promise.race([embedder.embed(text, controller.signal), timeout]);
  } finally {
    clearTimeout(timer);
  }
}

async function planSemantic(
  request: SearchRequest,
  dataset: DatasetManager,
  config: LibraryConfig,
  resolve: typeof resolveEmbedder,
): Promise<SemanticPlan> {
  const { model, dimensions, indexState } = dataset.withSnapshot((generation) => {
    const meta = generation.meta;
    const usable = meta.embeddingModel !== '' && config.semanticEnabled && request.mode === 'auto';
    return {
      model: meta.embeddingModel,
      dimensions: meta.embeddingDimensions,
      indexState: usable
        ? vectorIndexFor(generation, meta.embeddingDimensions, config.maxVectors)
        : undefined,
    };
  });
  const fileModel = model === '' ? null : model;

  if (request.mode === 'lexical') return unavailable('not_requested', fileModel);
  if (!config.semanticEnabled) return unavailable('disabled', fileModel);
  if (!fileModel || !indexState) return unavailable('no_vectors', fileModel);
  if (!indexState.ok) return unavailable(indexState.reason, fileModel);

  const resolution: EmbedderResolution = await resolve(fileModel, dimensions, config);
  if (!resolution.ok) return unavailable(resolution.reason, fileModel);

  try {
    const vector = await embedWithTimeout(resolution.embedder, request.query, config.embedTimeoutMs);
    if (vector.length !== dimensions) return unavailable('dimension_mismatch', fileModel);
    return {
      embedder: resolution.embedder,
      vector,
      info: {
        available: true,
        model: fileModel,
        test_only: resolution.embedder.testOnly,
        reason: null,
      },
    };
  } catch {
    return unavailable('embedder_error', fileModel);
  }
}

function allowedIds(generation: Generation, filters: VideoFilters): Set<string> | undefined {
  if (!hasFilters(filters)) return undefined;
  const { sql, params } = filterClause(filters);
  return new Set(
    generation.db
      .all<{ public_id: string }>(`SELECT v.public_id FROM videos v WHERE ${sql}`, ...params)
      .map((row) => row.public_id),
  );
}

interface MomentDraft {
  source: 'chapter' | 'transcript';
  start: number;
  end: number;
  snippet: SnippetSegment[];
  matchedBy: MatchedBy[];
}

interface SemanticRow {
  publicId: string;
  similarity: number;
  source: 'chapter' | 'transcript';
  start: number;
  end: number;
  text: string;
}

function truncate(text: string): string {
  const flat = text.replace(/\s+/g, ' ').trim();
  if (flat.length <= SEMANTIC_TEXT_LIMIT) return flat;
  const cut = flat.slice(0, SEMANTIC_TEXT_LIMIT);
  const space = cut.lastIndexOf(' ');
  return `${space > 0 ? cut.slice(0, space) : cut}…`;
}

function semanticRows(generation: Generation, hits: readonly ScanHit[]): SemanticRow[] {
  const rows: SemanticRow[] = [];
  for (const hit of hits) {
    const row = generation.db.get<{
      level: string;
      start_seconds: number;
      end_seconds: number;
      text: string;
    }>(
      'SELECT level, start_seconds, end_seconds, text FROM embeddings WHERE public_id = ? AND position = ?',
      hit.publicId,
      hit.position,
    );
    if (!row) continue;
    rows.push({
      publicId: hit.publicId,
      similarity: hit.similarity,
      source: row.level === 'chapter' ? 'chapter' : 'transcript',
      start: row.start_seconds,
      end: row.end_seconds,
      text: row.text,
    });
  }
  return rows;
}

function overlaps(a: MomentDraft, start: number, end: number): boolean {
  return a.start < end && start < a.end;
}

/** Lexical moments first, then semantic ones; an overlapping pair becomes one. */
function draftMoments(lexical: readonly LexicalHit[], semantic: readonly SemanticRow[]): MomentDraft[] {
  const drafts: MomentDraft[] = [];
  const sorted = [...lexical].sort((a, b) => b.score - a.score);
  for (const hit of sorted) {
    if (hit.source !== 'chapter' && hit.source !== 'transcript') continue;
    if (hit.startSeconds === null) continue;
    drafts.push({
      source: hit.source,
      start: hit.startSeconds,
      end: hit.endSeconds ?? hit.startSeconds,
      snippet: hit.snippet,
      matchedBy: ['lexical'],
    });
  }
  for (const row of semantic) {
    const existing = drafts.find((draft) => overlaps(draft, row.start, row.end));
    if (existing) {
      if (!existing.matchedBy.includes('semantic')) existing.matchedBy.push('semantic');
      continue;
    }
    drafts.push({
      source: row.source,
      start: row.start,
      end: row.end,
      snippet: [{ text: truncate(row.text), match: false }],
      matchedBy: ['semantic'],
    });
  }
  return drafts;
}

function rankLexical(hits: readonly LexicalHit[], metadata: MetadataMatches): string[] {
  const best = new Map<string, number>();
  for (const hit of hits) best.set(hit.publicId, Math.max(best.get(hit.publicId) ?? -Infinity, hit.score));
  const byScore = (a: string, b: string) =>
    (best.get(b) ?? 0) - (best.get(a) ?? 0) || (a < b ? -1 : a > b ? 1 : 0);
  const fromMetadata = [...metadata.keys()].sort(byScore);
  const fromText = [...best.keys()].filter((id) => !metadata.has(id)).sort(byScore);
  return [...fromMetadata, ...fromText];
}

function rankSemantic(rows: readonly SemanticRow[]): string[] {
  const seen = new Set<string>();
  const ranking: string[] = [];
  for (const row of rows) {
    if (seen.has(row.publicId)) continue;
    seen.add(row.publicId);
    ranking.push(row.publicId);
  }
  return ranking;
}

function compareNewest(a: VideoSummary | undefined, b: VideoSummary | undefined): number {
  if (!a || !b) return 0;
  if (a.publishedAt !== b.publishedAt) return a.publishedAt < b.publishedAt ? 1 : -1;
  return a.publicId < b.publicId ? -1 : a.publicId > b.publicId ? 1 : 0;
}

function buildResult(
  generation: Generation,
  video: VideoSummary,
  score: number,
  lexical: readonly LexicalHit[],
  metadataFields: readonly MatchedIn[],
  semantic: readonly SemanticRow[],
): SearchResult {
  const primary = pickPrimary(getDestinations(generation, video.publicId));
  const drafts = draftMoments(lexical, semantic);
  const moments: SearchMoment[] = drafts
    .slice(0, MOMENTS_PER_RESULT)
    .sort((a, b) => a.start - b.start)
    .map((draft) => ({
      source: draft.source,
      start_seconds: draft.start,
      end_seconds: draft.end,
      snippet: draft.snippet,
      url: videoPath(video.publicId, draft.start),
      destination_url: primary ? destinationUrlAt(primary, draft.start) : null,
      matched_by: draft.matchedBy,
    }));

  const matchedIn = new Set<MatchedIn>(metadataFields);
  for (const hit of lexical) matchedIn.add(hit.source);
  for (const row of semantic) matchedIn.add(row.source);

  const matchedBy: MatchedBy[] = [];
  if (lexical.length > 0 || metadataFields.length > 0) matchedBy.push('lexical');
  if (semantic.length > 0) matchedBy.push('semantic');

  return {
    public_id: video.publicId,
    title: video.title,
    description: video.description,
    kind: video.kind,
    category: video.category,
    language: video.language,
    speaker: video.speaker,
    tags: video.tags,
    duration_seconds: video.durationSeconds,
    published_at: video.publishedAt,
    thumbnail_url: video.thumbnailUrl,
    url: videoPath(video.publicId),
    score: Number(score.toFixed(6)),
    matched_by: matchedBy,
    matched_in: [...matchedIn],
    snippet: lexical.find((hit) => hit.source === 'description')?.snippet ?? null,
    moments,
    moment_count: drafts.length,
  };
}

/**
 * Runs a search. Throws DatasetUnavailableError when the file is not ready. The
 * query is never logged (I254).
 */
export async function search(
  request: SearchRequest,
  dependencies: SearchDependencies = {},
): Promise<SearchResponse> {
  const config = dependencies.config ?? readConfig();
  const dataset = dependencies.dataset ?? getDataset(config.publicDbPath);
  const parsed = parseQuery(request.query);

  const plan: SemanticPlan =
    parsed.terms.length === 0
      ? unavailable('not_requested', null)
      : await planSemantic(request, dataset, config, dependencies.resolve ?? resolveEmbedder);

  // All reads happen here, in one generation. The file may have been replaced while
  // waiting for the embedder, so the plan is checked against this generation.
  return dataset.withSnapshot((generation) => {
    const allowed = allowedIds(generation, request.filters);
    const { hits: lexical, match } = lexicalSearch(generation, parsed, request.filters);
    const metadata = metadataSearch(generation, parsed, allowed);

    let semanticInfo = plan.info;
    let semantic: SemanticRow[] = [];
    if (plan.embedder && plan.vector) {
      const index =
        generation.meta.embeddingModel === plan.embedder.model
          ? vectorIndexFor(generation, plan.vector.length, config.maxVectors)
          : undefined;
      if (index?.ok) {
        semantic = semanticRows(
          generation,
          scanVectors(index.index, plan.vector, plan.embedder.minSimilarity, allowed),
        );
      } else {
        semanticInfo = {
          available: false,
          model: generation.meta.embeddingModel || null,
          test_only: false,
          reason: index ? index.reason : 'no_embedder',
        };
      }
    }

    const lexicalRanking = rankLexical(lexical, metadata);
    const semanticRanking = rankSemantic(semantic);
    const scores = reciprocalRankFusion(
      semanticInfo.available ? [lexicalRanking, semanticRanking] : [lexicalRanking],
    );

    const videos = new Map(
      getVideosByIds(generation, [...scores.keys()]).map((video) => [video.publicId, video]),
    );
    // A row in search_fts or embeddings with no video is not a result.
    const ordered = [...scores.entries()]
      .filter(([id]) => videos.has(id))
      .sort(
        ([idA, a], [idB, b]) => b - a || compareNewest(videos.get(idA), videos.get(idB)),
      );

    const start = (request.page - 1) * request.limit;
    const results = ordered.slice(start, start + request.limit).map(([id, score]) =>
      buildResult(
        generation,
        videos.get(id) as VideoSummary,
        score,
        lexical.filter((hit) => hit.publicId === id),
        metadata.get(id) ?? [],
        semantic.filter((row) => row.publicId === id),
      ),
    );

    return {
      query: request.query,
      mode: semanticInfo.available ? 'hybrid' : 'lexical',
      match,
      semantic: semanticInfo,
      total: ordered.length,
      page: request.page,
      limit: request.limit,
      results,
      dataset: { generated_at: generation.meta.generatedAt },
    } satisfies SearchResponse;
  });
}
