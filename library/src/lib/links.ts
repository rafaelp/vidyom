import type { Destination } from './types';

/**
 * URLs of a destination and position links (docs/decisions/library.md, I251).
 * `supports_timestamps` decides whether a position link exists at all; the driver
 * only chooses its form.
 */

export function safeHttpUrl(value: string): string {
  try {
    const url = new URL(value);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : '';
  } catch {
    return '';
  }
}

/** Embeds and thumbnails must be https: the CSP and mixed content rule out the rest. */
export function safeHttpsUrl(value: string): string {
  try {
    const url = new URL(value);
    return url.protocol === 'https:' ? url.href : '';
  } catch {
    return '';
  }
}

function wholeSeconds(seconds: number): number {
  return Number.isFinite(seconds) && seconds > 0 ? Math.floor(seconds) : 0;
}

function withParam(href: string, name: string, value: string): string {
  const url = new URL(href);
  url.searchParams.set(name, value);
  return url.href;
}

function withFragment(href: string, fragment: string): string {
  const url = new URL(href);
  url.hash = fragment;
  return url.href;
}

/** The destination's own page at a position, or null when it cannot jump there. */
export function destinationUrlAt(destination: Destination, seconds: number): string | null {
  if (!destination.supportsTimestamps) return null;
  const at = wholeSeconds(seconds);
  if (at === 0) return destination.url;
  switch (destination.driver) {
    case 'youtube':
      return withParam(destination.url, 't', `${at}s`);
    case 'vimeo':
      return withFragment(destination.url, `t=${at}s`);
    case 'peertube':
      return withParam(destination.url, 'start', String(at));
    default:
      return withParam(destination.url, 't', String(at));
  }
}

/** The embed URL, positioned only when the destination declares support. */
export function embedUrlAt(destination: Destination, seconds: number): string {
  const at = wholeSeconds(seconds);
  if (at === 0 || !destination.supportsTimestamps) return destination.embedUrl;
  if (destination.driver === 'vimeo') return withFragment(destination.embedUrl, `t=${at}s`);
  return withParam(destination.embedUrl, 'start', String(at));
}

/** The URL the iframe loads once the visitor has asked to play (I252). */
export function playerUrl(destination: Destination, seconds: number): string {
  return withParam(embedUrlAt(destination, seconds), 'autoplay', '1');
}

function preference(destination: Destination): number {
  const embed = destination.embedUrl !== '';
  if (embed && destination.supportsTimestamps) return 0;
  if (embed) return 1;
  if (destination.supportsTimestamps) return 2;
  return 3;
}

/** The destination the page plays or links first, chosen deterministically. */
export function pickPrimary(destinations: readonly Destination[]): Destination | undefined {
  return [...destinations].sort(
    (a, b) =>
      preference(a) - preference(b) ||
      a.driver.localeCompare(b.driver) ||
      a.url.localeCompare(b.url),
  )[0];
}

/** The site's own link to a video, optionally at a moment. */
export function videoPath(publicId: string, seconds?: number): string {
  const path = `/videos/${encodeURIComponent(publicId)}`;
  const at = seconds === undefined ? 0 : wholeSeconds(seconds);
  return at > 0 ? `${path}?t=${at}` : path;
}

export interface MomentLink {
  href: string;
  /** True when the link leaves the site for the destination. */
  external: boolean;
}

/**
 * Where a timestamp goes. With an embed, the site's own page seeks the player. With
 * only a destination that declares timestamps, the destination at that position.
 * Otherwise the site's own page, which says where the moment is (I251).
 */
export function momentLink(
  publicId: string,
  primary: Destination | undefined,
  seconds: number,
): MomentLink {
  if (primary && !primary.embedUrl) {
    const external = destinationUrlAt(primary, seconds);
    if (external) return { href: external, external: true };
  }
  return { href: `${videoPath(publicId, seconds)}#player`, external: false };
}

/** A `?t=` value as whole seconds within the video, or 0 when it is not usable. */
export function parsePosition(value: string | null, durationSeconds: number): number {
  if (!value || !/^\d{1,9}(\.\d+)?$/.test(value)) return 0;
  const seconds = Math.floor(Number(value));
  if (durationSeconds > 0 && seconds >= durationSeconds) return 0;
  return seconds;
}
