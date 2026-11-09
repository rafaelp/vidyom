import { readConfig } from '@/lib/config';
import { getDataset, isDatasetUnavailable } from '@/lib/dataset';
import { videoPath } from '@/lib/links';
import { allVideoEntries } from '@/lib/queries';

export const dynamic = 'force-dynamic';

function escapeXml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&apos;');
}

/** Absolute URLs need a configured origin; the Host header is never trusted for it (I256). */
export function GET(): Response {
  const config = readConfig();
  if (!config.siteUrl) return new Response('Not found\n', { status: 404 });
  let entries: ReturnType<typeof allVideoEntries>;
  try {
    entries = getDataset(config.publicDbPath).withSnapshot(allVideoEntries);
  } catch (error) {
    if (isDatasetUnavailable(error)) {
      return new Response('Unavailable\n', { status: 503, headers: { 'Retry-After': '60' } });
    }
    throw error;
  }
  const urls = [
    `  <url><loc>${escapeXml(`${config.siteUrl}/`)}</loc></url>`,
    ...entries.map(({ publicId, publishedAt }) => {
      const loc = escapeXml(`${config.siteUrl}${videoPath(publicId)}`);
      const date = new Date(publishedAt);
      const lastmod = Number.isNaN(date.getTime()) ? '' : `<lastmod>${date.toISOString()}</lastmod>`;
      return `  <url><loc>${loc}</loc>${lastmod}</url>`;
    }),
  ];
  const body = `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls.join('\n')}\n</urlset>\n`;
  return new Response(body, {
    headers: { 'Content-Type': 'application/xml; charset=utf-8', 'Cache-Control': 'no-store' },
  });
}
