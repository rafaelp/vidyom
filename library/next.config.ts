import type { NextConfig } from 'next';

// The site makes no call to Vercel or anyone else (docs/decisions/library.md, I255).
process.env.NEXT_TELEMETRY_DISABLED = '1';

const config: NextConfig = {
  poweredByHeader: false,
  // The queries are local and fast, so metadata is never streamed after the shell:
  // every response has its <title> in <head> and its status code decided up front.
  htmlLimitedBots: /.*/,
  reactStrictMode: true,
  // Thumbnails are plain <img> from the destination (I252); the optimizer is unused.
  images: { unoptimized: true },
  // library/ is its own project inside a repository that is mostly Go.
  turbopack: { root: __dirname },
  outputFileTracingRoot: __dirname,
  // `next start` refuses standalone builds, so only the image asks for one (I258).
  ...(process.env.VIDYOM_LIBRARY_STANDALONE === '1' ? { output: 'standalone' as const } : {}),
  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'Referrer-Policy', value: 'strict-origin-when-cross-origin' },
          {
            key: 'Permissions-Policy',
            value: 'camera=(), microphone=(), geolocation=(), payment=(), usb=(), browsing-topics=()',
          },
        ],
      },
    ];
  },
};

export default config;
