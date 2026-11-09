'use client';

import { useState, type MouseEvent } from 'react';
import { destinationLabel, formatTimestamp } from '@/lib/format';
import { destinationUrlAt, playerUrl } from '@/lib/links';
import type { Destination } from '@/lib/types';
import { PlayIcon } from './icons';
import { Thumbnail } from './Thumbnail';

interface PlayerProps {
  title: string;
  thumbnailUrl: string;
  destination: Destination | undefined;
  /** Whole seconds to start from, or 0. */
  start: number;
}

/**
 * A facade first: the destination's player is third-party code, so it loads only when
 * the visitor asks (docs/decisions/library.md, I252). The control is a link to the
 * destination, so with no JavaScript, or no embed, it still gets you to the video.
 */
export function Player({ title, thumbnailUrl, destination, start }: PlayerProps) {
  const [active, setActive] = useState(false);
  const [lastStart, setLastStart] = useState(start);

  // Following a chapter or transcript link means "play from here".
  if (lastStart !== start) {
    setLastStart(start);
    if (destination?.embedUrl) setActive(true);
  }

  if (!destination) {
    return (
      <div className="player player--empty" id="player">
        <Thumbnail url={thumbnailUrl} />
      </div>
    );
  }

  const label = destinationLabel(destination.driver);
  if (active && destination.embedUrl) {
    return (
      <div className="player" id="player">
        <iframe
          key={start}
          src={playerUrl(destination, start)}
          title={`Video player: ${title}`}
          allow="autoplay; encrypted-media; fullscreen; picture-in-picture"
          allowFullScreen
          sandbox="allow-scripts allow-same-origin allow-presentation allow-popups allow-popups-to-escape-sandbox"
          referrerPolicy="strict-origin-when-cross-origin"
        />
      </div>
    );
  }

  const canEmbed = destination.embedUrl !== '';
  const href = destinationUrlAt(destination, start) ?? destination.url;
  const fromPosition = start > 0 && destination.supportsTimestamps;
  const action = canEmbed
    ? `Play${fromPosition ? ` from ${formatTimestamp(start)}` : ''}`
    : `Watch on ${label}${fromPosition ? ` from ${formatTimestamp(start)}` : ''}`;

  const onClick = (event: MouseEvent<HTMLAnchorElement>) => {
    if (!canEmbed) return;
    event.preventDefault();
    setActive(true);
  };

  return (
    <div className="player" id="player">
      <a
        className="player__facade"
        href={href}
        onClick={onClick}
        aria-label={`${action}: ${title}. ${canEmbed ? `Loads the player from ${label}.` : 'Opens in a new tab.'}`}
        {...(canEmbed ? {} : { target: '_blank', rel: 'noopener noreferrer' })}
      >
        <Thumbnail url={thumbnailUrl} />
        <span className="player__scrim">
          <span className="player__button">
            <PlayIcon />
          </span>
          <span className="player__label">{action}</span>
        </span>
      </a>
    </div>
  );
}
