'use client';

import { useSyncExternalStore } from 'react';
import { MoonIcon, SunIcon } from './icons';

const COOKIE = 'vidyom_theme';
const ONE_YEAR_SECONDS = 31_536_000;

function subscribe(): () => void {
  return () => {};
}

function effectiveTheme(): 'light' | 'dark' {
  const chosen = document.documentElement.dataset.theme;
  if (chosen === 'light' || chosen === 'dark') return chosen;
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

/**
 * Switches between light and dark. The choice goes in a cookie the server reads, so
 * the next page renders in that theme with no script and no flash. No cookie exists
 * until this button is used (docs/decisions/library.md, I253).
 */
export function ThemeToggle() {
  // Without JavaScript this button could not work, so it only appears after hydration.
  const hydrated = useSyncExternalStore(subscribe, () => true, () => false);
  if (!hydrated) return <span className="icon-button" aria-hidden="true" />;

  const toggle = () => {
    const next = effectiveTheme() === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    const secure = window.location.protocol === 'https:' ? '; Secure' : '';
    document.cookie = `${COOKIE}=${next}; Path=/; Max-Age=${ONE_YEAR_SECONDS}; SameSite=Lax${secure}`;
  };

  return (
    <button
      type="button"
      className="icon-button"
      onClick={toggle}
      aria-label="Switch between light and dark theme"
      title="Switch between light and dark theme"
    >
      <SunIcon className="theme-icon--sun" />
      <MoonIcon className="theme-icon--moon" />
    </button>
  );
}
