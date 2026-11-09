import { beforeAll, describe, expect, it } from 'vitest';
import type { DatasetManager } from '@/lib/dataset';
import { DatasetUnavailableError } from '@/lib/dataset';
import { fakeHashEmbedder } from '@/lib/embedders/fake-hash';
import type { QueryEmbedder } from '@/lib/embedders/types';
import { parseQuery } from '@/lib/search/fts-query';
import { reciprocalRankFusion } from '@/lib/search/fusion';
import { parseSnippet } from '@/lib/search/lexical';
import { parseSearchParams } from '@/lib/search/params';
import { search } from '@/lib/search/service';
import type { SearchRequest, SearchResponse } from '@/lib/search/types';
import { CATALOG, fillerVideos, type FixtureVideo } from '../../scripts/generate-fixture';
import { fixtureDataset, replaceAtomically, testConfig } from '../helpers';

const CONSCIOUS = 'k7q2m4x9a1b3c5d7';
const STORY = 'p3n8w6t2c4v9h1j5';
const RESPIRACAO = 'r9d4f7s1b6m2k8x3';
const FULL_CLASS = 'h1y5c8z3n7q2d6w4';
const MEDITATION = 'm6t2v9k4j8s3f1g7';

function request(query: string, overrides: Partial<SearchRequest> = {}): SearchRequest {
  return { query, mode: 'auto', filters: {}, page: 1, limit: 20, ...overrides };
}

function ids(response: SearchResponse): string[] {
  return response.results.map((result) => result.public_id);
}

function text(response: SearchResponse): string {
  return JSON.stringify(response);
}

let dataset: DatasetManager;
const config = testConfig();

const run = (query: string, overrides: Partial<SearchRequest> = {}) =>
  search(request(query, overrides), { dataset, config });

beforeAll(() => {
  ({ dataset } = fixtureDataset());
});

describe('parseQuery', () => {
  it('quotes tokens and makes the last one a prefix', () => {
    expect(parseQuery('conscious breath').all).toBe('"conscious" AND "breath"*');
    expect(parseQuery('conscious breath').any).toBe('"conscious" OR "breath"*');
  });

  it('does not treat a finished word as a prefix', () => {
    expect(parseQuery('breath ').all).toBe('"breath"');
  });

  it('makes phrases from quotes', () => {
    expect(parseQuery('"conscious breathing" class').all).toBe('"conscious breathing" AND "class"*');
    expect(parseQuery('"conscious breathing"').all).toBe('"conscious breathing"');
    expect(parseQuery('"conscious breathing"').any).toBeNull();
  });

  it('treats an unmatched quote as punctuation', () => {
    expect(parseQuery('conscious "breath').all).toBe('"conscious" AND "breath"*');
  });

  it('never lets FTS5 syntax through', () => {
    for (const hostile of [
      'title:breathing',
      'a NEAR(b, 2)',
      '(a OR b) NOT c',
      '^start',
      'a*',
      '-a',
      '{a b}: c',
      'a"; DROP TABLE videos; --',
    ]) {
      const { all } = parseQuery(hostile);
      expect(all).toMatch(/^"[\p{L}\p{N} ]+"\*?( AND "[\p{L}\p{N} ]+"\*?)*$/u);
    }
  });

  it('returns nothing for a query with no words', () => {
    expect(parseQuery('!!! ... ???')).toEqual({ terms: [], all: null, any: null });
    expect(parseQuery('')).toEqual({ terms: [], all: null, any: null });
  });

  it('keeps diacritics and normalizes composition', () => {
    expect(parseQuery('respiração').all).toBe('"respiração"*');
    expect(parseQuery('respiração').all).toBe('"respiração"*');
  });

  it('limits the number of tokens', () => {
    const many = Array.from({ length: 40 }, (_, i) => `w${i}`).join(' ');
    expect(parseQuery(many).terms).toHaveLength(16);
  });
});

describe('parseSnippet', () => {
  it('splits marked text into segments and never produces markup', () => {
    expect(parseSnippet('a b <i>c</i>')).toEqual([
      { text: 'a ', match: false },
      { text: 'b', match: true },
      { text: ' <i>c</i>', match: false },
    ]);
  });
});

