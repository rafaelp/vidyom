# Implementation decisions: `library`

Decisions of the public library component, numbered `I240`–`I259` per section 8 of the [master plan](../plans/00-master-plan.md). They never change or relabel the maintainer's `Dn`. The format matches [`docs/decisions.md`](../decisions.md): each states the choice, the reason and what would make us revisit it. What a decision says the code does is a **target** until the component commits it.

**Validate** marks a working default resting on a §17.2 validation still unproven; for this component that is D58.

---

## Reading `public.db`

### I240. SQLite access: `node:sqlite` with `immutable=1`

- **Choice.** Node's built-in `node:sqlite`, opened through a `file:` URL with `?immutable=1` and `readOnly: true`, behind one adapter, `src/lib/sqlite.ts`. Node 22.13 or later. No native addon.
- **Why.** Tested before choosing (scratch spike on Node 22.16): a plain read-only open of a file whose header says WAL, in a read-only directory, fails with `attempt to write a readonly database`. `immutable=1` opens it, creates no side files, ignores stale `-wal` and `-shm`, and works on read-only mounts, which is what a shipped file on a site's host looks like. `better-sqlite3` compiles with `SQLITE_USE_URI=0`, so it cannot pass `immutable=1`, and it would add a native build step to the image and to Vercel. FTS5 with `unicode61 remove_diacritics 2`, `bm25` and `snippet` all work in Node's bundled SQLite 3.49.
- **The catch.** `node:sqlite` is marked experimental on Node 22 and 24, and prints one `ExperimentalWarning` line. The API surface used is small and stays in one file. `immutable` also means the file must not change in place; I242 covers that.
- **Revisit if.** The API changes incompatibly, or Node drops FTS5 from its build. `better-sqlite3` then fits behind the same adapter, with the exporter guaranteeing `journal_mode=DELETE`.

### I241. One process serves one tenant

- **Choice.** `VIDYOM_PUBLIC_DB` names one file. Several tenants means several processes, each with its own site name and origin. Configuration is read at request time, so one build serves any tenant. Default path `data/public.db`; I29's `library/data/<tenant>/public.db` is set explicitly.
- **Why.** D6 gives each tenant its own export and D18 says the site reads it. Each tenant's public site has its own domain and identity, and a process boundary is the simplest proof that no request can read another tenant's file.
- **Revisit if.** Many tenants on one host make per-tenant processes wasteful: then a host-to-file map, resolved server-side and never from a request parameter.

### I242. Replacing the file: `stat` per request, fail closed

- **Choice.** Every request compares `stat` (inode, size, mtime) with the open generation. Changed: open and validate the new file, close the old. Absent: close and report `missing`. A new file that fails validation stops the site with `invalid`; it does **not** fall back to the previous generation. An error during a query drops the generation and retries once from a fresh `stat`.
- **Why.** The exporter renames atomically (I29), and on POSIX an open descriptor keeps serving the old inode forever, so without this check a withdrawn video would stay visible. Withdrawal must be immediate (D17, §15.8), so a stale catalog is worse than an unavailable one. `fs.watch` is unreliable across mounts and platforms; `stat` is microseconds.
- **Contract with shipping.** The file must arrive by rename, as one checkpointed file. With `immutable=1` a pending `-wal` is ignored, and an in-place overwrite can be read half-written, in which case the site reports `invalid` until the file is whole.
- **Limit.** A single-file Docker bind mount pins the inode, so a rename on the host is never seen. Mount the directory. On native Windows SQLite holds the file without `FILE_SHARE_DELETE`, so a rename over it can fail while the site has it open. Not tested; Compose is the Windows path (D41). **Validate** with D58.
- **Revisit if.** A host needs the file on object storage, per D58.

### I243. No page or API cache

- **Choice.** All routes are `force-dynamic`, which sends `Cache-Control: private, no-cache, no-store`. No ISR, no CDN caching, no ETag.
- **Why.** §15.8 and D17 require a withdrawn video to leave the library immediately, and any TTL is a window in which it does not. The queries are local and fast.
- **Revisit if.** Traffic requires a CDN: then purge on the file's generation change.

### I244. Dataset states and status codes

