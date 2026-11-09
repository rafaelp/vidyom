import { SiteLink as Link } from '@/components/SiteLink';
import { formatTimestamp, spokenDuration } from '@/lib/format';
import { momentLink } from '@/lib/links';
import type { Destination } from '@/lib/types';

interface TimestampLinkProps {
  publicId: string;
  primary: Destination | undefined;
  seconds: number;
  className?: string;
  /** Prefixed for screen readers, such as a chapter's title. */
  context?: string;
}

/** A timestamp as a link: seeks the embed, or opens the destination at that position. */
export function TimestampLink({ publicId, primary, seconds, className, context }: TimestampLinkProps) {
  const link = momentLink(publicId, primary, seconds);
  const label = (
    <>
      <span aria-hidden="true">{formatTimestamp(seconds)}</span>
      <span className="sr-only">
        {`${context ? `${context}, ` : ''}at ${spokenDuration(seconds)}${link.external ? ', opens in a new tab' : ''}`}
      </span>
    </>
  );
  if (link.external) {
    return (
      <a className={className} href={link.href} target="_blank" rel="noopener noreferrer">
        {label}
      </a>
    );
  }
  return (
    <Link className={className} href={link.href}>
      {label}
    </Link>
  );
}