describe('parseSearchParams', () => {
  const parse = (query: string) => parseSearchParams(new URLSearchParams(query));

  it('requires q', () => {
    expect(parse('')).toMatchObject({ ok: false, code: 'query_required' });
    expect(parse('q=%20%20')).toMatchObject({ ok: false, code: 'query_required' });
  });

  it('limits q', () => {
    expect(parse(`q=${'a'.repeat(257)}`)).toMatchObject({ ok: false, code: 'query_too_long' });
    expect(parse(`q=${'a'.repeat(256)}`)).toMatchObject({ ok: true });
  });

  it('validates mode, kind, limit and page', () => {
    for (const bad of ['mode=semantic', 'kind=private', 'limit=0', 'limit=51', 'limit=x', 'page=0', 'page=-1']) {
      expect(parse(`q=a&${bad}`)).toMatchObject({ ok: false, code: 'invalid_parameter' });
    }
  });

  it('reads a full request', () => {
    expect(parse('q=breathing&mode=lexical&kind=clip&speaker=Ana&tag=hatha&limit=5&page=2')).toEqual({
      ok: true,
      request: {
        query: 'breathing',
        mode: 'lexical',
        filters: { kind: 'clip', speaker: 'Ana', tag: 'hatha' },
        limit: 5,
        page: 2,
      },
    });
  });
});

describe('fusion', () => {
  it('rewards videos ranked well in both lists', () => {
    const scores = reciprocalRankFusion([
      ['a', 'b'],
      ['b', 'c'],
    ]);
    expect(scores.get('b')).toBeCloseTo(1 / 62 + 1 / 61);
    expect(scores.get('a')).toBeCloseTo(1 / 61);
    expect((scores.get('b') as number) > (scores.get('a') as number)).toBe(true);
  });
});

