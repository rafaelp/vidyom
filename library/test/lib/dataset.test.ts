import fs from 'node:fs';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { describe, expect, it } from 'vitest';
import { DatasetManager, DatasetUnavailableError } from '@/lib/dataset';
import { countVideos } from '@/lib/queries';
import { CATALOG, buildFixture } from '../../scripts/generate-fixture';
import { fixtureDataset, quietLog, replaceAtomically, tempDir } from '../helpers';

function status(dataset: DatasetManager): string {
  return dataset.state().status;
}

describe('dataset states', () => {
  it('reports missing when there is no file, and recovers when one arrives', () => {
    const directory = tempDir();
    const file = path.join(directory, 'public.db');
    const dataset = new DatasetManager(file, quietLog);
    expect(status(dataset)).toBe('missing');
    expect(() => dataset.withSnapshot(() => 1)).toThrow(DatasetUnavailableError);

    buildFixture(file);
    expect(status(dataset)).toBe('ready');
    dataset.close();
  });

  it('reports missing for a directory at the path', () => {
    const directory = tempDir();
    expect(status(new DatasetManager(directory, quietLog))).toBe('missing');
  });

  it('reports invalid for a file that is not SQLite', () => {
    const file = path.join(tempDir(), 'public.db');
    fs.writeFileSync(file, 'this is not a database, not even close, not at all');
    expect(status(new DatasetManager(file, quietLog))).toBe('invalid');
  });

  it('reports invalid for a truncated file', () => {
    const { file, dataset } = fixtureDataset();
    dataset.close();
    const bytes = fs.readFileSync(file);
    fs.writeFileSync(file, bytes.subarray(0, 4096));
    expect(status(new DatasetManager(file, quietLog))).toBe('invalid');
  });

  it('reports invalid for a SQLite file missing required tables', () => {
    const file = path.join(tempDir(), 'public.db');
    const db = new DatabaseSync(file);
    db.exec("CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT); INSERT INTO meta VALUES ('schema_version', '1')");
    db.close();
    expect(status(new DatasetManager(file, quietLog))).toBe('invalid');
  });

  it('reports invalid when a required meta key is missing', () => {
    const { file, dataset } = fixtureDataset();
    dataset.close();
    const db = new DatabaseSync(file);
    db.exec("DELETE FROM meta WHERE key = 'generated_at'");
    db.close();
    expect(status(new DatasetManager(file, quietLog))).toBe('invalid');
  });

  it('refuses a newer schema rather than misreading it', () => {
    const { dataset } = fixtureDataset({ schemaVersion: '2' });
    expect(status(dataset)).toBe('unsupported_schema');
  });

  it('reads meta from a ready file', () => {
    const { dataset } = fixtureDataset({ generatedAt: '2026-11-01T00:00:00Z' });
    const meta = dataset.withSnapshot((generation) => generation.meta);
    expect(meta).toEqual({
      schemaVersion: '1',
      generatedAt: '2026-11-01T00:00:00Z',
      embeddingModel: 'fake-hash-embed@1',
      embeddingDimensions: 1024,
    });
    dataset.close();
  });
});

describe('opening the file read-only', () => {
  it('opens a file whose header says WAL, in a read-only directory, with no side files', () => {
    const { file, directory, dataset } = fixtureDataset({ journalMode: 'WAL' });
    expect(fs.readFileSync(file).subarray(18, 20)).toEqual(Buffer.from([2, 2]));
    fs.chmodSync(file, 0o444);
    fs.chmodSync(directory, 0o555);
    try {
      expect(dataset.withSnapshot(countVideos)).toBe(CATALOG.length);
      expect(fs.readdirSync(directory)).toEqual(['public.db']);
    } finally {
      dataset.close();
      fs.chmodSync(directory, 0o755);
    }
  });

  it('ignores a stale -wal left beside the file', () => {
    const { file, directory, dataset } = fixtureDataset();
    fs.writeFileSync(`${file}-wal`, 'garbage that must never be applied');
    expect(dataset.withSnapshot(countVideos)).toBe(CATALOG.length);
    dataset.close();
    expect(fs.readdirSync(directory).sort()).toEqual(['public.db', 'public.db-wal']);
  });

  it('refuses writes', () => {
    const { dataset } = fixtureDataset();
    expect(() =>
      dataset.withSnapshot((generation) => generation.db.get("INSERT INTO meta VALUES ('x', 'y')")),
    ).toThrow();
    dataset.close();
  });
});

