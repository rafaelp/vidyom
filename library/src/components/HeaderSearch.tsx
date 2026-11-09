'use client';

import Form from 'next/form';
import { usePathname } from 'next/navigation';

/** The compact search box in the header. The search page has its own, larger one. */
export function HeaderSearch() {
  const pathname = usePathname();
  if (pathname === '/search') return null;
  return (
    <Form action="/search" role="search" className="header-search">
      <div className="search-field">
        <label htmlFor="header-search-input" className="sr-only">
          Search videos
        </label>
        <input
          id="header-search-input"
          type="search"
          name="q"
          placeholder="Search videos"
          maxLength={256}
          enterKeyHint="search"
          autoComplete="off"
        />
        <button type="submit">Search</button>
      </div>
    </Form>
  );
}
