import { NextResponse, type NextRequest } from 'next/server';
import { readConfig } from '@/lib/config';
import { buildCsp } from '@/lib/csp';
import { getDataset, type DatasetState } from '@/lib/dataset';
import { renderUnavailablePage, UNAVAILABLE_CSP } from '@/lib/unavailable-page';

/**
 * Runs before every page: sets the per-request CSP nonce and answers `503` when the
 * dataset is not ready, so search engines see an outage rather than an empty site
 * (docs/decisions/library.md, I244, I254). The API answers its own `503` as JSON.
 *
 * The 503 is a self-contained response, not a rewrite to a route. Next.js decides whether
 * a rewrite is external by comparing origins, and NextURL turns 127.0.0.1 into localhost,
 * so on a server bound to an explicit address a rewrite becomes an HTTP proxy to itself.
 */
function datasetState(): DatasetState | undefined {
  try {
    return getDataset().state();
  } catch (error) {
    // Pages check the state themselves too, so this only loses the status code.
    console.error('[library] could not read the dataset state in the proxy', error);
    return undefined;
  }
}

export function proxy(request: NextRequest): NextResponse {
  const nonce = Buffer.from(crypto.randomUUID()).toString('base64');

  const state = datasetState();
  if (state && state.status !== 'ready') {
    const chosen = request.cookies.get('vidyom_theme')?.value;
    return new NextResponse(
      renderUnavailablePage({
        status: state.status,
        siteTitle: readConfig().title,
        nonce,
        theme: chosen === 'light' || chosen === 'dark' ? chosen : undefined,
      }),
      {
        status: 503,
        headers: {
          'Content-Type': 'text/html; charset=utf-8',
          'Content-Security-Policy': UNAVAILABLE_CSP(nonce),
          'Cache-Control': 'no-store',
          'Retry-After': '60',
        },
      },
    );
  }

  const csp = buildCsp(nonce, process.env.NODE_ENV === 'development');
  const requestHeaders = new Headers(request.headers);
  requestHeaders.set('Content-Security-Policy', csp);
  requestHeaders.set('x-nonce', nonce);
  const response = NextResponse.next({ request: { headers: requestHeaders } });
  response.headers.set('Content-Security-Policy', csp);
  return response;
}

export const config = {
  matcher: [
    {
      source: '/((?!api/|_next/static|_next/image|favicon.ico|vidyom-mark.png|robots.txt|sitemap.xml).*)',
    },
  ],
};
