import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { CATALOG, buildFixture, fillerVideos } from '../scripts/generate-fixture';

/**
 * Exercises the production server over real HTTP: `npm run build` must have run. The
 * server gets a free port and a temporary public.db, and is stopped when this ends.
 */

const ROOT = path.resolve(import.meta.dirname, '..');
const CONSCIOUS = 'k7q2m4x9a1b3c5d7';
const FULL_CLASS = 'h1y5c8z3n7q2d6w4';
const MEDITATION = 'm6t2v9k4j8s3f1g7';
const NO_DESTINATION_PLAYER = 'w8b3x5n1p7r4c9d2';
const SOURCE_GROUP = 'g7h3k2m9';

let directory: string;
let dbPath: string;
let baseUrl: string;
let server: ChildProcess | undefined;
let logs = '';

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const probe = net.createServer();
    probe.once('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const { port } = probe.address() as net.AddressInfo;
      probe.close(() => resolve(port));
    });
  });
}

async function waitUntilListening(): Promise<void> {
  const deadline = Date.now() + 60_000;
  for (;;) {
    try {
      const response = await fetch(`${baseUrl}/robots.txt`);
      if (response.ok) return;
    } catch {
      // Not listening yet.
    }
    if (Date.now() > deadline) throw new Error(`server did not start:\n${logs}`);
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
}

function replaceDataset(options: Parameters<typeof buildFixture>[1]): void {
  const next = `${dbPath}.next`;
  buildFixture(next, options);
  fs.renameSync(next, dbPath);
}

async function html(pathname: string): Promise<{ status: number; body: string; headers: Headers }> {
  const response = await fetch(`${baseUrl}${pathname}`);
  // React separates adjacent text with comments; a person reads through them.
  const body = (await response.text()).replaceAll('<!-- -->', '');
  return { status: response.status, body, headers: response.headers };
}

// The response shape is what these tests assert, so it is deliberately loose here.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function api(pathname: string): Promise<{ status: number; body: any; headers: Headers }> {
  const response = await fetch(`${baseUrl}${pathname}`);
  return { status: response.status, body: await response.json(), headers: response.headers };
}

