import { DatabaseSync } from 'node:sqlite';
import fs from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import {
  FAKE_EMBEDDING_DIMENSIONS,
  FAKE_EMBEDDING_MODEL,
  fakeHashEmbed,
} from '../src/lib/embedders/fake-hash';

/**
 * Builds a public.db fixture from the real schema (schemas/public-db/v1.sql), with
 * fictional data only. Tests call `buildFixture`; `npm run fixture` writes one for
 * local development at data/fixture/public.db.
 */

export const SCHEMA_PATH = path.resolve(__dirname, '../../schemas/public-db/v1.sql');

const EXAMPLE_HOST = 'https://media.example.internal';

export interface FixtureDestination {
  destination: string;
  url: string;
  embed_url?: string;
  supports_timestamps?: boolean;
}

export interface FixtureChapter {
  title: string;
  start: number;
  end: number;
  summary?: string;
}

export interface FixtureSegment {
  start: number;
  end: number;
  text: string;
}

export interface FixtureVideo {
  public_id: string;
  title: string;
  description?: string;
  kind: 'clip' | 'edited' | 'original';
  category?: string;
  language?: string;
  speaker?: string;
  tags?: string[] | string;
  duration_seconds?: number;
  published_at: string;
  thumbnail_url?: string;
  source_group?: string;
  destinations?: FixtureDestination[];
  chapters?: FixtureChapter[];
  transcript?: FixtureSegment[];
}

export interface FixtureOptions {
  videos?: FixtureVideo[];
  generatedAt?: string;
  schemaVersion?: string;
  embeddingModel?: string;
  embeddingDimensions?: number;
  /** Skips writing vectors, for files that declare no embedding model. */
  withVectors?: boolean;
  journalMode?: 'DELETE' | 'WAL';
}

function destinationFor(id: string, driver: string, options: Partial<FixtureDestination> = {}): FixtureDestination {
  return {
    destination: driver,
    url: `${EXAMPLE_HOST}/watch/${id}`,
    embed_url: `${EXAMPLE_HOST}/embed/${id}`,
    supports_timestamps: true,
    ...options,
  };
}

