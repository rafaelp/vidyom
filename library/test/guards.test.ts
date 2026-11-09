import fs from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { buildCsp } from '@/lib/csp';

const ROOT = path.resolve(import.meta.dirname, '..');

function sourceFiles(directory: string): string[] {
  const files: string[] = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const full = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...sourceFiles(full));
    else if (/\.(ts|tsx|css)$/.test(entry.name)) files.push(full);
  }
  return files;
}

const SOURCES = sourceFiles(path.join(ROOT, 'src')).map((file) => ({
  file: path.relative(ROOT, file),
  text: fs.readFileSync(file, 'utf8'),
}));

// Comments may name these things to explain that they are absent.
function withoutComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
}

describe('the site never contacts the daemon (D34, R44)', () => {
  it('has no configuration or client for it', () => {
    for (const { file, text } of SOURCES) {
      const code = withoutComments(text);
      expect(code, file).not.toMatch(/vidyomd|VIDYOM_(DAEMON|API|SERVER)|\/api\/v1\b|Vidyom-Tenant/);
    }
  });

  it('calls only its own search API from code', () => {
    const fetches = SOURCES.flatMap(({ file, text }) =>
      [...withoutComments(text).matchAll(/\bfetch\(([^,)]*)/g)].map((match) => ({ file, target: match[1] })),
    );
    expect(fetches).toEqual([
      { file: 'src/components/SearchExperience.tsx', target: '`/api/search?${paramsFor(trimmed' },
    ]);
  });

  it('reaches no third-party host from code', () => {
    const allowed = new Set([
      // A plain link in the footer; loaded only if the visitor follows it.
      'https://www.vidyom.org',
      // XML and structured data vocabularies; never fetched.
      'https://schema.org',
      'http://www.sitemaps.org/schemas/sitemap/0.9',
    ]);
    for (const { file, text } of SOURCES) {
      for (const match of withoutComments(text).matchAll(/https?:\/\/[^\s"'`)<]+/g)) {
        expect(allowed.has(match[0]), `${file}: ${match[0]}`).toBe(true);
      }
    }
  });

  it('opens SQLite in one place only', () => {
    const importers = SOURCES.filter(({ text }) => /from 'node:sqlite'/.test(text) && !/import type/.test(text)).map(
      ({ file }) => file,
    );
    expect(importers).toEqual(['src/lib/sqlite.ts']);
  });
});

describe('no scripts or trackers', () => {
  it('declares no analytics or tag manager dependency', () => {
    const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, 'package.json'), 'utf8')) as {
      dependencies: Record<string, string>;
    };
    expect(Object.keys(manifest.dependencies).sort()).toEqual([
      '@fontsource-variable/dm-sans',
      '@fontsource-variable/space-grotesk',
      'next',
      'react',
      'react-dom',
    ]);
  });

  it('runs the framework only with its telemetry off', () => {
    const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, 'package.json'), 'utf8')) as {
      scripts: Record<string, string>;
    };
    for (const name of ['dev', 'build', 'start', 'typecheck']) {
      expect(manifest.scripts[name], name).toMatch(/^node scripts\/next\.mjs /);
    }
    expect(fs.readFileSync(path.join(ROOT, 'scripts/next.mjs'), 'utf8')).toContain(
      "process.env.NEXT_TELEMETRY_DISABLED = '1'",
    );
  });

  it('pins every dependency exactly', () => {
    const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, 'package.json'), 'utf8')) as {
      dependencies: Record<string, string>;
      devDependencies: Record<string, string>;
    };
    for (const [name, version] of Object.entries({ ...manifest.dependencies, ...manifest.devDependencies })) {
      expect(version, name).toMatch(/^\d+\.\d+\.\d+$/);
    }
  });
});

describe('CSP', () => {
  it('runs scripts only with the nonce, and adds nothing unsafe in production', () => {
    const csp = buildCsp('n0nce', false);
    expect(csp).toContain("script-src 'self' 'nonce-n0nce'");
    expect(csp).not.toContain('unsafe');
    expect(csp).toContain("object-src 'none'");
    expect(csp).toContain("connect-src 'self'");
    expect(csp).toContain('frame-src https:');
    expect(csp).toContain("frame-ancestors 'self'");
  });

  it('loosens only for the development server', () => {
    expect(buildCsp('n0nce', true)).toContain("'unsafe-eval'");
  });
});

/** WCAG relative luminance. */
function luminance(hex: string): number {
  const channels = [1, 3, 5].map((start) => parseInt(hex.slice(start, start + 2), 16) / 255);
  const [r, g, b] = channels.map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4)) as [
    number,
    number,
    number,
  ];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

describe('theme tokens meet WCAG AA', () => {
  const css = fs.readFileSync(path.join(ROOT, 'src/app/globals.css'), 'utf8');
  const block = (selector: string): Record<string, string> => {
    const start = css.indexOf(selector);
    const open = css.indexOf('{', start);
    const close = css.indexOf('}', open);
    const tokens: Record<string, string> = {};
    for (const match of css.slice(open, close).matchAll(/--([\w-]+):\s*(#[0-9a-fA-F]{6})/g)) {
      tokens[match[1] as string] = match[2] as string;
    }
    return tokens;
  };

  const themes = {
    light: block(':root {'),
    'dark (system)': block(":root:not([data-theme='light']) {"),
    'dark (chosen)': block(":root[data-theme='dark'] {"),
  };

  it('defines the brand colors', () => {
    expect(themes.light.accent).toBe('#c54b00');
    expect(themes['dark (chosen)'].accent).toBe('#fd8a02');
    expect(themes['dark (chosen)'].bg).toBe('#15110f');
  });

  it('keeps both dark blocks identical', () => {
    expect(themes['dark (system)']).toEqual(themes['dark (chosen)']);
  });

  it.each(Object.entries(themes))('%s', (_name, tokens) => {
    const text: [string, string, number][] = [
      ['text', 'bg', 4.5],
      ['text', 'surface', 4.5],
      ['text', 'surface-2', 4.5],
      ['muted', 'bg', 4.5],
      ['muted', 'surface', 4.5],
      ['muted', 'surface-2', 4.5],
      ['link', 'bg', 4.5],
      ['link', 'surface', 4.5],
      ['link', 'surface-2', 4.5],
      ['on-accent', 'accent', 4.5],
      ['on-accent', 'accent-hover', 4.5],
      ['mark-text', 'mark-bg', 4.5],
      // Control borders and focus rings: WCAG 1.4.11 asks for 3:1.
      ['border-strong', 'bg', 3],
      ['border-strong', 'surface', 3],
      ['accent', 'bg', 3],
    ];
    for (const [foreground, background, minimum] of text) {
      const ratio = contrast(tokens[foreground] as string, tokens[background] as string);
      expect(ratio, `${foreground} on ${background}: ${ratio.toFixed(2)}`).toBeGreaterThanOrEqual(minimum);
    }
  });
});
