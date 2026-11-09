import path from 'node:path';
import { defineConfig } from 'vitest/config';

// Boots the production server, so `npm run build` must have run first.
export default defineConfig({
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, 'src') },
  },
  test: {
    include: ['e2e/**/*.e2e.ts'],
    environment: 'node',
    testTimeout: 60_000,
    hookTimeout: 120_000,
    fileParallelism: false,
  },
});
