---
title: "Documentation development"
sidebar_position: 1
---

# Documentation development

This page covers the documentation site in the [Vidyom repository](https://github.com/rafaelp/vidyom): a Docusaurus website in `website/`. The complete product specification lives in `spec/`.

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

Set `DOCS_SITE_URL` and `DOCS_BASE_URL` when deploying to a custom domain or subpath. The official site is deployed on Vercel at `https://www.vidyom.org`. Docusaurus generates static HTML/CSS/JS in `build/`.

## How to contribute content

- Add a Markdown or MDX file under `website/docs/` in the relevant section.
- Use English for page text, example commands, JSON keys, and comments.
- Describe product behavior in the present tense. Requirement status (**Decided**, **Proposed**, **Future**, **Validate**) belongs in `spec/`.
- Avoid secrets, real API tokens, or private customer data in examples.
- Run `npm run typecheck` and `npm run build` (which fails on broken links) before submitting a pull request.

## Runtime code

The runtime lives in this same repository, alongside `website/`. Its binaries are `vidyom` (client), `vidyomd` (server), and `vidyom-worker` (processor). See [Repository layout](../architecture/repository.md).
