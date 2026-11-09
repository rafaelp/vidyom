# Component plan: `library`

The public library: a Next.js app under `library/`, the project's only web interface. It reads one tenant's `public.db` read-only and never contacts `vidyomd`. Everything here is a **target** until committed. The contract is spec §13.2 and D18, D30, D34, D58, the master plan (sections 3, 5, 7, 8, 10) and `schemas/public-db/v1.sql`. Decisions taken to fill gaps are in [`docs/decisions/library.md`](../decisions/library.md) as `I240`–`I259`.

## 1. Scope

In scope:

- SSR pages: video list (`/`), video (`/videos/[id]`), search (`/search`).
- `GET /api/search`: lexical always, semantic when an embedder for the file's `embedding_model` is available, active mode in the response and in the UI.
- `GET /api/health` for Compose and the E2E wait loop, `sitemap.xml`, `robots.txt`.
- Timestamp links, embeds or links to the destination, chapters, released transcript, relations through `source_group`.
- Missing, invalid and unsupported files handled gracefully; replacing or removing the file while running takes effect on the next request.
- Accessible, responsive to phone width, light and dark themes, empty and error states.

Out of scope:

- Writing `public.db` (`publishing`). Shipping it to a host (`connector.PublicCatalog`, D58).
- Dockerfile and Compose service (`deploy/**`, `integration`).
- Any account, comment, analytics or admin feature. There is no write path at all.
- An in-process BGE-M3 model. D58 stays a validation; the embedder interface is where one goes.

## 2. Data contract

The reader treats `schemas/public-db/v1.sql` as read-only input from `publishing`:

| Table | Used for |
|---|---|
| `meta` | `schema_version` gate (only `1`), `generated_at` shown in the footer, `embedding_model`, `embedding_dimensions` |
| `videos` | List, video page, filters, metadata search on `speaker`, `category`, `tags` |
| `destinations` | Embed or link, `supports_timestamps` decides whether a position link exists |
| `chapters` | Chapter list, timestamp links |
| `transcript_segments` | Released transcript with timestamps (D30: the exporter has already removed sensitive segments) |
| `embeddings` | Semantic scan, little-endian float32 BLOBs, brute-force cosine (I29) |
| `search_fts` | Lexical search with `bm25` and `snippet`, tokenizer `unicode61 remove_diacritics 2` |

The library shows only fields present in the file. It computes nothing that looks like data about a person or a video beyond formatting, and fabricates no counts, views or claims.

## 3. Reading the file

`src/lib/sqlite.ts` is the only place that opens SQLite. `src/lib/dataset.ts` owns the lifecycle:

- Path from `VIDYOM_PUBLIC_DB`. One running site serves one tenant's file (D6 means one file per tenant, so one process per tenant).
- Opened read-only with `immutable=1`, so it creates no `-wal`, `-shm` or `-journal`, works on a read-only mount, and accepts a file whose header says WAL.
- Every request `stat`s the path and compares inode, size and mtime with the open generation. A change opens a new generation and closes the old. Absence closes the connection and reports `missing`, so a removed dataset stops being served at once.
- A generation is validated on open: `meta` present, `schema_version` equal to `1`, required keys, required tables. Outcomes: `ready`, `missing`, `invalid`, `unsupported_schema`.
- Queries run synchronously inside one `withSnapshot` call, so one page reads from one generation. A SQLite error during a snapshot drops the generation and retries once from a fresh `stat`.
- Nothing is cached across generations. Facets, metadata index and the vector matrix hang off the generation object and die with it.
- It fails closed: a replacement that does not validate stops the site serving rather than keeping the previous file, since the previous file may hold a withdrawn video. An `invalid` state is re-checked every 5 seconds even if the `stat` is unchanged.

Pages are `force-dynamic` with no full-route or CDN cache, because withdrawal must be immediate (D17, R15).

## 4. Search

`src/lib/search/`:

- `fts-query.ts`: turns free text into a safe FTS5 expression. Tokens are extracted and quoted, `"..."` makes a phrase, the last token is a prefix. User input never reaches `MATCH` as syntax.
- `lexical.ts`: AND query first, OR when AND finds nothing (reported as `match: all|any`). Ranks by `bm25` weighted per `source`. Snippets use private-use markers parsed into segments, so no HTML ever comes from the database.
- `metadata.ts`: matches `speaker`, `category` and `tags`, which `search_fts` does not hold, so §13.2's "search by instructor" works. Diacritic-insensitive, in memory per generation.
- `semantic.ts`: loads vectors lazily into one `Float32Array` per generation, L2-normalized, up to a cap. Cosine scan, top K.
- `fusion.ts`: reciprocal rank fusion with `k = 60` (I43), at video level, merging moments that overlap.
- `service.ts`: the single entry point used by both the API route and the SSR page.

