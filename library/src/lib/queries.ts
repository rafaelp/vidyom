import type { SQLInputValue } from 'node:sqlite';
import type { Generation } from './dataset';
import { safeHttpUrl, safeHttpsUrl } from './links';
import {
  VIDEO_KINDS,
  type Chapter,
  type Destination,
  type Facets,
  type TranscriptSegment,
  type VideoDetail,
  type VideoFilters,
  type VideoKind,
  type VideoPage,
  type VideoSummary,
} from './types';

/** Read queries over `schemas/public-db/v1.sql`. Synchronous; run them in a snapshot. */

export const LIST_PAGE_SIZE = 24;
const RELATED_LIMIT = 24;

interface VideoRow {
  public_id: string;
  title: string;
  description: string;
  kind: string;
  category: string;
  language: string;
  speaker: string;
  tags: string;
  duration_seconds: number;
  published_at: string;
  thumbnail_url: string;
  source_group: string;
}

/** The `tags` column is JSON. A malformed value means no tags, not a failed page. */
export function parseTags(value: string): string[] {
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((tag): tag is string => typeof tag === 'string' && tag.trim() !== '');
  } catch {
    return [];
  }
}

function toSummary(row: VideoRow): VideoSummary {
  return {
    publicId: row.public_id,
    title: row.title,
    description: row.description,
    kind: row.kind as VideoKind,
    category: row.category,
    language: row.language,
    speaker: row.speaker,
    tags: parseTags(row.tags),
    durationSeconds: row.duration_seconds,
    publishedAt: row.published_at,
    thumbnailUrl: safeHttpsUrl(row.thumbnail_url),
  };
}

export function isVideoKind(value: string): value is VideoKind {
  return (VIDEO_KINDS as readonly string[]).includes(value);
}

/** SQL conditions for the filters, over a `videos` row aliased as `v`. */
export function filterClause(filters: VideoFilters): { sql: string; params: SQLInputValue[] } {
  const conditions: string[] = [];
  const params: SQLInputValue[] = [];
  const equal = (column: string, value: string | undefined) => {
    if (value === undefined || value === '') return;
    conditions.push(`v.${column} = ?`);
    params.push(value);
  };
  equal('kind', filters.kind);
  equal('category', filters.category);
  equal('language', filters.language);
  equal('speaker', filters.speaker);
  if (filters.tag) {
    // CASE keeps a malformed tags value from failing the whole query.
    conditions.push(
      `CASE WHEN json_valid(v.tags) THEN EXISTS (
         SELECT 1 FROM json_each(v.tags) WHERE json_each.value = ?
       ) ELSE 0 END`,
    );
    params.push(filters.tag);
  }
  return { sql: conditions.join(' AND '), params };
}

export function hasFilters(filters: VideoFilters): boolean {
  return Boolean(
    filters.kind || filters.category || filters.language || filters.speaker || filters.tag,
  );
}

export function listVideos(
  generation: Generation,
  filters: VideoFilters,
  page: number,
  pageSize: number = LIST_PAGE_SIZE,
): VideoPage {
  const { sql, params } = filterClause(filters);
  const where = sql ? `WHERE ${sql}` : '';
  const total =
    generation.db.get<{ total: number }>(`SELECT count(*) AS total FROM videos v ${where}`, ...params)
      ?.total ?? 0;
  const rows = generation.db.all<VideoRow>(
    `SELECT v.* FROM videos v ${where}
     ORDER BY v.published_at DESC, v.public_id ASC
     LIMIT ? OFFSET ?`,
    ...params,
    pageSize,
    (page - 1) * pageSize,
  );
  return { items: rows.map(toSummary), total, page, pageSize };
}

export function countVideos(generation: Generation): number {
  return (
    generation.db.get<{ total: number }>('SELECT count(*) AS total FROM videos')?.total ?? 0
  );
}

function toDestination(row: {
  destination: string;
  url: string;
  embed_url: string;
  supports_timestamps: number;
}): Destination | null {
  const url = safeHttpUrl(row.url);
  if (!url) return null;
  return {
    driver: row.destination,
    url,
    embedUrl: safeHttpsUrl(row.embed_url),
    supportsTimestamps: row.supports_timestamps === 1,
  };
}

