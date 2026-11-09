import type { Metadata, Viewport } from 'next';
import localFont from 'next/font/local';
import { cookies } from 'next/headers';
import type { ReactNode } from 'react';
import { SiteFooter } from '@/components/SiteFooter';
import { SiteHeader } from '@/components/SiteHeader';
import { readConfig } from '@/lib/config';
import { getDataset } from '@/lib/dataset';
import './globals.css';

// Self-hosted, so neither the build nor a page view contacts a font host (I255).
const sans = localFont({
  src: '../../node_modules/@fontsource-variable/dm-sans/files/dm-sans-latin-wght-normal.woff2',
  variable: '--font-sans',
  weight: '100 1000',
  display: 'swap',
});

const heading = localFont({
  src: '../../node_modules/@fontsource-variable/space-grotesk/files/space-grotesk-latin-wght-normal.woff2',
  variable: '--font-heading',
  weight: '300 700',
  display: 'swap',
});

// Every route reflects the current public.db: nothing is prerendered or cached (I243).
export const dynamic = 'force-dynamic';

export const viewport: Viewport = {
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#fffcf9' },
    { media: '(prefers-color-scheme: dark)', color: '#15110f' },
  ],
};

export function generateMetadata(): Metadata {
  const config = readConfig();
  return {
    title: { default: config.title, template: `%s · ${config.title}` },
    description: config.description || undefined,
    applicationName: config.title,
    metadataBase: config.siteUrl ? new URL(`${config.siteUrl}/`) : undefined,
    openGraph: { siteName: config.title, type: 'website' },
  };
}

export default async function RootLayout({ children }: { children: ReactNode }) {
  const config = readConfig();
  const cookieStore = await cookies();
  const chosen = cookieStore.get('vidyom_theme')?.value;
  const theme = chosen === 'light' || chosen === 'dark' ? chosen : undefined;
  const state = getDataset(config.publicDbPath).state();

  return (
    <html lang="en" data-theme={theme} className={`${sans.variable} ${heading.variable}`}>
      <body>
        <a className="skip-link" href="#main">
          Skip to content
        </a>
        <SiteHeader title={config.title} />
        <main id="main" tabIndex={-1}>
          {children}
        </main>
        <SiteFooter generatedAt={state.status === 'ready' ? state.generation.meta.generatedAt : null} />
      </body>
    </html>
  );
}
