---
title: "Contributing to Vidyom"
sidebar_position: 1
---

# Contributing

Vidyom is intended to become an open-source project available to any person or organization, although it originates from the MapaDeva use case.

## Contribution principles

- Work in **English** for code, documentation, API schema descriptions, comments and commit descriptions.
- Respect canonical asset IDs, source provenance, portability and private/public boundaries.
- Preserve connector independence: do not add GCS, YouTube or Loki assumptions into generic node definitions.
- Treat any user-supplied workflow JSON and plugin package as potentially untrusted.
- Do not add APIs or CLI examples as if implemented until an integration test proves them.
- Add schemas, capability declarations, idempotency tests, structured logs, audit and permission tests for every new node or connector.
- Keep a public decision log and mark speculative designs as proposals.

## Documentation changes

Edit Markdown in `website/docs/`, then run from the `website/` directory:

```bash
npm install
npm run typecheck
npm run build
```

Pull requests should explain whether they change a decided requirement or only a proposed technical implementation. Changes to established user decisions should receive explicit design approval before merging.

## Open-source governance

The source code and documentation live at [github.com/rafaelp/vidyom](https://github.com/rafaelp/vidyom).

- **License:** [Apache License 2.0](https://github.com/rafaelp/vidyom/blob/main/LICENSE). Do not assume that every referenced third-party dependency is compatible with it; audit each one before adoption.
- **Code of conduct:** [Contributor Covenant 2.1](https://github.com/rafaelp/vidyom/blob/main/CODE_OF_CONDUCT.md).
- **Contributor guidelines:** [CONTRIBUTING.md](https://github.com/rafaelp/vidyom/blob/main/CONTRIBUTING.md).
- **Security reporting:** [SECURITY.md](https://github.com/rafaelp/vidyom/blob/main/SECURITY.md), through GitHub private vulnerability reporting.
- **Release process:** to be decided before the first runtime release.
