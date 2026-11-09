'use client';

import { useEffect } from 'react';
import { EmptyState } from '@/components/EmptyState';

/** Shows no details: the server log has them. */
export default function ErrorPage({ error, reset }: { error: Error; reset: () => void }) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <div className="page">
      <EmptyState
        title="Something went wrong"
        headingLevel="h1"
        actions={
          <button type="button" className="button" onClick={reset}>
            Try again
          </button>
        }
      >
        This page could not be shown. Trying again may help.
      </EmptyState>
    </div>
  );
}
