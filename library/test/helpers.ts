import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterAll } from 'vitest';
import { DatasetManager } from '@/lib/dataset';
import { readConfig, type LibraryConfig } from '@/lib/config';
import { buildFixture, type FixtureOptions } from '../scripts/generate-fixture';

const directories: string[] = [];

/** A temporary directory removed when the test file finishes. */
export function tempDir(): string {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'vidyom-library-test-'));
  directories.push(directory);
  return directory;
}

afterAll(() => {
  for (const directory of directories.splice(0)) {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

export const quietLog = { info: () => {}, warn: () => {} };

/** A fixture file in a temporary directory and a manager reading it. */
export function fixtureDataset(options: FixtureOptions = {}): {
  file: string;
  directory: string;
  dataset: DatasetManager;
} {
  const directory = tempDir();
  const file = path.join(directory, 'public.db');
  buildFixture(file, options);
  return { file, directory, dataset: new DatasetManager(file, quietLog) };
}

/** Replaces a file the way the exporter does: write beside it, then rename. */
export function replaceAtomically(file: string, options: FixtureOptions = {}): void {
  const next = `${file}.next`;
  buildFixture(next, options);
  fs.renameSync(next, file);
}

export function testConfig(overrides: Partial<LibraryConfig> = {}): LibraryConfig {
  return { ...readConfig({}), ...overrides };
}
