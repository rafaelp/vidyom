import path from 'node:path';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, 'src') },
  },
  oxc: { jsx: { runtime: 'automatic' } },
  test: {
    include: ['test/**/*.test.{ts,tsx}'],
    environment: 'node',
    setupFiles: ['test/setup.ts'],
    restoreMocks: true,
  },
});
