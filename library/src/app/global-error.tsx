'use client';

import { useEffect } from 'react';
import './globals.css';

/** Replaces the root layout when the layout itself fails, so it brings its own document. */
export default function GlobalError({ error, reset }: { error: Error; reset: () => void }) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <html lang="en">
      <body>
        <main className="page">
          <section className="state">
            <h1>Something went wrong</h1>
            <p>This page could not be shown. Trying again may help.</p>
            <div className="state__actions">
              <button type="button" className="button" onClick={reset}>
                Try again
              </button>
            </div>
          </section>
        </main>
      </body>
    </html>
  );
}
