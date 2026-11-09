/**
 * Content Security Policy for pages (docs/decisions/library.md, I254). Scripts run
 * only with the request's nonce, so nothing third-party can execute. Frames and images
 * accept https because destination embeds and thumbnails live there; a frame loads
 * only when the visitor asks for it (I252).
 */
export function buildCsp(nonce: string, development: boolean): string {
  const directives: string[] = [
    "default-src 'self'",
    `script-src 'self' 'nonce-${nonce}'${development ? " 'unsafe-eval'" : ''}`,
    // In development the framework injects styles; in production everything is a file.
    `style-src 'self'${development ? " 'unsafe-inline'" : ''}`,
    "img-src 'self' https: data:",
    "font-src 'self'",
    `connect-src 'self'${development ? ' ws:' : ''}`,
    'frame-src https:',
    "media-src 'none'",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'self'",
  ];
  return directives.join('; ');
}
