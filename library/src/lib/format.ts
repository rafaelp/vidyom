import type { VideoKind } from './types';

/** Display formatting. Pure, deterministic, and the same on server and client. */

const KIND_LABELS: Record<VideoKind, string> = {
  clip: 'Clip',
  edited: 'Edited video',
  original: 'Full recording',
};

export function kindLabel(kind: VideoKind): string {
  return KIND_LABELS[kind];
}

const DESTINATION_LABELS: Record<string, string> = {
  youtube: 'YouTube',
  vimeo: 'Vimeo',
  peertube: 'PeerTube',
  instagramreels: 'Instagram Reels',
  mock: 'Mock destination',
};

export function destinationLabel(driver: string): string {
  const known = DESTINATION_LABELS[driver];
  if (known) return known;
  return driver === '' ? 'Destination' : driver.charAt(0).toUpperCase() + driver.slice(1);
}

function parts(totalSeconds: number): { h: number; m: number; s: number } {
  const total = Number.isFinite(totalSeconds) && totalSeconds > 0 ? Math.floor(totalSeconds) : 0;
  return { h: Math.floor(total / 3600), m: Math.floor((total % 3600) / 60), s: total % 60 };
}

/** `2:03` or `1:02:03`. */
export function formatTimestamp(seconds: number): string {
  const { h, m, s } = parts(seconds);
  const ss = String(s).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`;
}

function plural(value: number, unit: string): string {
  return `${value} ${unit}${value === 1 ? '' : 's'}`;
}

/** Words for screen readers: `1 hour 2 minutes 3 seconds`. */
export function spokenDuration(seconds: number): string {
  const { h, m, s } = parts(seconds);
  const words = [
    h > 0 ? plural(h, 'hour') : '',
    m > 0 ? plural(m, 'minute') : '',
    s > 0 || (h === 0 && m === 0) ? plural(s, 'second') : '',
  ];
  return words.filter(Boolean).join(' ');
}

/** ISO 8601 duration for JSON-LD: `PT1H2M3S`. */
export function isoDuration(seconds: number): string {
  const { h, m, s } = parts(seconds);
  return `PT${h > 0 ? `${h}H` : ''}${m > 0 ? `${m}M` : ''}${s > 0 || (h === 0 && m === 0) ? `${s}S` : ''}`;
}

/** `Nov 9, 2026` in UTC, or '' when the value is not a date. */
export function formatDate(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeZone: 'UTC' }).format(date);
}

export function formatDateTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return new Intl.DateTimeFormat('en', {
    dateStyle: 'medium',
    timeStyle: 'short',
    timeZone: 'UTC',
  }).format(date);
}

/** A canonical BCP 47 tag for a `lang` attribute, or undefined when not valid. */
export function languageTag(value: string): string | undefined {
  if (!value) return undefined;
  try {
    return Intl.getCanonicalLocales(value)[0];
  } catch {
    return undefined;
  }
}

/** `Portuguese (Brazil)` for `pt-BR`; the raw value when it is not a known tag. */
export function languageName(value: string): string {
  const tag = languageTag(value);
  if (!tag) return value;
  try {
    return new Intl.DisplayNames(['en'], { type: 'language' }).of(tag) ?? value;
  } catch {
    return value;
  }
}

export function pluralize(count: number, singular: string, pluralForm = `${singular}s`): string {
  return `${count.toLocaleString('en')} ${count === 1 ? singular : pluralForm}`;
}
