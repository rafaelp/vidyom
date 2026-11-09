/** Types shared by the server and client code. No server-only imports here. */

export type VideoKind = 'clip' | 'edited' | 'original';

export const VIDEO_KINDS: readonly VideoKind[] = ['clip', 'edited', 'original'];

export interface VideoSummary {
  publicId: string;
  title: string;
  description: string;
  kind: VideoKind;
  category: string;
  language: string;
  speaker: string;
  tags: string[];
  durationSeconds: number;
  publishedAt: string;
  /** '' when the file has none or it is not https (I252). */
  thumbnailUrl: string;
}

export interface Destination {
  driver: string;
  /** Always http(s); rows with any other scheme are dropped at the reader. */
  url: string;
  /** '' when the destination has no embed or it is not https. */
  embedUrl: string;
  supportsTimestamps: boolean;
}

export interface Chapter {
  position: number;
  title: string;
  startSeconds: number;
  endSeconds: number;
  summary: string;
}

export interface TranscriptSegment {
  position: number;
  startSeconds: number;
  endSeconds: number;
  text: string;
}

export interface VideoDetail extends VideoSummary {
  destinations: Destination[];
  chapters: Chapter[];
  transcriptCount: number;
  /** Stays on the server; never serialized to HTML or JSON (I257). */
  sourceGroup: string;
}

export interface VideoFilters {
  kind?: VideoKind;
  category?: string;
  language?: string;
  speaker?: string;
  tag?: string;
}

export interface Facets {
  kinds: VideoKind[];
  categories: string[];
  languages: string[];
  speakers: string[];
  tags: string[];
}

export interface VideoPage {
  items: VideoSummary[];
  total: number;
  page: number;
  pageSize: number;
}
