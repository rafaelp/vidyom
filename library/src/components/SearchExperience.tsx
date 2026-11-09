'use client';

import { SiteLink as Link } from '@/components/SiteLink';
import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';
import { pluralize } from '@/lib/format';
import { filtersFromParams, pageFromParam } from '@/lib/search/params';
import { MAX_QUERY_LENGTH, type SearchErrorBody, type SearchResponse } from '@/lib/search/types';
import type { Facets, VideoFilters } from '@/lib/types';
import { EmptyState } from './EmptyState';
import { FilterDisclosure, FilterFields } from './FilterFields';
import { ModeNotice } from './ModeNotice';
import { Pagination } from './Pagination';
import { SearchResults } from './SearchResults';

const DEBOUNCE_MS = 250;

interface SearchExperienceProps {
  initialQuery: string;
  initialResponse: SearchResponse | null;
  initialError: string | null;
  filters: VideoFilters;
  facets: Facets;
}

function paramsFor(query: string, filters: VideoFilters, page = 1): URLSearchParams {
  const params = new URLSearchParams();
  if (query) params.set('q', query);
  for (const [key, value] of Object.entries(filters)) {
    if (value) params.set(key, value);
  }
  if (page > 1) params.set('page', String(page));
  return params;
}

interface UrlSearch {
  query: string;
  filters: VideoFilters;
  page: number;
}

/**
 * The search in the address bar, when it is not the one these props describe. Typing
 * updates the URL without the server, so Back returns here with the props of the page's
 * first render and the URL of the last search. Never true while hydrating: the server
 * rendered from this same URL.
 */
function searchInUrlIfDifferent(
  query: string,
  filters: VideoFilters,
  response: SearchResponse | null,
): UrlSearch | null {
  if (typeof window === 'undefined') return null;
  const params = new URLSearchParams(window.location.search);
  const inUrl: UrlSearch = {
    query: (params.get('q') ?? '').trim(),
    filters: filtersFromParams(params),
    page: pageFromParam(params.get('page')),
  };
  const fromProps = paramsFor(query.trim(), filters, response?.page ?? 1).toString();
  return paramsFor(inUrl.query, inUrl.filters, inUrl.page).toString() === fromProps ? null : inUrl;
}

/**
 * The search page. The server renders the first result, and the form is a plain GET,
 * so all of it works with no JavaScript. With JavaScript, typing updates the results
 * from /api/search and keeps the URL shareable.
 */