describe('lexical search', () => {
  it('finds spoken content with a timestamp and a link to the moment', async () => {
    const response = await run('nervous system', { mode: 'lexical' });
    expect(ids(response)).toEqual([CONSCIOUS]);
    const [moment] = response.results[0]!.moments;
    expect(moment).toMatchObject({
      source: 'transcript',
      start_seconds: 120,
      end_seconds: 198,
      url: `/videos/${CONSCIOUS}?t=120`,
      destination_url: `https://media.example.internal/watch/${CONSCIOUS}?t=120s`,
    });
    expect(moment!.snippet.filter((segment) => segment.match).map((segment) => segment.text)).toEqual([
      'nervous',
      'system',
    ]);
  });

  it('ignores diacritics in both directions', async () => {
    expect(ids(await run('respiracao', { mode: 'lexical' }))).toEqual([RESPIRACAO]);
    expect(ids(await run('respiração', { mode: 'lexical' }))).toEqual([RESPIRACAO]);
  });

  it('matches a word being typed by prefix, and a phrase exactly', async () => {
    expect(ids(await run('meditat', { mode: 'lexical' }))).toEqual([MEDITATION]);
    expect((await run('"breathing conscious"', { mode: 'lexical' })).total).toBe(0);
    expect((await run('"conscious breathing"', { mode: 'lexical' })).total).toBeGreaterThan(0);
  });

  it('searches titles, descriptions and chapters too', async () => {
    const title = await run('sun salutation', { mode: 'lexical' });
    expect(ids(title)).toContain(STORY);
    expect(title.results.find((r) => r.public_id === STORY)?.matched_in).toContain('title');

    const description = await run('twenty years', { mode: 'lexical' });
    expect(ids(description)).toEqual([STORY]);
    expect(description.results[0]?.matched_in).toEqual(['description']);
    expect(description.results[0]?.snippet?.some((s) => s.match)).toBe(true);

    const chapter = await run('bakery', { mode: 'lexical' });
    expect(chapter.results[0]?.matched_in).toContain('chapter');
  });

  it('falls back to any word and says so', async () => {
    const all = await run('hamstrings', { mode: 'lexical' });
    expect(all.match).toBe('all');
    const any = await run('hamstrings zebra', { mode: 'lexical' });
    expect(any.match).toBe('any');
    expect(ids(any)).toEqual([FULL_CLASS]);
  });

  it('searches the speaker, category and tags, which FTS does not hold', async () => {
    const speaker = await run('Carla', { mode: 'lexical' });
    expect(ids(speaker)).toEqual([MEDITATION]);
    expect(speaker.results[0]?.matched_in).toEqual(['speaker']);

    const accent = await run('ficticia', { mode: 'lexical' });
    expect(ids(accent)).toEqual([MEDITATION]);

    const tag = await run('stress', { mode: 'lexical' });
    expect(tag.results[0]?.matched_in).toContain('tag');

    const fullName = await run('Ana Exemplo', { mode: 'lexical' });
    expect(new Set(ids(fullName))).toEqual(new Set([CONSCIOUS, STORY, FULL_CLASS]));
  });

  it('applies filters to text and metadata results', async () => {
    const pt = await run('consciente', { mode: 'lexical', filters: { language: 'pt-BR' } });
    expect(ids(pt)).toEqual([RESPIRACAO]);
    const none = await run('consciente', { mode: 'lexical', filters: { language: 'en' } });
    expect(ids(none)).toEqual([]);
    const speaker = await run('Ana', { mode: 'lexical', filters: { kind: 'original' } });
    expect(ids(speaker)).toEqual([FULL_CLASS]);
    const tag = await run('breathing', { mode: 'lexical', filters: { tag: 'beginners' } });
    expect(ids(tag)).toEqual([CONSCIOUS]);
  });

  it('survives hostile input as plain text', async () => {
    for (const hostile of ['title:breathing', 'a NEAR(b, 2)', '"', '""', '***', 'a"; DROP TABLE videos; --', '\u0000', '(((']) {
      await expect(run(hostile, { mode: 'lexical' })).resolves.toBeDefined();
    }
    expect((await run('(((', { mode: 'lexical' })).total).toBe(0);
    expect((await run('breathing', { mode: 'lexical' })).total).toBeGreaterThan(0);
  });

  it('returns no results, not an error, for a query with no words', async () => {
    const response = await run('!!!');
    expect(response.total).toBe(0);
    expect(response.mode).toBe('lexical');
  });

  it('never returns HTML from the database as markup', async () => {
    const { dataset: local } = fixtureDataset({
      videos: [
        {
          public_id: 'xss',
          title: '<img src=x onerror=alert(1)>',
          kind: 'clip',
          published_at: '2026-01-01T00:00:00Z',
          transcript: [{ start: 1, end: 2, text: 'hello <script>alert(1)</script> world' }],
        },
      ],
    });
    const response = await search(request('hello'), { dataset: local, config });
    const joined = response.results[0]!.moments[0]!.snippet.map((s) => s.text).join('');
    expect(joined).toContain('<script>');
    expect(response.results[0]!.moments[0]!.snippet[0]).toHaveProperty('match');
    local.close();
  });

  it('skips rows with no matching video and tolerates odd positions', async () => {
    const { file, dataset: local } = fixtureDataset({
      videos: [
        {
          public_id: 'a',
          title: 'Alpha',
          kind: 'clip',
          published_at: '2026-01-01T00:00:00Z',
          transcript: [{ start: 3, end: 4, text: 'alpha spoken' }],
        },
      ],
    });
    local.close();
    const { DatabaseSync } = await import('node:sqlite');
    const db = new DatabaseSync(file);
    db.exec(`INSERT INTO search_fts (public_id, source, start_seconds, end_seconds, text)
             VALUES ('ghost', 'transcript', 1, 2, 'alpha ghost'),
                    ('a', 'transcript', '', '', 'alpha with an empty position'),
                    ('a', 'unknown-source', 1, 2, 'alpha with an unknown source')`);
    db.close();
    const response = await search(request('alpha'), { dataset: local, config });
    expect(ids(response)).toEqual(['a']);
    expect(response.results[0]!.moments.map((m) => m.start_seconds)).toEqual([3]);
    local.close();
  });

  it('pages results and reports the total', async () => {
    const { dataset: local } = fixtureDataset({ videos: fillerVideos(30) });
    const first = await search(request('morning', { limit: 10 }), { dataset: local, config });
    const second = await search(request('morning', { limit: 10, page: 2 }), { dataset: local, config });
    const past = await search(request('morning', { limit: 10, page: 9 }), { dataset: local, config });
    expect(first.total).toBe(30);
    expect(first.results).toHaveLength(10);
    expect(new Set([...ids(first), ...ids(second)]).size).toBe(20);
    expect(past.results).toEqual([]);
    expect(past.total).toBe(30);
    local.close();
  });

  it('caps moments per result and orders them by time', async () => {
    const video: FixtureVideo = {
      public_id: 'long',
      title: 'Long',
      kind: 'clip',
      published_at: '2026-01-01T00:00:00Z',
      transcript: Array.from({ length: 8 }, (_, i) => ({
        start: (8 - i) * 100,
        end: (8 - i) * 100 + 50,
        text: `breathing moment ${i}`,
      })),
    };
    const { dataset: local } = fixtureDataset({ videos: [video] });
    const response = await search(request('breathing', { mode: 'lexical' }), { dataset: local, config });
    const moments = response.results[0]!.moments;
    expect(moments).toHaveLength(3);
    expect(response.results[0]!.moment_count).toBe(8);
    const starts = moments.map((m) => m.start_seconds);
    expect([...starts].sort((a, b) => a - b)).toEqual(starts);
    local.close();
  });

  it('shows one moment per timestamp, the best match', async () => {
    const response = await run('conscious breathing', { mode: 'lexical' });
    const result = response.results.find((r) => r.public_id === FULL_CLASS);
    const starts = result?.moments.map((m) => Math.floor(m.start_seconds)) ?? [];
    expect(starts.length).toBeGreaterThan(0);
    expect(new Set(starts).size).toBe(starts.length);
    expect(result?.moment_count).toBe(starts.length);
  });

  it('gives no destination link when the destination has no timestamps', async () => {
    const response = await run('busy mind', { mode: 'lexical' });
    const result = response.results.find((r) => r.public_id === MEDITATION);
    expect(result).toBeDefined();
    const noisy = await run('noise without arguing', { mode: 'lexical' });
    expect(noisy.results[0]!.moments[0]!.destination_url).toBeNull();
    expect(noisy.results[0]!.moments[0]!.url).toBe(`/videos/${MEDITATION}`);
  });

  it('throws when the dataset is not ready', async () => {
    const { file, dataset: local } = fixtureDataset();
    const { rmSync } = await import('node:fs');
    rmSync(file);
    await expect(search(request('a'), { dataset: local, config })).rejects.toThrow(DatasetUnavailableError);
  });
});

