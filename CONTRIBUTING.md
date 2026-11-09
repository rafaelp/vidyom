# Contributing to Vidyom

Thank you for your interest in Vidyom! The project is at the specification stage, so most contributions today improve the architecture and implementation specification in [`website/`](website/).

By participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Ground rules

- **English only.** Code, documentation, comments, schema descriptions, UI copy and commit messages are written in English.
- **Do not describe planned behavior as implemented.** CLI commands, API endpoints, workflow nodes and plugin contracts are target interfaces until an integration test proves them.
- **Mark the status of requirements** with **Decided**, **Proposed**, **Future** or **Validate**.
- **Never include credentials**, real tokens or private customer data in examples.
- **Report security issues privately** as described in [SECURITY.md](SECURITY.md).

## Ways to contribute

- **Report a documentation problem** (an error, a gap or an unclear explanation) with the *Documentation issue* template.
- **Propose a design change** with the *Design proposal* template. Changes to a **Decided** requirement need maintainer approval in an issue before a pull request is opened.
- **Improve the documentation** directly through a pull request.

## Development setup

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
