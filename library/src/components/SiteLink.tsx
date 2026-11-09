import Link from 'next/link';
import type { ComponentProps } from 'react';

/**
 * Next's Link with prefetching off. Every page here is rendered on demand, so
 * prefetching would turn each card in view into server requests the visitor may never
 * use: about thirty for one page of videos, measured in the browser.
 */
export function SiteLink(props: ComponentProps<typeof Link>) {
  return <Link prefetch={false} {...props} />;
}