export function getDestinations(generation: Generation, publicId: string): Destination[] {
  return generation.db
    .all<{ destination: string; url: string; embed_url: string; supports_timestamps: number }>(
      `SELECT destination, url, embed_url, supports_timestamps FROM destinations
       WHERE public_id = ? ORDER BY destination, url`,
      publicId,
    )
    .map(toDestination)
    .filter((destination): destination is Destination => destination !== null);
}

export function getVideo(generation: Generation, publicId: string): VideoDetail | null {
  const row = generation.db.get<VideoRow>('SELECT * FROM videos WHERE public_id = ?', publicId);
  if (!row) return null;
  const chapters = generation.db
    .all<{
      position: number;
      title: string;
      start_seconds: number;
      end_seconds: number;
      summary: string;
    }>(
      `SELECT position, title, start_seconds, end_seconds, summary FROM chapters
       WHERE public_id = ? ORDER BY position`,
      publicId,
    )
    .map(
      (chapter): Chapter => ({
        position: chapter.position,
        title: chapter.title,
        startSeconds: chapter.start_seconds,
        endSeconds: chapter.end_seconds,
        summary: chapter.summary,
      }),
    );
  const transcriptCount =
    generation.db.get<{ total: number }>(
      'SELECT count(*) AS total FROM transcript_segments WHERE public_id = ?',
      publicId,
    )?.total ?? 0;
  return {
    ...toSummary(row),
    destinations: getDestinations(generation, publicId),
    chapters,
    transcriptCount,
    sourceGroup: row.source_group,
  };
}

export function getTranscript(
  generation: Generation,
  publicId: string,
  limit?: number,
): TranscriptSegment[] {
  const rows = generation.db.all<{
    position: number;
    start_seconds: number;
    end_seconds: number;
    text: string;
  }>(
    `SELECT position, start_seconds, end_seconds, text FROM transcript_segments
     WHERE public_id = ? ORDER BY position LIMIT ?`,
    publicId,
    limit ?? -1,
  );
  return rows.map((row) => ({
    position: row.position,
    startSeconds: row.start_seconds,
    endSeconds: row.end_seconds,
    text: row.text,
  }));
}

/** Other public videos from the same source, without naming the source (§13.2, I257). */
export function getRelated(generation: Generation, video: VideoDetail): VideoSummary[] {
  if (!video.sourceGroup) return [];
  return generation.db
    .all<VideoRow>(
      `SELECT * FROM videos WHERE source_group = ? AND public_id <> ?
       ORDER BY published_at DESC, public_id ASC LIMIT ?`,
      video.sourceGroup,
      video.publicId,
      RELATED_LIMIT,
    )
    .map(toSummary);
}

/** Summaries by ID, in no particular order. */
export function getVideosByIds(generation: Generation, ids: readonly string[]): VideoSummary[] {
  if (ids.length === 0) return [];
  const placeholders = ids.map(() => '?').join(',');
  return generation.db
    .all<VideoRow>(`SELECT * FROM videos WHERE public_id IN (${placeholders})`, ...ids)
    .map(toSummary);
}

export function getFacets(generation: Generation): Facets {
  return generation.memo('facets', () => {
    const distinct = (column: 'category' | 'language' | 'speaker'): string[] =>
      generation.db
        .all<{ value: string }>(
          `SELECT DISTINCT ${column} AS value FROM videos WHERE ${column} <> '' ORDER BY value`,
        )
        .map((row) => row.value);
    const tags = new Set<string>();
    for (const row of generation.db.all<{ tags: string }>('SELECT tags FROM videos')) {
      for (const tag of parseTags(row.tags)) tags.add(tag);
    }
    const kinds = generation.db
      .all<{ kind: string }>('SELECT DISTINCT kind FROM videos')
      .map((row) => row.kind)
      .filter(isVideoKind);
    return {
      kinds: VIDEO_KINDS.filter((kind) => kinds.includes(kind)),
      categories: distinct('category'),
      languages: distinct('language'),
      speakers: distinct('speaker'),
      tags: [...tags].sort((a, b) => a.localeCompare(b)),
    };
  });
}

export function allVideoEntries(generation: Generation): { publicId: string; publishedAt: string }[] {
  return generation.db
    .all<{ public_id: string; published_at: string }>(
      'SELECT public_id, published_at FROM videos ORDER BY published_at DESC, public_id ASC',
    )
    .map((row) => ({ publicId: row.public_id, publishedAt: row.published_at }));
}
