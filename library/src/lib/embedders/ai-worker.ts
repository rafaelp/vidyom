import net from 'node:net';
import type { QueryEmbedder } from './types';

/**
 * Optional client of the Python AI worker's JSON-RPC 2.0 protocol
 * (schemas/ai-worker-rpc.v1.json): one JSON object per line, `initialize` first.
 *
 * The worker holds the model, so nothing large lives in the site. It is not the
 * daemon: it has no database, Redis, storage or secrets (I6), and the site sends it
 * only the query text. This client has been tested against a local stand-in and,
 * where noted in the README, the worker's fake engine; real BGE-M3 through this path
 * is an open validation (D58, docs/decisions/library.md, I249).
 */

const PROTOCOL_VERSION = 1;
const RECONNECT_BACKOFF_MS = 5_000;
const MAX_IN_FLIGHT = 4;

export interface AiWorkerOptions {
  socket?: string;
  tcp?: string;
  token?: string;
  timeoutMs: number;
  minSimilarity: number;
}

export interface AiWorkerInfo {
  engine: string;
  model: string;
  dimensions: number;
}

interface Pending {
  resolve(result: unknown): void;
  reject(error: Error): void;
  timer: NodeJS.Timeout;
}

/** `host:port` on a loopback address, or null: the protocol is loopback only (I6). */
export function parseLoopbackAddress(value: string): { host: string; port: number } | null {
  const match = /^(.*):(\d+)$/.exec(value.trim());
  if (!match) return null;
  const host = (match[1] ?? '').replace(/^\[|\]$/g, '');
  const port = Number(match[2]);
  if (host !== '127.0.0.1' && host !== '::1' && host !== 'localhost') return null;
  if (!Number.isInteger(port) || port <= 0 || port > 65535) return null;
  return { host, port };
}

export class AiWorkerClient {
  private readonly options: AiWorkerOptions;
  private socket: net.Socket | undefined;
  private session: Promise<AiWorkerInfo> | undefined;
  private info: AiWorkerInfo | undefined;
  private readonly pending = new Map<number, Pending>();
  private nextId = 1;
  private buffer = '';
  private failedAt = 0;
  private inFlight = 0;

  constructor(options: AiWorkerOptions) {
    this.options = options;
  }

  /** Connects and initializes if needed, and reports what the worker serves. */
  connect(): Promise<AiWorkerInfo> {
    if (this.session) return this.session;
    if (Date.now() - this.failedAt < RECONNECT_BACKOFF_MS) {
      return Promise.reject(new Error('AI worker unavailable; waiting before reconnecting'));
    }
    this.session = this.open().catch((error: unknown) => {
      this.failedAt = Date.now();
      this.reset(error instanceof Error ? error : new Error(String(error)));
      throw error;
    });
    return this.session;
  }

  /** An embedder bound to what the worker reported. */
  asEmbedder(info: AiWorkerInfo): QueryEmbedder {
    return {
      model: info.model,
      dimensions: info.dimensions,
      minSimilarity: this.options.minSimilarity,
      testOnly: info.engine === 'fake',
      embed: (text, signal) => this.embed(text, signal),
    };
  }

  async embed(text: string, signal?: AbortSignal): Promise<Float32Array> {
    if (this.inFlight >= MAX_IN_FLIGHT) throw new Error('AI worker busy');
    this.inFlight += 1;
    try {
      await this.connect();
      const result = (await this.call('embed', { texts: [text], kind: 'query' }, signal)) as {
        vectors?: number[][];
      };
      const vector = result.vectors?.[0];
      if (!Array.isArray(vector)) throw new Error('AI worker returned no vector');
      return Float32Array.from(vector);
    } finally {
      this.inFlight -= 1;
    }
  }

  close(): void {
    this.reset(new Error('AI worker client closed'));
  }

