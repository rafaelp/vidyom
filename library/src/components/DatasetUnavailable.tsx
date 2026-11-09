import type { UnavailableStatus } from '@/lib/dataset';
import { UNAVAILABLE_COPY } from '@/lib/unavailable-page';

/**
 * Shown when the file disappears between the proxy's check and the page's read; the
 * proxy answers every other case with a 503 (I244).
 */
export function DatasetUnavailable({ status }: { status: UnavailableStatus }) {
  const copy = UNAVAILABLE_COPY[status];
  return (
    <div className="page">
      <section className="state">
        <h1>{copy.title}</h1>
        <p>{copy.body}</p>
      </section>
    </div>
  );
}
