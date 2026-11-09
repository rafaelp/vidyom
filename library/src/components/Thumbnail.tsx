import { spokenDuration, formatTimestamp, kindLabel } from '@/lib/format';
import type { VideoKind } from '@/lib/types';
import { PlayIcon } from './icons';

interface ThumbnailProps {
  url: string;
  kind?: VideoKind;
  durationSeconds?: number;
}

/**
 * A destination's thumbnail over a generated tile. The tile sits behind the image, so
 * a missing or broken image shows it without any script. A plain <img>, not next/image,
 * because destination hosts are not known at build time (I252).
 */
export function Thumbnail({ url, kind, durationSeconds }: ThumbnailProps) {
  return (
    <span className="thumb">
      <PlayIcon className="thumb__glyph" />
      {url ? (
        // eslint-disable-next-line @next/next/no-img-element
        <img src={url} alt="" loading="lazy" decoding="async" referrerPolicy="no-referrer" />
      ) : null}
      {kind ? <span className="chip thumb__kind">{kindLabel(kind)}</span> : null}
      {durationSeconds && durationSeconds > 0 ? (
        <span className="chip thumb__duration">
          <span aria-hidden="true">{formatTimestamp(durationSeconds)}</span>
          <span className="sr-only">{`Duration ${spokenDuration(durationSeconds)}`}</span>
        </span>
      ) : null}
    </span>
  );
}