Response shape, `snake_case` per I15:

```json
{
  "query": "conscious breathing",
  "mode": "hybrid",
  "match": "all",
  "semantic": { "available": true, "model": "fake-hash-embed@1", "test_only": true, "reason": null },
  "total": 1,
  "page": 1,
  "limit": 20,
  "results": [
    {
      "public_id": "...",
      "title": "...",
      "kind": "clip",
      "url": "/videos/...",
      "matched_by": ["lexical", "semantic"],
      "matched_in": ["title", "transcript"],
      "score": 0.0327,
      "snippet": [{ "text": "...", "match": false }],
      "moments": [
        {
          "source": "transcript",
          "start_seconds": 12.5,
          "end_seconds": 20,
          "snippet": [{ "text": "...", "match": false }, { "text": "breathing", "match": true }],
          "url": "/videos/...?t=12",
          "destination_url": "https://media.example.internal/watch/...?t=12",
          "matched_by": ["lexical"]
        }
      ],
      "moment_count": 1
    }
  ],
  "dataset": { "generated_at": "2026-11-09T00:00:00Z" }
}
```

`mode` is `lexical` or `hybrid`. When it is `lexical`, `semantic.reason` says why: `no_vectors`, `no_embedder`, `dimension_mismatch`, `embedder_error`, `too_many_vectors`, `invalid_vectors`, `disabled` or `not_requested`. The UI turns these into plain sentences. `score` only orders one response; it is not comparable between responses.

Request: `q` (required, at most 256 characters), `mode` (`auto` by default, or `lexical`), `kind`, `category`, `language`, `speaker`, `tag`, `page`, `limit` (default 20, max 50). Errors use `{ "error": { "code", "message" } }`: `400 query_required`, `400 query_too_long`, `400 invalid_parameter`, `503 dataset_unavailable` with `Retry-After`. Filters also apply on the list page. A query with no searchable word is a `200` with no results.

## 5. Embedders

`src/lib/embedders/`:

```ts
interface QueryEmbedder {
  readonly model: string;
  readonly dimensions: number;
  readonly minSimilarity: number;
  readonly testOnly: boolean;
  embed(text: string, signal?: AbortSignal): Promise<Float32Array>;
}
```

- `fake-hash.ts`: `fake-hash-embed@1` per `schemas/ai-worker-rpc.v1.json#/$defs/fakeHashEmbed` (I27), checked against that file's test vectors. Selected only when the file says that model, so it can never be mixed with real vectors, and the UI says it matches shared words only.
- `ai-worker.ts`: optional client of the Python AI worker's JSON-RPC (`initialize`, `embed` with `kind: "query"`) over a Unix socket or loopback TCP. It holds no model. The worker is not the daemon, and it reports its `embedding_model`, which must equal the file's. Mock-tested; real BGE-M3 stays **Validate** (D58).
- `registry.ts`: resolves an embedder for the generation's model. No match means lexical, never a substitute (R18, I27).

No model is bundled.

## 6. Pages and components

| Route | Content | Empty and error states |
|---|---|---|
| `/` | Filter form, card grid, pagination | Nothing published yet; no match for filters; page past the end |
| `/videos/[id]` | Player facade or destination links, metadata, chapters, transcript, related | `404` for unknown or withdrawn; no destination; no chapters; no transcript |
| `/search` | Query box, mode notice, results with moments | No query; no results; `mode` notice; fetch failure |
| any, dataset not ready | Answered by `proxy.ts` with a self-contained `503` and `Retry-After` | Copy differs for missing, invalid and unsupported |

Components: `SiteHeader`, `SiteFooter`, `ThemeToggle`, `HeaderSearch`, `SearchExperience`, `SearchResults`, `Player`, `Thumbnail`, `VideoCard`, `TimestampLink`, `Pagination`, `FilterForm`, `FilterFields`, `ModeNotice`, `Snippet`, `EmptyState`, `DatasetUnavailable`, `JsonLd`.

Rules:

