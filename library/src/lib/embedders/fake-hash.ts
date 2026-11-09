import type { QueryEmbedder } from './types';

/**
 * `fake-hash-embed@1` (I27), the TypeScript copy of the algorithm defined in
 * schemas/ai-worker-rpc.v1.json#/$defs/fakeHashEmbed, whose test vectors are checked
 * in test/lib/embedders.test.ts. Lowercased Unicode word tokens are feature-hashed
 * with 32-bit FNV-1a; bit 31 gives the sign; the result is L2 normalized. It only
 * finds shared words: it proves the search path, never semantic quality, which is why
 * the UI and the API mark it as test only.
 */
export const FAKE_EMBEDDING_MODEL = 'fake-hash-embed@1';
export const FAKE_EMBEDDING_DIMENSIONS = 1024;

const FNV_OFFSET_BASIS = 2166136261;
const FNV_PRIME = 16777619;
const SIGN_BIT = 0x80000000;

const encoder = new TextEncoder();

function fnv1a32(text: string): number {
  let hash = FNV_OFFSET_BASIS;
  for (const byte of encoder.encode(text)) {
    hash ^= byte;
    hash = Math.imul(hash, FNV_PRIME) >>> 0;
  }
  return hash >>> 0;
}

/**
 * One code point at a time, keeping the first of a multi-character result, as Go's
 * strings.ToLower does. String.prototype.toLowerCase on the whole text differs: it
 * turns U+0130 into two code points and applies the final-sigma rule.
 */
function lowerLikeGo(text: string): string {
  let out = '';
  for (const character of text) {
    const lowered = character.toLowerCase();
    out += String.fromCodePoint(lowered.codePointAt(0) as number);
  }
  return out;
}

export function fakeHashEmbed(text: string): Float32Array {
  const sums = new Float64Array(FAKE_EMBEDDING_DIMENSIONS);
  for (const match of lowerLikeGo(text).matchAll(/[\p{L}\p{N}]+/gu)) {
    const hash = fnv1a32(match[0]);
    const bucket = hash % FAKE_EMBEDDING_DIMENSIONS;
    sums[bucket] = (sums[bucket] ?? 0) + ((hash & SIGN_BIT) !== 0 ? -1 : 1);
  }
  let norm = 0;
  for (const value of sums) norm += value * value;
  const out = new Float32Array(FAKE_EMBEDDING_DIMENSIONS);
  if (norm === 0) return out;
  norm = Math.sqrt(norm);
  for (let i = 0; i < out.length; i += 1) out[i] = (sums[i] ?? 0) / norm;
  return out;
}

export const fakeHashEmbedder: QueryEmbedder = {
  model: FAKE_EMBEDDING_MODEL,
  dimensions: FAKE_EMBEDDING_DIMENSIONS,
  // Any shared word gives a positive similarity; zero means nothing in common.
  minSimilarity: 0.01,
  testOnly: true,
  embed: async (text) => fakeHashEmbed(text),
};
