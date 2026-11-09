// @vitest-environment jsdom
import { fireEvent, render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { ThemeToggle } from '@/components/ThemeToggle';

function setSystemTheme(dark: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({ matches: dark && query.includes('dark') }));
}

describe('ThemeToggle', () => {
  beforeEach(() => {
    delete document.documentElement.dataset.theme;
    document.cookie = 'vidyom_theme=; Max-Age=0; Path=/';
    setSystemTheme(false);
  });

  it('renders no control on the server, where it could not work', () => {
    expect(renderToStaticMarkup(<ThemeToggle />)).not.toContain('<button');
  });

  it('sets no cookie until it is used', () => {
    render(<ThemeToggle />);
    expect(document.cookie).toBe('');
  });

  it('switches from the system theme and stores the choice', () => {
    setSystemTheme(true);
    render(<ThemeToggle />);
    fireEvent.click(screen.getByRole('button', { name: 'Switch between light and dark theme' }));
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(document.cookie).toBe('vidyom_theme=light');
  });

  it('switches back', () => {
    render(<ThemeToggle />);
    const button = screen.getByRole('button');
    fireEvent.click(button);
    expect(document.documentElement.dataset.theme).toBe('dark');
    fireEvent.click(button);
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(document.cookie).toBe('vidyom_theme=light');
  });
});
