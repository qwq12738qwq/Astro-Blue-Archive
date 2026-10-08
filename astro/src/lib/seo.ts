/**
 * Document metadata: the single place that decides what the `<head>` says.
 *
 * ARCHITECTURE.md §77: title, description, canonical, favicon and
 * Open Graph are decided once, here, in the core. A layout renders
 * them; it does not decide them. That is the same rule the theme
 * contract follows everywhere else: the core owns what is true, the
 * theme owns how it looks.
 *
 * ARCHITECTURE.md §38: the document title format is a CMS decision, not a per-layout
 * one. `Post | Blog` written in three layouts is three chances to disagree, and the
 * third is the browser tab of a page whose title is wrong.
 *
 * ARCHITECTURE.md §36/§39: nothing here reads the database or the filesystem. The
 * caller passes plain values and a base URL; the result is plain values.
 */
import type { SiteSettings } from './site';
import { mediaSrc } from './media';

/** What kind of document is being described. Drives the title format and og:type. */
export type DocumentKind = 'home' | 'post' | 'page' | 'admin' | 'listing';

export type SeoInput = {
  kind: DocumentKind;
  /** The page's own title, without the site name. Empty on the home page. */
  title?: string;
  /** The page's own description, without any fallback chain. */
  description?: string;
  /** Absolute base for canonical and og:url. */
  base: URL;
  /** The current pathname, used for the canonical URL. */
  path: string;
  settings: SiteSettings;
  /** A cover image: a media path, a root-relative URL, or an absolute URL. */
  cover?: string;
  publishedAt?: Date;
  modifiedAt?: Date;
  /** Admin screens are never indexed. */
  noindex?: boolean;
  /**
   * The word an admin tab gets after the site name.
   *
   * Supplied by the theme for the same reason the screen name is (§78 decides the
   * *format*; the *words* are presentation). Defaults to "Admin".
   */
  adminSuffix?: string;
};

/** The favicon the layout should render. */
export type IconRef = {
  href: string;
  /**
   * The MIME type for the `type` attribute, or an empty string when the extension
   * is not one this resolver recognises.
   *
   * Guessing would be worse than saying nothing: a browser told `image/svg+xml`
   * about a GIF may refuse to use it, and the symptom is a silently missing icon
   * with nothing in any log. An absent `type` means "let the browser sniff", which is
   * exactly what it is for.
   */
  type: string;
};

/** Everything a layout needs to write the head, and nothing it has to compute. */
export type SeoView = {
  documentTitle: string;
  description: string;
  canonical: string;
  icon: IconRef;
  og: {
    title: string;
    description: string;
    type: string;
    url: string;
    image?: string;
  };
  /** The site itself, as distinct from `documentTitle` (§39). */
  siteName: string;
  publishedTime?: string;
  modifiedTime?: string;
  noindex: boolean;
};

/**
 * The core default favicon.
 *
 * ARCHITECTURE.md §35: a site with no icon configured still needs one, and that
 * fallback must be a *core* asset. A theme is not allowed to name a favicon — two
 * themes would disagree, and swapping the theme would swap the site's identity.
 */
export const DEFAULT_ICON: IconRef = { href: '/favicon.png', type: 'image/png' };

/**
 * The brand mark the header, footer and admin shell show beside
 * the site title.
 *
 * Distinct from the favicon on purpose: the tab icon keeps the
 * original core mark, while the in-page brand is the KivoTos
 * artwork, served from the core's public tree at the path it
 * occupies there. A theme names its own mark; the core owns
 * where that mark lives.
 */
export const BRAND_MARK =
  '/WordPress/data/wp-content/themes/lolimeow-lolimeowV13.13/assets/images/KivoTos.png';

/**
 * Builds the head metadata.
 *
 * The fallback chain is deliberately total: every field is always a non-empty string
 * unless it is genuinely optional, so a layout can render `<title>{seo.documentTitle}</title>`
 * without a conditional and a mis-set description cannot produce `<meta content="">`,
 * which some crawlers read as an instruction to index the page as undescribed.
 */
