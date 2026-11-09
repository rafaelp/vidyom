import Link from 'next/link';
import { formatDate, languageTag } from '@/lib/format';
import { videoPath } from '@/lib/links';
import type { VideoSummary } from '@/lib/types';
import { Thumbnail } from './Thumbnail';

interface VideoCardProps {
  video: VideoSummary;
  /** The heading level that follows the surrounding section's heading. */
  heading: 'h2' | 'h3';
}

export function VideoCard({ video, heading: Heading }: VideoCardProps) {
  const date = formatDate(video.publishedAt);
  return (
    <article className="video-card">
      <Thumbnail url={video.thumbnailUrl} kind={video.kind} durationSeconds={video.durationSeconds} />
      <Heading className="video-card__title" lang={languageTag(video.language)}>
        <Link href={videoPath(video.publicId)}>{video.title}</Link>
      </Heading>
      <p className="meta-line">
        {video.speaker ? <span>{video.speaker}</span> : null}
        {date ? (
          <span>
            <time dateTime={video.publishedAt}>{date}</time>
          </span>
        ) : null}
      </p>
    </article>
  );
}

export function VideoGrid({ videos, heading }: { videos: VideoSummary[]; heading: 'h2' | 'h3' }) {
  return (
    <ul className="video-grid">
      {videos.map((video) => (
        <li key={video.publicId}>
          <VideoCard video={video} heading={heading} />
        </li>
      ))}
    </ul>
  );
}