  private open(): Promise<AiWorkerInfo> {
    return new Promise<AiWorkerInfo>((resolve, reject) => {
      const socket = this.createSocket();
      this.socket = socket;
      socket.setEncoding('utf8');
      const connectTimer = setTimeout(() => {
        reject(new Error('AI worker connection timed out'));
        socket.destroy();
      }, this.options.timeoutMs);

      socket.once('connect', () => {
        clearTimeout(connectTimer);
        const params: Record<string, unknown> = {
          protocol_version: PROTOCOL_VERSION,
          client: 'vidyom-library',
        };
        if (this.options.token) params.token = this.options.token;
        this.call('initialize', params)
          .then((raw) => {
            const result = raw as {
              protocol_version?: number;
              engine?: string;
              embedding_model?: string;
              embedding_dimensions?: number;
            };
            if (
              result.protocol_version !== PROTOCOL_VERSION ||
              typeof result.embedding_model !== 'string' ||
              typeof result.embedding_dimensions !== 'number'
            ) {
              throw new Error('AI worker returned an unexpected initialize result');
            }
            this.info = {
              engine: result.engine ?? '',
              model: result.embedding_model,
              dimensions: result.embedding_dimensions,
            };
            resolve(this.info);
          })
          .catch(reject);
      });

      socket.on('data', (chunk: string) => this.onData(chunk));
      socket.on('error', (error) => {
        clearTimeout(connectTimer);
        reject(error);
        this.reset(error);
      });
      socket.on('close', () => {
        clearTimeout(connectTimer);
        reject(new Error('AI worker closed the connection'));
        this.reset(new Error('AI worker closed the connection'));
      });
    });
  }

  private createSocket(): net.Socket {
    if (this.options.socket) return net.createConnection({ path: this.options.socket });
    const address = parseLoopbackAddress(this.options.tcp ?? '');
    if (!address) throw new Error('AI worker TCP address must be on a loopback interface');
    return net.createConnection(address);
  }

  private call(method: string, params: unknown, signal?: AbortSignal): Promise<unknown> {
    const socket = this.socket;
    if (!socket || socket.destroyed) return Promise.reject(new Error('AI worker not connected'));
    const id = this.nextId;
    this.nextId += 1;
    return new Promise((resolve, reject) => {
      const settle = () => {
        clearTimeout(timer);
        signal?.removeEventListener('abort', onAbort);
        this.pending.delete(id);
      };
      const onAbort = () => {
        settle();
        reject(new Error('AI worker call aborted'));
      };
      const timer = setTimeout(() => {
        settle();
        reject(new Error(`AI worker call ${method} timed out`));
      }, this.options.timeoutMs);
      signal?.addEventListener('abort', onAbort, { once: true });
      this.pending.set(id, {
        timer,
        resolve: (result) => {
          settle();
          resolve(result);
        },
        reject: (error) => {
          settle();
          reject(error);
        },
      });
      socket.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
    });
  }

  private onData(chunk: string): void {
    this.buffer += chunk;
    for (;;) {
      const end = this.buffer.indexOf('\n');
      if (end < 0) return;
      const line = this.buffer.slice(0, end).trim();
      this.buffer = this.buffer.slice(end + 1);
      if (line) this.onLine(line);
    }
  }

  private onLine(line: string): void {
    let message: { id?: number; result?: unknown; error?: { message?: string } };
    try {
      message = JSON.parse(line);
    } catch {
      this.reset(new Error('AI worker sent invalid JSON'));
      return;
    }
    // Notifications such as $/progress carry no id and are not for this client.
    if (typeof message.id !== 'number') return;
    const pending = this.pending.get(message.id);
    if (!pending) return;
    if (message.error) pending.reject(new Error(message.error.message ?? 'AI worker error'));
    else pending.resolve(message.result);
  }

  private reset(error: Error): void {
    const socket = this.socket;
    this.socket = undefined;
    this.session = undefined;
    this.info = undefined;
    this.buffer = '';
    for (const pending of [...this.pending.values()]) pending.reject(error);
    if (socket && !socket.destroyed) socket.destroy();
  }
}
