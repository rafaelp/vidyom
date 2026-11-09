// @vitest-environment jsdom
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Player } from '@/components/Player';
import type { Destination } from '@/lib/types';

const embeddable: Destination = {
  driver: 'youtube',
  url: 'https://media.example.internal/watch/a',
  embedUrl: 'https://media.example.internal/embed/a',
  supportsTimestamps: true,
};

describe('Player', () => {
  it('loads no third-party frame until the visitor asks', () => {
    const { container } = render(<Player title="A class" thumbnailUrl="" destination={embeddable} start={0} />);
    expect(container.querySelector('iframe')).toBeNull();

    const link = screen.getByRole('link');
    expect(link.getAttribute('href')).toBe(embeddable.url);
    expect(link.getAttribute('aria-label')).toBe('Play: A class. Loads the player from YouTube.');

    fireEvent.click(link);
    const frame = container.querySelector('iframe');
    expect(frame?.getAttribute('src')).toBe('https://media.example.internal/embed/a?autoplay=1');
    expect(frame?.getAttribute('title')).toBe('Video player: A class');
    expect(frame?.getAttribute('sandbox')).toContain('allow-scripts');
    expect(frame?.getAttribute('sandbox')).not.toContain('allow-top-navigation');
  });

  it('starts from the position and links the destination at it', () => {
    const { container } = render(<Player title="A class" thumbnailUrl="" destination={embeddable} start={90} />);
    const link = screen.getByRole('link');
    expect(link.textContent).toContain('Play from 1:30');
    expect(link.getAttribute('href')).toBe('https://media.example.internal/watch/a?t=90s');
    fireEvent.click(link);
    expect(container.querySelector('iframe')?.getAttribute('src')).toBe(
      'https://media.example.internal/embed/a?start=90&autoplay=1',
    );
  });

  it('plays from a new position when a chapter link changes it', () => {
    const { container, rerender } = render(
      <Player title="A class" thumbnailUrl="" destination={embeddable} start={0} />,
    );
    expect(container.querySelector('iframe')).toBeNull();
    rerender(<Player title="A class" thumbnailUrl="" destination={embeddable} start={120} />);
    expect(container.querySelector('iframe')?.getAttribute('src')).toContain('start=120');
  });

  it('claims no position when the destination cannot jump', () => {
    const noJump = { ...embeddable, supportsTimestamps: false };
    const { container } = render(<Player title="A class" thumbnailUrl="" destination={noJump} start={90} />);
    const link = screen.getByRole('link');
    expect(link.textContent).toContain('Play');
    expect(link.textContent).not.toContain('1:30');
    expect(link.getAttribute('href')).toBe(embeddable.url);
    fireEvent.click(link);
    expect(container.querySelector('iframe')?.getAttribute('src')).toBe(
      'https://media.example.internal/embed/a?autoplay=1',
    );
  });

  it('links out in a new tab when there is no embed', () => {
    const linkOnly = { ...embeddable, embedUrl: '' };
    const { container } = render(<Player title="A class" thumbnailUrl="" destination={linkOnly} start={90} />);
    const link = screen.getByRole('link');
    expect(link.textContent).toContain('Watch on YouTube from 1:30');
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toContain('noopener');
    fireEvent.click(link);
    expect(container.querySelector('iframe')).toBeNull();
  });

  it('shows the tile with nothing to click when there is no destination', () => {
    const { container } = render(<Player title="A class" thumbnailUrl="" destination={undefined} start={0} />);
    expect(screen.queryByRole('link')).toBeNull();
    expect(container.querySelector('.thumb')).not.toBeNull();
  });

  it('keeps the referrer from thumbnail requests', () => {
    const { container } = render(
      <Player title="A class" thumbnailUrl="https://media.example.internal/t.jpg" destination={embeddable} start={0} />,
    );
    const image = container.querySelector('img');
    expect(image?.getAttribute('referrerpolicy')).toBe('no-referrer');
    expect(image?.getAttribute('alt')).toBe('');
  });
});
