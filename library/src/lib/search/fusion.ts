/** Reciprocal rank fusion with k = 60, the same as the daemon's search (I43, I247). */
export const RRF_K = 60;

export function reciprocalRankFusion(
  rankings: readonly (readonly string[])[],
  k: number = RRF_K,
): Map<string, number> {
  const scores = new Map<string, number>();
  for (const ranking of rankings) {
    ranking.forEach((id, index) => {
      scores.set(id, (scores.get(id) ?? 0) + 1 / (k + index + 1));
    });
  }
  return scores;
}