- Everything works without JavaScript: forms are GET, chapters are links. JavaScript adds live search, the embed swap and the theme toggle.
- The player is a facade. The destination's iframe loads only when the visitor asks, so a page view makes no request to a destination except its thumbnail, with `referrerPolicy="no-referrer"`.
- Position links: `?t=<seconds>` on our own page seeks the embed. External links get a position only when `supports_timestamps` is set, with a per-driver URL form.
- Destination links are rendered only when `http:` or `https:`. Embeds and thumbnails only when `https:`, since CSP and mixed content rule out the rest.
- A transcript renders its first 300 segments, with a link to the rest, because a three-hour recording would otherwise send over a megabyte twice.
- Theme: a cookie written only when the visitor uses the toggle, read on the server, so no inline script and no flash. No cookie means `prefers-color-scheme`.
- CSP with a per-request nonce from `proxy.ts`. No third-party scripts, analytics or font hosts. Fonts are self-hosted from `@fontsource-variable` packages. Next.js telemetry is disabled through `scripts/next.mjs`, which every npm script uses.
- SEO: metadata per page, canonical when the site URL is configured, Open Graph, `VideoObject` JSON-LD with `Clip` parts for chapters, sitemap from the current file. Search and filtered list pages are `noindex`. `?t=` pages are canonical to the bare video URL.

## 7. Configuration

| Variable | Default | Meaning |
|---|---|---|
| `VIDYOM_PUBLIC_DB` | `data/public.db` | Path of the tenant's `public.db` |
| `VIDYOM_LIBRARY_TITLE` | `Video library` | Site name |
| `VIDYOM_LIBRARY_DESCRIPTION` | empty | Site description for metadata |
| `VIDYOM_LIBRARY_SITE_URL` | empty | Absolute origin for canonical URLs and the sitemap |
| `VIDYOM_LIBRARY_SEMANTIC` | `on` | `off` forces lexical |
| `VIDYOM_LIBRARY_MAX_VECTORS` | `50000` | Above this, semantic reports `too_many_vectors` (I43's cap) |
| `VIDYOM_LIBRARY_AI_WORKER_SOCKET`, `VIDYOM_LIBRARY_AI_WORKER_TCP`, `VIDYOM_LIBRARY_AI_WORKER_TOKEN` | empty | Optional query embedder |
| `VIDYOM_LIBRARY_EMBED_TIMEOUT_MS` | `5000` | Timeout before falling back to lexical |
| `VIDYOM_LIBRARY_MIN_SIMILARITY` | `0.3` | Floor for the AI worker embedder (**Validate**) |

All are read at request time, so one build serves any tenant. A relative `VIDYOM_PUBLIC_DB` resolves against the working directory; use an absolute path outside `npm start`.

## 8. Tests

`npm test` (Vitest), all fictional data, no network:

- Fixture generator builds a database from `schemas/public-db/v1.sql`; tests read the real schema, so a schema change that breaks the reader fails here.
- Dataset lifecycle: missing, garbage, truncated, unsupported version, WAL-header file, read-only directory, no side files, writes refused, atomic replace, in-place overwrite, removal.
- Queries: pagination, filters, children, related, malformed `tags`.
- Search: diacritics, phrase, prefix, AND to OR fallback, metadata, filters, FTS syntax injection, hybrid mode and reported reasons, model mismatch, embedder failure, dimension mismatch, bad vectors.
- Embedders: contract test vectors, AI worker client against a local NDJSON server (initialize, token, model mismatch, timeout, reconnect).
- API handlers: status codes, error body, validation, shape.
- Components: empty and error states, position links with and without `supports_timestamps`, no unsafe URLs, snippet rendering without HTML, keyboard-visible controls.
- Contract guard: source scan proving no daemon client, no third-party script host.

`npm run test:e2e`: boots the production server on a free non-default port against a temporary file and exercises pages and API over HTTP, including `503` with no file, replacement while running and removal. Stops the server it started.

Also: `npm run typecheck`, `npm run lint`, `npm run build`. A browser pass at phone and desktop width, light and dark, is done manually with screenshots, since CI has no browser.

## 9. Order of work

1. Scaffold with pinned versions, `.gitignore`, lockfile. Commit.
2. `lib`: SQLite adapter, dataset lifecycle, fixture generator, tests. Commit.
3. `lib`: queries. Commit.
4. Embedders and search service with tests. Commit.
5. API routes. Commit.
6. Theme, layout, CSP, list and video pages. Commit.
7. Search UI and client behavior. Commit.
8. E2E over HTTP, SEO routes, README. Commit.
9. Run the production server for real, visual pass, fix, final results.

## 10. Coordination

- `publishing`: the reader relies on `meta.schema_version` and required keys, on a single checkpointed file being renamed into place, and on `search_fts.start_seconds` being numeric for `chapter` and `transcript` rows. Tolerated: `NULL`, `''` or `0` for `title` and `description`.
- `integration`: env variables above, `GET /api/health`, standalone output for the image through `VIDYOM_LIBRARY_STANDALONE=1` at build time, Node 22.13 or later. Mount the **directory** holding `public.db`, never the single file: a rename does not propagate through a single-file bind mount, so the site would keep serving a withdrawn video.
- No schema change is planned.