describe('semantic and hybrid search', () => {
  it('is hybrid with the fake embedder when the file says so, and marks it test only', async () => {
    const response = await run('conscious breathing');
    expect(response.mode).toBe('hybrid');
    expect(response.semantic).toEqual({
      available: true,
      model: 'fake-hash-embed@1',
      test_only: true,
      reason: null,
    });
    expect(ids(response)).toContain(CONSCIOUS);
    expect(response.results.find((r) => r.public_id === CONSCIOUS)?.matched_by).toEqual([
      'lexical',
      'semantic',
    ]);
  });

  it('finds a word semantic matching can reach beyond a strict AND', async () => {
    // No single row has both words, so lexical AND falls to OR; semantic still ranks.
    const response = await run('ribs hamstrings');
    expect(response.mode).toBe('hybrid');
    expect(ids(response)).toContain(FULL_CLASS);
  });

  it('marks a moment found by semantic only', async () => {
    const { dataset: local } = fixtureDataset();
    const embedder: QueryEmbedder = {
      ...fakeHashEmbedder,
      // Stands in for a real model: it maps the query to words the text actually has.
      embed: () => fakeHashEmbedder.embed('Lie down for savasana. There is nothing left to do.'),
    };
    const response = await search(request('zebra quantum', {}), {
      dataset: local,
      config,
      resolve: async () => ({ ok: true, embedder }),
    });
    const result = response.results.find((r) => r.public_id === FULL_CLASS);
    expect(result?.matched_by).toEqual(['semantic']);
    const moment = result?.moments.find((m) => m.start_seconds === 2100);
    expect(moment).toMatchObject({
      url: `/videos/${FULL_CLASS}?t=2100`,
      matched_by: ['semantic'],
    });
    // The text comes from the embeddings row, with nothing marked as a word match.
    expect(moment?.snippet).toEqual([
      { text: 'Lie down for savasana. There is nothing left to do.', match: false },
    ]);
    local.close();
  });

  it('merges an overlapping semantic moment into the lexical one', async () => {
    const response = await run('savasana');
    const result = response.results.find((r) => r.public_id === FULL_CLASS);
    const moment = result?.moments.find((m) => m.start_seconds === 2100);
    expect(moment?.matched_by).toEqual(['lexical', 'semantic']);
    expect(moment?.snippet.some((s) => s.match)).toBe(true);
  });

  it('applies filters to semantic hits', async () => {
    const response = await run('conscious breathing', { filters: { language: 'pt-BR' } });
    expect(response.mode).toBe('hybrid');
    expect(ids(response).every((id) => id === RESPIRACAO)).toBe(true);
  });

  const reasons: [string, () => Promise<SearchResponse>, string][] = [
    ['forced lexical', async () => run('breathing', { mode: 'lexical' }), 'not_requested'],
    [
      'disabled by configuration',
      async () => search(request('breathing'), { dataset, config: testConfig({ semanticEnabled: false }) }),
      'disabled',
    ],
    [
      'a file with no vectors',
      async () => {
        const { dataset: local } = fixtureDataset({ withVectors: false });
        const response = await search(request('breathing'), { dataset: local, config });
        local.close();
        return response;
      },
      'no_vectors',
    ],
    [
      'a model with no embedder',
      async () => {
        const { dataset: local } = fixtureDataset({ embeddingModel: 'bge-m3@1' });
        const response = await search(request('breathing'), { dataset: local, config });
        local.close();
        return response;
      },
      'no_embedder',
    ],
    [
      'a file above the vector cap',
      async () =>
        search(request('breathing'), { dataset, config: testConfig({ maxVectors: 1 }) }),
      'too_many_vectors',
    ],
    [
      'a file declaring the wrong dimensions',
      async () => {
        const { dataset: local } = fixtureDataset({ embeddingDimensions: 8 });
        const response = await search(request('breathing'), { dataset: local, config });
        local.close();
        return response;
      },
      'invalid_vectors',
    ],
    [
      'an embedder that fails',
      async () =>
        search(request('breathing'), {
          dataset,
          config,
          resolve: async () => ({
            ok: true,
            embedder: { ...fakeHashEmbedder, embed: () => Promise.reject(new Error('down')) },
          }),
        }),
      'embedder_error',
    ],
    [
      'an embedder that hangs',
      async () =>
        search(request('breathing'), {
          dataset,
          config: testConfig({ embedTimeoutMs: 20 }),
          resolve: async () => ({
            ok: true,
            embedder: { ...fakeHashEmbedder, embed: () => new Promise(() => {}) },
          }),
        }),
      'embedder_error',
    ],
    [
      'an embedder returning the wrong size',
      async () =>
        search(request('breathing'), {
          dataset,
          config,
          resolve: async () => ({
            ok: true,
            embedder: { ...fakeHashEmbedder, embed: async () => new Float32Array(3) },
          }),
        }),
      'dimension_mismatch',
    ],
    [
      'a resolver reporting no embedder',
      async () =>
        search(request('breathing'), {
          dataset,
          config,
          resolve: async () => ({ ok: false, reason: 'no_embedder' }),
        }),
      'no_embedder',
    ],
  ];

  it.each(reasons)('falls back to lexical and says why: %s', async (_name, runCase, reason) => {
    const response = await runCase();
    expect(response.mode).toBe('lexical');
    expect(response.semantic.available).toBe(false);
    expect(response.semantic.reason).toBe(reason);
    // Lexical still answers.
    expect(response.total).toBeGreaterThan(0);
    expect(response.results.every((r) => r.matched_by.includes('lexical'))).toBe(true);
  });

  it('reports a wrong-sized vector as invalid rather than reading past it', async () => {
    const { file, dataset: local } = fixtureDataset();
    local.close();
    const { DatabaseSync } = await import('node:sqlite');
    const db = new DatabaseSync(file);
    db.prepare('UPDATE embeddings SET vector = ? WHERE position = 0').run(new Uint8Array(12));
    db.close();
    const response = await search(request('breathing'), { dataset: local, config });
    expect(response.mode).toBe('lexical');
    expect(response.semantic.reason).toBe('invalid_vectors');
    local.close();
  });

  it('loads vectors once per generation and rebuilds them after a replacement', async () => {
    const { file, dataset: local } = fixtureDataset({ videos: CATALOG.slice(0, 1) });
    const before = await search(request('savasana'), { dataset: local, config });
    expect(ids(before)).toEqual([]);

    replaceAtomically(file, { videos: CATALOG });
    const after = await search(request('savasana'), { dataset: local, config });
    expect(after.mode).toBe('hybrid');
    expect(ids(after)).toEqual([FULL_CLASS]);
    local.close();
  });

  it('never shows a withdrawn video once the file is replaced', async () => {
    const { file, dataset: local } = fixtureDataset();
    expect(ids(await search(request('savasana'), { dataset: local, config }))).toEqual([FULL_CLASS]);
    replaceAtomically(file, { videos: CATALOG.filter((v) => v.public_id !== FULL_CLASS) });
    const after = await search(request('savasana'), { dataset: local, config });
    expect(ids(after)).toEqual([]);
    expect(text(after)).not.toContain(FULL_CLASS);
    local.close();
  });

  it('keeps file-private values out of the response', async () => {
    const response = await run('conscious breathing');
    expect(text(response)).not.toContain('g7h3k2m9');
    expect(text(response)).not.toContain('source_group');
    expect(text(response)).not.toContain('public.db');
  });
});
