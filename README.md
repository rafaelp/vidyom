<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="website/static/img/vidyom-logo-dark.png">
    <img src="website/static/img/vidyom-logo.png" alt="Vidyom" height="80">
  </picture>
</p>

<p align="center">
  <strong>The open-source video editing studio run by AI agents.</strong><br>
  Preserve · Discover · Transform · Share<br>
  <sub>Originally built for <a href="https://www.mapadeva.com.br/">Mapadeva</a></sub>
</p>

<p align="center">
  <a href="https://github.com/rafaelp/vidyom/actions/workflows/docs.yml"><img src="https://github.com/rafaelp/vidyom/actions/workflows/docs.yml/badge.svg" alt="Documentation CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-c54b00" alt="License: Apache-2.0"></a>
</p>

## What is Vidyom?

Vidyom is an open-source, vendor-independent video editing studio built for AI agents to operate. Hand your agent hours of raw footage and a brief: it finds the stories, cuts the clips and prepares them to publish, and a person keeps the final cut. It was originally designed for [Mapadeva](https://www.mapadeva.com.br/), and it is useful to anyone: yoga schools, course creators, educators, cultural institutions, companies and personal archives.

A single recording can be 10 GB or larger and contain many distinct stories. Vidyom keeps the raw original in archival storage, keeps a production-quality working master in direct-access storage, transcribes and indexes it, and lets an agent generate clips from it **at any future time** without touching the archive. Every approved clip becomes its own traceable asset, with its own backup, master, metadata, catalog entry and optional publication through connectors.

```bash
vidyom original search "conscious breathing" --json
vidyom clip create --source ORI-001 --prompt "conscious breathing" --destination mock-main --json
vidyom approval list --json
vidyom clip preview APR-001                           # watch the candidate
vidyom approval approve APR-001 --start 10 --end 40   # a person approves the selection
vidyom approval approve APR-002                       # a person approves the publication
```

## Status

Vidyom has made no release yet: nothing is tagged or published. The first executable version runs the whole lifecycle on one host, natively or under Docker Compose: import an original, ingest, search, a clip through two approval gates, publish, export the public library and withdraw.

That lifecycle is proven with a fake AI engine, a mock LLM, a mock destination and local storage. Not ready yet:

- **AI.** Real transcription and embedding models ran only by hand, on six short spoken stories, under four minutes of synthetic speech in all. No real LLM has answered.
- **Publishing.** The YouTube connector has run only against a local mock. Vimeo, PeerTube and Instagram Reels are skeletons that answer `not_implemented`.
- **Storage.** The GCS connector has run only against a local fake.
- **Catalog.** MediaCMS, VideoVortex and Rewind are skeletons.
- **Agent access.** Agents use the CLI or the HTTPS API. There is no MCP server.
- **Key protection.** Under Docker Compose the master key is a plaintext file. Keychain, systemd-creds and TPM have not been exercised, and `gcp-kms` is not implemented.
- **Platforms.** It has run natively on macOS arm64 and in Linux arm64 containers only. No Windows or x86-64 host has run it.

The website at [www.vidyom.org](https://www.vidyom.org) describes the complete product, as if every roadmap phase were implemented. Where the two differ, [`docs/status.md`](docs/status.md) is right: it records the test results, every mock and skeleton, every gap and every open validation.

## Quick start

Requirements: Go 1.27, Python 3.12+, Node 22.13+, FFmpeg 5.1+, `sqlite3` and Redis.

```bash
scripts/demo.sh   # the whole lifecycle for a person to watch, then removes what it made
make build        # vidyom, vidyomd and vidyom-worker into bin/
make e2e          # the acceptance suite
```

The demo uses the fake AI engine and the mock destination, so it needs no cloud account and makes no paid call. To run under Docker Compose, the supported deployment path, follow the [Quick start in `docs/status.md`](docs/status.md#1-quick-start).

## Highlights

- **Built for agents.** Every action is a CLI command or a versioned JSON API call, with `--json` output, idempotency keys for retries and distinct exit codes.
- **People keep the final cut.** An editor approves every clip selection and a publisher approves what goes public.
- **Durable jobs.** A thin `vidyom` client controls persistent background jobs running in `vidyomd`, locally or across a private network, and they keep running after the client disconnects.
- **Semantic clipping.** Find complete stories from natural-language prompts and render them as independent derivative assets.
- **Workflow-native.** Repeatable video pipelines defined as versioned JSON graphs, inspired by n8n.
- **Portable media archive.** Canonical assets, metadata, captions and lineage stay independent of any platform.
- **Connector ecosystem.** Pluggable storage, publishing, AI, catalog, transcription and observability providers.
- **Multi-tenant and secure by design.** Isolated tenants, encrypted secrets, private networking, sandboxed plugins and auditable jobs.

## Documentation

The documentation is published at **[www.vidyom.org](https://www.vidyom.org)**, a [Docusaurus](https://docusaurus.io/) site built from [`website/`](website/). To run it locally (Node.js 20+, Node 22 LTS recommended):

```bash
cd website
npm install
npm run start
```

Then open http://localhost:3000. Good starting points:

- [What works today](docs/status.md)
- [Introduction](website/docs/intro.md)
- [System architecture](website/docs/architecture/overview.md)
- [Ingest originals](website/docs/workflows/ingest-original.md) and [create semantic clips](website/docs/workflows/create-clip.md)
- [Product specification](spec/vidyom-specification-v1.2.md)

## Repository layout

```text
vidyom/
├── .github/          # CI, issue and pull request templates
├── aiworker/         # Python AI worker
├── cmd/              # vidyom (CLI), vidyomd (daemon) and vidyom-worker (media worker)
├── config/           # Configuration samples
├── connectors/       # Storage, publishing, AI, transcription, catalog and observability drivers
├── deploy/           # Dockerfiles and Docker Compose
├── docs/             # Status, implementation decisions, build plans and reports
├── internal/         # Daemon, API, CLI, job engine, persistence, security and other Go packages
├── library/          # Next.js public library
├── nodes/            # Workflow node types
├── plugins/          # Sample plugin
├── profiles/         # Media profiles
├── schemas/          # JSON schemas
├── scripts/          # Demo and shared tooling
├── spec/             # Complete product specification (v1.2 current; v1.1 and v1.0 for history)
├── tests/            # Integration and end-to-end suites
├── workflows/        # Shipped workflow definitions
└── website/          # Docusaurus documentation site
    ├── docs/         # Documentation content
    ├── examples/     # Example workflows, provider bindings and media profiles
    ├── src/          # Site pages and styles
    └── static/       # Images and other static assets
```

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening an issue or pull request. Everything in this project (code, documentation, comments and commit messages) is written in English.

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Please do not report vulnerabilities in public issues. See [SECURITY.md](SECURITY.md) for how to report them privately.

## License

Vidyom is licensed under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for attribution.
