# Configuration

Everything here uses fictional values. Files hold only `credentialsRef` references, never a secret (§9.1).

| Directory | Used by | What it is |
|---|---|---|
| `local/` | `scripts/demo.sh`, `make e2e` | Templates for one host: `@ROOT@`, `@REDIS_PORT@`, `@HTTPS_PORT@` and `@MOCK_LLM_PORT@` are filled at run time. `local` storage, `mock` destinations, mock LLM, fake AI engine |
| `compose/` | `deploy/docker-compose.yml` | The same scenario in containers, mounted read-only at `/etc/vidyom`. `no-provider/` and `missing-key/` exist so the suite can prove the daemon refuses to start without a master key (D13) |
| `samples/` | People | What a real deployment looks like: GCS, YouTube, an overlay interface. **Validate:** none of these targets has been exercised for real; see `docs/status.md` |

Layout: `daemon.json` beside `tenants/<tenant>/{storages,publishing,ai,catalogs,observability,hooks,budget,plugins}.json` (I34, I115, I261). A missing tenant file means an empty section. `tests/integration` checks that every file here loads through the real validators.
