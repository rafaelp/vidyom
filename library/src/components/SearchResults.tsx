import { SiteLink as Link } from '@/components/SiteLink';
import { formatDate, formatTimestamp, kindLabel, languageTag, pluralize, spokenDuration } from '@/lib/format';
import type { MatchedIn, SearchMoment, SearchResult } from '@/lib/search/types';
import { Snippet } from './Snippet';
import { Thumbnail } from './Thumbnail';

const MATCHED_LABELS: Record<MatchedIn, string> = {
  title: 'title',
  description: 'description',
  chapter: 'chapter',
  transcript: 'what is said',
  speaker: 'speaker',
  category: 'category',
  tag: 'tag',
};

function matchedSentence(result: SearchResult): string {
  const places = result.matched_in.map((place) => MATCHED_LABELS[place]);
  if (places.length === 0) return '';
  const list =
    places.length === 1
      ? places[0]
      : `${places.slice(0, -1).join(', ')} and ${places[places.length - 1]}`;
  return `Found in ${list}`;
}

function Moment({ moment, lang }: { moment: SearchMoment; lang?: string }) {
  return (
    <li className="moment">
      <Link className="moment__time" href={`${moment.url}#player`}>
        <span aria-hidden="true">{formatTimestamp(moment.start_seconds)}</span>
        <span className="sr-only">{`Play from ${spokenDuration(moment.start_seconds)}`}</span>
      </Link>
      <Snippet segments={moment.snippet} lang={lang} />
    </li>
  );
}

function Result({ result }: { result: SearchResult }) {
  const lang = languageTag(result.language);
  const date = formatDate(result.published_at);
  const extraMoments = result.moment_count - result.moments.length;
  return (
    <li>
      <article className="result">
        {/* The title below is the same link, so this one is for pointers only. */}
        <Link href={result.url} tabIndex={-1} aria-hidden="true">
          <Thumbnail url={result.thumbnail_url} kind={result.kind} durationSeconds={result.duration_seconds} />
        </Link>
        <div className="result__body">
          <h2 className="result__title" lang={lang}>
            <Link href={result.url}>{result.title}</Link>
          </h2>
          <p className="meta-line">
            <span>{kindLabel(result.kind)}</span>
            {result.speaker ? <span>{result.speaker}</span> : null}
            {date ? (
              <span>
                <time dateTime={result.published_at}>{date}</time>
              </span>
            ) : null}
          </p>
          {result.snippet ? (
            <p className="result__excerpt">
              <Snippet segments={result.snippet} lang={lang} />
            </p>
          ) : null}
          {result.moments.length > 0 ? (
            <ol className="moments" aria-label="Matching moments">
              {result.moments.map((moment) => (
                <Moment key={`${moment.start_seconds}-${moment.end_seconds}`} moment={moment} lang={lang} />
              ))}
            </ol>
          ) : null}
          {extraMoments > 0 ? (
            <p className="moments__more">
              {pluralize(extraMoments, 'more moment')} in this video
            </p>
          ) : null}
          <p className="result__matched">{matchedSentence(result)}</p>
        </div>
      </article>
    </li>
  );
}

export function SearchResults({ results }: { results: SearchResult[] }) {
  return (
    <ol className="result-list">
      {results.map((result) => (
        <Result key={result.public_id} result={result} />
      ))}
    </ol>
  );
}
