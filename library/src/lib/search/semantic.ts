import { endianness } from 'node:os';
import type { Generation } from '../dataset';
import type { SemanticReason } from './types';

/**
 * Brute-force cosine over the file's vectors (I29). All vectors load once per
 * generation into one Float32Array, L2-normalized, so a query is a dot product over
 * that array. Text stays in the database until a row is a hit (I248).
 */

export const SEMANTIC_TOP_K = 200;

export interface VectorIndex {
  size: number;
  dimensions: number;
  publicIds: string[];
  positions: Int32Array;
  matrix: Float32Array;
}

export type VectorIndexResult =
  | { ok: true; index: VectorIndex }
  | { ok: false; reason: Extract<SemanticReason, 'no_vectors' | 'too_many_vectors' | 'invalid_vectors'> };

function copyVector(blob: Uint8Array, matrix: Float32Array, offset: number, dimensions: number): void {
  if (endianness() === 'LE') {
    new Uint8Array(matrix.buffer, offset * dimensions * 4, dimensions * 4).set(blob);
    return;
  }
  const view = new DataView(blob.buffer, blob.byteOffset, blob.byteLength);
  for (let i = 0; i < dimensions; i += 1) {
    matrix[offset * dimensions + i] = view.getFloat32(i * 4, true);
  }
}

function normalize(matrix: Float32Array, offset: number, dimensions: number): void {
  const start = offset * dimensions;
  let norm = 0;
  for (let i = 0; i < dimensions; i += 1) {
    const value = matrix[start + i] as number;
    norm += value * value;
  }
  if (norm === 0) return;
  norm = Math.sqrt(norm);
  for (let i = 0; i < dimensions; i += 1) {
    matrix[start + i] = (matrix[start + i] as number) / norm;
  }
}

function load(generation: Generation, dimensions: number, maxVectors: number): VectorIndexResult {
  const size =
    generation.db.get<{ total: number }>('SELECT count(*) AS total FROM embeddings')?.total ?? 0;
  if (size === 0) return { ok: false, reason: 'no_vectors' };
  if (size > maxVectors) return { ok: false, reason: 'too_many_vectors' };
  if (!Number.isInteger(dimensions) || dimensions <= 0) {
    return { ok: false, reason: 'invalid_vectors' };
  }

  const matrix = new Float32Array(size * dimensions);
  const publicIds: string[] = new Array(size);
  const positions = new Int32Array(size);
  let row = 0;
  for (const entry of generation.db.iterate<{
    public_id: string;
    position: number;
    vector: Uint8Array;
  }>('SELECT public_id, position, vector FROM embeddings')) {
    if (!(entry.vector instanceof Uint8Array) || entry.vector.byteLength !== dimensions * 4) {
      return { ok: false, reason: 'invalid_vectors' };
    }
    copyVector(entry.vector, matrix, row, dimensions);
    normalize(matrix, row, dimensions);
    publicIds[row] = entry.public_id;
    positions[row] = entry.position;
    row += 1;
  }
  return { ok: true, index: { size, dimensions, publicIds, positions, matrix } };
}

/** The generation's vector index, built on first use and dropped with it. */
export function vectorIndexFor(
  generation: Generation,
  dimensions: number,
  maxVectors: number,
): VectorIndexResult {
  return generation.memo(`vectors:${dimensions}:${maxVectors}`, () =>
    load(generation, dimensions, maxVectors),
  );
}

export interface ScanHit {
  publicId: string;
  position: number;
  similarity: number;
}

export function normalizedCopy(vector: Float32Array): Float32Array {
  const copy = Float32Array.from(vector);
  normalize(copy, 0, copy.length);
  return copy;
}

/** Top K rows by cosine similarity, best first. Ties keep the file's order. */
export function scanVectors(
  index: VectorIndex,
  query: Float32Array,
  minSimilarity: number,
  allowed?: ReadonlySet<string>,
  topK: number = SEMANTIC_TOP_K,
): ScanHit[] {
  const unit = normalizedCopy(query);
  const { matrix, dimensions, size } = index;
  const best: { row: number; similarity: number }[] = [];
  for (let row = 0; row < size; row += 1) {
    if (allowed && !allowed.has(index.publicIds[row] as string)) continue;
    const base = row * dimensions;
    let similarity = 0;
    for (let i = 0; i < dimensions; i += 1) {
      similarity += (matrix[base + i] as number) * (unit[i] as number);
    }
    if (similarity < minSimilarity) continue;
    if (best.length === topK && similarity <= (best[best.length - 1]?.similarity ?? 0)) continue;
    let slot = best.length;
    while (slot > 0 && (best[slot - 1]?.similarity ?? 0) < similarity) slot -= 1;
    best.splice(slot, 0, { row, similarity });
    if (best.length > topK) best.pop();
  }
  return best.map(({ row, similarity }) => ({
    publicId: index.publicIds[row] as string,
    position: index.positions[row] as number,
    similarity,
  }));
}
