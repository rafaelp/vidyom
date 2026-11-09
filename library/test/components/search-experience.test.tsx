// @vitest-environment jsdom
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SearchExperience } from '@/components/SearchExperience';
import type { SearchResponse, SearchResult, SemanticInfo } from '@/lib/search/types';
import type { Facets } from '@/lib/types';

const facets: Facets = {
  kinds: ['clip', 'edited'],
  categories: ['Yoga', 'Stories'],
  languages: ['en'],
  speakers: ['Ana Exemplo', 'Bruno Amostra'],
  tags: ['hatha'],
};

const HYBRID: SemanticInfo = { available: true, model: 'bge-m3@1', test_only: false, reason: null };

function result(overrides: Partial<SearchResult> = {}): SearchResult {
  return {
    public_id: 'abc',
    title: 'Conscious breathing',
    description: '',
    kind: 'clip',
    category: 'Yoga',
    language: 'en',
    speaker: 'Ana Exemplo',
    tags: [],
    duration_seconds: 312,
    published_at: '2026-10-28T14:00:00Z',
    thumbnail_url: '',
    url: '/videos/abc',
    score: 0.03,
    matched_by: ['lexical'],
    matched_in: ['transcript'],
    snippet: null,
    moments: [
      {
        source: 'transcript',
        start_seconds: 90,
        end_seconds: 100,
        snippet: [
          { text: 'say <b>', match: false },
          { text: 'breathing', match: true },
        ],
        url: '/videos/abc?t=90',
        destination_url: null,
        matched_by: ['lexical'],
      },
    ],
    moment_count: 1,
    ...overrides,
  };
}

function response(overrides: Partial<SearchResponse> = {}): SearchResponse {
  return {
    query: 'breathing',
    mode: 'hybrid',
    match: 'all',
    semantic: HYBRID,
    total: 1,
    page: 1,
    limit: 20,
    results: [result()],
    dataset: { generated_at: '2026-11-09T06:00:00Z' },
    ...overrides,
  };
}

