/**
 * Free text to a safe FTS5 expression (docs/decisions/library.md, I245). Whatever the
 * visitor types is reduced to tokens and quoted, so nothing reaches MATCH as syntax.
 * `"..."` makes a phrase, and the last token outside a phrase is a prefix.
 */

export interface QueryTerm {
  /** Tokens in order; more than one only for a phrase. */
  tokens: string[];
  prefix: boolean;
}

export interface ParsedQuery {
  terms: QueryTerm[];
  /** Every term must match; null when the query has no searchable word. */
  all: string | null;
  /** Any term may match; null when that would be the same as `all`. */
  any: string | null;
}

const MAX_TOKENS = 16;
const TOKEN = /[\p{L}\p{N}]+/gu;

function tokenize(text: string): string[] {
  return text.match(TOKEN) ?? [];
}

function quoted(term: QueryTerm): string {
  const body = term.tokens.join(' ').replaceAll('"', '""');
  return `"${body}"${term.prefix ? '*' : ''}`;
}

export function parseQuery(raw: string): ParsedQuery {
  const text = raw.normalize('NFC');
  const terms: QueryTerm[] = [];
  let used = 0;

  const add = (tokens: string[], phrase: boolean) => {
    if (phrase) {
      const kept = tokens.slice(0, MAX_TOKENS - used);
      if (kept.length > 0) {
        terms.push({ tokens: kept, prefix: false });
        used += kept.length;
      }
      return;
    }
    for (const token of tokens) {
      if (used >= MAX_TOKENS) return;
      terms.push({ tokens: [token], prefix: false });
      used += 1;
    }
  };

  // An unmatched quote is ordinary punctuation: it just splits words.
  let cursor = 0;
  for (const match of text.matchAll(/"([^"]*)"/g)) {
    add(tokenize(text.slice(cursor, match.index)), false);
    add(tokenize(match[1] ?? ''), true);
    cursor = match.index + match[0].length;
  }
  const tail = tokenize(text.slice(cursor));
  add(tail, false);

  // Only a word still being typed is a prefix, so never one that ends in a space.
  const last = terms.at(-1);
  if (last && tail.length > 0 && last.tokens.length === 1 && !/\s$/.test(text)) {
    last.prefix = true;
  }

  if (terms.length === 0) return { terms, all: null, any: null };
  const expressions = terms.map(quoted);
  return {
    terms,
    all: expressions.join(' AND '),
    any: expressions.length > 1 ? expressions.join(' OR ') : null,
  };
}

/** Same folding the FTS tokenizer applies, for matching outside FTS. */
export function fold(text: string): string {
  return text
    .normalize('NFKD')
    .replace(/\p{M}+/gu, '')
    .toLowerCase();
}
