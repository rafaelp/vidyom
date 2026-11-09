/**
 * A query embedder turns one search query into one vector for the file's
 * `embedding_model` (docs/decisions/library.md, I249). It is optional: with none
 * available, search is lexical and says so. Vectors from different models are never
 * compared (R18), so an embedder is used only when its model equals the file's.
 */
export interface QueryEmbedder {
  /** `<name>@<version>`, compared with `meta.embedding_model`. */
  readonly model: string;
  readonly dimensions: number;
  /** Semantic hits below this cosine similarity are dropped. */
  readonly minSimilarity: number;
  /** True for embedders that prove the path but carry no semantic quality (I27). */
  readonly testOnly: boolean;
  embed(text: string, signal?: AbortSignal): Promise<Float32Array>;
}

export type EmbedderResolution =
  | { ok: true; embedder: QueryEmbedder }
  | { ok: false; reason: 'no_embedder' | 'dimension_mismatch' | 'embedder_error' };
