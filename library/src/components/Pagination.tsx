import { SiteLink as Link } from '@/components/SiteLink';

interface PaginationProps {
  page: number;
  totalPages: number;
  /** Builds the link for a page number, preserving the current parameters. */
  hrefFor: (page: number) => string;
}

export function Pagination({ page, totalPages, hrefFor }: PaginationProps) {
  if (totalPages <= 1) return null;
  return (
    <nav className="pagination" aria-label="Pagination">
      {page > 1 ? (
        <Link className="button button--quiet" href={hrefFor(Math.min(page - 1, totalPages))} rel="prev">
          Previous
        </Link>
      ) : (
        <span className="pagination__spacer" />
      )}
      <span className="pagination__status">
        Page {page.toLocaleString('en')} of {totalPages.toLocaleString('en')}
      </span>
      {page < totalPages ? (
        <Link className="button button--quiet" href={hrefFor(page + 1)} rel="next">
          Next
        </Link>
      ) : (
        <span className="pagination__spacer" />
      )}
    </nav>
  );
}
