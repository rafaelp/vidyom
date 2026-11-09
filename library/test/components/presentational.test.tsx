import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { DatasetUnavailable } from '@/components/DatasetUnavailable';
import { JsonLd } from '@/components/JsonLd';
import { Pagination } from '@/components/Pagination';
import { Thumbnail } from '@/components/Thumbnail';
import { TimestampLink } from '@/components/TimestampLink';
import { VideoCard } from '@/components/VideoCard';
import { momentLink, parsePosition } from '@/lib/links';
import { videoJsonLd } from '@/lib/jsonld';
import { renderUnavailablePage, UNAVAILABLE_CSP } from '@/lib/unavailable-page';
import type { Destination, VideoDetail, VideoSummary } from '@/lib/types';

const summary: VideoSummary = {
  publicId: 'abc',
  title: 'Conscious breathing',
  description: 'A description.',
  kind: 'clip',
  category: 'Yoga',
  language: 'pt-BR',
  speaker: 'Ana Exemplo',
  tags: ['hatha'],
  durationSeconds: 312,
  publishedAt: '2026-10-28T14:00:00Z',
  thumbnailUrl: 'https://media.example.internal/t.jpg',
};

const embeddable: Destination = {
  driver: 'youtube',
  url: 'https://media.example.internal/watch/a',
  embedUrl: 'https://media.example.internal/embed/a',
  supportsTimestamps: true,
};

describe('VideoCard', () => {
  it('shows only fields from the file, with the content language marked', () => {
    const html = renderToStaticMarkup(<VideoCard video={summary} heading="h2" />);
    expect(html).toContain('Conscious breathing');
    expect(html).toContain('Ana Exemplo');
    expect(html).toContain('Oct 28, 2026');
    expect(html).toContain('5:12');
    expect(html).toContain('Duration 5 minutes 12 seconds');
    expect(html).toContain('href="/videos/abc"');
    expect(html).toContain('lang="pt-BR"');
    expect(html).toContain('<h2');
  });

  it('copes with a sparse row', () => {
    const html = renderToStaticMarkup(
      <VideoCard
        video={{ ...summary, speaker: '', language: '', thumbnailUrl: '', durationSeconds: 0, publishedAt: 'garbage' }}
        heading="h3"
      />,
    );
    expect(html).not.toContain('<img');
    expect(html).not.toContain('thumb__duration');
    expect(html).not.toContain('lang=');
    expect(html).not.toContain('Invalid');
    expect(html).toContain('<h3');
  });
});

describe('Thumbnail', () => {
  it('keeps the fallback tile behind the image, with no referrer and lazy loading', () => {
    const html = renderToStaticMarkup(<Thumbnail url={summary.thumbnailUrl} />);
    expect(html).toContain('thumb__glyph');
    expect(html).toContain('referrerPolicy="no-referrer"');
    expect(html).toContain('loading="lazy"');
    expect(html).toContain('alt=""');
  });
});

describe('TimestampLink and momentLink', () => {
  it('seeks our own player when there is an embed', () => {
    expect(momentLink('abc', embeddable, 90)).toEqual({ href: '/videos/abc?t=90#player', external: false });
    const html = renderToStaticMarkup(
      <TimestampLink publicId="abc" primary={embeddable} seconds={90} context="Counting" />,
    );
    expect(html).toContain('href="/videos/abc?t=90#player"');
    expect(html).toContain('1:30');
    expect(html).toContain('Counting, at 1 minute 30 seconds');
    expect(html).not.toContain('target=');
  });

  it('opens the destination at the position when there is no embed but timestamps work', () => {
    const linkOnly = { ...embeddable, embedUrl: '' };
    expect(momentLink('abc', linkOnly, 90)).toEqual({
      href: 'https://media.example.internal/watch/a?t=90s',
      external: true,
    });
    const html = renderToStaticMarkup(<TimestampLink publicId="abc" primary={linkOnly} seconds={90} />);
    expect(html).toContain('target="_blank"');
    expect(html).toContain('rel="noopener noreferrer"');
    expect(html).toContain('opens in a new tab');
  });

  it('stays on our page when the destination cannot jump, so the page can say where it is', () => {
    const none = { ...embeddable, embedUrl: '', supportsTimestamps: false };
    expect(momentLink('abc', none, 90)).toEqual({ href: '/videos/abc?t=90#player', external: false });
    expect(momentLink('abc', undefined, 90)).toEqual({ href: '/videos/abc?t=90#player', external: false });
  });

  it('accepts only sane positions', () => {
    expect(parsePosition('90', 312)).toBe(90);
    expect(parsePosition('90.9', 312)).toBe(90);
    expect(parsePosition('90', 0)).toBe(90);
    for (const bad of [null, '', 'abc', '-5', '1e9', '9999', '312']) {
      expect(parsePosition(bad, 312)).toBe(0);
    }
  });
});

