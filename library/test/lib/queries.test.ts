import { beforeAll, describe, expect, it } from 'vitest';
import type { DatasetManager, Generation } from '@/lib/dataset';
import {
  allVideoEntries,
  getFacets,
  getRelated,
  getTranscript,
  getVideo,
  listVideos,
} from '@/lib/queries';
import { CATALOG, fillerVideos, type FixtureVideo } from '../../scripts/generate-fixture';
import { fixtureDataset } from '../helpers';

let dataset: DatasetManager;

function read<T>(fn: (generation: Generation) => T): T {
  return dataset.withSnapshot(fn);
}

beforeAll(() => {
  ({ dataset } = fixtureDataset({ videos: [...CATALOG, ...fillerVideos(30)] }));
});

const CONSCIOUS = 'k7q2m4x9a1b3c5d7';

describe('listVideos', () => {
  it('pages newest first with a stable total', () => {
    const first = read((g) => listVideos(g, {}, 1));
    expect(first.total).toBe(CATALOG.length + 30);
    expect(first.items).toHaveLength(24);
    expect(first.items[0]?.publicId).toBe(CONSCIOUS);
    const dates = first.items.map((video) => video.publishedAt);
    expect([...dates].sort().reverse()).toEqual(dates);

    const second = read((g) => listVideos(g, {}, 2));
    expect(second.items).toHaveLength(12);
    const ids = new Set([...first.items, ...second.items].map((video) => video.publicId));
    expect(ids.size).toBe(36);

    expect(read((g) => listVideos(g, {}, 3)).items).toEqual([]);
  });

  it('filters by kind, category, language, speaker and tag', () => {
    expect(read((g) => listVideos(g, { kind: 'original' }, 1)).items.map((v) => v.publicId)).toEqual([
      'h1y5c8z3n7q2d6w4',
    ]);
    expect(read((g) => listVideos(g, { category: 'Breathwork' }, 1)).total).toBe(2);
    expect(read((g) => listVideos(g, { language: 'pt-BR' }, 1)).total).toBe(1);
    expect(read((g) => listVideos(g, { speaker: 'Ana Exemplo' }, 1)).total).toBe(3);
    expect(read((g) => listVideos(g, { tag: 'hatha' }, 1)).total).toBe(3);
    expect(read((g) => listVideos(g, { tag: 'hatha', speaker: 'Bruno Amostra' }, 1)).total).toBe(0);
  });
});

describe('getVideo', () => {
  it('returns the video with destinations and chapters in order', () => {
    const video = read((g) => getVideo(g, CONSCIOUS));
    expect(video?.title).toBe('Conscious breathing for beginners');
    expect(video?.tags).toEqual(['breathing', 'beginners', 'hatha']);
    expect(video?.destinations).toEqual([
      {
        driver: 'youtube',
        url: `https://media.example.internal/watch/${CONSCIOUS}`,
        embedUrl: `https://media.example.internal/embed/${CONSCIOUS}`,
        supportsTimestamps: true,
      },
    ]);
    expect(video?.chapters.map((chapter) => chapter.title)).toEqual([
      'Finding a comfortable seat',
      'Counting the breath',
      'Returning to stillness',
    ]);
    expect(video?.transcriptCount).toBe(5);
  });

  it('returns null for an unknown or withdrawn ID', () => {
    expect(read((g) => getVideo(g, 'nope'))).toBeNull();
  });

  it('treats an ID as data, not SQL', () => {
    expect(read((g) => getVideo(g, "' OR '1'='1"))).toBeNull();
  });
});

describe('getTranscript', () => {
  it('returns segments in order, with an optional limit', () => {
    expect(read((g) => getTranscript(g, CONSCIOUS)).map((s) => s.position)).toEqual([0, 1, 2, 3, 4]);
    expect(read((g) => getTranscript(g, CONSCIOUS, 2))).toHaveLength(2);
  });
});

describe('getRelated', () => {
  it('relates videos by source group, excluding the video itself', () => {
    const video = read((g) => getVideo(g, CONSCIOUS));
    const related = read((g) => getRelated(g, video!));
    expect(related.map((v) => v.publicId).sort()).toEqual(['h1y5c8z3n7q2d6w4', 'p3n8w6t2c4v9h1j5']);
  });

  it('relates nothing when the group is empty', () => {
    const video = read((g) => getVideo(g, 'm6t2v9k4j8s3f1g7'));
    expect(video?.sourceGroup).toBe('');
    expect(read((g) => getRelated(g, video!))).toEqual([]);
  });
});

describe('getFacets', () => {
  it('lists the distinct values present in the file', () => {
    const facets = read(getFacets);
    expect(facets.kinds).toEqual(['clip', 'edited', 'original']);
    expect(facets.speakers).toContain('Ana Exemplo');
    expect(facets.languages).toEqual(['en', 'pt-BR']);
    expect(facets.tags).toContain('respiração');
  });
});

describe('values the schema allows but the site must survive', () => {
  const odd: FixtureVideo = {
    public_id: 'odd',
    title: 'Odd row',
    kind: 'clip',
    published_at: '2026-01-01T00:00:00Z',
    tags: 'not json',
    thumbnail_url: 'http://media.example.internal/plain-http.jpg',
    destinations: [
      { destination: 'mock', url: 'javascript:alert(1)' },
      {
        destination: 'youtube',
        url: 'https://media.example.internal/watch/odd',
        embed_url: 'http://media.example.internal/embed/odd',
        supports_timestamps: true,
      },
    ],
  };

  it('drops malformed tags, unsafe URLs and non-https embeds and thumbnails', () => {
    const { dataset: local } = fixtureDataset({ videos: [odd] });
    const video = local.withSnapshot((g) => getVideo(g, 'odd'));
    expect(video?.tags).toEqual([]);
    expect(video?.thumbnailUrl).toBe('');
    expect(video?.destinations).toEqual([
      {
        driver: 'youtube',
        url: 'https://media.example.internal/watch/odd',
        embedUrl: '',
        supportsTimestamps: true,
      },
    ]);
    // A malformed tags value must not fail a tag filter for the whole file.
    expect(local.withSnapshot((g) => listVideos(g, { tag: 'x' }, 1)).total).toBe(0);
    expect(local.withSnapshot(allVideoEntries)).toEqual([
      { publicId: 'odd', publishedAt: '2026-01-01T00:00:00Z' },
    ]);
    local.close();
  });
});