/** The curated catalog: a fictional yoga studio, matching the spec's example scenario. */
export const CATALOG: FixtureVideo[] = [
  {
    public_id: 'k7q2m4x9a1b3c5d7',
    title: 'Conscious breathing for beginners',
    description:
      'A short, gentle introduction to conscious breathing, taken from a Hatha yoga class. Sit comfortably and follow along.',
    kind: 'clip',
    category: 'Breathwork',
    language: 'en',
    speaker: 'Ana Exemplo',
    tags: ['breathing', 'beginners', 'hatha'],
    duration_seconds: 312,
    published_at: '2026-10-28T14:00:00Z',
    thumbnail_url: '',
    source_group: 'g7h3k2m9',
    destinations: [destinationFor('k7q2m4x9a1b3c5d7', 'youtube')],
    chapters: [
      { title: 'Finding a comfortable seat', start: 0, end: 74, summary: 'Settle the shoulders and let the spine lengthen.' },
      { title: 'Counting the breath', start: 74, end: 198, summary: 'A four-count inhale and a six-count exhale.' },
      { title: 'Returning to stillness', start: 198, end: 312, summary: 'Rest and notice how the body feels.' },
    ],
    transcript: [
      { start: 0, end: 22, text: 'Welcome. Find a comfortable seat and let your shoulders soften.' },
      { start: 22, end: 74, text: 'There is nothing to achieve here. We are only going to notice the breath.' },
      { start: 74, end: 120, text: 'Conscious breathing starts with counting. Inhale for four, slowly, through the nose.' },
      { start: 120, end: 198, text: 'Now exhale for six. A longer exhale tells the nervous system it is safe to slow down.' },
      { start: 198, end: 312, text: 'Let the counting go. Stay with this stillness for a few moments before you open your eyes.' },
    ],
  },
  {
    public_id: 'p3n8w6t2c4v9h1j5',
    title: 'The story of the first sun salutation',
    description: 'Why the instructor still begins every class with the same sequence she learned twenty years ago.',
    kind: 'clip',
    category: 'Stories',
    language: 'en',
    speaker: 'Ana Exemplo',
    tags: ['stories', 'sun salutation', 'hatha'],
    duration_seconds: 186,
    published_at: '2026-10-21T10:30:00Z',
    source_group: 'g7h3k2m9',
    destinations: [destinationFor('p3n8w6t2c4v9h1j5', 'youtube')],
    chapters: [
      { title: 'A studio with no mirrors', start: 0, end: 95, summary: 'The first class, in a small room above a bakery.' },
      { title: 'What the sequence teaches', start: 95, end: 186, summary: 'Repetition as a way of paying attention.' },
    ],
    transcript: [
      { start: 0, end: 48, text: 'My first class was in a small room above a bakery. There were no mirrors at all.' },
      { start: 48, end: 95, text: 'The teacher asked us to repeat one sun salutation for an entire hour.' },
      { start: 95, end: 186, text: 'I still begin every class that way, because repetition is how we learn to pay attention.' },
    ],
  },
  {
    public_id: 'r9d4f7s1b6m2k8x3',
    title: 'Respiração consciente na prática diária',
    description: 'Como levar a respiração consciente para o dia a dia, sem precisar de um tapete.',
    kind: 'edited',
    category: 'Breathwork',
    language: 'pt-BR',
    speaker: 'Bruno Amostra',
    tags: ['respiração', 'dia a dia'],
    duration_seconds: 1485,
    published_at: '2026-10-14T18:45:00Z',
    source_group: 'b2n6q8w4',
    destinations: [
      destinationFor('r9d4f7s1b6m2k8x3', 'vimeo'),
      destinationFor('r9d4f7s1b6m2k8x3', 'peertube', {
        url: `${EXAMPLE_HOST}/w/r9d4f7s1b6m2k8x3`,
        embed_url: `${EXAMPLE_HOST}/videos/embed/r9d4f7s1b6m2k8x3`,
      }),
    ],
    chapters: [
      { title: 'Por que a expiração importa', start: 0, end: 610, summary: 'O que acontece com o corpo em uma expiração longa.' },
      { title: 'Três pausas no seu dia', start: 610, end: 1485, summary: 'Antes de uma reunião, no trânsito e ao deitar.' },
    ],
    transcript: [
      { start: 0, end: 300, text: 'A respiração consciente não exige silêncio nem tapete, só um instante de atenção.' },
      { start: 300, end: 610, text: 'Quando a expiração fica mais longa que a inspiração, o coração desacelera.' },
      { start: 610, end: 1485, text: 'Experimente três pausas: antes de uma reunião, no trânsito e na hora de deitar.' },
    ],
  },
  {
    public_id: 'h1y5c8z3n7q2d6w4',
    title: 'Hatha Yoga Class: slow morning flow',
    description: 'The complete recording of a 45-minute morning class, released with the students’ agreement.',
    kind: 'original',
    category: 'Classes',
    language: 'en',
    speaker: 'Ana Exemplo',
    tags: ['hatha', 'full class', 'morning'],
    duration_seconds: 2712,
    published_at: '2026-09-30T08:00:00Z',
    source_group: 'g7h3k2m9',
    destinations: [
      destinationFor('h1y5c8z3n7q2d6w4', 'youtube'),
      destinationFor('h1y5c8z3n7q2d6w4', 'mock', { embed_url: '', supports_timestamps: false }),
    ],
    chapters: [
      { title: 'Arrival and breath', start: 0, end: 420, summary: 'Opening the class with conscious breathing.' },
      { title: 'Sun salutations', start: 420, end: 1260, summary: 'Six slow rounds, with options for tight hamstrings.' },
      { title: 'Standing balance', start: 1260, end: 2100, summary: 'Tree pose and warrior three, near a wall if you like.' },
      { title: 'Savasana', start: 2100, end: 2712, summary: 'A long rest to finish.' },
    ],
    transcript: [
      { start: 0, end: 200, text: 'Good morning, everyone. Let us arrive with a few rounds of conscious breathing.' },
      { start: 200, end: 420, text: 'Good. Feel the ribs widen on the inhale and draw in on the exhale.' },
      { start: 420, end: 1260, text: 'From here we begin the sun salutations, slowly, six rounds, bending the knees as much as you need.' },
      { start: 1260, end: 2100, text: 'Standing balance now. Tree pose first, and stay near a wall if that helps.' },
      { start: 2100, end: 2712, text: 'Lie down for savasana. There is nothing left to do.' },
    ],
  },
  {
    public_id: 'm6t2v9k4j8s3f1g7',
    title: 'Meditation for a busy mind',
    description: 'A ten-minute guided meditation for days when sitting still feels impossible.',
    kind: 'edited',
    category: 'Meditation',
    language: 'en',
    speaker: 'Carla Ficticia',
    tags: ['meditation', 'stress'],
    duration_seconds: 604,
    published_at: '2026-09-18T20:15:00Z',
    destinations: [
      destinationFor('m6t2v9k4j8s3f1g7', 'instagramreels', { embed_url: '', supports_timestamps: false }),
    ],
    chapters: [
      { title: 'Noticing the noise', start: 0, end: 300 },
      { title: 'Letting it pass', start: 300, end: 604 },
    ],
    transcript: [
      { start: 0, end: 300, text: 'Your mind will be busy today, and that is fine. Notice the noise without arguing with it.' },
      { start: 300, end: 604, text: 'Let each thought pass, the way a car passes outside your window.' },
    ],
  },
  {
    public_id: 'w8b3x5n1p7r4c9d2',
    title: 'Keeping the spine long in seated poses',
    description: 'Alignment cues for seated forward folds.',
    kind: 'clip',
    category: 'Alignment',
    language: 'en',
    speaker: 'Bruno Amostra',
    tags: ['alignment', 'spine'],
    duration_seconds: 148,
    published_at: '2026-09-02T12:00:00Z',
    source_group: 'b2n6q8w4',
    destinations: [destinationFor('w8b3x5n1p7r4c9d2', 'mock', { embed_url: '', supports_timestamps: false })],
  },
];

