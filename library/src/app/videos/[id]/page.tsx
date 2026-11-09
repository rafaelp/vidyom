import type { Metadata } from 'next';
import { headers } from 'next/headers';
import Link from 'next/link';
import { notFound } from 'next/navigation';
import { DatasetUnavailable } from '@/components/DatasetUnavailable';
import { ExternalIcon } from '@/components/icons';
import { JsonLd } from '@/components/JsonLd';
import { Player } from '@/components/Player';
import { TimestampLink } from '@/components/TimestampLink';
import { VideoGrid } from '@/components/VideoCard';
import { readConfig } from '@/lib/config';
import {
  destinationLabel,
  formatDate,
  formatTimestamp,
  kindLabel,
  languageName,
  languageTag,
  pluralize,
  spokenDuration,
} from '@/lib/format';
import { videoJsonLd } from '@/lib/jsonld';
import { destinationUrlAt, parsePosition, pickPrimary, videoPath } from '@/lib/links';
import { readDataset, toURLSearchParams, type RawSearchParams } from '@/lib/page-data';
import { getRelated, getTranscript, getVideo } from '@/lib/queries';

export const dynamic = 'force-dynamic';

/** A long transcript is sent on request: App Router pages carry their content twice (I257). */
const TRANSCRIPT_PREVIEW = 300;
const MAX_ID_LENGTH = 128;

interface VideoPageProps {
  params: Promise<{ id: string }>;
  searchParams: Promise<RawSearchParams>;
}

function excerpt(text: string, limit = 160): string {
  const flat = text.replace(/\s+/g, ' ').trim();
  if (flat.length <= limit) return flat;
  const cut = flat.slice(0, limit);
  const space = cut.lastIndexOf(' ');
  return `${space > 0 ? cut.slice(0, space) : cut}…`;
}

export async function generateMetadata({ params }: VideoPageProps): Promise<Metadata> {
  const { id } = await params;
  const data = readDataset((generation) => (id.length <= MAX_ID_LENGTH ? getVideo(generation, id) : null));
  if (!data.ready || !data.value) return { title: 'Video not found', robots: { index: false } };

  const video = data.value;
  const description = video.description ? excerpt(video.description) : undefined;
  const images = video.thumbnailUrl ? [{ url: video.thumbnailUrl }] : undefined;
  const hasOrigin = readConfig().siteUrl !== '';
  return {
    title: video.title,
    description,
    // A ?t= link is the same page, so it points at the bare URL. Needs an origin (I256).
    alternates: hasOrigin ? { canonical: videoPath(video.publicId) } : undefined,
    openGraph: {
      type: 'video.other',
      title: video.title,
      description,
      images,
      url: hasOrigin ? videoPath(video.publicId) : undefined,
    },
    twitter: {
      card: images ? 'summary_large_image' : 'summary',
      title: video.title,
      description,
      images: images?.map((image) => image.url),
    },
  };
}

