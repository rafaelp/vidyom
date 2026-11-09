import { statSync, type Stats } from 'node:fs';
import { readConfig } from './config';
import { isSqliteError, openReadOnly, type ReadOnlyDatabase } from './sqlite';

/**
 * Lifecycle of the tenant's public.db (docs/decisions/library.md, I242, I244).
 *
 * Every access compares the file's stat with the open generation. A replacement
 * opens a new generation and closes the old one; a removal closes it. It fails
 * closed: a replacement that does not validate is not served, and neither is the
 * previous file, because the previous file may still hold a withdrawn video.
 */

export const SUPPORTED_SCHEMA_VERSION = '1';

const REQUIRED_TABLES = [
  'meta',
  'videos',
  'destinations',
  'chapters',
  'transcript_segments',
  'embeddings',
  'search_fts',
] as const;

const REQUIRED_META_KEYS = [
  'schema_version',
  'generated_at',
  'embedding_model',
  'embedding_dimensions',
] as const;

/** How often an invalid file is re-read even when its stat has not changed. */
const INVALID_RECHECK_MS = 5_000;

export interface DatasetMeta {
  schemaVersion: string;
  generatedAt: string;
  embeddingModel: string;
  embeddingDimensions: number;
}

/** One opened, validated version of the file. Memoized data dies with it. */
export interface Generation {
  readonly db: ReadOnlyDatabase;
  readonly meta: DatasetMeta;
  /** Stat signature; changes whenever the file is replaced. */
  readonly id: string;
  memo<T>(key: string, compute: () => T): T;
}

export type DatasetStatus = 'ready' | 'missing' | 'invalid' | 'unsupported_schema';
export type UnavailableStatus = Exclude<DatasetStatus, 'ready'>;

export type DatasetState =
  | { status: 'ready'; generation: Generation }
  | { status: UnavailableStatus };

export class DatasetUnavailableError extends Error {
  readonly status: UnavailableStatus;

  constructor(status: UnavailableStatus) {
    super(`public dataset unavailable: ${status}`);
    this.name = 'DatasetUnavailableError';
    this.status = status;
  }
}

/**
 * Use this rather than `instanceof`: Next.js may evaluate this module once per route
 * bundle, and the shared manager throws the class from whichever bundle created it.
 */
export function isDatasetUnavailable(error: unknown): error is DatasetUnavailableError {
  return error instanceof Error && error.name === 'DatasetUnavailableError';
}

export interface DatasetLogger {
  info(message: string): void;
  warn(message: string): void;
}

function signatureOf(stats: Stats): string {
  return `${stats.ino}:${stats.size}:${stats.mtimeMs}`;
}

type OpenResult =
  | { status: 'ready'; generation: Generation }
  | { status: 'invalid' | 'unsupported_schema'; detail: string };

function openGeneration(filePath: string, stats: Stats): OpenResult {
  let db: ReadOnlyDatabase | undefined;
  try {
    db = openReadOnly(filePath);
    const fail = (status: 'invalid' | 'unsupported_schema', detail: string): OpenResult => {
      db?.close();
      return { status, detail };
    };

    // A truncated file can answer the first pages and fail later, so compare sizes.
    const pageCount = db.get<{ page_count: number }>('PRAGMA page_count')?.page_count ?? 0;
    const pageSize = db.get<{ page_size: number }>('PRAGMA page_size')?.page_size ?? 0;
    if (pageCount === 0 || stats.size < pageCount * pageSize) {
      return fail('invalid', 'file is smaller than its header says');
    }

    const tables = new Set(
      db
        .all<{ name: string }>("SELECT name FROM sqlite_master WHERE type IN ('table', 'view')")
        .map((row) => row.name),
    );
    const missingTable = REQUIRED_TABLES.find((table) => !tables.has(table));
    if (missingTable) return fail('invalid', `missing table ${missingTable}`);

    const meta = new Map(
      db.all<{ key: string; value: string }>('SELECT key, value FROM meta').map((row) => [
        row.key,
        row.value,
      ]),
    );
    const missingKey = REQUIRED_META_KEYS.find((key) => !meta.has(key));
    if (missingKey) return fail('invalid', `missing meta key ${missingKey}`);

    const schemaVersion = meta.get('schema_version') as string;
    if (schemaVersion !== SUPPORTED_SCHEMA_VERSION) {
      return fail('unsupported_schema', `schema_version ${schemaVersion}`);
    }

    // Touch every table the pages read, so damage shows now rather than mid-render.
    for (const table of REQUIRED_TABLES) {
      db.get(`SELECT 1 FROM ${table} LIMIT 1`);
    }

    const memos = new Map<string, unknown>();
    const generation: Generation = {
      db,
      id: signatureOf(stats),
      meta: {
        schemaVersion,
        generatedAt: meta.get('generated_at') as string,
        embeddingModel: meta.get('embedding_model') as string,
        embeddingDimensions: Number(meta.get('embedding_dimensions')),
      },
      memo<T>(key: string, compute: () => T): T {
        if (memos.has(key)) return memos.get(key) as T;
        const value = compute();
        memos.set(key, value);
        return value;
      },
    };
    return { status: 'ready', generation };
  } catch (error) {
    try {
      db?.close();
    } catch {
      // Already failing; the original error is the useful one.
    }
    return { status: 'invalid', detail: error instanceof Error ? error.message : String(error) };
  }
}

