import { getDataset, isDatasetUnavailable, type Generation, type UnavailableStatus } from './dataset';

/** A page's read of the file: the value, or the state to render instead. */
export type PageData<T> = { ready: true; value: T } | { ready: false; status: UnavailableStatus };

export function readDataset<T>(read: (generation: Generation) => T): PageData<T> {
  try {
    return { ready: true, value: getDataset().withSnapshot(read) };
  } catch (error) {
    if (isDatasetUnavailable(error)) return { ready: false, status: error.status };
    throw error;
  }
}

export type RawSearchParams = Record<string, string | string[] | undefined>;

/** Next's search params to a URLSearchParams, taking the first of repeated values. */
export function toURLSearchParams(raw: RawSearchParams): URLSearchParams {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(raw)) {
    const first = Array.isArray(value) ? value[0] : value;
    if (first !== undefined) params.set(key, first);
  }
  return params;
}
