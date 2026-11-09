#!/usr/bin/env node
/**
 * Runs the Next.js CLI with its telemetry off, so the framework reports nothing to
 * Vercel from a build or a server (docs/decisions/library.md, I255). It has to be set
 * here: Next.js resets process.env while loading its environment files, so setting
 * this in next.config.ts is lost, as `NEXT_TELEMETRY_DEBUG=1 next build` shows.
 */
process.env.NEXT_TELEMETRY_DISABLED = '1';
await import('next/dist/bin/next');
