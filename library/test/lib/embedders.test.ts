import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  FAKE_EMBEDDING_DIMENSIONS,
  FAKE_EMBEDDING_MODEL,
  fakeHashEmbed,
} from '@/lib/embedders/fake-hash';
import { AiWorkerClient, parseLoopbackAddress } from '@/lib/embedders/ai-worker';
import { resolveEmbedder } from '@/lib/embedders/registry';
import { tempDir, testConfig } from '../helpers';

const contract = JSON.parse(
  fs.readFileSync(path.resolve(import.meta.dirname, '../../../schemas/ai-worker-rpc.v1.json'), 'utf8'),
) as {
  $defs: {
    fakeHashEmbed: {
      dimensions: number;
      testVectors: { text: string; nonZeroBuckets: Record<string, number> }[];
    };
  };
};

describe('fake-hash-embed@1', () => {
  it('matches the dimensions in the contract', () => {
    expect(FAKE_EMBEDDING_DIMENSIONS).toBe(contract.$defs.fakeHashEmbed.dimensions);
  });

  it.each(contract.$defs.fakeHashEmbed.testVectors)(
    'matches the contract test vector for "$text"',
    ({ text, nonZeroBuckets }) => {
      const vector = fakeHashEmbed(text);
      expect(vector).toHaveLength(FAKE_EMBEDDING_DIMENSIONS);
      const nonZero: Record<string, number> = {};
      vector.forEach((value, index) => {
        if (value !== 0) nonZero[String(index)] = value;
      });
      expect(Object.keys(nonZero).sort()).toEqual(Object.keys(nonZeroBuckets).sort());
      for (const [bucket, value] of Object.entries(nonZeroBuckets)) {
        expect(nonZero[bucket]).toBeCloseTo(value, 5);
      }
    },
  );

  it('ignores case and punctuation', () => {
    expect(fakeHashEmbed('conscious, BREATHING!')).toEqual(fakeHashEmbed('Conscious breathing'));
  });

  it('leaves a text with no words as a zero vector', () => {
    expect(fakeHashEmbed('!!!').every((value) => value === 0)).toBe(true);
  });

  it('is L2 normalized', () => {
    let sum = 0;
    for (const value of fakeHashEmbed('one two three four')) sum += value * value;
    expect(sum).toBeCloseTo(1, 5);
  });
});

/** A stand-in for the Python AI worker: NDJSON JSON-RPC, per the contract. */
interface StubOptions {
  model?: string;
  dimensions?: number;
  engine?: string;
  requireToken?: string;
  ignoreEmbed?: boolean;
  dropAfterInit?: boolean;
  badInitialize?: boolean;
}

