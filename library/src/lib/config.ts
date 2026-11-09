/**
 * Site configuration, read from the environment at request time so one build can
 * serve any tenant's file (docs/decisions/library.md, I241).
 */

export interface AiWorkerConfig {
  socket: string;
  tcp: string;
  token: string;
}

export interface LibraryConfig {
  publicDbPath: string;
  title: string;
  description: string;
  /** Absolute origin without a trailing slash, or '' when not configured (I256). */
  siteUrl: string;
  semanticEnabled: boolean;
  maxVectors: number;
  aiWorker: AiWorkerConfig;
  embedTimeoutMs: number;
  minSimilarity: number;
}

export const DEFAULT_PUBLIC_DB_PATH = 'data/public.db';
export const DEFAULT_TITLE = 'Video library';
const DEFAULT_MAX_VECTORS = 50_000;
const DEFAULT_EMBED_TIMEOUT_MS = 5_000;
const DEFAULT_MIN_SIMILARITY = 0.3;

function positiveInt(value: string | undefined, fallback: number): number {
  if (value === undefined || value.trim() === '') return fallback;
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}

function similarity(value: string | undefined, fallback: number): number {
  if (value === undefined || value.trim() === '') return fallback;
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed >= -1 && parsed <= 1 ? parsed : fallback;
}

/** Accepts only an absolute http(s) origin; anything else means "not configured". */
export function normalizeSiteUrl(value: string | undefined): string {
  if (!value) return '';
  try {
    const url = new URL(value.trim());
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return '';
    return `${url.origin}${url.pathname.replace(/\/+$/, '')}`;
  } catch {
    return '';
  }
}

export function readConfig(env: Record<string, string | undefined> = process.env): LibraryConfig {
  return {
    publicDbPath: env.VIDYOM_PUBLIC_DB?.trim() || DEFAULT_PUBLIC_DB_PATH,
    title: env.VIDYOM_LIBRARY_TITLE?.trim() || DEFAULT_TITLE,
    description: env.VIDYOM_LIBRARY_DESCRIPTION?.trim() || '',
    siteUrl: normalizeSiteUrl(env.VIDYOM_LIBRARY_SITE_URL),
    semanticEnabled: (env.VIDYOM_LIBRARY_SEMANTIC ?? 'on').trim().toLowerCase() !== 'off',
    maxVectors: positiveInt(env.VIDYOM_LIBRARY_MAX_VECTORS, DEFAULT_MAX_VECTORS),
    aiWorker: {
      socket: env.VIDYOM_LIBRARY_AI_WORKER_SOCKET?.trim() || '',
      tcp: env.VIDYOM_LIBRARY_AI_WORKER_TCP?.trim() || '',
      token: env.VIDYOM_LIBRARY_AI_WORKER_TOKEN ?? '',
    },
    embedTimeoutMs: positiveInt(env.VIDYOM_LIBRARY_EMBED_TIMEOUT_MS, DEFAULT_EMBED_TIMEOUT_MS),
    minSimilarity: similarity(env.VIDYOM_LIBRARY_MIN_SIMILARITY, DEFAULT_MIN_SIMILARITY),
  };
}
