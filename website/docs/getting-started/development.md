---
title: "Documentation development"
sidebar_position: 1
---

# Documentation development

The [Vidyom repository](https://github.com/rafaelp/vidyom) currently contains the specification: this Docusaurus website in `website/` and the original Portuguese product specification in `spec/`. The Vidyom runtime has **not** been implemented yet.

## Prerequisites

Install Node.js 20+ and npm. The site lives in the `website/` directory of the repository:

```bash
cd website
npm install
npm run start
```

The site is served at `http://localhost:3000`. For a production build:

```bash
npm run build
npm run serve
```

Set `DOCS_SITE_URL` and `DOCS_BASE_URL` when deploying to a custom domain or subpath. Docusaurus generates static HTML/CSS/JS in `build/`.

## How to contribute content

- Add a Markdown or MDX file under `website/docs/` in the relevant section.
- Use English for page text, example commands, JSON keys, and comments.
- Mark requirement status clearly: **Decided**, **Proposed**, **Future**, or **Validate**.
- Do not claim that planned CLI or API behavior already exists.
- Avoid secrets, real API tokens, or private customer data in examples.
- Run `npm run typecheck` and `npm run build` (which fails on broken links) before submitting a pull request.

## Runtime code

The runtime will live in this same repository, alongside `website/`. Its conceptual binaries are `vidyom` (client), `vidyomd` (server), and `vidyom-worker` (processor). See [Repository layout](../architecture/repository.md).
