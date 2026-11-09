import type { Generation } from '../dataset';
import { fold, type ParsedQuery, type QueryTerm } from './fts-query';
import type { MatchedIn } from './types';

/**
 * Search over `speaker`, `category` and `tags`, which `search_fts` does not hold, so
 * "search by instructor" (§13.2) works (docs/decisions/library.md, I246). The file is
 * read-only, so this is a small in-memory scan, built once per generation.
 */

const MATCH_LIMIT = 400;

interface MetadataEntry {
  publicId: string;
  speaker: string;
  category: string;
  tags: string;
}

type MetadataField = Extract<MatchedIn, 'speaker' | 'category' | 'tag'>;

export type MetadataMatches = Map<string, MetadataField[]>;

/** Folded words with sentinel spaces, so a term can match on word boundaries. */
function indexText(value: string): string {
  const words = fold(value).match(/[\p{L}\p{N}]+/gu) ?? [];
  return words.length > 0 ? ` ${words.join(' ')} ` : '';
}

function parseTags(value: string): string[] {
  try {
    const parsed: unknown = JSON.parse(value);
    return Array.isArray(parsed)
      ? parsed.filter((tag): tag is string => typeof tag === 'string')
      : [];
  } catch {
    return [];
  }
}

function entries(generation: Generation): MetadataEntry[] {
  return generation.memo('metadata-index', () =>
    generation.db
      .all<{ public_id: string; speaker: string; category: string; tags: string }>(
        'SELECT public_id, speaker, category, tags FROM videos ORDER BY public_id',
      )
      .map((row) => ({
        publicId: row.public_id,
        speaker: indexText(row.speaker),
        category: indexText(row.category),
        tags: parseTags(row.tags).map(indexText).join(''),
      })),
  );
}

function needle(term: QueryTerm): string {
  const body = fold(term.tokens.join(' '));
  return term.prefix ? ` ${body}` : ` ${body} `;
}

export function metadataSearch(
  generation: Generation,
  query: ParsedQuery,
  allowed?: ReadonlySet<string>,
): MetadataMatches {
  const matches: MetadataMatches = new Map();
  if (query.terms.length === 0) return matches;
  const needles = query.terms.map(needle);
  for (const entry of entries(generation)) {
    if (allowed && !allowed.has(entry.publicId)) continue;
    const fields: MetadataField[] = [];
    let everyTermMatched = true;
    for (const term of needles) {
      const inSpeaker = entry.speaker.includes(term);
      const inTags = entry.tags.includes(term);
      const inCategory = entry.category.includes(term);
      if (!inSpeaker && !inTags && !inCategory) {
        everyTermMatched = false;
        break;
      }
      if (inSpeaker && !fields.includes('speaker')) fields.push('speaker');
      if (inTags && !fields.includes('tag')) fields.push('tag');
      if (inCategory && !fields.includes('category')) fields.push('category');
    }
    if (!everyTermMatched) continue;
    matches.set(entry.publicId, fields);
    if (matches.size >= MATCH_LIMIT) break;
  }
  return matches;
}
