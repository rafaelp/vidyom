import fs from 'node:fs';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { GET as health } from '@/app/api/health/route';
import { GET as searchRoute } from '@/app/api/search/route';
import { CATALOG } from '../../scripts/generate-fixture';
import { fixtureDataset, replaceAtomically, tempDir } from '../helpers';
import path from 'node:path';

const saved = process.env.VIDYOM_PUBLIC_DB;

function get(query: string): Promise<Response> {
  return searchRoute(new Request(`http://library.test/api/search?${query}`));
}

describe('with a ready file', () => {
  let file: string;

  beforeEach(() => {
    ({ file } = fixtureDataset());
    process.env.VIDYOM_PUBLIC_DB = file;
  });

  afterEach(() => {
    process.env.VIDYOM_PUBLIC_DB = saved;
  });

  it('answers a search with the mode and no caching', async () => {
    const response = await get('q=conscious+breathing');
    expect(response.status).toBe(200);
    expect(response.headers.get('cache-control')).toBe('no-store');
    expect(response.headers.get('content-type')).toContain('application/json');
    const body = await response.json();
    expect(body.mode).toBe('hybrid');
    expect(body.semantic.model).toBe('fake-hash-embed@1');
    expect(body.semantic.test_only).toBe(true);
    expect(body.results[0].url).toMatch(/^\/videos\//);
    expect(body.dataset.generated_at).toBe('2026-11-09T06:00:00Z');
  });

  it('forces lexical on request', async () => {
    const body = await (await get('q=breathing&mode=lexical')).json();
    expect(body.mode).toBe('lexical');
    expect(body.semantic).toMatchObject({ available: false, reason: 'not_requested' });
  });

  it.each([
    ['', 'query_required'],
    ['q=', 'query_required'],
    [`q=${'a'.repeat(300)}`, 'query_too_long'],
    ['q=a&mode=semantic', 'invalid_parameter'],
    ['q=a&kind=private', 'invalid_parameter'],
    ['q=a&limit=1000', 'invalid_parameter'],
  ])('rejects %s with %s', async (query, code) => {
    const response = await get(query);
    expect(response.status).toBe(400);
    const body = await response.json();
    expect(body.error.code).toBe(code);
    expect(typeof body.error.message).toBe('string');
  });

  it('sees a replaced file on the next request', async () => {
    const id = 'h1y5c8z3n7q2d6w4';
    const before = await (await get('q=savasana')).json();
    expect(before.results.map((r: { public_id: string }) => r.public_id)).toEqual([id]);
    replaceAtomically(file, { videos: CATALOG.filter((video) => video.public_id !== id) });
    const after = await (await get('q=savasana')).json();
    expect(after.results).toEqual([]);
  });

  it('reports health from the public file only', async () => {
    const response = await health();
    expect(response.status).toBe(200);
    const body = await response.json();
    expect(body).toEqual({
      status: 'ok',
      dataset: {
        status: 'ready',
        schema_version: '1',
        generated_at: '2026-11-09T06:00:00Z',
        embedding_model: 'fake-hash-embed@1',
        videos: CATALOG.length,
      },
    });
    expect(JSON.stringify(body)).not.toContain(file);
  });
});

describe('with no ready file', () => {
  afterEach(() => {
    process.env.VIDYOM_PUBLIC_DB = saved;
  });

  it('answers 503 and never reveals the path when the file is missing', async () => {
    const file = path.join(tempDir(), 'secret-location', 'public.db');
    process.env.VIDYOM_PUBLIC_DB = file;

    const response = await get('q=breathing');
    expect(response.status).toBe(503);
    expect(response.headers.get('retry-after')).toBe('60');
    const body = await response.json();
    expect(body.error.code).toBe('dataset_unavailable');
    expect(body.error.details.status).toBe('missing');
    expect(JSON.stringify(body)).not.toContain('secret-location');

    const healthResponse = await health();
    expect(healthResponse.status).toBe(503);
    expect(await healthResponse.json()).toEqual({ status: 'unavailable', dataset: { status: 'missing' } });
  });

  it('answers 503 for a file that is not a database', async () => {
    const file = path.join(tempDir(), 'public.db');
    fs.writeFileSync(file, 'not a database, this is not a database at all');
    process.env.VIDYOM_PUBLIC_DB = file;
    const response = await get('q=breathing');
    expect(response.status).toBe(503);
    expect((await response.json()).error.details.status).toBe('invalid');
  });
});
