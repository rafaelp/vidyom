import { describe, expect, it } from 'vitest';
import {
  destinationLabel,
  formatDate,
  formatTimestamp,
  isoDuration,
  languageName,
  languageTag,
  spokenDuration,
} from '@/lib/format';
import { destinationUrlAt, embedUrlAt, pickPrimary, safeHttpUrl, safeHttpsUrl, videoPath } from '@/lib/links';
import { normalizeSiteUrl, readConfig } from '@/lib/config';
import type { Destination } from '@/lib/types';

const base: Destination = {
  driver: 'youtube',
  url: 'https://media.example.internal/watch?v=abc',
  embedUrl: 'https://media.example.internal/embed/abc',
  supportsTimestamps: true,
};

describe('position links', () => {
  it('uses each driver’s form', () => {
    expect(destinationUrlAt(base, 90.7)).toBe('https://media.example.internal/watch?v=abc&t=90s');
    expect(embedUrlAt(base, 90.7)).toBe('https://media.example.internal/embed/abc?start=90');

    const vimeo = { ...base, driver: 'vimeo', url: 'https://media.example.internal/1', embedUrl: 'https://media.example.internal/e/1' };
    expect(destinationUrlAt(vimeo, 90)).toBe('https://media.example.internal/1#t=90s');
    expect(embedUrlAt(vimeo, 90)).toBe('https://media.example.internal/e/1#t=90s');

    const peertube = { ...base, driver: 'peertube', url: 'https://media.example.internal/w/1' };
    expect(destinationUrlAt(peertube, 90)).toBe('https://media.example.internal/w/1?start=90');

    const other = { ...base, driver: 'mock', url: 'https://media.example.internal/watch/1' };
    expect(destinationUrlAt(other, 90)).toBe('https://media.example.internal/watch/1?t=90');
  });

  it('gives no position link without declared support', () => {
    const none = { ...base, supportsTimestamps: false };
    expect(destinationUrlAt(none, 90)).toBeNull();
    expect(embedUrlAt(none, 90)).toBe(base.embedUrl);
  });

  it('leaves the URL alone at the start', () => {
    expect(destinationUrlAt(base, 0)).toBe(base.url);
    expect(embedUrlAt(base, 0)).toBe(base.embedUrl);
  });
});

describe('pickPrimary', () => {
  const linkOnly = { ...base, driver: 'a', embedUrl: '', supportsTimestamps: false };
  const timestamped = { ...base, driver: 'b', embedUrl: '' };
  const embedOnly = { ...base, driver: 'c', supportsTimestamps: false };
  const both = { ...base, driver: 'd' };

  it('prefers an embed with timestamps, then an embed, then timestamps', () => {
    expect(pickPrimary([linkOnly, timestamped, embedOnly, both])).toBe(both);
    expect(pickPrimary([linkOnly, timestamped, embedOnly])).toBe(embedOnly);
    expect(pickPrimary([linkOnly, timestamped])).toBe(timestamped);
    expect(pickPrimary([linkOnly])).toBe(linkOnly);
    expect(pickPrimary([])).toBeUndefined();
  });
});

describe('safe URLs', () => {
  it('keeps only http and https for links', () => {
    expect(safeHttpUrl('https://media.example.internal/a')).toBe('https://media.example.internal/a');
    expect(safeHttpUrl('http://media.example.internal/a')).toBe('http://media.example.internal/a');
    for (const bad of ['javascript:alert(1)', 'data:text/html,x', 'file:///etc/passwd', '', 'not a url', '//x']) {
      expect(safeHttpUrl(bad)).toBe('');
    }
  });

  it('keeps only https for embeds and thumbnails', () => {
    expect(safeHttpsUrl('https://media.example.internal/a')).toBe('https://media.example.internal/a');
    expect(safeHttpsUrl('http://media.example.internal/a')).toBe('');
  });
});

describe('videoPath', () => {
  it('encodes the ID and adds a position only when there is one', () => {
    expect(videoPath('abc')).toBe('/videos/abc');
    expect(videoPath('a/b c')).toBe('/videos/a%2Fb%20c');
    expect(videoPath('abc', 12.9)).toBe('/videos/abc?t=12');
    expect(videoPath('abc', 0)).toBe('/videos/abc');
  });
});

describe('formatting', () => {
  it('formats timestamps', () => {
    expect(formatTimestamp(0)).toBe('0:00');
    expect(formatTimestamp(65.9)).toBe('1:05');
    expect(formatTimestamp(3723)).toBe('1:02:03');
    expect(formatTimestamp(Number.NaN)).toBe('0:00');
  });

  it('spells durations for screen readers', () => {
    expect(spokenDuration(3723)).toBe('1 hour 2 minutes 3 seconds');
    expect(spokenDuration(60)).toBe('1 minute');
    expect(spokenDuration(0)).toBe('0 seconds');
  });

  it('writes ISO 8601 durations', () => {
    expect(isoDuration(3723)).toBe('PT1H2M3S');
    expect(isoDuration(120)).toBe('PT2M');
    expect(isoDuration(0)).toBe('PT0S');
  });

  it('formats dates in UTC and rejects garbage', () => {
    expect(formatDate('2026-11-09T23:59:00Z')).toBe('Nov 9, 2026');
    expect(formatDate('yesterday-ish')).toBe('');
  });

  it('handles language tags', () => {
    expect(languageTag('pt-br')).toBe('pt-BR');
    expect(languageTag('')).toBeUndefined();
    expect(languageTag('not a tag!')).toBeUndefined();
    expect(languageName('pt-BR')).toBe('Brazilian Portuguese');
    expect(languageName('not a tag!')).toBe('not a tag!');
  });

  it('labels destinations', () => {
    expect(destinationLabel('youtube')).toBe('YouTube');
    expect(destinationLabel('newhost')).toBe('Newhost');
    expect(destinationLabel('')).toBe('Destination');
  });
});

describe('configuration', () => {
  it('uses defaults for an empty environment', () => {
    const config = readConfig({});
    expect(config.publicDbPath).toBe('data/public.db');
    expect(config.title).toBe('Video library');
    expect(config.semanticEnabled).toBe(true);
    expect(config.maxVectors).toBe(50_000);
    expect(config.siteUrl).toBe('');
  });

  it('reads values and ignores malformed numbers', () => {
    const config = readConfig({
      VIDYOM_PUBLIC_DB: '/srv/acme/public.db',
      VIDYOM_LIBRARY_SEMANTIC: 'OFF',
      VIDYOM_LIBRARY_MAX_VECTORS: 'lots',
      VIDYOM_LIBRARY_MIN_SIMILARITY: '7',
    });
    expect(config.publicDbPath).toBe('/srv/acme/public.db');
    expect(config.semanticEnabled).toBe(false);
    expect(config.maxVectors).toBe(50_000);
    expect(config.minSimilarity).toBe(0.3);
  });

  it('accepts only an absolute http(s) site URL', () => {
    expect(normalizeSiteUrl('https://media.example.internal/')).toBe('https://media.example.internal');
    expect(normalizeSiteUrl('https://media.example.internal/lib/')).toBe('https://media.example.internal/lib');
    expect(normalizeSiteUrl('media.example.internal')).toBe('');
    expect(normalizeSiteUrl('javascript:alert(1)')).toBe('');
  });
});
