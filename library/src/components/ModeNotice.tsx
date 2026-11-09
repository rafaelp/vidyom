import type { SemanticInfo, SemanticReason } from '@/lib/search/types';

const REASONS: Record<SemanticReason, (model: string | null) => string> = {
  disabled: () => 'Semantic search is turned off on this site.',
  not_requested: () => 'Semantic search was not requested.',
  no_vectors: () => 'This catalog has no semantic index.',
  no_embedder: (model) =>
    model
      ? `This site has no query embedder for the catalog’s model, ${model}.`
      : 'This site has no query embedder for the catalog’s model.',
  dimension_mismatch: () => 'The query embedder does not match the catalog’s semantic index.',
  too_many_vectors: () => 'The semantic index is larger than this site is set up to scan.',
  invalid_vectors: () => 'The catalog’s semantic index could not be read.',
  embedder_error: () => 'The query embedder did not answer, so this result uses word matching only.',
};

/** Says which search is running and, when it is not hybrid, why (I250). */
export function ModeNotice({ mode, semantic }: { mode: 'lexical' | 'hybrid'; semantic: SemanticInfo }) {
  if (mode === 'hybrid') {
    return (
      <p className="mode-notice">
        <span className="mode-pill">Hybrid search</span>
        {semantic.test_only ? (
          <span>
            Word matching plus a test embedder ({semantic.model}), which only finds shared words. Its results say
            nothing about real semantic quality.
          </span>
        ) : (
          <span>Word matching plus semantic matching ({semantic.model}).</span>
        )}
      </p>
    );
  }
  const reason = semantic.reason ? REASONS[semantic.reason](semantic.model) : '';
  return (
    <p className="mode-notice">
      <span className="mode-pill">Word search</span>
      <span>Matching the words you typed.{reason ? ` ${reason}` : ''}</span>
    </p>
  );
}