describe('replacing the file while running', () => {
  it('serves the new file on the next access after an atomic rename', () => {
    const { file, dataset } = fixtureDataset({ videos: CATALOG.slice(0, 2) });
    expect(dataset.withSnapshot(countVideos)).toBe(2);
    const firstId = dataset.withSnapshot((generation) => generation.id);

    replaceAtomically(file, { videos: CATALOG.slice(0, 5) });

    expect(dataset.withSnapshot(countVideos)).toBe(5);
    expect(dataset.withSnapshot((generation) => generation.id)).not.toBe(firstId);
    dataset.close();
  });

  it('drops memoized data with the generation', () => {
    const { file, dataset } = fixtureDataset({ videos: CATALOG.slice(0, 1) });
    const first = dataset.withSnapshot((generation) => generation.memo('value', () => ({ from: 1 })));
    expect(dataset.withSnapshot((generation) => generation.memo('value', () => ({ from: 2 })))).toBe(first);
    replaceAtomically(file, { videos: CATALOG.slice(0, 2) });
    expect(dataset.withSnapshot((generation) => generation.memo('value', () => ({ from: 3 })))).toEqual({
      from: 3,
    });
    dataset.close();
  });

  it('stops serving at once when the file is removed', () => {
    const { file, dataset } = fixtureDataset();
    expect(status(dataset)).toBe('ready');
    fs.rmSync(file);
    expect(status(dataset)).toBe('missing');
    expect(() => dataset.withSnapshot(countVideos)).toThrow(DatasetUnavailableError);
  });

  it('fails closed: an invalid replacement is not served, nor is the previous file', () => {
    const { file, dataset } = fixtureDataset();
    expect(status(dataset)).toBe('ready');

    const next = `${file}.next`;
    fs.writeFileSync(next, 'not a database, this is not a database at all');
    fs.renameSync(next, file);

    expect(status(dataset)).toBe('invalid');
    expect(() => dataset.withSnapshot(countVideos)).toThrow(DatasetUnavailableError);

    replaceAtomically(file);
    expect(status(dataset)).toBe('ready');
    dataset.close();
  });

  it('notices a file overwritten in place through its stat', () => {
    const { file, dataset } = fixtureDataset({ videos: CATALOG.slice(0, 2) });
    expect(dataset.withSnapshot(countVideos)).toBe(2);
    const other = `${file}.other`;
    buildFixture(other, { videos: CATALOG });
    fs.copyFileSync(other, file);
    expect(dataset.withSnapshot(countVideos)).toBe(CATALOG.length);
    dataset.close();
  });

  it('retries a failed read once from a fresh stat, then reports invalid', () => {
    const { dataset } = fixtureDataset();
    let attempts = 0;
    const sqliteError = Object.assign(new Error('database disk image is malformed'), {
      code: 'ERR_SQLITE_ERROR',
    });

    expect(
      dataset.withSnapshot(() => {
        attempts += 1;
        if (attempts === 1) throw sqliteError;
        return 'recovered';
      }),
    ).toBe('recovered');
    expect(attempts).toBe(2);

    attempts = 0;
    expect(() =>
      dataset.withSnapshot(() => {
        attempts += 1;
        throw sqliteError;
      }),
    ).toThrow(DatasetUnavailableError);
    expect(attempts).toBe(2);
    expect(status(dataset)).toBe('invalid');
  });

  it('does not treat an application error as a damaged file', () => {
    const { dataset } = fixtureDataset();
    expect(() =>
      dataset.withSnapshot(() => {
        throw new TypeError('a bug');
      }),
    ).toThrow(TypeError);
    expect(status(dataset)).toBe('ready');
    dataset.close();
  });
});
