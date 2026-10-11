/**
 * Content query helpers shared by the public pages.
 *
 * ARCHITECTURE.md §10: a collection-level error can now only mean a broken
 * CONTENT_ROOT, because the loader isolates per-file problems (D5). Those must
 * fail loudly rather than render an empty blog, so they are re-thrown here.
 */
import { getLiveCollection, getLiveEntry } from 'astro:content';

import type { PostData, PageData } from './schema';
import type { CollectionFilter, EntryFilter } from '../loaders/fs';

/** Page slugs that would shadow a framework route. */
export const RESERVED_SLUGS = new Set(['admin', 'posts', 'tags', 'login', 'logout', 'markdown']);

/**
 * Astro reports a missing live entry as an error rather than `undefined`, so a
 * plain `if (error) throw` turns every 404 into a 500.
 *
 * `LiveEntryNotFoundError.is()` is defined by Astro as exactly this `name`
 * comparison, so this check matches Astro's own semantics without importing
 * another internal module (ARCHITECTURE.md §8 keeps internal imports isolated).
 */
function isNotFound(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    (error as { name?: string }).name === 'LiveEntryNotFoundError'
  );
}

export type PostEntry = {
  id: string;
  data: PostData;
  rendered?: { html: string };
};

export type PageEntry = {
  id: string;
  data: PageData;
  rendered?: { html: string };
};

export async function getPublishedPosts(): Promise<PostEntry[]> {
  const { entries, error } = await getLiveCollection('posts');
  if (error) throw error;
  const posts = (entries ?? []) as PostEntry[];
  return posts.sort((a, b) => b.data.date.getTime() - a.data.date.getTime());
}

export async function getPostBySlug(
  slug: string,
  filter: CollectionFilter = {},
): Promise<PostEntry | undefined> {
  const { entry, error } = await getLiveEntry('posts', {
    id: slug,
    ...filter,
  } as EntryFilter);
  if (error) {
    if (isNotFound(error)) return undefined;
    throw error;
  }
  return entry as PostEntry | undefined;
}

export async function getPublishedPages(): Promise<PageEntry[]> {
  const { entries, error } = await getLiveCollection('pages');
  if (error) throw error;
  return (entries ?? []) as PageEntry[];
}

export async function getPageBySlug(
  slug: string,
  filter: CollectionFilter = {},
): Promise<PageEntry | undefined> {
  const { entry, error } = await getLiveEntry('pages', {
    id: slug,
    ...filter,
  } as EntryFilter);
  if (error) {
    if (isNotFound(error)) return undefined;
    throw error;
  }
  return entry as PageEntry | undefined;
}

/** All distinct tags across published posts, sorted. */
export function collectTags(posts: PostEntry[]): string[] {
  const set = new Set<string>();
  for (const p of posts) for (const t of p.data.tags) set.add(t);
  return [...set].sort();
}

export function isReservedSlug(slug: string): boolean {
  return RESERVED_SLUGS.has(slug);
}
