# Vidyom documentation website

An English-language Docusaurus documentation site for **Vidyom**, an open-source AI-powered video workflow engine originally envisioned for [MapaDeva](https://www.mapadeva.com.br/).

> **Project status:** This site contains architecture and implementation specifications. `vidyom`, `vidyomd`, the workers, and all sample commands are **proposed interfaces**, not an already released product.

## Requirements

- Node.js 20 or newer (Node 22 LTS recommended)
- npm

See the [repository README](../README.md) for the project overview, license and contribution guidelines.

## Local development

Run these commands from this `website/` directory:

```bash
npm install
npm run start
```

Open http://localhost:3000. To verify the production output:

```bash
npm run build
npm run serve
```

## Deployment

The official site is deployed on Vercel at https://www.vidyom.org, which is the default `url` in `docusaurus.config.ts`. To deploy elsewhere, set `DOCS_SITE_URL` and `DOCS_BASE_URL` for your domain and path, run `npm run build` and serve `build/` from any static host. Pages are emitted as `path/index.html`, so `/docs/intro` works without server rewrites.

## Dependency notes

`package.json` pins `d3` 7.9.0, `d3-selection` 3.0.0 and `d3-transition` 3.0.1 through `overrides`. The releases of 2026-11-03 (`d3` 7.9.1+, `d3-selection` 3.0.1, `d3-transition` 3.0.2) ship bundled entry points that let webpack drop `d3-transition`'s side effects, so hovering a Mermaid diagram node throws `transition is not a function`. Remove the overrides only after checking that hovering diagram nodes no longer logs errors.

## Content conventions

- All documentation, UI copy, sample code, and code comments must be in English.
- Use **Decided**, **Proposed**, **Future**, and **Validate** to distinguish decisions from design suggestions and integration assumptions.
- Workflows and commands are target interfaces until the implementation lands.
- Never include credentials in documentation examples.

See [Documentation introduction](./docs/intro.md) and [Implementation roadmap](./docs/operations/roadmap.md).

## Design examples

`examples/workflows/` contains illustrative workflow graphs, `examples/config/` holds provider binding examples, and `examples/profiles/` includes proposed media profiles. They are **not executable** until the Vidyom runtime and node registry are implemented.
