import { isoDuration } from './format';
import { destinationUrlAt, pickPrimary } from './links';
import type { VideoDetail } from './types';

/**
 * Schema.org `VideoObject` for a video page. Only fields present in the file; chapters
 * become `Clip` parts when the destination can open at a position (docs/decisions/
 * library.md, I256).
 */
export function videoJsonLd(video: VideoDetail, pageUrl: string | undefined): object {
  const primary = pickPrimary(video.destinations);
  const hasParts =
    primary?.supportsTimestamps === true
      ? video.chapters.map((chapter) => ({
          '@type': 'Clip',
          name: chapter.title,
          startOffset: Math.floor(chapter.startSeconds),
          endOffset: Math.floor(chapter.endSeconds),
          url: destinationUrlAt(primary, chapter.startSeconds) ?? undefined,
        }))
      : [];
  return {
    '@context': 'https://schema.org',
    '@type': 'VideoObject',
    name: video.title,
    description: video.description || undefined,
    thumbnailUrl: video.thumbnailUrl || undefined,
    uploadDate: video.publishedAt,
    duration: video.durationSeconds > 0 ? isoDuration(video.durationSeconds) : undefined,
    inLanguage: video.language || undefined,
    keywords: video.tags.length > 0 ? video.tags.join(', ') : undefined,
    embedUrl: primary?.embedUrl || undefined,
    url: pageUrl,
    sameAs: video.destinations.map((destination) => destination.url),
    ...(hasParts.length > 0 ? { hasPart: hasParts } : {}),
  };
}
