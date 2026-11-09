import type { UnavailableStatus } from './dataset';

/**
 * What a visitor sees when the dataset is not ready (docs/decisions/library.md, I244).
 * It never reveals the file's path or the underlying error. No import reaches the
 * database, so the proxy can render it with nothing else working.
 */

export const UNAVAILABLE_COPY: Record<UnavailableStatus, { title: string; body: string }> = {
  missing: {
    title: 'This library is not available yet',
    body: 'The site has not received its catalog. Please check back soon.',
  },
  invalid: {
    title: 'This library is temporarily unavailable',
    body: 'The catalog could not be read. The site will recover as soon as a good copy arrives.',
  },
  unsupported_schema: {
    title: 'This library needs an update',
    body: 'The catalog was written for a newer version of this site, so it is not being shown.',
  },
};

export function UNAVAILABLE_CSP(nonce: string): string {
  return `default-src 'none'; style-src 'nonce-${nonce}'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'`;
}

function escapeHtml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

const DARK_TOKENS = '--bg:#15110f;--surface:#1d1714;--text:#f5ece5;--muted:#bcaca0;--border:#8d7a6c';

export function renderUnavailablePage(options: {
  status: UnavailableStatus;
  siteTitle: string;
  nonce: string;
  theme?: 'light' | 'dark';
}): string {
  const copy = UNAVAILABLE_COPY[options.status];
  const attribute = options.theme ? ` data-theme="${options.theme}"` : '';
  return `<!doctype html>
<html lang="en"${attribute}>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>${escapeHtml(copy.title)} · ${escapeHtml(options.siteTitle)}</title>
<style nonce="${escapeHtml(options.nonce)}">
:root{color-scheme:light dark;--bg:#fffcf9;--surface:#fff;--text:#1e1410;--muted:#65554a;--border:#8a7768}
@media (prefers-color-scheme:dark){:root:not([data-theme='light']){${DARK_TOKENS}}}
:root[data-theme='dark']{${DARK_TOKENS};color-scheme:dark}
:root[data-theme='light']{color-scheme:light}
body{margin:0;min-height:100dvh;display:flex;flex-direction:column;background:var(--bg);color:var(--text);font:1rem/1.55 system-ui,-apple-system,'Segoe UI',sans-serif}
header{padding:.9rem clamp(1rem,3.5vw,2.5rem);font-weight:700;font-size:1.12rem;border-bottom:1px solid var(--border)}
main{flex:1;width:100%;max-width:76rem;margin:0 auto;padding:clamp(1.25rem,3vw,2.25rem) clamp(1rem,3.5vw,2.5rem);box-sizing:border-box}
section{padding:clamp(1.5rem,4vw,3rem);border:1px dashed var(--border);border-radius:14px;background:var(--surface)}
h1{margin:0 0 .75rem;font-size:clamp(1.65rem,1.3rem + 1.6vw,2.4rem);line-height:1.18}
p{margin:0;color:var(--muted);max-width:44rem}
</style>
</head>
<body>
<header>${escapeHtml(options.siteTitle)}</header>
<main>
<section>
<h1>${escapeHtml(copy.title)}</h1>
<p>${escapeHtml(copy.body)}</p>
</section>
</main>
</body>
</html>
`;
}
