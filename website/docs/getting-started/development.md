---
title: "Documentation development"
sidebar_position: 1
---

# Documentation development

This repository contains the Docusaurus documentation website. It does **not** yet ship the Vidyom runtime.

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
- Check broken links by running `npm run build` before submitting a pull request.

## Proposed application repository

The future runtime will live in this same repository, alongside `website/`. Its conceptual binaries are `vidyom` (client), `vidyomd` (server), and `vidyom-worker` (processor). See [Repository layout](../architecture/repository.md).
