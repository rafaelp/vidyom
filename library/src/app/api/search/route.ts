import { isDatasetUnavailable } from '@/lib/dataset';
import { parseSearchParams } from '@/lib/search/params';
import { search } from '@/lib/search/service';
import type { SearchErrorBody } from '@/lib/search/types';

// Results must reflect the current file: a withdrawn video leaves at once (I243).
export const dynamic = 'force-dynamic';

const NO_STORE = { 'Cache-Control': 'no-store' };

function errorBody(code: string, message: string, details?: Record<string, unknown>): SearchErrorBody {
  return { error: { code, message, ...(details ? { details } : {}) } };
}

export async function GET(request: Request): Promise<Response> {
  const parsed = parseSearchParams(new URL(request.url).searchParams);
  if (!parsed.ok) {
    return Response.json(errorBody(parsed.code, parsed.message), { status: 400, headers: NO_STORE });
  }
  try {
    return Response.json(await search(parsed.request), { headers: NO_STORE });
  } catch (error) {
    if (isDatasetUnavailable(error)) {
      return Response.json(
        errorBody('dataset_unavailable', 'The public catalog is not available right now.', {
          status: error.status,
        }),
        { status: 503, headers: { ...NO_STORE, 'Retry-After': '60' } },
      );
    }
    // The query is deliberately left out of the log (I254).
    console.error('[library] search failed', error);
    return Response.json(errorBody('internal', 'Search failed.'), { status: 500, headers: NO_STORE });
  }
}
