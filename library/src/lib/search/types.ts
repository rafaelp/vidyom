import type { VideoFilters, VideoKind } from '../types';

/** The JSON contract of `GET /api/search`, `snake_case` per I15. Isomorphic. */

export type SearchMode = 'lexical' | 'hybrid';

export type SemanticReason =
  | 'disabled'
  | 'not_requested'
  | 'no_vectors'
  | 'no_embedder'
  | 'dimension_mismatch'
  | 'too_many_vectors'
  | 'invalid_vectors'
  | 'embedder_error';

export interface SemanticInfo {
  available: boolean;
  /** The file's `embedding_model`, or null when it has no vectors. */
  model: string | null;
  /** True when the embedder proves the path but carries no semantic quality (I27). */
  test_only: boolean;
  reason: SemanticReason | null;
}

export interface SnippetSegment {
  text: string;
  match: boolean;
}

export type MatchedBy = 'lexical' | 'semantic';

export type MatchedIn =
  | 'title'
  | 'description'
  | 'chapter'
  | 'transcript'
  | 'speaker'
  | 'category'
  | 'tag';

export interface SearchMoment {
  source: 'chapter' | 'transcript';
  start_seconds: number;
  end_seconds: number;
  snippet: SnippetSegment[];
  /** The site's own page at this moment. */
  url: string;
  /** The destination at this moment, or null when it cannot jump there. */
  destination_url: string | null;
  matched_by: MatchedBy[];
}

export interface SearchResult {
  public_id: string;
  title: string;
  description: string;
  kind: VideoKind;
  category: string;
  language: string;
  speaker: string;
  tags: string[];
  duration_seconds: number;
  published_at: string;
  thumbnail_url: string;
  url: string;
  /** Orders one response only; not comparable between responses. */
  score: number;
  matched_by: MatchedBy[];
  matched_in: MatchedIn[];
  /** A highlighted description excerpt, when the description matched. */
  snippet: SnippetSegment[] | null;
  moments: SearchMoment[];
  /** How many moments matched in total; `moments` holds the best few. */
  moment_count: number;
}

export interface SearchResponse {
  query: string;
  mode: SearchMode;
  /** Whether results contain all the query's words, or only some (I245). */
  match: 'all' | 'any';
  semantic: SemanticInfo;
  total: number;
  page: number;
  limit: number;
  results: SearchResult[];
  dataset: { generated_at: string };
}

export interface SearchRequest {
  query: string;
  /** `auto` runs semantic when available; `lexical` forces lexical. */
  mode: 'auto' | 'lexical';
  filters: VideoFilters;
  page: number;
  limit: number;
}

export interface SearchErrorBody {
  error: { code: string; message: string; details?: Record<string, unknown> };
}

export const DEFAULT_SEARCH_LIMIT = 20;
export const MAX_SEARCH_LIMIT = 50;
export const MAX_QUERY_LENGTH = 256;