describe('Pagination', () => {
  const hrefFor = (page: number) => `/?page=${page}`;

  it('hides with a single page', () => {
    expect(renderToStaticMarkup(<Pagination page={1} totalPages={1} hrefFor={hrefFor} />)).toBe('');
  });

  it('links previous and next where they exist', () => {
    const first = renderToStaticMarkup(<Pagination page={1} totalPages={3} hrefFor={hrefFor} />);
    expect(first).not.toContain('Previous');
    expect(first).toContain('href="/?page=2"');
    expect(first).toContain('Page 1 of 3');
    const last = renderToStaticMarkup(<Pagination page={3} totalPages={3} hrefFor={hrefFor} />);
    expect(last).not.toContain('Next');
    expect(last).toContain('href="/?page=2"');
  });
});

describe('DatasetUnavailable', () => {
  it.each(['missing', 'invalid', 'unsupported_schema'] as const)('explains %s without internals', (status) => {
    const html = renderToStaticMarkup(<DatasetUnavailable status={status} />);
    expect(html).toContain('<h1');
    expect(html).not.toMatch(/public\.db|sqlite|\/var|\/srv|data\//i);
  });
});

describe('JSON-LD', () => {
  const detail: VideoDetail = {
    ...summary,
    destinations: [embeddable],
    chapters: [{ position: 0, title: 'Counting', startSeconds: 74, endSeconds: 198, summary: '' }],
    transcriptCount: 0,
    sourceGroup: 'opaque-group',
  };

  it('describes the video with chapters as clips, from file fields only', () => {
    const data = videoJsonLd(detail, 'https://media.example.internal/videos/abc') as Record<string, unknown>;
    expect(data).toMatchObject({
      '@type': 'VideoObject',
      name: 'Conscious breathing',
      uploadDate: '2026-10-28T14:00:00Z',
      duration: 'PT5M12S',
      embedUrl: embeddable.embedUrl,
      url: 'https://media.example.internal/videos/abc',
      hasPart: [
        {
          '@type': 'Clip',
          name: 'Counting',
          startOffset: 74,
          endOffset: 198,
          url: 'https://media.example.internal/watch/a?t=74s',
        },
      ],
    });
    expect(JSON.stringify(data)).not.toContain('opaque-group');
  });

  it('omits clips when the destination cannot open at a position', () => {
    const data = videoJsonLd(
      { ...detail, destinations: [{ ...embeddable, supportsTimestamps: false }] },
      undefined,
    ) as Record<string, unknown>;
    expect(data.hasPart).toBeUndefined();
    expect(data.url).toBeUndefined();
  });

  it('cannot be broken out of by text in the file', () => {
    const html = renderToStaticMarkup(
      <JsonLd data={{ name: '</script><script>alert(1)</script>' }} />,
    );
    expect(html).not.toContain('<script>alert');
    expect(html).toContain('\\u003c/script');
  });
});

describe('the 503 page', () => {
  const page = (status: 'missing' | 'invalid' | 'unsupported_schema', theme?: 'light' | 'dark', siteTitle = 'Acme') =>
    renderUnavailablePage({ status, siteTitle, nonce: 'n0nce', theme });

  it.each(['missing', 'invalid', 'unsupported_schema'] as const)('explains %s with no internals', (status) => {
    const html = page(status);
    expect(html).toContain('<meta name="robots" content="noindex">');
    expect(html).toContain('<h1>');
    expect(html).toContain('<style nonce="n0nce">');
    expect(html).not.toMatch(/public\.db|sqlite|<script/i);
  });

  it('escapes the site title', () => {
    const html = page('missing', undefined, '<script>alert(1)</script>');
    expect(html).not.toContain('<script>');
    expect(html).toContain('&lt;script&gt;');
  });

  it('applies a chosen theme, and otherwise follows the system', () => {
    expect(page('missing', 'dark')).toContain('<html lang="en" data-theme="dark">');
    expect(page('missing')).toContain('<html lang="en">');
  });

  it('allows nothing but its own style', () => {
    expect(UNAVAILABLE_CSP('n0nce')).toBe(
      "default-src 'none'; style-src 'nonce-n0nce'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'",
    );
  });
});
