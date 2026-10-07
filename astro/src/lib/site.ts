/**
 * Site presentation settings, read once per process instead of once per request.
 *
 * ARCHITECTURE.md §3: settings are runtime state, not content, so the site title
 * lives in SQLite rather than in a Markdown file. That makes the public site
 * depend on the Go backend, which the file-first design otherwise avoids
 * completely: articles and pages are read from `content/` and render fine while
 * Go is down.
 *
 * Doing that naively — `fetch` on every render — is worse than it looks. Every
 * anonymous request would open its own connection to the backend, and a slow one
 * (Go alive but SQLite wedged behind `SetMaxOpenConns(1)`) would hold the render
 * for the full timeout. A burst of N anonymous requests became N concurrent
 * backend calls, aimed at a backend that was already the bottleneck. Measured
 * here: 20 concurrent requests all blocked for the full 3s timeout.
 *
 * So the value is cached, concurrent callers share one in-flight request, a
 * failure opens a short circuit instead of hammering a struggling backend, and
 * the last known good value survives an outage. A page title is not worth three
 * seconds of latency.
 *
 * The same cache carries `themeId`. That is deliberate rather than convenient:
 * the theme has to be resolved in the middleware, before any page runs, so every
 * request needs the value anyway, and a second uncached fetch per request would
 * undo the fix above.
 */

import { apiRequest } from './api';
import { DEFAULT_THEME_ID, normalizeThemeId, type ThemeId } from '../theme-system/ids';

export type SiteSettings = {
  siteTitle: string;
  siteSubtitle: string;
  siteDescription: string;
  /**
   * The site icon, resolved by the backend to a public media URL.
   *
   * ARCHITECTURE.md §35/§116: it is a media id, not a path, so the icon goes through
   * exactly the same upload validation and delivery layer as any other image. The
   * URL carries a version token so a browser does not keep serving a cached icon
   * after the admin replaced it (ARCHITECTURE.md §76). Empty means "no icon
   * configured", and the layout falls back to a core default asset.
   */
  siteIconUrl: string;
  siteIconVersion: string;
  commentsEnabled: boolean;
  commentAutoModerate: boolean;
  /** Which installed theme renders this site. Normalised, never trusted raw. */
  themeId: ThemeId;
  /**
   * Whether the RSS feed exists at all.
   *
   * ARCHITECTURE.md §65/§71: this is a setting, not a theme choice. When it is off
   * there is no feed, and no discovery link — advertising a URL that answers 404 is
   * worse than advertising nothing.
   */
  rssEnabled: boolean;
  rssTitle: string;
  rssDescription: string;
  rssItemLimit: number;
};

const FALLBACK: SiteSettings = {
  siteTitle: 'Blog',
  siteSubtitle: '',
  siteDescription: '',
  siteIconUrl: '',
  siteIconVersion: '',
  commentsEnabled: true,
  commentAutoModerate: true,
  themeId: DEFAULT_THEME_ID,
  rssEnabled: true,
  rssTitle: '',
  rssDescription: '',
  rssItemLimit: 20,
};

/** Bounds on the item limit, mirroring the backend's validation. */
export const RSS_ITEM_LIMIT_MIN = 1;
export const RSS_ITEM_LIMIT_MAX = 100;

/** How long a fetched value stays fresh. */
const TTL_MS = 30_000;
/** How long to wait on the backend. Short, because a fallback exists. */
const TIMEOUT_MS = 600;
/** How long to stop trying after a failure. */
const BREAKER_MS = 5_000;

let cached: SiteSettings | null = null;
let cachedAt = 0;
let inFlight: Promise<SiteSettings> | null = null;
let breakerUntil = 0;