export default async function VideoPage({ params, searchParams }: VideoPageProps) {
  const { id } = await params;
  if (id.length > MAX_ID_LENGTH) notFound();
  const query = toURLSearchParams(await searchParams);
  const showFullTranscript = query.get('transcript') === 'all';

  const data = readDataset((generation) => {
    const video = getVideo(generation, id);
    if (!video) return null;
    return {
      video,
      transcript: getTranscript(generation, id, showFullTranscript ? undefined : TRANSCRIPT_PREVIEW),
      related: getRelated(generation, video),
    };
  });
  if (!data.ready) return <DatasetUnavailable status={data.status} />;
  if (!data.value) notFound();

  const { video, transcript, related } = data.value;
  const primary = pickPrimary(video.destinations);
  const start = parsePosition(query.get('t'), video.durationSeconds);
  const date = formatDate(video.publishedAt);
  const lang = languageTag(video.language);
  const config = readConfig();
  const nonce = (await headers()).get('x-nonce') ?? undefined;
  const pageUrl = config.siteUrl ? `${config.siteUrl}${videoPath(video.publicId)}` : undefined;

  const currentChapter =
    start > 0
      ? video.chapters.findLast((chapter) => chapter.startSeconds <= start && start < chapter.endSeconds)
      : undefined;
  const cannotJump = start > 0 && primary !== undefined && !primary.supportsTimestamps;

  return (
    <div className="page stack">
      <JsonLd data={videoJsonLd(video, pageUrl)} nonce={nonce} />

      <div className="video-layout">
        <div className="stack video-layout__main">
          <Player title={video.title} thumbnailUrl={video.thumbnailUrl} destination={primary} start={start} />

          {cannotJump ? (
            <p className="note" role="note">
              This moment starts at {formatTimestamp(start)}. {destinationLabel(primary.driver)} cannot open at a
              position, so skip to it in the player.
            </p>
          ) : null}
          {!primary ? (
            <p className="note" role="note">
              This video has no destination available right now.
            </p>
          ) : null}

          <div className="stack">
            <div>
              <h1 className="video-title" lang={lang}>
                {video.title}
              </h1>
            </div>
            <p className="meta-line">
              <span>{kindLabel(video.kind)}</span>
              {video.speaker ? (
                <span>
                  <Link href={`/?speaker=${encodeURIComponent(video.speaker)}`}>{video.speaker}</Link>
                </span>
              ) : null}
              {date ? (
                <span>
                  <time dateTime={video.publishedAt}>{date}</time>
                </span>
              ) : null}
              {video.durationSeconds > 0 ? (
                <span>
                  <span aria-hidden="true">{formatTimestamp(video.durationSeconds)}</span>
                  <span className="sr-only">{spokenDuration(video.durationSeconds)}</span>
                </span>
              ) : null}
              {video.language ? <span>{languageName(video.language)}</span> : null}
              {video.category ? (
                <span>
                  <Link href={`/?category=${encodeURIComponent(video.category)}`}>{video.category}</Link>
                </span>
              ) : null}
            </p>

            {video.destinations.length > 0 ? (
              <div className="watch-links">
                {[...video.destinations]
                  .sort((a, b) => Number(b === primary) - Number(a === primary))
                  .map((destination) => (
                  <a
                    key={`${destination.driver}:${destination.url}`}
                    className="button button--quiet"
                    href={destinationUrlAt(destination, start) ?? destination.url}
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    Watch on {destinationLabel(destination.driver)}
                    <ExternalIcon />
                    <span className="sr-only">(opens in a new tab)</span>
                  </a>
                ))}
              </div>
            ) : null}

            {video.description ? (
              <p className="description" lang={lang}>
                {video.description}
              </p>
            ) : null}

            {video.tags.length > 0 ? (
              <ul className="tag-list" aria-label="Tags">
                {video.tags.map((tag) => (
                  <li key={tag}>
                    <Link className="tag" href={`/?tag=${encodeURIComponent(tag)}`}>
                      {tag}
                    </Link>
                  </li>
                ))}
              </ul>
            ) : null}
          </div>
        </div>

        {video.chapters.length > 0 ? (
          <aside className="video-layout__rail panel" aria-labelledby="chapters-heading">
            <h2 id="chapters-heading">Chapters</h2>
            <ol className="timeline" lang={lang}>
              {video.chapters.map((chapter) => (
                <li
                  key={chapter.position}
                  className="timeline__item"
                  aria-current={currentChapter?.position === chapter.position ? 'true' : undefined}
                >
                  <TimestampLink
                    className="timeline__time"
                    publicId={video.publicId}
                    primary={primary}
                    seconds={chapter.startSeconds}
                    context={chapter.title}
                  />
                  <span>{chapter.title}</span>
                  {chapter.summary ? <span className="timeline__summary">{chapter.summary}</span> : null}
                </li>
              ))}
            </ol>
          </aside>
        ) : null}

        {video.transcriptCount > 0 ? (
          <details className="panel transcript video-layout__transcript" id="transcript" open={showFullTranscript || undefined}>
            <summary>Transcript</summary>
            <ol className="timeline" lang={lang}>
              {transcript.map((segment) => (
                <li key={segment.position} className="timeline__item">
                  <TimestampLink
                    className="timeline__time"
                    publicId={video.publicId}
                    primary={primary}
                    seconds={segment.startSeconds}
                  />
                  <span>{segment.text}</span>
                </li>
              ))}
            </ol>
            {transcript.length < video.transcriptCount ? (
              <p>
                <Link href={`${videoPath(video.publicId)}?transcript=all#transcript`}>
                  Show the full transcript ({pluralize(video.transcriptCount, 'segment')})
                </Link>
              </p>
            ) : null}
          </details>
        ) : null}
      </div>

      {related.length > 0 ? (
        <section className="stack" aria-labelledby="related-heading">
          <h2 id="related-heading">From the same recording</h2>
          <VideoGrid videos={related} heading="h3" />
        </section>
      ) : null}
    </div>
  );
}
