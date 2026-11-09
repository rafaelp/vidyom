import type { Metadata } from 'next';
import { DatasetUnavailable } from '@/components/DatasetUnavailable';
import { SearchExperience } from '@/components/SearchExperience';
import { isDatasetUnavailable } from '@/lib/dataset';
import { readDataset, toURLSearchParams, type RawSearchParams } from '@/lib/page-data';
import { getFacets } from '@/lib/queries';
import { filtersFromParams, parseSearchParams } from '@/lib/search/params';
import { search } from '@/lib/search/service';
import type { SearchResponse } from '@/lib/search/types';

export const dynamic = 'force-dynamic';

interface SearchPageProps {
  searchParams: Promise<RawSearchParams>;
}

export async function generateMetadata({ searchParams }: SearchPageProps): Promise<Metadata> {
  const query = toURLSearchParams(await searchParams).get('q')?.trim();
  // Search results are an unbounded URL space with no content of their own (I256).
  return { title: query ? `Search: ${query}` : 'Search', robots: { index: false, follow: true } };
}

export default async function SearchPage({ searchParams }: SearchPageProps) {
  const params = toURLSearchParams(await searchParams);
  const query = params.get('q') ?? '';
  const filters = filtersFromParams(params);

  const facets = readDataset(getFacets);
  if (!facets.ready) return <DatasetUnavailable status={facets.status} />;

  let response: SearchResponse | null = null;
  let errorMessage: string | null = null;
  const parsed = parseSearchParams(params);
  if (parsed.ok) {
    try {
      response = await search(parsed.request);
    } catch (error) {
      if (isDatasetUnavailable(error)) return <DatasetUnavailable status={error.status} />;
      throw error;
    }
  } else if (parsed.code !== 'query_required') {
    errorMessage = parsed.message;
  }

  // Remount when the server sends new results, for example after following a page link.
  const key = params.toString();
  return (
    <SearchExperience
      key={key}
      initialQuery={query}
      initialResponse={response}
      initialError={errorMessage}
      filters={filters}
      facets={facets.value}
    />
  );
}
