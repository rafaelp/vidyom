import { DatabaseSync, type StatementSync, type SQLInputValue } from 'node:sqlite';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

/**
 * The only place the site opens SQLite (docs/decisions/library.md, I240).
 *
 * The file is opened read-only with `immutable=1`: SQLite takes no locks and creates
 * no -wal, -shm or -journal file, so it works on a read-only mount and with a file
 * whose header says WAL. The price is that the file must not change in place; the
 * exporter replaces it by rename, and dataset.ts opens a new handle when it does.
 */
export interface ReadOnlyDatabase {
  all<T>(sql: string, ...params: SQLInputValue[]): T[];
  get<T>(sql: string, ...params: SQLInputValue[]): T | undefined;
  iterate<T>(sql: string, ...params: SQLInputValue[]): IterableIterator<T>;
  close(): void;
}

export function openReadOnly(filePath: string): ReadOnlyDatabase {
  const url = pathToFileURL(path.resolve(filePath));
  url.searchParams.set('immutable', '1');
  const db = new DatabaseSync(url, { readOnly: true });
  const statements = new Map<string, StatementSync>();

  const prepare = (sql: string): StatementSync => {
    let statement = statements.get(sql);
    if (!statement) {
      statement = db.prepare(sql);
      statements.set(sql, statement);
    }
    return statement;
  };

  return {
    all: <T>(sql: string, ...params: SQLInputValue[]) => prepare(sql).all(...params) as T[],
    get: <T>(sql: string, ...params: SQLInputValue[]) =>
      prepare(sql).get(...params) as T | undefined,
    iterate: <T>(sql: string, ...params: SQLInputValue[]) =>
      prepare(sql).iterate(...params) as unknown as IterableIterator<T>,
    close: () => {
      statements.clear();
      db.close();
    },
  };
}

/** True for errors raised by SQLite itself, as opposed to the site's own code. */
export function isSqliteError(error: unknown): boolean {
  return (
    error instanceof Error &&
    (error as NodeJS.ErrnoException).code === 'ERR_SQLITE_ERROR'
  );
}
