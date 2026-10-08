/**
 * The media URL contract.
 *
 * ARCHITECTURE.md §12/§13: a media URL is *derived*, never assembled. Nothing in
 * this codebase writes `"/media/" + filename` — not a theme, not an admin screen,
 * not a Markdown file, not this file's own callers. The reason is not tidiness: the
 * day a theme hardcodes the prefix, the storage layout becomes part of the theme's
 * public API, and swapping local filesystem for object storage means editing every
 * theme instead of one resolver.
 *
 * So a theme knows exactly one thing about an image: a URL. It does not know the
 * storage path, MEDIA_ROOT, the cache directory, or whether WebP is in play.
 *
 * ARCHITECTURE.md §11/§81: the URL always names the ORIGINAL. A `.webp` never
 * appears in a URL, in Markdown or in frontmatter, because WebP is a delivery
 * representation chosen by content negotiation at request time. `MEDIA_URL_PREFIX`
 * exists so a deployment can put a CDN or a bucket in front of the delivery layer;
 * the response headers (`Vary: Accept`, the ETag) are what make that safe, and they
 * travel with the asset rather than with the URL.
 */

/**
 * The public path segment images are served under.
 *
 * Overridable per deployment, which is the whole point: a deployer can front the
 * media delivery layer with a CDN by setting one environment variable, and nothing
 * in `astro/src` changes. There is deliberately no per-request or per-media value —
 * one authority, one prefix.
 */
export const MEDIA_URL_PREFIX: string = normalisePrefix(
  process.env.MEDIA_URL_PREFIX?.trim() || '/media',
);

/** Only an absolute, single-slash path prefix is accepted. */
function normalisePrefix(raw: string): string {
  const collapsed = `/${raw}`.replace(/\/{2,}/g, '/').replace(/\/+$/, '');
  return collapsed === '' ? '/media' : collapsed;
}

/**
 * The public URL of a stored asset.
 *
 * `relPath` is the database-relative storage path (`2026/10/abc123.jpg`). It is
 * always server-generated, never taken from an upload filename.
 *
 * The value is encoded segment by segment rather than with `encodeURIComponent` on
 * the whole path, because the separators must survive: encoding a whole path turns
 * `/` into `%2F` and the delivery layer receives one strange filename.
 */
export function mediaUrl(relPath: string): string {
  const clean = String(relPath ?? '')
    .trim()
    .replace(/^\/+/, '');
  if (clean === '') return '';
  const encoded = clean
    .split('/')
    .filter((segment) => segment !== '' && segment !== '.' && segment !== '..')
    .map(encodeURIComponent)
    .join('/');
  if (encoded === '') return '';
  return `${MEDIA_URL_PREFIX}/${encoded}`;
}

/**
 * The public URL for a value that may already be one.
 *
 * Content stores `/media/2026/10/x.jpg`, while the media library stores the bare
 * storage path `2026/10/x.jpg`. Both are legitimate inputs for a frontmatter
 * `cover:` or an editor field, and neither the theme nor a page should have to know
 * which. A URL-looking value is passed through (its extension is preserved
 * unchanged — ARCHITECTURE.md §81); a bare path is resolved.
 *
 * With a `base` the answer is absolute, which is what a feed or a shareable preview
 * needs (ARCHITECTURE.md §70): a reader resolving `/media/x.jpg` against its own
 * origin finds nothing, and the failure is silent.
 */
export function mediaSrc(value: string | undefined | null, base?: URL): string | undefined {
  const raw = String(value ?? '').trim();
  if (raw === '') return undefined;

  // An external host is left entirely alone. A same-origin or root-relative media URL
  // is passed through, because rewriting it would risk changing a URL an author
  // wrote by hand in Markdown.
  if (/^https?:\/\//i.test(raw)) return raw;

  if (raw.startsWith('/')) {
    return base ? new URL(raw, base).href : raw;
  }
  const resolved = mediaUrl(raw);
  return base ? new URL(resolved, base).href : resolved;
}
