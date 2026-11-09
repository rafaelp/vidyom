import type { Metadata } from 'next';
import { SiteLink as Link } from '@/components/SiteLink';
import { EmptyState } from '@/components/EmptyState';

// The framework adds noindex to a 404 by itself.
export const metadata: Metadata = {
  title: 'Not found',
};

export default function NotFound() {
  return (
    <div className="page">
      <EmptyState
        title="We can’t find that page"
        headingLevel="h1"
        actions={
          <>
            <Link className="button" href="/">
              See all videos
            </Link>
            <Link className="button button--quiet" href="/search">
              Search
            </Link>
          </>
        }
      >
        If you followed a link to a video, it may have been withdrawn, or the link may be wrong.
      </EmptyState>
    </div>
  );
}
