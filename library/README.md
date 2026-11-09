# Vidyom public library

A read-only Next.js site over one tenant's `public.db`, the dataset that `vidyomd` exports (spec §13.2, D18, D34). It is the project's only web interface. It has no accounts and no write path, and it never contacts the daemon.

Design decisions are in [`docs/decisions/library.md`](../docs/decisions/library.md).

## Run it

Node.js 22.13 or later. Node 22 and 24 print one `ExperimentalWarning` line for `node:sqlite` (I240).

```bash
npm ci
npm run fixture      # writes a fictional catalog to data/fixture/public.db
cp .env.example .env.local
npm run build
PORT=3100 npm start
```

Then open `http://localhost:3100`, or:

```bash
curl 'http://localhost:3100/api/search?q=conscious+breathing'
```

`npm run dev` runs the development server. A dataset is read at request time, so `npm run build` needs none.

## Configuration

All values are read at request time, so one build can serve any tenant. See `.env.example`.

| Variable | Default | Meaning |
|---|---|---|
| `VIDYOM_PUBLIC_DB` | `data/public.db` | Path of the tenant's `public.db`. Use an absolute path outside `npm start` |
| `VIDYOM_LIBRARY_TITLE` | `Video library` | Site name |
| `VIDYOM_LIBRARY_DESCRIPTION` | empty | Description for metadata |
| `VIDYOM_LIBRARY_SITE_URL` | empty | Absolute origin. Without it there are no canonical URLs and `sitemap.xml` answers `404` |
| `VIDYOM_LIBRARY_SEMANTIC` | `on` | `off` forces lexical search |
| `VIDYOM_LIBRARY_MAX_VECTORS` | `50000` | Above this, semantic search turns off and says so |
| `VIDYOM_LIBRARY_AI_WORKER_SOCKET` | empty | Unix socket of a Python AI worker, for query embeddings |
| `VIDYOM_LIBRARY_AI_WORKER_TCP` | empty | Or `127.0.0.1:PORT`. Loopback only |
| `VIDYOM_LIBRARY_AI_WORKER_TOKEN` | empty | The worker's `initialize` token, required on TCP |
| `VIDYOM_LIBRARY_EMBED_TIMEOUT_MS` | `5000` | After this, one query falls back to lexical |
| `VIDYOM_LIBRARY_MIN_SIMILARITY` | `0.3` | Floor for AI worker hits. An unmeasured guess |

`PORT` and `HOSTNAME` are read by Next.js.

## The dataset file

- One process serves one file. Several tenants means several processes.
- Replace the file by renaming a complete file over it. The next request serves the new one, with no restart.
- Removing the file stops the site serving it at once. Pages answer `503` and the API answers `dataset_unavailable`.
- A file that does not validate is not served, and neither is the previous one, because that one may still hold a withdrawn video.
- In a container, mount the **directory** holding the file. A rename does not propagate through a single-file bind mount.
- The file is opened with `immutable=1`: it can live on a read-only mount, and no `-wal`, `-shm` or `-journal` file appears beside it.

## Search

`GET /api/search?q=...`. Optional: `mode` (`auto` or `lexical`), `kind`, `category`, `language`, `speaker`, `tag`, `page`, `limit` (at most 50). `q` is at most 256 characters.

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
      "public_id": "k7q2m4x9a1b3c5d7",
      "title": "Conscious breathing for beginners",
      "url": "/videos/k7q2m4x9a1b3c5d7",
      "matched_by": ["lexical", "semantic"],
      "matched_in": ["title", "transcript"],
      "moments": [
        {
          "source": "transcript",
          "start_seconds": 74,
          "end_seconds": 120,
          "snippet": [{ "text": "Conscious", "match": true }],
          "url": "/videos/k7q2m4x9a1b3c5d7?t=74",
          "destination_url": "https://media.example.internal/watch/k7q2m4x9a1b3c5d7?t=74s",
          "matched_by": ["lexical", "semantic"]
        }
      ]
    }
  ],
  "dataset": { "generated_at": "2026-11-09T06:00:00Z" }
}
```

`mode` is `lexical` or `hybrid`. When it is `lexical`, `semantic.reason` says why: `disabled`, `not_requested`, `no_vectors`, `no_embedder`, `dimension_mismatch`, `too_many_vectors`, `invalid_vectors` or `embedder_error`. The search page states the same thing.

Errors are `{ "error": { "code", "message" } }`: `400` for `query_required`, `query_too_long` or `invalid_parameter`, and `503` for `dataset_unavailable`.

`GET /api/health` answers `200` when the dataset is ready and `503` when it is not.

### Query embedders

Lexical search needs nothing. Semantic search needs an embedder for the file's `embedding_model`, and vectors from different models are never compared. The embedder is the `QueryEmbedder` interface in `src/lib/embedders/`.

- **`fake-hash-embed@1`** is built in and used only when the file says so. It finds shared words and nothing else, so the page and API mark it as a test embedder. It proves the search path, not semantic quality.
- **The Python AI worker** is optional, for `bge-m3@1`. Point `VIDYOM_LIBRARY_AI_WORKER_SOCKET` at a worker on this host. No model is bundled in the site. This path is tested with a stand-in and with the worker's fake engine. **Real BGE-M3 has not been run through it, and whether query embeddings on CPU fit a site's host is an open validation (D58).** With no worker, a `bge-m3@1` file gets lexical search, including on Vercel.

## Build for an image

`npm run build` keeps Next.js telemetry off. If you run `next` directly, set `NEXT_TELEMETRY_DISABLED=1` yourself.

```bash
VIDYOM_LIBRARY_STANDALONE=1 npm run build
cp -r .next/static .next/standalone/.next/static
cp -r public .next/standalone/public
PORT=3100 HOSTNAME=0.0.0.0 node .next/standalone/server.js
```

## Checks

```bash
npm run typecheck
npm run lint
npm test             # no network, fictional data, fixture built from schemas/public-db/v1.sql
npm run build
npm run test:e2e     # boots the production server on a free port over a temporary file
```

`npm test` includes a check against the real AI worker's fake engine, skipped when `python3` or `aiworker/` is not available.

## Not validated

- Hosting on Vercel: `node:sqlite` in its runtime, the proxy reading the file, and how an updated file reaches the deployment (D58).
- Real BGE-M3 through the AI worker, its latency on CPU, and the `0.3` similarity floor.
- Memory and event-loop blocking at the 50 000 vector cap.
- Native Windows: SQLite may hold the file so that a rename over it fails. Compose is the Windows path (D41).