- **Choice.** `ready`, `missing`, `invalid`, `unsupported_schema`. Not ready gives `503` with `Retry-After: 60` on pages, as a self-contained response from `proxy.ts`, and on the API, with `dataset_unavailable`. Pages also render that state themselves, in case the file disappears between the proxy and the page; that window answers `200` with `noindex`. A valid file with no videos is a normal `200` with an empty state. The UI and API never reveal the file path.
- **Not a rewrite.** A rewrite to an `/unavailable` route was the first version. Run over HTTP it hung for two minutes: Next.js treats a rewrite as external when its origin differs from the server's, `NextURL` turns `127.0.0.1` into `localhost`, so on a server started with `-H 127.0.0.1` the rewrite becomes an HTTP proxy to `localhost`, which on macOS resolves to `::1`, where nothing listens. Building the target from the `Host` header would fix that and open an SSRF. A plain response has no such path.
- **Why.** A search engine reading `200` on an outage could drop pages; `503` tells it to come back. An empty catalog is a legitimate state, and after a withdrawal it may be the correct one. `schema_version` must equal `1`: v1.sql says a new version is a new file, so a newer file is refused rather than misread.
- **Revisit if.** A schema version 2 ships; the reader then gains a second branch.

---

## Search

### I245. Lexical query language

- **Choice.** Free text is reduced to tokens (`[\p{L}\p{N}]+` after NFC, at most 16), each quoted for FTS5. `"..."` makes a phrase. The last token outside a phrase is a prefix, so typing works and `breath` finds `breathing`. All tokens must match; when that finds nothing, any token may, and the response says `match: "any"`. Nothing the user types reaches `MATCH` as syntax. At most 256 characters. A query with no token answers `200` with no results.
- **Why.** FTS5 syntax errors on ordinary punctuation, and exposing `NEAR` or column filters adds attack surface for no user need. The tokenizer has no stemmer and the content is multilingual (D9), so a stemmer is not an option at query time.
- **Known limit.** `unicode61` treats a run of CJK characters as one token, so those languages match poorly. The schema's tokenizer decides that.
- **Revisit if.** The schema adds a tokenizer that needs other handling.

### I246. Metadata search for `speaker`, `category`, `tags`

- **Choice.** These fields match in memory, per generation, with the same folding as FTS (NFKD, marks removed, lowercase, prefix on the last token). A video matches when all query tokens appear across them. Such videos rank ahead of text hits.
- **Why.** §13.2 requires search by instructor, and `search_fts` holds titles, descriptions, chapters and transcript only. The file is read-only, so no index can be added at runtime, and changing the schema for this would widen `publishing`'s contract for a small gain. A full scan of one row per video, once per generation, is cheap.
- **Revisit if.** Catalogs reach hundreds of thousands of videos, or `publishing` adds these fields to `search_fts`.

### I247. Ranking and fusion

- **Choice.** Lexical hits score `-bm25 × weight`, with weights title 4, chapter 2.5, description 1.5, transcript 1. The top 400 rows are grouped by video. Hybrid combines the lexical and semantic video rankings with reciprocal rank fusion, `k = 60`. Ties break by newest, then `public_id`, for stable output. Each result carries at most 3 moments, chosen by relevance and shown in time order. A semantic window overlapping a lexical moment merges into it. `score` orders one response only.
- **Why.** Same fusion and `k` as the daemon's search (I43), so public and private ranking behave alike. `source` is an unindexed FTS column, so `bm25()` column weights cannot express this; a post-hoc multiplier can.
- **Revisit if.** Real content with real models shows poor ordering. Weights are unmeasured.

### I248. Semantic scan

- **Choice.** On the first semantic query of a generation, all vectors load into one `Float32Array`, L2-normalized, with their text left in the database. Cosine is then a dot product over that array, on the request's thread, keeping the top 200. Above `VIDYOM_LIBRARY_MAX_VECTORS` (default 50 000, I43's cap, about 200 MB) or with a wrong-sized BLOB, semantic turns off with a reported reason.
- **Why.** I29 stores plain BLOBs so the site can brute-force. Reading 50 000 BLOBs per query would dominate the latency, and the file cannot change within a generation, so one load is safe. The stored vectors are normalized here rather than trusted to be.
- **Validate (D58).** Memory and event-loop blocking at the cap on the real host. `VIDYOM_LIBRARY_SEMANTIC=off` is the escape.
- **Revisit if.** Load tests set another cap, or a worker thread is needed.

### I249. Query embedders