export function seoView(input: SeoInput): SeoView {
  const { kind, settings, base } = input;

  const pageTitle = (input.title ?? '').trim();
  // §38: one place decides the format, and §78: the admin suffix is the theme's word.
  const documentTitle = formatTitle(kind, pageTitle, settings.siteTitle, input.adminSuffix);

  // §37/§67: page description, then site description, then subtitle, then title.
  const description =
    (input.description ?? '').trim() ||
    settings.siteDescription.trim() ||
    settings.siteSubtitle.trim() ||
    pageTitle ||
    settings.siteTitle;

  const canonical = new URL(normalisePath(input.path), base).href;

  const icon = resolveIcon(settings);

  // ARCHITECTURE.md §39: a page's cover is preferred, and the site icon is the
  // fallback, so a site with no cover art still produces a shareable preview rather
  // than an empty one.
  const image = mediaSrc(input.cover, base) ?? absoluteOrUndefined(icon.href, base);

  const ogType = kind === 'post' ? 'article' : 'website';

  return {
    documentTitle,
    description,
    canonical,
    icon,
    og: {
      title: documentTitle,
      description,
      type: ogType,
      url: canonical,
      image,
    },
    // The site's own name, never the document's. `og:site_name` is what a reader sees
    // after the headline, so "Blue Archive Notes" is right there and
    // "Blue Archive Notes | Hello World | Blue Archive Notes" is not — which is exactly
    // what documentTitle is on a post. Both themes render this field, so the resolver
    // owns the value.
    siteName: settings.siteTitle.trim(),
    publishedTime: input.publishedAt?.toISOString(),
    modifiedTime: input.modifiedAt?.toISOString(),
    noindex: input.noindex === true,
  };
}

/**
 * ARCHITECTURE.md §38: one place decides the format.
 *
 * The home page is the site itself and carries only its name; a post or a page adds
 * its own name first. An admin screen adds "Admin" last, because a moderator with
 * forty tabs open needs to tell which is which.
 */
function formatTitle(
  kind: DocumentKind,
  pageTitle: string,
  siteTitle: string,
  adminSuffix = 'Admin',
): string {
  const site = siteTitle.trim();
  switch (kind) {
    case 'home':
      return site;
    case 'admin':
      // ARCHITECTURE.md §78: an admin tab called just "Admin" tells you nothing.
      return pageTitle ? `${pageTitle} — ${site} ${adminSuffix}` : `${site} ${adminSuffix}`;
    default:
      return pageTitle && site ? `${pageTitle} | ${site}` : pageTitle || site;
  }
}

/**
 * Resolves the favicon.
 *
 * A configured icon wins, and the version token defeats a browser's favicon cache
 * without rewriting the storage path (ARCHITECTURE.md §76). Without one, the core
 * default is used — never a theme's choice.
 */
function resolveIcon(settings: SiteSettings): IconRef {
  if (!settings.siteIconUrl) return DEFAULT_ICON;
  const href =
    settings.siteIconVersion === ''
      ? settings.siteIconUrl
      : `${settings.siteIconUrl}${settings.siteIconUrl.includes('?') ? '&' : '?'}v=${settings.siteIconVersion}`;
  return { href, type: mimeForIcon(href) };
}

function mimeForIcon(href: string): string {
  const clean = href.split(/[?#]/)[0]?.toLowerCase() ?? '';
  if (clean.endsWith('.png')) return 'image/png';
  if (clean.endsWith('.ico')) return 'image/x-icon';
  if (clean.endsWith('.jpg') || clean.endsWith('.jpeg')) return 'image/jpeg';
  if (clean.endsWith('.gif')) return 'image/gif';
  if (clean.endsWith('.webp')) return 'image/webp';
  // SVG is only ever the core default, and the media layer rejects SVG uploads
  // (ARCHITECTURE.md §33), so this is not a claim about arbitrary icons.
  if (clean.endsWith('.svg')) return 'image/svg+xml';
  return '';
}

function absoluteOrUndefined(href: string, base: URL): string | undefined {
  if (!href) return undefined;
  if (/^https?:\/\//i.test(href)) return href;
  if (!href.startsWith('/')) return undefined;
  return new URL(href, base).href;
}

/** A canonical URL never carries a query string or a fragment. */
function normalisePath(pathname: string): string {
  const raw = pathname.length > 0 ? pathname : '/';
  const withoutFragment = raw.split('#')[0] ?? '/';
  return withoutFragment.startsWith('/') ? withoutFragment : `/${withoutFragment}`;
}