function renderSearch(props: Partial<Parameters<typeof SearchExperience>[0]> = {}) {
  // The server renders from the URL, so the page always starts with the two in step.
  const params = new URLSearchParams();
  if (props.initialQuery) params.set('q', props.initialQuery);
  for (const [key, value] of Object.entries(props.filters ?? {})) params.set(key, value as string);
  const page = props.initialResponse?.page ?? 1;
  if (page > 1) params.set('page', String(page));
  window.history.replaceState(null, '', `/search?${params}`);
  return render(
    <SearchExperience
      initialQuery=""
      initialResponse={null}
      initialError={null}
      filters={{}}
      facets={facets}
      {...props}
    />,
  );
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

describe('SearchExperience', () => {
  const fetchMock = vi.fn<typeof fetch>();

  beforeEach(() => {
    vi.stubGlobal('fetch', fetchMock);
    fetchMock.mockReset();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('invites a search when there is no query', () => {
    renderSearch();
    expect(screen.getByRole('heading', { name: 'What are you looking for?' })).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders the server’s first result without fetching', () => {
    renderSearch({ initialQuery: 'breathing', initialResponse: response() });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(screen.getByRole('link', { name: 'Conscious breathing' }).getAttribute('href')).toBe('/videos/abc');
    expect(screen.getByText('Hybrid search')).toBeTruthy();
    expect(screen.getByText(/semantic matching \(bge-m3@1\)/)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Play from 1 minute 30 seconds' }).getAttribute('href')).toBe(
      '/videos/abc?t=90#player',
    );
  });

  it('renders snippet text as text and marks only matches', () => {
    const { container } = renderSearch({ initialQuery: 'breathing', initialResponse: response() });
    expect(container.querySelector('b')).toBeNull();
    expect(container.textContent).toContain('say <b>');
    const marks = [...container.querySelectorAll('mark')].map((mark) => mark.textContent);
    expect(marks).toEqual(['breathing']);
  });

  it('states the mode and the reason when search is lexical', () => {
    renderSearch({
      initialQuery: 'breathing',
      initialResponse: response({
        mode: 'lexical',
        semantic: { available: false, model: 'bge-m3@1', test_only: false, reason: 'no_embedder' },
      }),
    });
    expect(screen.getByText('Word search')).toBeTruthy();
    expect(screen.getByText(/no query embedder for the catalog’s model, bge-m3@1/)).toBeTruthy();
  });

  it('says a test embedder is a test embedder', () => {
    renderSearch({
      initialQuery: 'breathing',
      initialResponse: response({
        semantic: { available: true, model: 'fake-hash-embed@1', test_only: true, reason: null },
      }),
    });
    expect(screen.getByText(/test embedder \(fake-hash-embed@1\), which only finds shared words/)).toBeTruthy();
  });

  it('says when results have only some of the words', () => {
    renderSearch({ initialQuery: 'a b', initialResponse: response({ match: 'any' }) });
    expect(screen.getByText(/No video has all your words/)).toBeTruthy();
  });

  it('shows an empty state for no results, offering to remove filters when there are some', () => {
    const empty = response({ total: 0, results: [] });
    const { unmount } = renderSearch({ initialQuery: 'zebra', initialResponse: empty });
    expect(screen.getByText('No videos match “breathing”')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'See all videos' })).toBeTruthy();
    unmount();

    renderSearch({ initialQuery: 'zebra', initialResponse: empty, filters: { kind: 'clip' } });
    const remove = screen.getByRole('link', { name: 'Remove filters' });
    expect(remove.getAttribute('href')).toBe('/search?q=breathing');
  });

  it('opens filters by itself when one is applied', () => {
    const { container, unmount } = renderSearch();
    expect(container.querySelector('details.filters')?.hasAttribute('open')).toBe(false);
    unmount();
    const applied = renderSearch({ filters: { speaker: 'Ana Exemplo' } });
    expect(applied.container.querySelector('details.filters')?.hasAttribute('open')).toBe(true);
    expect(applied.container.querySelector('summary')?.textContent).toBe('Filters (1 active)');
  });

  it('searches while typing, debounced, and keeps the URL shareable', async () => {
    vi.useFakeTimers();
    fetchMock.mockResolvedValue(json(response()));
    renderSearch();
    const input = screen.getByRole('searchbox');

    fireEvent.change(input, { target: { value: 'br' } });
    fireEvent.change(input, { target: { value: 'breathing' } });
    expect(fetchMock).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(300);
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe('/api/search?q=breathing');
    expect(window.location.pathname + window.location.search).toBe('/search?q=breathing');
    expect(screen.getByRole('link', { name: 'Conscious breathing' })).toBeTruthy();
  });

  it('never calls anything but its own search API', async () => {
    fetchMock.mockResolvedValue(json(response()));
    renderSearch();
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'breathing' } });
    fireEvent.submit(screen.getByRole('search'));
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    for (const [url] of fetchMock.mock.calls) {
      expect(String(url).startsWith('/api/search?')).toBe(true);
    }
  });

  it('sends filters and applies them when changed', async () => {
    fetchMock.mockResolvedValue(json(response()));
    renderSearch({ initialQuery: 'breathing', initialResponse: response() });
    fireEvent.change(screen.getByLabelText('Speaker'), { target: { value: 'Ana Exemplo' } });
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(fetchMock.mock.calls[0]?.[0]).toBe('/api/search?q=breathing&speaker=Ana+Exemplo');
    await waitFor(() => expect(window.location.search).toBe('?q=breathing&speaker=Ana+Exemplo'));
  });

  it('ignores the answer to a superseded query', async () => {
    let resolveFirst: (value: Response) => void = () => {};
    fetchMock.mockImplementationOnce(
      (_url, init) =>
        new Promise((resolve, reject) => {
          resolveFirst = resolve;
          init?.signal?.addEventListener('abort', () =>
            reject(new DOMException('Aborted', 'AbortError')),
          );
        }),
    );
    fetchMock.mockResolvedValueOnce(json(response({ query: 'second', results: [result({ title: 'Second' })] })));

    renderSearch();
    const input = screen.getByRole('searchbox');
    fireEvent.change(input, { target: { value: 'first' } });
    fireEvent.submit(screen.getByRole('search'));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    fireEvent.change(input, { target: { value: 'second' } });
    fireEvent.submit(screen.getByRole('search'));
    await waitFor(() => expect(screen.getByRole('link', { name: 'Second' })).toBeTruthy());

    resolveFirst(json(response({ results: [result({ title: 'First' })] })));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByRole('link', { name: 'First' })).toBeNull();
  });

  it('shows a clear message when the catalog is unavailable, then recovers', async () => {
    fetchMock.mockResolvedValueOnce(json({ error: { code: 'dataset_unavailable', message: 'x' } }, 503));
    fetchMock.mockResolvedValueOnce(json(response()));
    renderSearch();
    const input = screen.getByRole('searchbox');
    fireEvent.change(input, { target: { value: 'breathing' } });
    fireEvent.submit(screen.getByRole('search'));
    await waitFor(() => expect(screen.getByText(/catalog is not available right now/)).toBeTruthy());

    fireEvent.submit(screen.getByRole('search'));
    await waitFor(() => expect(screen.getByRole('link', { name: 'Conscious breathing' })).toBeTruthy());
    expect(screen.queryByText(/catalog is not available right now/)).toBeNull();
  });

  it('shows a clear message when the network fails', async () => {
    fetchMock.mockRejectedValueOnce(new TypeError('Failed to fetch'));
    renderSearch();
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'breathing' } });
    fireEvent.submit(screen.getByRole('search'));
    await waitFor(() => expect(screen.getByText(/Check your connection/)).toBeTruthy());
  });

  it('clears results and the URL when the box is emptied', async () => {
    renderSearch({ initialQuery: 'breathing', initialResponse: response() });
    const input = screen.getByRole('searchbox');
    fireEvent.change(input, { target: { value: '' } });
    fireEvent.submit(screen.getByRole('search'));
    await waitFor(() => expect(screen.getByRole('heading', { name: 'What are you looking for?' })).toBeTruthy());
    expect(window.location.search).toBe('');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shows the server’s validation message', () => {
    renderSearch({ initialQuery: 'x'.repeat(300), initialError: 'The q parameter is limited to 256 characters.' });
    expect(screen.getByText('The q parameter is limited to 256 characters.')).toBeTruthy();
  });

  describe('coming Back to a search typed after the page loaded', () => {
    // The router restores the page's first render, so the props say "no query".
    const renderFromRouterCache = (search: string) => {
      window.history.replaceState(null, '', `/search${search}`);
      return render(
        <SearchExperience initialQuery="" initialResponse={null} initialError={null} filters={{}} facets={facets} />,
      );
    };

    it('runs the search in the URL rather than showing an empty page', async () => {
      fetchMock.mockResolvedValue(json(response({ query: 'savasana' })));
      renderFromRouterCache('?q=savasana');
      expect((screen.getByRole('searchbox') as HTMLInputElement).value).toBe('savasana');
      expect(screen.queryByRole('heading', { name: 'What are you looking for?' })).toBeNull();
      await waitFor(() => expect(screen.getByRole('link', { name: 'Conscious breathing' })).toBeTruthy());
      expect(fetchMock.mock.calls[0]?.[0]).toBe('/api/search?q=savasana');
      expect(window.location.search).toBe('?q=savasana');
    });

    it('keeps its filters and page', async () => {
      fetchMock.mockResolvedValue(json(response({ query: 'savasana', page: 2, total: 45 })));
      renderFromRouterCache('?q=savasana&speaker=Ana+Exemplo&page=2');
      await waitFor(() => expect(fetchMock).toHaveBeenCalled());
      expect(fetchMock.mock.calls[0]?.[0]).toBe('/api/search?q=savasana&speaker=Ana+Exemplo&page=2');
      expect((screen.getByLabelText('Speaker') as HTMLSelectElement).value).toBe('Ana Exemplo');
      await waitFor(() => expect(window.location.search).toBe('?q=savasana&speaker=Ana+Exemplo&page=2'));
    });

    it('does not fetch when the props already match the URL', () => {
      renderSearch({ initialQuery: 'breathing', initialResponse: response(), filters: { kind: 'clip' } });
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it('does not fetch for a URL with no query', () => {
      renderFromRouterCache('');
      expect(fetchMock).not.toHaveBeenCalled();
      expect(screen.getByRole('heading', { name: 'What are you looking for?' })).toBeTruthy();
    });
  });

  it('pages through the server, keeping the query and filters', () => {
    renderSearch({
      initialQuery: 'breathing',
      initialResponse: response({ total: 45, limit: 20, page: 2 }),
      filters: { kind: 'clip' },
    });
    expect(screen.getByRole('link', { name: 'Next' }).getAttribute('href')).toBe(
      '/search?q=breathing&kind=clip&page=3',
    );
    expect(screen.getByRole('link', { name: 'Previous' }).getAttribute('href')).toBe('/search?q=breathing&kind=clip');
    expect(screen.getByText('Page 2 of 3')).toBeTruthy();
  });

  it('has a labelled search box', () => {
    renderSearch();
    expect(
      screen.getByLabelText('Search titles, chapters and what is said in the videos').getAttribute('type'),
    ).toBe('search');
  });
});
