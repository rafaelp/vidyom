'use client';

import { useRouter } from 'next/navigation';
import type { FormEvent, ReactNode } from 'react';

/**
 * A GET form that works without JavaScript. With it, changing a field applies the
 * filter at once, and empty fields stay out of the URL.
 */
export function FilterForm({ action, children }: { action: string; children: ReactNode }) {
  const router = useRouter();

  const apply = (form: HTMLFormElement) => {
    const params = new URLSearchParams();
    for (const [key, value] of new FormData(form)) {
      if (typeof value === 'string' && value !== '') params.set(key, value);
    }
    const query = params.toString();
    router.push(query ? `${action}?${query}` : action);
  };

  const onSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    apply(event.currentTarget);
  };

  return (
    <form
      method="get"
      action={action}
      className="filter-bar"
      aria-label="Filter videos"
      onSubmit={onSubmit}
      onChange={(event) => apply(event.currentTarget)}
    >
      {children}
    </form>
  );
}