function startStub(options: StubOptions = {}) {
  const received: { method: string; params: Record<string, unknown> }[] = [];
  const sockets = new Set<net.Socket>();
  const server = net.createServer((socket) => {
    sockets.add(socket);
    socket.setEncoding('utf8');
    let buffer = '';
    let initialized = false;
    socket.on('error', () => {});
    socket.on('data', (chunk: string) => {
      buffer += chunk;
      for (;;) {
        const end = buffer.indexOf('\n');
        if (end < 0) return;
        const message = JSON.parse(buffer.slice(0, end));
        buffer = buffer.slice(end + 1);
        received.push({ method: message.method, params: message.params });
        const reply = (body: object) =>
          socket.write(`${JSON.stringify({ jsonrpc: '2.0', id: message.id, ...body })}\n`);
        if (message.method === 'initialize') {
          if (options.requireToken && message.params.token !== options.requireToken) {
            reply({ error: { code: -32002, message: 'bad token' } });
            continue;
          }
          initialized = true;
          reply({
            result: options.badInitialize
              ? { nonsense: true }
              : {
                  protocol_version: 1,
                  engine: options.engine ?? 'real',
                  transcription_model: 'none',
                  embedding_model: options.model ?? 'bge-m3@1',
                  embedding_dimensions: options.dimensions ?? 4,
                  device: 'cpu',
                  capabilities: ['embed'],
                },
          });
          if (options.dropAfterInit) socket.destroy();
          continue;
        }
        if (message.method === 'embed') {
          if (!initialized) {
            reply({ error: { code: -32600, message: 'initialize first' } });
            continue;
          }
          if (options.ignoreEmbed) continue;
          // An unrelated notification must not confuse the client.
          socket.write(`${JSON.stringify({ jsonrpc: '2.0', method: '$/progress', params: {} })}\n`);
          reply({
            result: {
              model: options.model ?? 'bge-m3@1',
              dimensions: options.dimensions ?? 4,
              vectors: [Array.from({ length: options.dimensions ?? 4 }, (_, i) => i + 1)],
            },
          });
        }
      }
    });
  });
  return {
    server,
    received,
    listenSocket(file: string) {
      return new Promise<void>((resolve) => server.listen(file, resolve));
    },
    listenTcp() {
      return new Promise<number>((resolve) => {
        server.listen(0, '127.0.0.1', () => resolve((server.address() as net.AddressInfo).port));
      });
    },
    close() {
      for (const socket of sockets) socket.destroy();
      return new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

function shortSocket(): string {
  // Unix socket paths are limited to about 100 bytes.
  return path.join(tempDir(), 'ai.sock');
}

describe('AI worker embedder', () => {
  const cleanups: (() => unknown)[] = [];
  afterEach(async () => {
    for (const cleanup of cleanups.splice(0)) await cleanup();
  });

  it('initializes and embeds a query over a Unix socket', async () => {
    const stub = startStub();
    const file = shortSocket();
    await stub.listenSocket(file);
    const client = new AiWorkerClient({ socket: file, timeoutMs: 1000, minSimilarity: 0.3 });
    cleanups.push(() => client.close(), () => stub.close());

    const info = await client.connect();
    expect(info).toEqual({ engine: 'real', model: 'bge-m3@1', dimensions: 4 });

    const vector = await client.asEmbedder(info).embed('conscious breathing');
    expect(Array.from(vector)).toEqual([1, 2, 3, 4]);
    expect(stub.received[0]).toMatchObject({
      method: 'initialize',
      params: { protocol_version: 1, client: 'vidyom-library' },
    });
    expect(stub.received[1]).toEqual({
      method: 'embed',
      params: { texts: ['conscious breathing'], kind: 'query' },
    });
  });

  it('sends the token over loopback TCP', async () => {
    const stub = startStub({ requireToken: 'fictional-token' });
    const port = await stub.listenTcp();
    const client = new AiWorkerClient({
      tcp: `127.0.0.1:${port}`,
      token: 'fictional-token',
      timeoutMs: 1000,
      minSimilarity: 0.3,
    });
    cleanups.push(() => client.close(), () => stub.close());
    await expect(client.connect()).resolves.toMatchObject({ model: 'bge-m3@1' });
  });

  it('fails and backs off when the token is refused', async () => {
    const stub = startStub({ requireToken: 'fictional-token' });
    const port = await stub.listenTcp();
    const client = new AiWorkerClient({
      tcp: `127.0.0.1:${port}`,
      token: 'wrong',
      timeoutMs: 1000,
      minSimilarity: 0.3,
    });
    cleanups.push(() => client.close(), () => stub.close());
    await expect(client.connect()).rejects.toThrow(/bad token/);
    await expect(client.connect()).rejects.toThrow(/waiting before reconnecting/);
  });

  it('rejects an unexpected initialize result', async () => {
    const stub = startStub({ badInitialize: true });
    const file = shortSocket();
    await stub.listenSocket(file);
    const client = new AiWorkerClient({ socket: file, timeoutMs: 1000, minSimilarity: 0.3 });
    cleanups.push(() => client.close(), () => stub.close());
    await expect(client.connect()).rejects.toThrow(/unexpected initialize/);
  });

  it('times out when the worker does not answer, and aborts on request', async () => {
    const stub = startStub({ ignoreEmbed: true });
    const file = shortSocket();
    await stub.listenSocket(file);
    const client = new AiWorkerClient({ socket: file, timeoutMs: 50, minSimilarity: 0.3 });
    cleanups.push(() => client.close(), () => stub.close());
    await client.connect();
    await expect(client.embed('a')).rejects.toThrow(/timed out/);
    const controller = new AbortController();
    const aborted = client.embed('b', controller.signal);
    setTimeout(() => controller.abort(), 5);
    await expect(aborted).rejects.toThrow(/aborted/);
  });

  it('fails when nothing is listening', async () => {
    const client = new AiWorkerClient({ socket: shortSocket(), timeoutMs: 200, minSimilarity: 0.3 });
    cleanups.push(() => client.close());
    await expect(client.connect()).rejects.toThrow();
  });

  it('reconnects after the connection drops and the backoff passes', async () => {
    const stub = startStub({ dropAfterInit: true });
    const file = shortSocket();
    await stub.listenSocket(file);
    const client = new AiWorkerClient({ socket: file, timeoutMs: 200, minSimilarity: 0.3 });
    cleanups.push(() => client.close(), () => stub.close());
    await client.connect();
    await new Promise((resolve) => setTimeout(resolve, 20));
    // The dropped session was forgotten, so the next call opens a new connection.
    await expect(client.connect()).resolves.toMatchObject({ model: 'bge-m3@1' });
    const initializeCalls = stub.received.filter((m) => m.method === 'initialize');
    expect(initializeCalls.length).toBeGreaterThanOrEqual(2);
  });

  it('accepts only loopback TCP addresses', () => {
    expect(parseLoopbackAddress('127.0.0.1:8123')).toEqual({ host: '127.0.0.1', port: 8123 });
    expect(parseLoopbackAddress('[::1]:8123')).toEqual({ host: '::1', port: 8123 });
    expect(parseLoopbackAddress('localhost:8123')).toEqual({ host: 'localhost', port: 8123 });
    expect(parseLoopbackAddress('media.example.internal:8123')).toBeNull();
    expect(parseLoopbackAddress('10.0.0.1:8123')).toBeNull();
    expect(parseLoopbackAddress('127.0.0.1')).toBeNull();
    expect(parseLoopbackAddress('127.0.0.1:99999')).toBeNull();
  });
});

describe('resolveEmbedder', () => {
  it('selects the fake embedder only for its own model', async () => {
    const config = testConfig();
    await expect(resolveEmbedder(FAKE_EMBEDDING_MODEL, 1024, config)).resolves.toMatchObject({
      ok: true,
      embedder: { model: FAKE_EMBEDDING_MODEL, testOnly: true },
    });
    await expect(resolveEmbedder('bge-m3@1', 1024, config)).resolves.toEqual({
      ok: false,
      reason: 'no_embedder',
    });
    await expect(resolveEmbedder(FAKE_EMBEDDING_MODEL, 8, config)).resolves.toEqual({
      ok: false,
      reason: 'dimension_mismatch',
    });
  });

  describe('with an AI worker', () => {
    const cleanups: (() => unknown)[] = [];
    afterEach(async () => {
      for (const cleanup of cleanups.splice(0)) await cleanup();
    });

    async function workerConfig(options: StubOptions) {
      const stub = startStub(options);
      const file = shortSocket();
      await stub.listenSocket(file);
      cleanups.push(() => stub.close());
      return testConfig({ aiWorker: { socket: file, tcp: '', token: '' }, embedTimeoutMs: 500 });
    }

    it('uses the worker when its model and dimensions equal the file’s', async () => {
      const config = await workerConfig({ model: 'bge-m3@1', dimensions: 1024 });
      const resolution = await resolveEmbedder('bge-m3@1', 1024, config);
      expect(resolution).toMatchObject({
        ok: true,
        embedder: { model: 'bge-m3@1', dimensions: 1024, testOnly: false, minSimilarity: 0.3 },
      });
    });

    it('never uses a worker serving another model', async () => {
      const config = await workerConfig({ model: 'other-model@9', dimensions: 1024 });
      await expect(resolveEmbedder('bge-m3@1', 1024, config)).resolves.toEqual({
        ok: false,
        reason: 'no_embedder',
      });
    });

    it('reports a dimension mismatch', async () => {
      const config = await workerConfig({ model: 'bge-m3@1', dimensions: 8 });
      await expect(resolveEmbedder('bge-m3@1', 1024, config)).resolves.toEqual({
        ok: false,
        reason: 'dimension_mismatch',
      });
    });

    it('reports an error when the worker cannot be reached', async () => {
      const config = testConfig({
        aiWorker: { socket: shortSocket(), tcp: '', token: '' },
        embedTimeoutMs: 200,
      });
      await expect(resolveEmbedder('bge-m3@1', 1024, config)).resolves.toEqual({
        ok: false,
        reason: 'embedder_error',
      });
    });
  });
});
