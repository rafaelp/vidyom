import type { LibraryConfig } from '../config';
import { AiWorkerClient } from './ai-worker';
import { FAKE_EMBEDDING_DIMENSIONS, FAKE_EMBEDDING_MODEL, fakeHashEmbedder } from './fake-hash';
import type { EmbedderResolution } from './types';

/**
 * Picks the query embedder for the file's `embedding_model`, or says why there is
 * none. There is no fallback between models (R18, I27): `fake-hash-embed@1` is chosen
 * only when the file itself says so, and never stands in for real vectors.
 */

const CLIENT = Symbol.for('vidyom.library.aiWorkerClient');
type ClientHost = { [CLIENT]?: { key: string; client: AiWorkerClient } };

function aiWorkerClient(config: LibraryConfig): AiWorkerClient | undefined {
  const { socket, tcp, token } = config.aiWorker;
  if (!socket && !tcp) return undefined;
  const key = JSON.stringify([socket, tcp, token, config.embedTimeoutMs, config.minSimilarity]);
  const host = globalThis as ClientHost;
  if (host[CLIENT]?.key !== key) {
    host[CLIENT]?.client.close();
    host[CLIENT] = {
      key,
      client: new AiWorkerClient({
        socket,
        tcp,
        token,
        timeoutMs: config.embedTimeoutMs,
        minSimilarity: config.minSimilarity,
      }),
    };
  }
  return host[CLIENT].client;
}

export async function resolveEmbedder(
  model: string,
  dimensions: number,
  config: LibraryConfig,
): Promise<EmbedderResolution> {
  if (model === FAKE_EMBEDDING_MODEL) {
    return dimensions === FAKE_EMBEDDING_DIMENSIONS
      ? { ok: true, embedder: fakeHashEmbedder }
      : { ok: false, reason: 'dimension_mismatch' };
  }
  const client = aiWorkerClient(config);
  if (!client) return { ok: false, reason: 'no_embedder' };
  try {
    const info = await client.connect();
    if (info.model !== model) return { ok: false, reason: 'no_embedder' };
    if (info.dimensions !== dimensions) return { ok: false, reason: 'dimension_mismatch' };
    return { ok: true, embedder: client.asEmbedder(info) };
  } catch {
    return { ok: false, reason: 'embedder_error' };
  }
}
