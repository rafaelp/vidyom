import type { SnippetSegment } from '@/lib/search/types';

/** Renders parsed segments as text nodes, so database text can never become markup. */
export function Snippet({ segments, lang }: { segments: SnippetSegment[]; lang?: string }) {
  return (
    <span lang={lang}>
      {segments.map((segment, index) =>
        segment.match ? <mark key={index}>{segment.text}</mark> : <span key={index}>{segment.text}</span>,
      )}
    </span>
  );
}
