<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="website/static/img/vidyom-logo-dark.png">
    <img src="website/static/img/vidyom-logo.png" alt="Vidyom" height="80">
  </picture>
</p>

<p align="center">
  <strong>Open-source, AI-powered video workflow engine.</strong><br>
  Preserve · Discover · Transform · Share
</p>

<p align="center">
  <a href="https://github.com/rafaelp/vidyom/actions/workflows/docs.yml"><img src="https://github.com/rafaelp/vidyom/actions/workflows/docs.yml/badge.svg" alt="Documentation CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-c54b00" alt="License: Apache-2.0"></a>
</p>

> [!IMPORTANT]
> **Project status: specification.** This repository currently contains the architecture and implementation specification for Vidyom. `vidyom`, `vidyomd`, `vidyom-worker` and every command shown in the documentation are **proposed interfaces**, not released software.

## What is Vidyom?

Vidyom is a vendor-independent system for preserving, cataloging, searching, editing and publishing audiovisual knowledge. It was originally designed for [MapaDeva](https://www.mapadeva.com.br/), but is meant to be useful to anyone: yoga schools, course creators, educators, cultural institutions, companies and personal archives.

A single recording can be 10 GB or larger and contain many distinct stories. Vidyom keeps the raw original in archival storage, keeps a production-quality working master in direct-access storage, transcribes and indexes it, and lets you generate clips from it **at any future time** without touching the archive. Every approved clip becomes its own traceable asset, with its own backup, master, metadata, catalog entry and optional publication to multiple platforms.

## Highlights

- **Workflow-native.** Repeatable video pipelines defined as versioned JSON graphs, inspired by n8n.
- **CLI + daemon.** A thin `vidyom` client controls persistent background jobs running in `vidyomd`, locally or across a private network.
- **AI-assisted clipping.** Find complete stories from natural-language prompts and render them as independent derivative assets.
- **Portable media archive.** Canonical assets, metadata, captions and lineage stay independent of any platform.
- **Connector ecosystem.** Pluggable storage, publishing, AI, catalog, transcription and observability providers.
- **Multi-tenant and secure by design.** Isolated tenants, encrypted secrets, private networking, policy-aware plugins and auditable jobs.

## Documentation

The specification is published at **[www.vidyom.org](https://www.vidyom.org)**, a [Docusaurus](https://docusaurus.io/) site built from [`website/`](website/). To run it locally (Node.js 20+, Node 22 LTS recommended):

```bash
cd website
npm install
npm run start
```

Then open http://localhost:3000. Good starting points:

- [Introduction](website/docs/intro.md)
- [System architecture](website/docs/architecture/overview.md)
- [Ingest originals](website/docs/workflows/ingest-original.md) and [create semantic clips](website/docs/workflows/create-clip.md)
- [Implementation roadmap](website/docs/operations/roadmap.md)
- [Open questions](website/docs/reference/open-questions.md)

## Repository layout

```text
vidyom/
├── .github/          # CI, issue and pull request templates
├── spec/             # Complete product specification (v1.2 current; v1.1 and v1.0 for history)
└── website/          # Docusaurus documentation site
    ├── docs/         # Specification content
    ├── examples/     # Illustrative workflows, provider bindings and media profiles
    ├── src/          # Site pages and styles
    └── static/       # Images and other static assets
```

The runtime code will live alongside `website/`, following the [proposed repository layout](website/docs/architecture/repository.md).

## Contributing

Contributions to the specification are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening an issue or pull request. Everything in this project (code, documentation, comments and commit messages) is written in English.

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Please do not report vulnerabilities in public issues. See [SECURITY.md](SECURITY.md) for how to report them privately.

## License

Vidyom is licensed under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for attribution.
