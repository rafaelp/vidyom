---
title: "Contributing to Vidyom"
sidebar_position: 1
---

# Contributing

Vidyom is an open-source project under the Apache License 2.0, meant for any person or organization, although it originates from the Mapadeva use case.

## Contribution principles

- Work in **English** for code, documentation, API schema descriptions, comments and commit descriptions.
- Respect canonical asset IDs, source provenance, portability and private/public boundaries.
- Preserve connector independence: do not add GCS, YouTube or Loki assumptions into generic node definitions.
- Treat any user-supplied workflow JSON and plugin package as potentially untrusted.
- Back every documented API or CLI example with an integration test.
- Add schemas, capability declarations, idempotency tests, structured logs, audit and permission tests for every new node or connector.
- In `spec/`, mark requirement status as **Decided**, **Proposed**, **Future** or **Validate**, and label speculative designs as proposals. In `website/docs/`, describe product behavior in the present tense.

## Documentation changes

Edit Markdown in `website/docs/`, then run from the `website/` directory:

```bash
npm install
npm run typecheck
npm run build
```

Pull requests should explain whether they change a requirement marked **Decided** in the specification or only a technical implementation. Changes to a Decided requirement need maintainer approval in an issue before merging.

## Open-source governance

The source code and documentation live at [github.com/rafaelp/vidyom](https://github.com/rafaelp/vidyom).

- **Names and distribution:** the documentation site is at `https://www.vidyom.org`. Go module `github.com/rafaelp/vidyom`, container images at `ghcr.io/rafaelp/vidyom`, CLI distribution through the Homebrew tap `rafaelp/vidyom`, and the `vidyom` name reserved on PyPI for the Python AI worker. A trademark check is still pending.
- **License:** [Apache License 2.0](https://github.com/rafaelp/vidyom/blob/main/LICENSE). Do not assume that every referenced third-party dependency is compatible with it; audit each one before adoption.
- **Code of conduct:** [Contributor Covenant 2.1](https://github.com/rafaelp/vidyom/blob/main/CODE_OF_CONDUCT.md).
- **Contributor guidelines:** [CONTRIBUTING.md](https://github.com/rafaelp/vidyom/blob/main/CONTRIBUTING.md).
- **Security reporting:** [SECURITY.md](https://github.com/rafaelp/vidyom/blob/main/SECURITY.md), through GitHub private vulnerability reporting.
- **Release process:** Semantic Versioning, staying on `0.x` until stable. Releases are tagged `vX.Y.Z`, published as GitHub Releases with CLI binaries, with Docker images on GitHub Container Registry (GHCR) and a `CHANGELOG.md`.