- **Choice.** Interface `QueryEmbedder` in `src/lib/embedders/`. Two implementations, no model bundled:
  1. `fake-hash-embed@1` in TypeScript per I27. Selected only when the file's `embedding_model` says so. The UI and API mark it `test_only` and say it matches shared words only.
  2. An optional JSON-RPC client of the Python AI worker (`schemas/ai-worker-rpc.v1.json`, `embed` with `kind: "query"`), over a Unix socket or loopback TCP, configured by `VIDYOM_LIBRARY_AI_WORKER_SOCKET` or `VIDYOM_LIBRARY_AI_WORKER_TCP`. Used only when the worker's reported `embedding_model` and dimensions equal the file's.

  Anything else means lexical, never a substitute (R18). A failure or a timeout (5 s) on one query falls back to lexical for that response, with the reason reported. At most 4 queries wait on the worker at once.
- **Why.** The master plan asks for the embedder behind an interface and optional. The AI worker already exists as a separate process holding the model, and it has no access to Redis, storage or secrets (I6), so the site reaching it is not the site reaching the daemon. The site sends it only the query text.
- **Deviation to flag.** D18 says the site "computes query embeddings on CPU", which reads as in-process. This version does it on the site's host through a sidecar, and a host with no sidecar, Vercel included, gets lexical search. An in-process model is exactly what D58 leaves to validate, and it would fit behind this interface.
- **Validate (D58).** No real BGE-M3 has been run through this path; `VIDYOM_LIBRARY_MIN_SIMILARITY` (0.3) is an unmeasured guess.
- **Revisit if.** D58's result chooses a host and a runtime.

### I250. Mode reporting

- **Choice.** `mode` is `lexical` or `hybrid`. `semantic` carries `available`, `model`, `test_only`, and `reason` when unavailable: `disabled`, `not_requested`, `no_vectors`, `no_embedder`, `dimension_mismatch`, `too_many_vectors`, `invalid_vectors`, `embedder_error`. The same object drives the UI notice. `mode=lexical` on the API forces lexical; there is no semantic-only mode, because lexical is always on.
- **Why.** The master plan requires the mode in the response and the UI. A site that silently lost semantic search would look like a site with worse content.
- **Revisit if.** Clients need a semantic-only ranking.

---

## Pages and behavior

### I251. Position links

- **Choice.** The site's own `?t=<seconds>` is the link to a moment, deep-linkable and working without JavaScript. External position links exist only when the row's `supports_timestamps` is set:

  | Driver | Watch URL | Embed URL |
  |---|---|---|
  | `youtube` | `t=<n>s` | `start=<n>` |
  | `vimeo` | `#t=<n>s` | `#t=<n>s` |
  | `peertube` | `start=<n>` | `start=<n>` |
  | any other | `t=<n>` | `start=<n>` |

  Without support, the page says where the moment is and that the destination cannot jump to it. The preferred destination is the first with an embed and timestamps, then an embed, then timestamps, then any, ordered by driver and URL.
- **Why.** §13.2 says "when supported by the platform", and the schema carries that per row. The URL form is the only thing the file does not say, and it is well known for these three. The generic form is a guess that only applies when a new driver declares support.
- **Revisit if.** A driver needs another form: then it belongs in the schema.

### I252. Player facade

- **Choice.** A page shows the thumbnail and a play control, linking to the destination. The iframe loads only on request, with `sandbox="allow-scripts allow-same-origin allow-presentation allow-popups"`, `referrerPolicy="strict-origin-when-cross-origin"`, and no rewriting of the URL. Only `https:` embeds. Thumbnails are plain `<img>` with `referrerPolicy="no-referrer"`, falling back to a generated tile. `next/image` is not used.
- **Why.** Destination players are third-party scripts, and the project allows none on a page view. Loading one must be the visitor's act. `next/image` would need an allow-list of destination hosts and would make the site fetch them. The visitor still fetches thumbnails from the destination, which is what `thumbnail_url` is for: the schema says it comes from the destination, never a bucket.
- **Revisit if.** Thumbnails should be proxied so a page view contacts no destination at all.

### I253. Theme

- **Choice.** Light and dark through CSS tokens. With no preference stored, `prefers-color-scheme` decides. The toggle writes a `vidyom_theme` cookie (one year, `SameSite=Lax`), which the server reads to set `data-theme`, so there is no inline script and no flash. No cookie exists until the visitor uses the toggle. Brand colors match the docs site: `#c54b00` and `#fd8a02`, on warm near-black `#15110f`.
- **Why.** Pages are dynamic anyway (I243), so reading a cookie costs nothing, and it keeps the CSP free of inline script.
- **Revisit if.** Page caching arrives; a cookie would split the cache.

