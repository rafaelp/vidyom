import { getDataset } from '@/lib/dataset';
import { countVideos } from '@/lib/queries';

export const dynamic = 'force-dynamic';

/**
 * For Compose healthchecks and the E2E wait loop. It reports only what the public file
 * already says, and never the file's path. `503` while the dataset is not ready.
 */
export async function GET(): Promise<Response> {
  const dataset = getDataset();
  const state = dataset.state();
  const headers = { 'Cache-Control': 'no-store' };
  if (state.status !== 'ready') {
    return Response.json(
      { status: 'unavailable', dataset: { status: state.status } },
      { status: 503, headers },
    );
  }
  const { meta } = state.generation;
  return Response.json(
    {
      status: 'ok',
      dataset: {
        status: 'ready',
        schema_version: meta.schemaVersion,
        generated_at: meta.generatedAt,
        embedding_model: meta.embeddingModel || null,
        videos: dataset.withSnapshot(countVideos),
      },
    },
    { headers },
  );
}
