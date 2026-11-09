import type { SQLInputValue } from 'node:sqlite';
import type { Generation } from '../dataset';
import { filterClause } from '../queries';
import type { VideoFilters } from '../types';
import type { ParsedQuery } from './fts-query';
import type { MatchedIn, SnippetSegment } from './types';

/** FTS5 search over titles, descriptions, chapters and transcript (I245, I247). */

// Private-use markers: database text cannot be confused with HTML, and the snippet
// is parsed into segments rather than rendered as markup.
const MARK_START = '';
const MARK_END = '';

const ROW_LIMIT = 400;
const SNIPPET_TOKENS = 28;

const SOURCE_WEIGHTS: Record<string, number> = {
  title: 4,
  chapter: 2.5,
  description: 1.5,
  transcript: 1,
};

export interface LexicalHit {
  publicId: string;
  source: Extract<MatchedIn, 'title' | 'description' | 'chapter' | 'transcript'>;
  /** Null for rows with no position, such as titles. */
  startSeconds: number | null;
  endSeconds: number | null;
  snippet: SnippetSegment[];
  score: number;
}

export function parseSnippet(marked: string): SnippetSegment[] {
  const segments: SnippetSegment[] = [];
  let inside = false;
  let current = '';
  const flush = () => {
    if (current !== '') segments.push({ text: current, match: inside });
    current = '';
  };
  for (const character of marked) {
    if (character === MARK_START) {
      flush();
      inside = true;
    } else if (character === MARK_END) {
      flush();
      inside = false;
    } else {
      current += character;
    }
  }
  flush();
  return segments;
}

/** FTS5 columns are typeless: a position may be a number, NULL or an empty string. */
function position(value: unknown): number | null {
  if (typeof value === 'number') return Number.isFinite(value) && value >= 0 ? value : null;
  if (typeof value === 'bigint') return Number(value);
  if (typeof value === 'string' && value.trim() !== '') {
    const parsed = Number(value);
    return Number.isFinite(parsed) && parsed >= 0 ? parsed : null;
  }
  return null;
}

function isSource(value: string): value is LexicalHit['source'] {
  return value in SOURCE_WEIGHTS;
}

function run(generation: Generation, expression: string, filters: VideoFilters): LexicalHit[] {
  const { sql, params } = filterClause(filters);
  // CROSS JOIN keeps FTS as the outer loop, which bm25() and snippet() require.
  const rows = generation.db.all<{
    public_id: string;
    source: string;
    start_seconds: unknown;
    end_seconds: unknown;
    relevance: number;
    marked: string;
  }>(
    `SELECT search_fts.public_id AS public_id,
            search_fts.source AS source,
            search_fts.start_seconds AS start_seconds,
            search_fts.end_seconds AS end_seconds,
            bm25(search_fts) AS relevance,
            snippet(search_fts, 4, '${MARK_START}', '${MARK_END}', '…', ${SNIPPET_TOKENS}) AS marked
     FROM search_fts CROSS JOIN videos v ON v.public_id = search_fts.public_id
     WHERE search_fts MATCH ? ${sql ? `AND ${sql}` : ''}
     ORDER BY relevance
     LIMIT ${ROW_LIMIT}`,
    ...([expression, ...params] as SQLInputValue[]),
  );
  const hits: LexicalHit[] = [];
  for (const row of rows) {
    if (!isSource(row.source)) continue;
    hits.push({
      publicId: row.public_id,
      source: row.source,
      startSeconds: position(row.start_seconds),
      endSeconds: position(row.end_seconds),
      snippet: parseSnippet(row.marked),
      // bm25() is negative and better when lower.
      score: -row.relevance * (SOURCE_WEIGHTS[row.source] ?? 1),
    });
  }
  return hits;
}

export function lexicalSearch(
  generation: Generation,
  query: ParsedQuery,
  filters: VideoFilters,
): { hits: LexicalHit[]; match: 'all' | 'any' } {
  if (!query.all) return { hits: [], match: 'all' };
  const hits = run(generation, query.all, filters);
  if (hits.length > 0 || !query.any) return { hits, match: 'all' };
  return { hits: run(generation, query.any, filters), match: 'any' };
}
