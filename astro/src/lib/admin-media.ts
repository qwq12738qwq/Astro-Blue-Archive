/**
 * The admin's view of the media library.
 *
 * ARCHITECTURE.md §12/§56/§91: every admin surface that shows or links an image goes
 * through here, and every URL in the result comes from the backend's own resolver.
 * The admin never assembles `/media/…` and never sees a filesystem path — so the
 * preview is served by the same delivery layer a visitor gets, the copy button
 * copies the URL a visitor would actually use, and neither can be wrong about which
 * representation the browser will receive.
 */
import { adminFetch } from './admin-api';
import type { ImageCacheView, MediaItem } from '../theme-system/contract';

/** The raw shape the API returns. Not exported: callers get resolved items. */
type MediaApiItem = {
  id: string;
  filename: string;
  path: string;
  url: string;
  mime: string;
  size: number;
  width?: number;
  height?: number;
  alt?: string;
  createdAt: string;
  usageCount?: number;
  missing?: boolean;
  usedAs?: string;
  usedBy?: { kind: string; slug: string; href: string; referenceCount: number }[];
};

export type MediaListResult = {
  items: MediaItem[];
  cache: ImageCacheView;
};

/**
 * The upload ceiling, in bytes.
 *
 * The backend is the authority and the admin form states this number, so the two
 * halves must agree. It is a build-time constant rather than a settings read because
 * it is a deployment limit (MAX_UPLOAD_BYTES), not something an admin changes, and a
 * failed settings read must not silently change what the form claims.
 */
export const MAX_UPLOAD_BYTES = 5 * 1024 * 1024;

/** The MIME types the delivery layer serves. Used by the type filter. */
export const MEDIA_TYPES = ['image/jpeg', 'image/png', 'image/gif', 'image/webp'] as const;

/** The default cache counters, so a screen renders before the API answers. */
const EMPTY_CACHE: ImageCacheView = {
  memoryLimitBytes: 0,
  memoryUsedBytes: 0,
  memoryEntries: 0,
  diskEntries: 0,
  diskBytes: 0,
  hits: 0,
  misses: 0,
  evictions: 0,
  conversions: 0,
  failures: 0,
  quality: 0,
  maxPixels: 0,
};

/** Human-readable size, computed here so every theme labels it identically. */
function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function toItem(raw: MediaApiItem): MediaItem {
  return {
    id: raw.id,
    filename: raw.filename,
    path: raw.path,
    // The backend builds this with the media resolver. If it ever arrives empty the
    // item is rendered without a preview rather than with a guessed URL.
    url: typeof raw.url === 'string' ? raw.url : '',
    mime: raw.mime,
    size: raw.size,
    sizeLabel: formatSize(raw.size),
    width: raw.width,
    height: raw.height,
    alt: typeof raw.alt === 'string' ? raw.alt : '',
    createdAt: raw.createdAt,
    usageCount: typeof raw.usageCount === 'number' ? raw.usageCount : 0,
    missing: raw.missing === true,
    usedAs: typeof raw.usedAs === 'string' && raw.usedAs !== '' ? raw.usedAs : undefined,
    usedBy: Array.isArray(raw.usedBy) ? raw.usedBy : [],
  };
}

/** The library, with the cache counters, honouring the active filters. */
export async function listMedia(
  cookie: string | null,
  filters: { search?: string; mime?: string; unused?: boolean; missing?: boolean } = {},
): Promise<MediaListResult> {
  const params = new URLSearchParams();
  if (filters.search) params.set('q', filters.search);
  if (filters.mime) params.set('type', filters.mime);
  if (filters.unused) params.set('unused', '1');
  if (filters.missing) params.set('missing', '1');

  const res = await adminFetch<{ items?: MediaApiItem[]; cache?: ImageCacheView }>(
    `/api/v1/admin/media?${params.toString()}`,
    { cookie },
  );
  return {
    items: (res.items ?? []).map(toItem),
    cache: res.cache ?? EMPTY_CACHE,
  };
}

/**
 * The library, for a screen that only needs thumbnails — the editor's picker.
 *
 * It swallows its own errors: a failed read costs the picker, not the form the
 * editor is in the middle of filling in.
 */
export async function listMediaItems(cookie: string | null): Promise<MediaItem[]> {
  try {
    return (await listMedia(cookie)).items;
  } catch {
    return [];
  }
}

listMediaItems.maxBytes = MAX_UPLOAD_BYTES;