### I254. Security posture

- **Choice.** CSP with a per-request nonce from `proxy.ts`: `script-src 'self' 'nonce-…'`, `connect-src 'self'`, `img-src 'self' https: data:`, `frame-src https:`, `font-src 'self'`, `object-src 'none'`, `base-uri 'self'`, `form-action 'self'`, `frame-ancestors 'self'`. `nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, a narrow `Permissions-Policy`, no `X-Powered-By`. Snippets are parsed into segments, never rendered as HTML. Destination URLs are scheme-checked at the reader. `/api/search` has no CORS headers. Query text is never logged. No login, no write path.
- **No rate limiter in this version.** A limiter keyed on client IP needs to trust a reverse proxy this project does not specify. Cost per query is bounded by the query length, result and embedder limits. Deployments put limits at the proxy or on Vercel.
- **Revisit if.** Abuse appears on a self-hosted deployment.

### I255. No external calls

- **Choice.** Fonts (DM Sans, Space Grotesk, as on the docs site) are self-hosted from `@fontsource-variable` npm packages through `next/font/local`, so neither the build nor a page view contacts a font host. `next.config.ts` sets `NEXT_TELEMETRY_DISABLED`, so the framework reports nothing to Vercel. Client code calls only `/api/search`. A test scans the source for other hosts and for anything naming the daemon.
- **Why.** The project allows no third-party scripts or analytics (section 8 of the plan), and D34 requires the site never to reach the daemon.
- **Also.** Both fonts are OFL-1.1. They count for the open license audit (§17.2, I32).
- **Revisit if.** Never for scripts or analytics without a maintainer decision.

### I256. SEO

- **Choice.** Per-page metadata and Open Graph. `VideoObject` JSON-LD with `hasPart` `Clip`s when a position link exists. `sitemap.xml` from the current file. Canonical URLs, Open Graph URLs and the sitemap need `VIDYOM_LIBRARY_SITE_URL`: without it canonical is omitted and `sitemap.xml` answers `404`. Search and filtered list pages are `noindex, follow`. `?t=` pages are canonical to the bare video URL. Host headers are never used to build absolute URLs.
- **Why.** D34 chooses SSR for SEO. Deriving an origin from `Host` invites poisoning. Search pages are an unbounded URL space with no unique content.
- **Revisit if.** Destinations prefer `SeekToAction`.

### I257. Transcripts and relations

- **Choice.** The released transcript shows on the video page with position links, first 300 segments with a link to the rest (`?transcript=all`). Gaps left by excluded segments are not marked. Related videos come from equal `source_group` and show under "From the same recording"; the group value stays on the server and never appears in HTML or JSON.
- **Why.** The exporter has already removed sensitive segments (D30); a visible marker would point at them. §13.2 allows relations "without exposing the private original", and the group value has no use to a visitor. The App Router sends content twice (HTML plus flight data), so a three-hour transcript would cost over a megabyte per view.
- **Revisit if.** Editors want a "part omitted" marker.

### I258. Versions and build

- **Choice.** Exact pins: Next.js 16.4.5, React 19.3.1, TypeScript 6.0.3, Vitest 5.1.0. `engines.node >= 22.13`. Turbopack, the default. `VIDYOM_LIBRARY_STANDALONE=1` at build time produces `output: "standalone"` for the image; otherwise `next start` runs normally, since it refuses standalone builds.
- **Why.** TypeScript 7.0.2 builds and typechecks, but `typescript-eslint` refuses it (`does not support TS 7.0`, supported range below 6.1), so 6.0.3 keeps lint working.
- **Revisit if.** `typescript-eslint` supports 7.

---

## Disagreements found

Pointed out rather than resolved in the sources:

| # | Where | Disagreement | This component |
|---|---|---|---|
| 1 | D18 vs D58 | D18 decides the site computes query embeddings on CPU; D58 leaves open whether that fits the host | Sidecar or lexical (I249), reported in the UI |
| 2 | §13.2, R44 vs D18 | R44 allows "a bucket with JSON/static files", D18 fixes SQLite | SQLite only |
| 3 | §13.2 vs `search_fts` | The spec asks for search by instructor; the schema's FTS table does not hold `speaker` | In-memory metadata match (I246), no schema change |
