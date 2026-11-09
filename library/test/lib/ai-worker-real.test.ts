import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { AiWorkerClient } from '@/lib/embedders/ai-worker';
import { FAKE_EMBEDDING_MODEL, fakeHashEmbed } from '@/lib/embedders/fake-hash';
import { tempDir } from '../helpers';

/**
 * The client against the real Python AI worker with its fake engine, when this machine
 * has both. It proves the protocol and that TypeScript and Python agree on the fake
 * vectors. Real BGE-M3 through this path stays an open validation (D58).
 */
const WORKER_DIR = path.resolve(import.meta.dirname, '../../../aiworker');
const python = process.env.PYTHON ?? 'python3';
// Keeps these runs from writing bytecode into the worker's directory.
const env = { ...process.env, PYTHONDONTWRITEBYTECODE: '1' };
const available =
  fs.existsSync(path.join(WORKER_DIR, 'vidyom', 'cli.py')) &&
  spawnSync(python, ['-m', 'vidyom', '--version'], { cwd: WORKER_DIR, env }).status === 0;

describe.skipIf(!available)('the real AI worker, fake engine', () => {
  let worker: ChildProcess | undefined;
  let socket = '';
  let client: AiWorkerClient;

  beforeAll(async () => {
    socket = path.join(tempDir(), 'ai.sock');
    worker = spawn(python, ['-m', 'vidyom', '--engine', 'fake', '--socket', socket], {
      cwd: WORKER_DIR,
      env,
      stdio: 'ignore',
    });
    const deadline = Date.now() + 10_000;
    while (!fs.existsSync(socket)) {
      if (Date.now() > deadline) throw new Error('AI worker did not start');
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    client = new AiWorkerClient({ socket, timeoutMs: 5000, minSimilarity: 0.3 });
  });

  afterAll(() => {
    client?.close();
    worker?.kill('SIGTERM');
  });

  it('initializes, reports its model and is marked test only', async () => {
    const info = await client.connect();
    expect(info).toEqual({ engine: 'fake', model: FAKE_EMBEDDING_MODEL, dimensions: 1024 });
    expect(client.asEmbedder(info).testOnly).toBe(true);
  });

  it.each(['Conscious breathing', 'Respiração consciente, breathing breathing!', 'İstanbul ΣΑΣ', '½ ٣ 呼吸 Ⅷ'])(
    'embeds %s exactly as this site does',
    async (text) => {
      const fromWorker = await client.embed(text);
      const here = fakeHashEmbed(text);
      expect(fromWorker).toHaveLength(here.length);
      for (let i = 0; i < here.length; i += 1) {
        expect(fromWorker[i]).toBeCloseTo(here[i] as number, 5);
      }
    },
  );
});
