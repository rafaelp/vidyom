import type { Metadata } from 'next';
import { SiteLink as Link } from '@/components/SiteLink';
import { DatasetUnavailable } from '@/components/DatasetUnavailable';
import { EmptyState } from '@/components/EmptyState';
import { FilterDisclosure, FilterFields } from '@/components/FilterFields';
import { FilterForm } from '@/components/FilterForm';
import { Pagination } from '@/components/Pagination';
import { VideoGrid } from '@/components/VideoCard';
import { pluralize } from '@/lib/format';
import { readDataset, toURLSearchParams, type RawSearchParams } from '@/lib/page-data';
import { readConfig } from '@/lib/config';
import { countVideos, getFacets, hasFilters, listVideos } from '@/lib/queries';
import { filtersFromParams, pageFromParam } from '@/lib/search/params';

export const dynamic = 'force-dynamic';

interface HomeProps {
  searchParams: Promise<RawSearchParams>;
}

export async function generateMetadata({ searchParams }: HomeProps): Promise<Metadata> {
  const params = toURLSearchParams(await searchParams);
  const page = pageFromParam(params.get('page'));
  const config = readConfig();
  // A layout's title template does not reach a page in its own segment, so these are whole.
  // Filter combinations are an unbounded URL space with no content of their own (I256).
  if (hasFilters(filtersFromParams(params))) {
    return { title: `Videos · ${config.title}`, robots: { index: false, follow: true } };
  }
  return {
    ...(page > 1 ? { title: `Videos, page ${page} · ${config.title}` } : {}),
    // Canonical URLs need a configured origin (I256).
    ...(config.siteUrl ? { alternates: { canonical: page > 1 ? `/?page=${page}` : '/' } } : {}),
  };
}

export default async function HomePage({ searchParams }: HomeProps) {
  const params = toURLSearchParams(await searchParams);
  const filters = filtersFromParams(params);
  const requestedPage = pageFromParam(params.get('page'));

  const data = readDataset((generation) => ({
    list: listVideos(generation, filters, requestedPage),
    facets: getFacets(generation),
    catalogSize: countVideos(generation),
  }));
  if (!data.ready) return <DatasetUnavailable status={data.status} />;

  const { list, facets, catalogSize } = data.value;
  const filtered = hasFilters(filters);
  const totalPages = Math.max(1, Math.ceil(list.total / list.pageSize));

  const hrefFor = (page: number) => {
    const next = new URLSearchParams();
    for (const [key, value] of Object.entries(filters)) {
      if (value) next.set(key, value);
    }
    if (page > 1) next.set('page', String(page));
    const query = next.toString();
    return query ? `/?${query}` : '/';
  };

  if (catalogSize === 0) {
    return (
      <div className="page">
        <EmptyState title="No videos have been published yet" headingLevel="h1">
          Videos appear here once they are approved and published.
        </EmptyState>
      </div>
    );
  }

  return (
    <div className="page stack">
      <div className="section-head">
        <h1>Videos</h1>
        <p className="count" role="status">
          {filtered
            ? `${pluralize(list.total, 'video')} match these filters`
            : pluralize(list.total, 'video')}
        </p>
      </div>

      <FilterDisclosure active={Object.values(filters).filter(Boolean).length}>
        <FilterForm action="/">
          <FilterFields facets={facets} filters={filters} />
          <noscript>
            <button type="submit" className="button">
              Apply
            </button>
          </noscript>
          {filtered ? (
            <Link className="button button--quiet" href="/">
              Clear filters
            </Link>
          ) : null}
        </FilterForm>
      </FilterDisclosure>

      {list.items.length > 0 ? (
        <>
          <VideoGrid videos={list.items} heading="h2" />
          <Pagination page={requestedPage} totalPages={totalPages} hrefFor={hrefFor} />
        </>
      ) : list.total === 0 ? (
        <EmptyState
          title="No videos match these filters"
          actions={
            <Link className="button button--quiet" href="/">
              Clear filters
            </Link>
          }
        >
          Try removing a filter to see more.
        </EmptyState>
      ) : (
        <EmptyState
          title="This page is past the end of the list"
          actions={
            <Link className="button button--quiet" href={hrefFor(totalPages)}>
              Go to the last page
            </Link>
          }
        />
      )}
    </div>
  );
}