/** Enough additional plain videos to exercise pagination. */
export function fillerVideos(count: number): FixtureVideo[] {
  return Array.from({ length: count }, (_, index) => {
    const number = String(index + 1).padStart(3, '0');
    const day = String((index % 27) + 1).padStart(2, '0');
    const id = `filler${number}x0000000`;
    return {
      public_id: id,
      title: `Morning practice, session ${index + 1}`,
      description: `Session ${index + 1} of the fictional morning practice series.`,
      kind: 'clip' as const,
      category: 'Classes',
      language: 'en',
      speaker: 'Dora Modelo',
      tags: ['morning'],
      duration_seconds: 600 + index * 15,
      published_at: `2026-08-${day}T07:00:00Z`,
      destinations: [destinationFor(id, 'youtube')],
    };
  });
}

function vectorBlob(text: string): Uint8Array {
  const vector = fakeHashEmbed(text);
  const bytes = new Uint8Array(vector.length * 4);
  const view = new DataView(bytes.buffer);
  vector.forEach((value, index) => view.setFloat32(index * 4, value, true));
  return bytes;
}

export function buildFixture(filePath: string, options: FixtureOptions = {}): void {
  const schema = fs.readFileSync(SCHEMA_PATH, 'utf8');
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.rmSync(filePath, { force: true });

  const videos = options.videos ?? CATALOG;
  const withVectors = options.withVectors ?? true;
  const model = options.embeddingModel ?? (withVectors ? FAKE_EMBEDDING_MODEL : '');
  const dimensions = options.embeddingDimensions ?? (withVectors ? FAKE_EMBEDDING_DIMENSIONS : 0);

  const db = new DatabaseSync(filePath);
  db.exec(`PRAGMA journal_mode=${options.journalMode ?? 'DELETE'}`);
  db.exec(schema);

  const insertMeta = db.prepare('INSERT INTO meta (key, value) VALUES (?, ?)');
  insertMeta.run('schema_version', options.schemaVersion ?? '1');
  insertMeta.run('generated_at', options.generatedAt ?? '2026-11-09T06:00:00Z');
  insertMeta.run('embedding_model', model);
  insertMeta.run('embedding_dimensions', String(dimensions));

  const insertVideo = db.prepare(
    `INSERT INTO videos (public_id, title, description, kind, category, language, speaker, tags,
       duration_seconds, published_at, thumbnail_url, source_group)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  );
  const insertDestination = db.prepare(
    `INSERT INTO destinations (public_id, destination, url, embed_url, supports_timestamps)
     VALUES (?, ?, ?, ?, ?)`,
  );
  const insertChapter = db.prepare(
    `INSERT INTO chapters (public_id, position, title, start_seconds, end_seconds, summary)
     VALUES (?, ?, ?, ?, ?, ?)`,
  );
  const insertSegment = db.prepare(
    `INSERT INTO transcript_segments (public_id, position, start_seconds, end_seconds, text)
     VALUES (?, ?, ?, ?, ?)`,
  );
  const insertEmbedding = db.prepare(
    `INSERT INTO embeddings (public_id, position, level, start_seconds, end_seconds, text, vector)
     VALUES (?, ?, ?, ?, ?, ?, ?)`,
  );
  const insertFts = db.prepare(
    `INSERT INTO search_fts (public_id, source, start_seconds, end_seconds, text)
     VALUES (?, ?, ?, ?, ?)`,
  );

  db.exec('BEGIN');
  for (const video of videos) {
    insertVideo.run(
      video.public_id,
      video.title,
      video.description ?? '',
      video.kind,
      video.category ?? '',
      video.language ?? '',
      video.speaker ?? '',
      typeof video.tags === 'string' ? video.tags : JSON.stringify(video.tags ?? []),
      video.duration_seconds ?? 0,
      video.published_at,
      video.thumbnail_url ?? '',
      video.source_group ?? '',
    );
    for (const destination of video.destinations ?? []) {
      insertDestination.run(
        video.public_id,
        destination.destination,
        destination.url,
        destination.embed_url ?? '',
        destination.supports_timestamps ? 1 : 0,
      );
    }
    insertFts.run(video.public_id, 'title', null, null, video.title);
    if (video.description) insertFts.run(video.public_id, 'description', null, null, video.description);

    let vectorPosition = 0;
    (video.chapters ?? []).forEach((chapter, position) => {
      insertChapter.run(video.public_id, position, chapter.title, chapter.start, chapter.end, chapter.summary ?? '');
      const text = chapter.summary ? `${chapter.title}. ${chapter.summary}` : chapter.title;
      insertFts.run(video.public_id, 'chapter', chapter.start, chapter.end, text);
      if (withVectors) {
        insertEmbedding.run(video.public_id, vectorPosition, 'chapter', chapter.start, chapter.end, text, vectorBlob(text));
        vectorPosition += 1;
      }
    });
    (video.transcript ?? []).forEach((segment, position) => {
      insertSegment.run(video.public_id, position, segment.start, segment.end, segment.text);
      insertFts.run(video.public_id, 'transcript', segment.start, segment.end, segment.text);
      if (withVectors) {
        insertEmbedding.run(video.public_id, vectorPosition, 'window', segment.start, segment.end, segment.text, vectorBlob(segment.text));
        vectorPosition += 1;
      }
    });
  }
  db.exec('COMMIT');
  db.close();
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const target = path.resolve(process.argv[2] ?? 'data/fixture/public.db');
  buildFixture(target, { videos: [...CATALOG, ...fillerVideos(24)] });
  console.log(`Fictional fixture written to ${target}`);
}
