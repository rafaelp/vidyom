# Contributing to Vidyom

Thank you for your interest in Vidyom! You can contribute to the code, the documentation in [`website/`](website/) and the product specification in [`spec/`](spec/). [`docs/status.md`](docs/status.md) lists what works today, along with the skeletons and gaps that still need work.

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Ground rules

- **English only.** Code, documentation, comments, schema descriptions, UI copy and commit messages are written in English.
- **Write the documentation in the present tense.** Pages in `website/docs/` describe how Vidyom behaves.
- **Mark the status of requirements in `spec/`** with **Decided**, **Proposed**, **Future** or **Validate**.
- **Never include credentials**, real tokens or private customer data in examples.
- **Report security issues privately** as described in [SECURITY.md](SECURITY.md).

## Ways to contribute

- **Report a bug** by opening a blank issue, with the commit and the steps to reproduce it.
- **Report a documentation problem** (an error, a gap or an unclear explanation) with the *Documentation issue* template.
- **Propose a design change** with the *Design proposal* template. Changes to a **Decided** requirement need maintainer approval in an issue before a pull request is opened.
- **Improve the code or the documentation** directly through a pull request.

## Development setup

### Code

Requirements: Go 1.27, Python 3.12 or newer, Node.js 22.13 or newer, FFmpeg 5.1 or newer, `sqlite3` and Redis.

```bash
make build        # vidyom, vidyomd and vidyom-worker into bin/
scripts/demo.sh   # the whole lifecycle for a person to watch, then removes what it made
```

Before opening a pull request, run what covers your change:

```bash
make check        # formatting, vet, static checks and Go tests
make test-python  # the AI worker
make e2e          # the acceptance scenario on the real binaries

cd library
npm run typecheck && npm run lint && npm test && npm run build && npm run test:e2e
```

### Website

Requirements: Node.js 20 or newer (Node 22 LTS recommended) and npm.

```bash
cd website
npm install
npm run start   # http://localhost:3000
```

Before opening a pull request, make sure these pass:

```bash
npm run typecheck
npm run build   # fails on broken links
```

Documentation pages live in `website/docs/`. See the [documentation development guide](website/docs/getting-started/development.md) and the [contributing guide](website/docs/contributing/guide.md) for content conventions.

## Pull requests

1. Fork the repository and create a branch from `main`.
2. Keep each pull request focused on one change.
3. Explain whether it changes a **Decided** requirement or only a **Proposed** implementation, and link the related issue.
4. Make sure CI passes.

## License

Vidyom is licensed under the [Apache License 2.0](LICENSE). Unless you state otherwise, any contribution you submit is licensed under the same terms, as described in Section 5 of the license.
