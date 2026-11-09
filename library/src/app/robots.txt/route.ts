import { readConfig } from '@/lib/config';

export const dynamic = 'force-dynamic';

export function GET(): Response {
  const { siteUrl } = readConfig();
  const lines = ['User-agent: *', 'Allow: /', 'Disallow: /api/'];
  if (siteUrl) lines.push(`Sitemap: ${siteUrl}/sitemap.xml`);
  return new Response(`${lines.join('\n')}\n`, {
    headers: { 'Content-Type': 'text/plain; charset=utf-8', 'Cache-Control': 'no-store' },
  });
}