beforeAll(async () => {
  if (!fs.existsSync(path.join(ROOT, '.next', 'BUILD_ID'))) {
    throw new Error('No production build found: run `npm run build` first.');
  }
  directory = fs.mkdtempSync(path.join(os.tmpdir(), 'vidyom-library-e2e-'));
  dbPath = path.join(directory, 'tenant', 'public.db');
  const port = await freePort();
  baseUrl = `http://127.0.0.1:${port}`;
  server = spawn(process.execPath, [path.join(ROOT, 'node_modules/next/dist/bin/next'), 'start', '-p', String(port), '-H', '127.0.0.1'], {
    cwd: ROOT,
    env: {
      ...process.env,
      NEXT_TELEMETRY_DISABLED: '1',
      VIDYOM_PUBLIC_DB: dbPath,
      VIDYOM_LIBRARY_TITLE: 'Acme video library',
      VIDYOM_LIBRARY_DESCRIPTION: 'Fictional catalog for tests.',
      VIDYOM_LIBRARY_SITE_URL: 'https://media.example.internal',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  server.stdout?.on('data', (chunk) => (logs += String(chunk)));
  server.stderr?.on('data', (chunk) => (logs += String(chunk)));
  await waitUntilListening();
});

afterAll(() => {
  server?.kill('SIGTERM');
  fs.rmSync(directory, { recursive: true, force: true });
});

describe('before any public.db has arrived', () => {
  it('answers 503 on pages, with a page a person can read and no path', async () => {
    for (const pathname of ['/', `/videos/${CONSCIOUS}`, '/search?q=a']) {
      const { status, body, headers } = await html(pathname);
      expect(status, pathname).toBe(503);
      expect(headers.get('retry-after')).toBe('60');
      expect(body).toContain('This library is not available yet');
      expect(body).toContain('noindex');
      expect(body).not.toContain(directory);
    }
  });

  it('answers 503 as JSON on the API', async () => {
    const { status, body, headers } = await api('/api/search?q=breathing');
    expect(status).toBe(503);
    expect(body.error.code).toBe('dataset_unavailable');
    expect(headers.get('retry-after')).toBe('60');
    expect(JSON.stringify(body)).not.toContain(directory);
  });

  it('reports itself unhealthy', async () => {
    const { status, body } = await api('/api/health');
    expect(status).toBe(503);
    expect(body).toEqual({ status: 'unavailable', dataset: { status: 'missing' } });
  });

  it('still validates requests', async () => {
    expect((await api('/api/search')).status).toBe(400);
  });
});

describe('once the file arrives, with no restart', () => {
  beforeAll(() => {
    buildFixture(dbPath, { videos: [...CATALOG, ...fillerVideos(24)] });
  });

  it('becomes healthy', async () => {
    const { status, body } = await api('/api/health');
    expect(status).toBe(200);
    expect(body.dataset).toMatchObject({
      status: 'ready',
      schema_version: '1',
      embedding_model: 'fake-hash-embed@1',
      videos: CATALOG.length + 24,
    });
  });

  it('serves the list as server-rendered HTML, with security headers and no caching', async () => {
    const { status, body, headers } = await html('/');
    expect(status).toBe(200);
    expect(body).toContain('<title>Acme video library</title>');
    expect(body).toContain('Conscious breathing for beginners');
    expect(body).toContain(`href="/videos/${CONSCIOUS}"`);
    expect(body).toContain('Page 1 of 2');
    expect(body).toContain('Catalog updated');
    expect(body).toContain('<link rel="canonical" href="https://media.example.internal"');
    expect(headers.get('cache-control')).toContain('no-store');
    expect(headers.get('x-content-type-options')).toBe('nosniff');
    expect(headers.get('x-powered-by')).toBeNull();
    const csp = headers.get('content-security-policy') ?? '';
    expect(csp).toMatch(/script-src 'self' 'nonce-[A-Za-z0-9+/=]+'/);
    expect(csp).not.toContain('unsafe');
  });

  it('uses a fresh nonce on each response, on every script', async () => {
    const first = await html('/');
    const second = await html('/');
    const nonceOf = (headers: Headers) => /nonce-([A-Za-z0-9+/=]+)/.exec(headers.get('content-security-policy') ?? '')?.[1];
    const nonce = nonceOf(first.headers);
    expect(nonce).toBeTruthy();
    expect(nonce).not.toBe(nonceOf(second.headers));
    const scripts = [...first.body.matchAll(/<script\b[^>]*>/g)].map((match) => match[0]);
    expect(scripts.length).toBeGreaterThan(0);
    for (const tag of scripts) expect(tag).toContain(`nonce="${nonce}"`);
  });

  it('loads scripts, styles and fonts only from itself', async () => {
    const { body } = await html('/');
    for (const match of body.matchAll(/<(?:script|link)\b[^>]*\b(?:src|href)="([^"]+)"/g)) {
      const target = match[1] as string;
      expect(target.startsWith('/') || target.startsWith('https://media.example.internal'), target).toBe(true);
    }
  });

  it('pages and filters on the server', async () => {
    const second = await html('/?page=2');
    expect(second.status).toBe(200);
    expect(second.body).toContain('Page 2 of 2');
    expect(second.body).toContain('Videos, page 2');
    expect(second.body).toContain('rel="prev"');

    const filtered = await html('/?speaker=Carla+Ficticia');
    expect(filtered.body).toContain('Meditation for a busy mind');
    expect(filtered.body).not.toContain('Conscious breathing for beginners');
    expect(filtered.body).toContain('1 video match');
    expect(filtered.body).toContain('noindex');

    const none = await html('/?speaker=Nobody');
    expect(none.status).toBe(200);
    expect(none.body).toContain('No videos match these filters');

    const past = await html('/?page=99');
    expect(past.status).toBe(200);
    expect(past.body).toContain('past the end of the list');
  });

  it('serves a video with chapters, transcript, destinations, relations and JSON-LD', async () => {
    const { status, body } = await html(`/videos/${CONSCIOUS}`);
    expect(status).toBe(200);
    expect(body).toContain('<title>Conscious breathing for beginners · Acme video library</title>');
    expect(body).toContain(`<link rel="canonical" href="https://media.example.internal/videos/${CONSCIOUS}"`);
    expect(body).toContain('Counting the breath');
    expect(body).toContain('Conscious breathing starts with counting');
    expect(body).toContain('Watch on YouTube');
    expect(body).toContain(`href="https://media.example.internal/watch/${CONSCIOUS}"`);
    expect(body).toContain('From the same recording');
    expect(body).toContain('The story of the first sun salutation');
    // The facade: no frame until the visitor asks.
    expect(body).not.toContain('<iframe');
    expect(body).toContain('application/ld+json');
    expect(body).toContain('"@type":"VideoObject"');
    expect(body).toContain('"@type":"Clip"');
    // The relation shows, and the group that makes it never does.
    expect(body).not.toContain(SOURCE_GROUP);
    expect(body).not.toContain('source_group');
  });

  it('positions by ?t= where the destination allows it, keeping the canonical URL bare', async () => {
    const { body } = await html(`/videos/${CONSCIOUS}?t=120`);
    expect(body).toContain('Play from 2:00');
    expect(body).toContain(`https://media.example.internal/watch/${CONSCIOUS}?t=120s`);
    expect(body).toContain(`<link rel="canonical" href="https://media.example.internal/videos/${CONSCIOUS}"`);
    expect(body).toContain('aria-current="true"');
  });

  it('says so when the destination cannot jump to a position', async () => {
    const { body } = await html(`/videos/${MEDITATION}?t=320`);
    expect(body).toContain('cannot open at a position');
    expect(body).toContain('Watch on Instagram Reels');
    expect(body).not.toContain(`/watch/${MEDITATION}?t=`);
  });

  it('renders a video with no chapters or transcript', async () => {
    const { status, body } = await html(`/videos/${NO_DESTINATION_PLAYER}`);
    expect(status).toBe(200);
    expect(body).toContain('Watch on Mock destination');
    expect(body).not.toContain('Chapters');
    expect(body).not.toContain('>Transcript<');
  });

  it('answers 404 for unknown videos, with a page and noindex', async () => {
    for (const pathname of ['/videos/does-not-exist', `/videos/${'x'.repeat(500)}`, '/no/such/page']) {
      const { status, body } = await html(pathname);
      expect(status, pathname).toBe(404);
      expect(body).toContain('find that page');
      expect(body).toContain('noindex');
    }
  });

  it('serves search results in the HTML, with the mode, without JavaScript', async () => {
    const { status, body } = await html('/search?q=nervous+system');
    expect(status).toBe(200);
    expect(body).toContain('Conscious breathing for beginners');
    expect(body).toContain('<mark>nervous</mark>');
    expect(body).toContain(`/videos/${CONSCIOUS}?t=120#player`);
    expect(body).toContain('Hybrid search');
    expect(body).toContain('fake-hash-embed@1');
    expect(body).toContain('noindex');
  });

  it('serves the empty search page and a no-results page', async () => {
    expect((await html('/search')).body).toContain('What are you looking for?');
    const none = await html('/search?q=zzzzqqqq');
    expect(none.status).toBe(200);
    expect(none.body).toContain('Nothing found');
    const long = await html(`/search?q=${'a'.repeat(300)}`);
    expect(long.status).toBe(200);
    expect(long.body).toContain('limited to 256 characters');
  });

  it('answers the search API with results, moments and the mode', async () => {
    const { status, body, headers } = await api('/api/search?q=conscious+breathing');
    expect(status).toBe(200);
    expect(headers.get('cache-control')).toBe('no-store');
    expect(body.mode).toBe('hybrid');
    expect(body.semantic).toEqual({
      available: true,
      model: 'fake-hash-embed@1',
      test_only: true,
      reason: null,
    });
    expect(body.total).toBeGreaterThan(0);
    const hit = body.results.find((result: { public_id: string }) => result.public_id === CONSCIOUS);
    expect(hit.matched_by).toEqual(['lexical', 'semantic']);
    expect(hit.moments[0].url).toMatch(new RegExp(`^/videos/${CONSCIOUS}\\?t=\\d+$`));
    expect(hit.moments[0].destination_url).toContain('https://media.example.internal/watch/');
  });

  it('searches diacritics, speakers and filters over HTTP, and forces lexical on request', async () => {
    const accent = await api('/api/search?q=respiracao&mode=lexical');
    expect(accent.body.mode).toBe('lexical');
    expect(accent.body.semantic.reason).toBe('not_requested');
    expect(accent.body.results[0].title).toBe('Respiração consciente na prática diária');

    const speaker = await api('/api/search?q=Carla&mode=lexical');
    expect(speaker.body.results[0].matched_in).toEqual(['speaker']);

    const filtered = await api('/api/search?q=breathing&kind=original');
    expect(filtered.body.results.map((r: { public_id: string }) => r.public_id)).toEqual([FULL_CLASS]);
  });

  it('rejects bad search requests with the error body', async () => {
    for (const [query, code] of [
      ['', 'query_required'],
      ['q=a&limit=9999', 'invalid_parameter'],
      ['q=a&mode=semantic', 'invalid_parameter'],
      [`q=${'a'.repeat(300)}`, 'query_too_long'],
    ] as const) {
      const { status, body } = await api(`/api/search?${query}`);
      expect(status, query).toBe(400);
      expect(body.error.code).toBe(code);
    }
  });

  it('treats FTS syntax in a query as text', async () => {
    for (const hostile of ['title:breathing', 'a NEAR(b)', '"', '(((', 'x"; DROP TABLE videos; --']) {
      const { status } = await api(`/api/search?q=${encodeURIComponent(hostile)}`);
      expect(status, hostile).toBe(200);
    }
    expect((await api('/api/health')).body.dataset.videos).toBe(CATALOG.length + 24);
  });

  it('never returns private-looking fields', async () => {
    const { body } = await api('/api/search?q=conscious+breathing');
    const text = JSON.stringify(body);
    expect(text).not.toContain(SOURCE_GROUP);
    expect(text).not.toContain('source_group');
    expect(text).not.toContain(directory);
  });

  it('serves robots and a sitemap of the current file', async () => {
    const robots = await html('/robots.txt');
    expect(robots.body).toContain('Sitemap: https://media.example.internal/sitemap.xml');
    expect(robots.body).toContain('Disallow: /api/');
    const sitemap = await html('/sitemap.xml');
    expect(sitemap.headers.get('content-type')).toContain('application/xml');
    expect(sitemap.body).toContain(`<loc>https://media.example.internal/videos/${CONSCIOUS}</loc>`);
  });

  it('honors the theme cookie on the server, and rejects anything else', async () => {
    const dark = await fetch(`${baseUrl}/`, { headers: { cookie: 'vidyom_theme=dark' } });
    expect(await dark.text()).toContain('data-theme="dark"');
    const evil = await fetch(`${baseUrl}/`, { headers: { cookie: 'vidyom_theme=x"><script>' } });
    const body = await evil.text();
    expect(body).not.toContain('data-theme=');
    expect(body).not.toContain('x"><script>');
    expect((await html('/')).body).not.toContain('data-theme=');
  });
});

describe('replacing and removing the file while running', () => {
  it('withdraws a video on the next request, from pages, search and the sitemap', async () => {
    replaceDataset({
      videos: CATALOG.filter((video) => video.public_id !== FULL_CLASS),
      generatedAt: '2026-11-10T06:00:00Z',
    });

    expect((await api('/api/health')).body.dataset.generated_at).toBe('2026-11-10T06:00:00Z');
    expect((await html(`/videos/${FULL_CLASS}`)).status).toBe(404);
    const search = await api('/api/search?q=savasana');
    expect(search.body.results).toEqual([]);
    expect((await html('/')).body).not.toContain('Hatha Yoga Class');
    expect((await html('/sitemap.xml')).body).not.toContain(FULL_CLASS);
    // The other clip of that source no longer lists it either.
    expect((await html(`/videos/${CONSCIOUS}`)).body).not.toContain('Hatha Yoga Class');
  });

  it('serves a file with no videos as an empty library, not an outage', async () => {
    replaceDataset({ videos: [], withVectors: false });
    const { status, body } = await html('/');
    expect(status).toBe(200);
    expect(body).toContain('No videos have been published yet');
    const search = await api('/api/search?q=breathing');
    expect(search.status).toBe(200);
    expect(search.body.mode).toBe('lexical');
    expect(search.body.semantic.reason).toBe('no_vectors');
  });

  it('is lexical, and says why, for a model with no embedder here', async () => {
    replaceDataset({ embeddingModel: 'bge-m3@1' });
    const { body } = await api('/api/search?q=breathing');
    expect(body.mode).toBe('lexical');
    expect(body.semantic).toEqual({
      available: false,
      model: 'bge-m3@1',
      test_only: false,
      reason: 'no_embedder',
    });
    expect(body.results.length).toBeGreaterThan(0);
    expect((await html('/search?q=breathing')).body).toContain('no query embedder');
  });

  it('refuses a newer schema with 503 and recovers when a good file arrives', async () => {
    replaceDataset({ schemaVersion: '2' });
    const page = await html('/');
    expect(page.status).toBe(503);
    expect(page.body).toContain('needs an update');
    expect((await api('/api/search?q=a')).body.error.details.status).toBe('unsupported_schema');

    replaceDataset({});
    expect((await html('/')).status).toBe(200);
  });

  it('fails closed on a corrupt replacement: the previous file is not served', async () => {
    const next = `${dbPath}.next`;
    fs.writeFileSync(next, 'not a database, this is not a database at all');
    fs.renameSync(next, dbPath);

    const page = await html(`/videos/${CONSCIOUS}`);
    expect(page.status).toBe(503);
    expect(page.body).toContain('temporarily unavailable');
    expect(page.body).not.toContain('Conscious breathing');
    expect((await api('/api/health')).body.dataset.status).toBe('invalid');

    replaceDataset({});
    expect((await html(`/videos/${CONSCIOUS}`)).status).toBe(200);
  });

  it('stops serving at once when the file is removed, and recovers', async () => {
    fs.rmSync(dbPath);
    expect((await html('/')).status).toBe(503);
    expect((await api('/api/search?q=breathing')).status).toBe(503);
    replaceDataset({});
    expect((await html('/')).status).toBe(200);
    expect((await api('/api/search?q=breathing')).status).toBe(200);
  });

  it('never logged a query or left a side file', async () => {
    expect(logs).not.toContain('zzzzqqqq');
    expect(logs).not.toContain('nervous');
    expect(fs.readdirSync(path.dirname(dbPath))).toEqual(['public.db']);
  });

  it('sends a long transcript on request', async () => {
    const id = 'longtranscript0000000000';
    replaceDataset({
      videos: [
        {
          public_id: id,
          title: 'A long class',
          kind: 'original',
          published_at: '2026-10-01T00:00:00Z',
          duration_seconds: 4000,
          destinations: [
            {
              destination: 'youtube',
              url: `https://media.example.internal/watch/${id}`,
              embed_url: `https://media.example.internal/embed/${id}`,
              supports_timestamps: true,
            },
          ],
          transcript: Array.from({ length: 400 }, (_, i) => ({
            start: i * 10,
            end: i * 10 + 10,
            text: `Spoken segment number ${i}.`,
          })),
        },
      ],
    });
    const preview = await html(`/videos/${id}`);
    expect(preview.body).toContain('Spoken segment number 299.');
    expect(preview.body).not.toContain('Spoken segment number 300.');
    expect(preview.body).toContain('Show the full transcript (400 segments)');

    const full = await html(`/videos/${id}?transcript=all`);
    expect(full.body).toContain('Spoken segment number 399.');
    expect(full.body).not.toContain('Show the full transcript');
    replaceDataset({});
  });

  it('did not print a framework error', () => {
    expect(logs).not.toMatch(/\bError:|unhandled/i);
  });
});