async function fetchSettings(): Promise<SiteSettings> {
  const site = await apiRequest<Partial<SiteSettings>>('/api/v1/site', {
    signal: AbortSignal.timeout(TIMEOUT_MS),
  });
  return {
    siteTitle:
      typeof site?.siteTitle === 'string' && site.siteTitle ? site.siteTitle : FALLBACK.siteTitle,
    siteSubtitle: text(site?.siteSubtitle),
    siteDescription: text(site?.siteDescription),
    // The URL is a backend-built media URL, so it is used as given. It is still
    // filtered for shape: a value that is not a root-relative or absolute path is
    // dropped rather than written into a <link rel="icon">, because that attribute is
    // one of the few places a theme hands a URL to the browser unfiltered.
    siteIconUrl: url(site?.siteIconUrl),
    siteIconVersion: /^[0-9a-f]{1,32}$/.test(String(site?.siteIconVersion ?? ''))
      ? String(site.siteIconVersion)
      : '',
    commentsEnabled:
      typeof site?.commentsEnabled === 'boolean' ? site.commentsEnabled : FALLBACK.commentsEnabled,
    commentAutoModerate:
      typeof site?.commentAutoModerate === 'boolean'
        ? site.commentAutoModerate
        : FALLBACK.commentAutoModerate,
    // The backend already validates this against its allowlist. Normalising here
    // as well means a value stored before a theme was uninstalled — or a response
    // from a newer backend — degrades to the default instead of 500ing the site.
    themeId: normalizeThemeId(site?.themeId),
    rssEnabled: typeof site?.rssEnabled === 'boolean' ? site.rssEnabled : FALLBACK.rssEnabled,
    rssTitle: text(site?.rssTitle),
    rssDescription: text(site?.rssDescription),
    rssItemLimit: clamp(
      Number(site?.rssItemLimit),
      RSS_ITEM_LIMIT_MIN,
      RSS_ITEM_LIMIT_MAX,
      FALLBACK.rssItemLimit,
    ),
  };
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

/** A site-supplied URL, accepted only when it is one. */
function url(value: unknown): string {
  if (typeof value !== 'string') return '';
  const raw = value.trim();
  if (raw === '') return '';
  if (raw.startsWith('/') || /^https?:\/\//i.test(raw)) return raw;
  return '';
}

function clamp(value: number, min: number, max: number, fallback: number): number {
  if (!Number.isFinite(value)) return fallback;
  const whole = Math.trunc(value);
  if (whole < min) return min;
  if (whole > max) return max;
  return whole;
}

/**
 * The RSS channel title, resolved.
 *
 * ARCHITECTURE.md §66: an explicit `rss_title` wins, and an empty one falls back to
 * the site title rather than to a literal, so renaming the site renames the feed.
 */
export function rssTitle(settings: SiteSettings): string {
  return settings.rssTitle.trim() || settings.siteTitle;
}

/**
 * The RSS channel description, resolved.
 *
 * ARCHITECTURE.md §67: site description, then subtitle, then the title. A feed with
 * no description is legal but every reader shows an empty channel blurb.
 */
export function rssDescription(settings: SiteSettings): string {
  return (
    settings.rssDescription.trim() ||
    settings.siteDescription.trim() ||
    settings.siteSubtitle.trim() ||
    settings.siteTitle
  );
}

/**
 * Drops the cached value so the next read goes to the backend.
 *
 * A 30s TTL would otherwise mean that saving settings in the admin showed the old
 * title on the public site for up to 30 seconds, which reads as "the save did not
 * work". Called when a settings write passes through the API proxy.
 */
export function invalidateSiteSettings(): void {
  cached = null;
  cachedAt = 0;
  breakerUntil = 0;
}

/**
 * Returns the current settings. Never throws and never blocks for more than
 * TIMEOUT_MS, because a page must render even when the backend cannot answer.
 */
export async function getSiteSettings(): Promise<SiteSettings> {
  const now = Date.now();

  // Serve a stale value rather than a default one: if the title changed just
  // before an outage, the outage should not silently revert it.
  if (cached) {
    if (now - cachedAt < TTL_MS) return cached;
    if (now < breakerUntil) return cached;
  }

  // One request serves every concurrent caller.
  if (inFlight) return inFlight;

  inFlight = fetchSettings()
    .then((value) => {
      cached = value;
      cachedAt = Date.now();
      breakerUntil = 0;
      return value;
    })
    .catch(() => {
      breakerUntil = Date.now() + BREAKER_MS;
      // Keep the last good value if we have one; otherwise the default.
      return cached ?? FALLBACK;
    })
    .finally(() => {
      inFlight = null;
    });

  return inFlight;
}