export function SearchExperience({
  initialQuery,
  initialResponse,
  initialError,
  filters: initialFilters,
  facets,
}: SearchExperienceProps) {
  const [restored] = useState(() =>
    searchInUrlIfDifferent(initialQuery, initialFilters, initialResponse),
  );
  const [query, setQuery] = useState(restored?.query ?? initialQuery);
  const [filters, setFilters] = useState(restored?.filters ?? initialFilters);
  const [response, setResponse] = useState(restored ? null : initialResponse);
  const [error, setError] = useState(restored ? null : initialError);
  const [loading, setLoading] = useState(Boolean(restored?.query));
  const controller = useRef<AbortController | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const stop = useCallback(() => {
    if (timer.current) clearTimeout(timer.current);
    controller.current?.abort();
  }, []);

  useEffect(() => stop, [stop]);

  const load = useCallback(async (text: string, nextFilters: VideoFilters, page: number) => {
    controller.current?.abort();
    const abort = new AbortController();
    controller.current = abort;
    try {
      const answer = await fetch(`/api/search?${paramsFor(text, nextFilters, page)}`, {
        signal: abort.signal,
      });
      if (!answer.ok) {
        const body = (await answer.json().catch(() => null)) as SearchErrorBody | null;
        throw new Error(
          answer.status === 503
            ? 'The catalog is not available right now. Please try again in a minute.'
            : (body?.error.message ?? 'Search failed.'),
        );
      }
      const results = (await answer.json()) as SearchResponse;
      if (abort.signal.aborted) return;
      setResponse(results);
      setError(null);
      setLoading(false);
    } catch (cause) {
      if (abort.signal.aborted) return;
      setError(
        cause instanceof Error && cause.name !== 'TypeError'
          ? cause.message
          : 'Search failed. Check your connection and try again.',
      );
      setLoading(false);
    }
  }, []);

  const run = useCallback(
    (text: string, nextFilters: VideoFilters) => {
      stop();
      const trimmed = text.trim();
      const url = trimmed === '' ? '/search' : `/search?${paramsFor(trimmed, nextFilters)}`;
      window.history.replaceState(window.history.state, '', url);
      if (trimmed === '') {
        setResponse(null);
        setError(null);
        setLoading(false);
        return;
      }
      setLoading(true);
      void load(trimmed, nextFilters, 1);
    },
    [load, stop],
  );

  useEffect(() => {
    // State changes only after the fetch resolves, which is what an effect is for.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (restored?.query) void load(restored.query, restored.filters, restored.page);
  }, [restored, load]);

  const onInput = (value: string) => {
    setQuery(value);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => run(value, filters), DEBOUNCE_MS);
  };

  const onSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    run(query, filters);
  };

  const onFilter = (name: string, value: string) => {
    const next = { ...filters, [name]: value || undefined } as VideoFilters;
    setFilters(next);
    run(query, next);
  };

  const hasQuery = query.trim() !== '' || response !== null;
  const hasActiveFilters = Object.values(filters).some(Boolean);
  const totalPages = response ? Math.ceil(response.total / response.limit) : 0;

  return (
    <div className="page stack">
      <div className="search-hero">
        <h1>Search</h1>
        <form method="get" action="/search" role="search" onSubmit={onSubmit} className="stack">
          <div>
            <label htmlFor="search-input" className="field-label">
              Search titles, chapters and what is said in the videos
            </label>
            <div className="search-field search-field--large">
              <input
                id="search-input"
                type="search"
                name="q"
                value={query}
                onChange={(event) => onInput(event.target.value)}
                maxLength={MAX_QUERY_LENGTH}
                enterKeyHint="search"
                autoComplete="off"
              />
              <button type="submit">Search</button>
            </div>
          </div>
          <FilterDisclosure active={Object.values(filters).filter(Boolean).length}>
            <div className="filter-bar">
              <FilterFields facets={facets} filters={filters} onChange={onFilter} />
            </div>
          </FilterDisclosure>
        </form>
      </div>

      <div className="search-status" role="status" aria-busy={loading}>
        {error ? <p>{error}</p> : null}
        {loading ? <p className="count">Searching…</p> : null}
        {!loading && !error && response ? (
          <>
            <p className="count">
              {response.total > 0
                ? pluralize(response.total, 'video')
                : `No videos match “${response.query}”`}
            </p>
            {response.match === 'any' && response.total > 0 ? (
              <p className="count">No video has all your words, so these have some of them.</p>
            ) : null}
            <ModeNotice mode={response.mode} semantic={response.semantic} />
          </>
        ) : null}
      </div>

      {response && response.results.length > 0 ? (
        <>
          <SearchResults results={response.results} />
          <Pagination
            page={response.page}
            totalPages={totalPages}
            hrefFor={(page) => `/search?${paramsFor(response.query, filters, page)}`}
          />
        </>
      ) : null}

      {response && response.total === 0 && !loading && !error ? (
        <EmptyState
          title="Nothing found"
          actions={
            hasActiveFilters ? (
              <Link className="button button--quiet" href={`/search?${paramsFor(response.query, {})}`}>
                Remove filters
              </Link>
            ) : (
              <Link className="button button--quiet" href="/">
                See all videos
              </Link>
            )
          }
        >
          {hasActiveFilters
            ? 'Try removing a filter, or use fewer or different words.'
            : 'Try fewer or different words.'}
        </EmptyState>
      ) : null}

      {response && response.total > 0 && response.results.length === 0 ? (
        <EmptyState
          title="This page is past the end of the results"
          actions={
            <Link className="button button--quiet" href={`/search?${paramsFor(response.query, filters)}`}>
              Go to the first page
            </Link>
          }
        />
      ) : null}

      {!hasQuery && !error ? (
        <EmptyState title="What are you looking for?">
          Type a topic, a speaker, a chapter, or a phrase someone says in a video. Results link to the moment it
          happens.
        </EmptyState>
      ) : null}
    </div>
  );
}