export class DatasetManager {
  private readonly filePath: string;
  private readonly log: DatasetLogger;
  private generation: Generation | undefined;
  private lastStatus: DatasetStatus | undefined;
  private failedSignature: string | undefined;
  private failedStatus: UnavailableStatus = 'invalid';
  private failedAt = 0;

  constructor(filePath: string, log: DatasetLogger = console) {
    this.filePath = filePath;
    this.log = log;
  }

  /** The current state, after noticing any replacement or removal of the file. */
  state(): DatasetState {
    let stats: Stats;
    try {
      stats = statSync(this.filePath);
    } catch {
      this.drop();
      this.note('missing', `public.db not found at ${this.filePath}`);
      return { status: 'missing' };
    }
    if (!stats.isFile()) {
      this.drop();
      this.note('missing', `public.db path is not a file: ${this.filePath}`);
      return { status: 'missing' };
    }

    const signature = signatureOf(stats);
    if (this.generation?.id === signature) return { status: 'ready', generation: this.generation };

    this.drop();

    const recheckDue = Date.now() - this.failedAt >= INVALID_RECHECK_MS;
    if (this.failedSignature === signature && !recheckDue) return { status: this.failedStatus };

    const opened = openGeneration(this.filePath, stats);
    if (opened.status === 'ready') {
      this.generation = opened.generation;
      this.failedSignature = undefined;
      this.note(
        'ready',
        `public.db ready, generated_at=${opened.generation.meta.generatedAt}, ` +
          `embedding_model=${opened.generation.meta.embeddingModel || '(none)'}`,
        true,
      );
      return { status: 'ready', generation: opened.generation };
    }
    this.failedSignature = signature;
    this.failedStatus = opened.status;
    this.failedAt = Date.now();
    this.note(opened.status, `public.db at ${this.filePath} not served: ${opened.detail}`);
    return { status: opened.status };
  }

  /**
   * Runs one synchronous read against one generation, so a page never mixes two
   * versions of the file. An error from SQLite drops the generation and retries
   * once from a fresh stat, which covers a file overwritten in place.
   */
  withSnapshot<T>(read: (generation: Generation) => T): T {
    for (let attempt = 0; ; attempt += 1) {
      const state = this.state();
      if (state.status !== 'ready') throw new DatasetUnavailableError(state.status);
      try {
        return read(state.generation);
      } catch (error) {
        if (!isSqliteError(error)) throw error;
        this.drop();
        if (attempt > 0) {
          this.failedSignature = state.generation.id;
          this.failedStatus = 'invalid';
          this.failedAt = Date.now();
          this.note('invalid', `public.db at ${this.filePath} failed a read: ${(error as Error).message}`);
          throw new DatasetUnavailableError('invalid');
        }
      }
    }
  }

  close(): void {
    this.drop();
  }

  private drop(): void {
    if (!this.generation) return;
    const old = this.generation;
    this.generation = undefined;
    old.db.close();
  }

  private note(status: DatasetStatus, message: string, always = false): void {
    if (this.lastStatus === status && !always) return;
    this.lastStatus = status;
    if (status === 'ready') this.log.info(`[library] ${message}`);
    else this.log.warn(`[library] ${message}`);
  }
}

// Next.js may evaluate a module once per route bundle, so the instance lives on
// globalThis to keep one open handle per file in the process.
const REGISTRY = Symbol.for('vidyom.library.datasets');
type RegistryHost = { [REGISTRY]?: Map<string, DatasetManager> };

export function getDataset(filePath: string = readConfig().publicDbPath): DatasetManager {
  const host = globalThis as RegistryHost;
  host[REGISTRY] ??= new Map();
  let manager = host[REGISTRY].get(filePath);
  if (!manager) {
    manager = new DatasetManager(filePath);
    host[REGISTRY].set(filePath, manager);
  }
  return manager;
}
