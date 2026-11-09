import { VIDEO_KINDS, type VideoFilters, type VideoKind } from '../types';
import {
  DEFAULT_SEARCH_LIMIT,
  MAX_QUERY_LENGTH,
  MAX_SEARCH_LIMIT,
  type SearchRequest,
} from './types';

/** Parsing of search and filter parameters, shared by the API route and the pages. */

export type ParseResult =
  | { ok: true; request: SearchRequest }
  | { ok: false; code: string; message: string };

function clean(value: string | null): string | undefined {
  const trimmed = value?.trim();
  return trimmed ? trimmed : undefined;
}

/** Filters from query parameters. Unknown kinds are dropped by this lenient form. */
export function filtersFromParams(params: URLSearchParams): VideoFilters {
  const kind = clean(params.get('kind'));
  const filters: VideoFilters = {};
  if (kind && (VIDEO_KINDS as readonly string[]).includes(kind)) filters.kind = kind as VideoKind;
  const category = clean(params.get('category'));
  if (category) filters.category = category;
  const language = clean(params.get('language'));
  if (language) filters.language = language;
  const speaker = clean(params.get('speaker'));
  if (speaker) filters.speaker = speaker;
  const tag = clean(params.get('tag'));
  if (tag) filters.tag = tag;
  return filters;
}

/** A page number from a parameter; anything but a positive integer means page 1. */
export function pageFromParam(value: string | null): number {
  if (!value || !/^\d{1,9}$/.test(value)) return 1;
  const page = Number(value);
  return page > 0 ? page : 1;
}

function integerParam(value: string | null): number | null {
  if (value === null) return null;
  return /^\d{1,9}$/.test(value) ? Number(value) : Number.NaN;
}

/** The strict form for the API: anything malformed is an error, not a guess. */
export function parseSearchParams(params: URLSearchParams): ParseResult {
  const query = params.get('q')?.trim() ?? '';
  if (query === '') {
    return { ok: false, code: 'query_required', message: 'The q parameter is required.' };
  }
  if (query.length > MAX_QUERY_LENGTH) {
    return {
      ok: false,
      code: 'query_too_long',
      message: `The q parameter is limited to ${MAX_QUERY_LENGTH} characters.`,
    };
  }

  const invalid = (message: string): ParseResult => ({
    ok: false,
    code: 'invalid_parameter',
    message,
  });

  const mode = params.get('mode') ?? 'auto';
  if (mode !== 'auto' && mode !== 'lexical') return invalid('mode must be auto or lexical.');

  const kind = clean(params.get('kind'));
  if (kind && !(VIDEO_KINDS as readonly string[]).includes(kind)) {
    return invalid(`kind must be one of ${VIDEO_KINDS.join(', ')}.`);
  }

  const limit = integerParam(params.get('limit'));
  if (limit !== null && (Number.isNaN(limit) || limit < 1 || limit > MAX_SEARCH_LIMIT)) {
    return invalid(`limit must be an integer from 1 to ${MAX_SEARCH_LIMIT}.`);
  }
  const page = integerParam(params.get('page'));
  if (page !== null && (Number.isNaN(page) || page < 1)) {
    return invalid('page must be a positive integer.');
  }

  return {
    ok: true,
    request: {
      query,
      mode,
      filters: filtersFromParams(params),
      limit: limit ?? DEFAULT_SEARCH_LIMIT,
      page: page ?? 1,
    },
  };
}
