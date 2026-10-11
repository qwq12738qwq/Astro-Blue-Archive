/**
 * View-model builders: the core's only way to talk to a theme.
 *
 * ARCHITECTURE.md §44: the core converts loader entries, API responses and
 * runtime settings into the plain shapes in `theme/contract.ts`, then hands them
 * over. A theme therefore cannot see a loader entry, a `fetch` Response, a
 * database row or an error object, and it cannot reach one: everything it is
 * given has already been reduced to strings, dates and booleans.
 *
 * This is also where "current" is computed. Active-link state depends on the
 * request URL and on the content model, both of which the core owns; a theme
 * asked to work it out would need the URL, and a theme given the URL would have
 * a second source of truth for routing.
 */
import type {
  AdminNavLink,
  AdminShellView,
  CommentsView,
  CoverRef,
  ListingView,
  NavLink,
  PageView,
  PostSummary,
  PostView,
  SiteView,
  TagRef,
  ThemeNotices,
  AdminTitles,
} from './contract';

import type { PageData } from '../lib/schema';
import type { PageEntry, PostEntry } from '../lib/queries';
import type { PublicComment } from '../lib/comments';
import type { SeoInput } from '../lib/seo';
import { seoView } from '../lib/seo';
import type { SiteSettings } from '../lib/site';
import { mediaSrc } from '../lib/media';
import { listThemes } from './registry';

// ---------------------------------------------------------------------------
// Public
// ---------------------------------------------------------------------------

/**
 * Tags become absolute URLs here, once.
 *
 * `tags` is tolerated as nullable on purpose. It arrives from two places — the
 * content loader, which guarantees an array, and the admin API, which is JSON and
 * therefore able to say `null` — and a view builder that threw on the second shape
 * took a whole admin screen down with it (ARCHITECTURE.md ID-20). A view model is
 * the boundary; a boundary that can be crashed by a field's absence is not one.
 */
export function tagRefs(tags: readonly string[] | null | undefined, currentTag?: string): TagRef[] {
  return (tags ?? []).map((name) => ({
    name,
    href: `/tags/${name}`,
    current: name === currentTag,
  }));
}

/**
 * The public navigation: Home, then standalone pages in navOrder.
 */
export function navLinks(
  navPages: readonly PageData[],
  currentPath: string,
  /**
   * The theme's name for the home entry.
   *
   * `navLinks` lives in the core, so a literal here would print "Home" inside a
   * Chinese header.
   */
  notices: ThemeNotices = {},
): NavLink[] {
  const pages = [...navPages]
    .filter((page) => page.draft !== true)
    .sort((a, b) => {
      const ao = a.navOrder ?? 1000;
      const bo = b.navOrder ?? 1000;
      if (ao !== bo) return ao - bo;
      return a.title.localeCompare(b.title);
    });

  const isCurrent = (href: string) =>
    currentPath === href || (href !== '/' && currentPath.startsWith(`${href}/`));

  // The home entry's name is the theme's, like every other word on the page:
  // `navLinks` is core, so a literal here would say "Home" inside a Chinese header.
  const links: NavLink[] = [
    { href: '/', label: notices.homeNav ?? 'Home', current: isCurrent('/') },
  ];

  for (const page of pages) {
    const href = `/${page.slug}`;
    links.push({ href, label: page.title, current: isCurrent(href) });
  }

  return links;
}

/**
 * Everything the shell needs that is not the content of one page.
 *
 * The SEO block is built here rather than in the layout because ARCHITECTURE.md §38
 * makes the title format a CMS decision: `Post | Blog` written in three layouts is
 * three chances to disagree.
 */
export function siteView(options: {
  settings: SiteSettings;
  url: URL;
  /** Astro.site, when the deployment declares one. */
  site?: URL | undefined;
  navPages: readonly PageData[];
  /** How the current document is described in the head. */
  seo: Omit<SeoInput, 'base' | 'path' | 'settings'>;
  /** Language for the shared core scripts. See ThemeNotices. */
  notices: ThemeNotices;
  /** Per-screen names, so a core page's English screen name becomes a themed word. */
  screenTitles: AdminTitles;
}): SiteView {
  const { settings, url, site, navPages, seo, notices, screenTitles } = options;
  const base: URL = site ?? new URL(url.origin);
  // A core page names its own screen in English; the theme supplies the word. See
  // AdminTitles. Applied here rather than at each call site so a new admin page cannot
  // forget, and so `adminSeo` and this path cannot disagree.
  const head = seoView({
    ...seo,
    title: seo.title ? (screenTitles[seo.title] ?? seo.title) : seo.title,
    adminSuffix: screenTitles.adminSuffix,
    base,
    path: url.pathname,
    settings,
  });

  return {
    title: settings.siteTitle,
    subtitle: settings.siteSubtitle,
    description: settings.siteDescription,
    icon: head.icon,
    themeId: settings.themeId,
    nav: navLinks(navPages, url.pathname, notices),
    notices,
    canonical: head.canonical,
    path: url.pathname,
    seo: head,
  };
}

/**
 * A cover image reference.
 *
 * ARCHITECTURE.md §133 asks for dimensions so the browser reserves the right box.
 * For a *public* page the core does not have them, and there are two reasons not to go
 * and get them: `content/*.md` stores a URL rather than media metadata, and looking
 * them up would mean a public page render depends on the backend — which ID-14 forbids
 * outright. So the attributes are emitted only when they are already known (the admin
 * screens read them from the media API) and the themes reserve the box in CSS with an
 * `aspect-ratio` instead. The alternative — emitting `width=""` — reserves nothing and
 * looks like it does.
 */
function coverRef(
  cover: string | undefined,
  options: {
    /** True when the image is decorative, e.g. a card next to the title it repeats. */
    decorative?: boolean;
    title: string;
    width?: number;
    height?: number;
    lazy: boolean;
    base?: URL;
  },
): CoverRef | undefined {
  const url = mediaSrc(cover, options.base);
  if (!url) return undefined;
  return {
    url,
    // ARCHITECTURE.md §135: Markdown's own alt wins. This is the fallback, and a
    // decorative cover gets an empty alt rather than a screen reader reading the same
    // words twice.
    alt: options.decorative ? '' : options.title,
    width: options.width,
    height: options.height,
    lazy: options.lazy,
  };
}

/** One listing entry. Never carries the body. */
export function postSummary(
  post: PostEntry,
  currentTag?: string,
  options: { base?: URL; lazy?: boolean; decorative?: boolean } = {},
): PostSummary {
  return {
    slug: post.id,
    href: `/posts/${post.id}`,
    title: post.data.title,
    description: post.data.description,
    date: post.data.date,
    updated: post.data.updated,
    tags: tagRefs(post.data.tags, currentTag),
    draft: post.data.draft === true,
    cover: coverRef(post.data.cover, {
      // §135: in a listing the image sits beside the title it repeats, so an alt
      // would make a screen reader say the same thing twice.
      decorative: options.decorative ?? true,
      title: post.data.title,
      lazy: options.lazy ?? true,
      base: options.base,
    }),
    path: '',
  };
}

export function postSummaries(
  posts: readonly PostEntry[],
  currentTag?: string,
  options: { base?: URL; lazy?: boolean; decorative?: boolean } = {},
): PostSummary[] {
  return posts.map((post) => postSummary(post, currentTag, options));
}

/**
 * The home/tag listing.
 *
 * `emptyMessage` is written here rather than in the theme so the wording cannot
 * drift between themes, and so a theme cannot accidentally tell a visitor that
 * content exists when the loader returned none.
 */
export function listingView(options: {
  /**
   * Which listing this is, rather than a pre-baked English heading.
   *
   * The earlier signature took `heading: 'Posts tagged “x”'` and had to recognise its own
   * English to find the tag again. Parsing a sentence to recover a value it already had
   * is how a translation breaks silently, so the core now says *which* listing it is and
   * the theme supplies the words.
   */
  heading: 'latest' | 'tagged';
  /** Required when `heading` is 'tagged'; substituted into `listing.tagged`. */
  tag?: string;
  posts: readonly PostEntry[];
  emptyMessage: 'none' | 'untagged';
  currentTag?: string;
  base?: URL;
  notices: ThemeNotices;
}): ListingView {
  const { heading, tag, posts, emptyMessage, currentTag, base, notices } = options;

  // An untranslated key falls through to the English default rather than rendering an
  // empty heading, so a half-finished translation still produces a readable page.
  const word = (key: string, fallback: string): string => {
    const value = notices[key as keyof ThemeNotices];
    return typeof value === 'string' && value !== '' ? value : fallback;
  };

  const labelled =
    heading === 'tagged'
      ? word('listing.tagged', 'Posts tagged “{tag}”').replace('{tag}', tag ?? '')
      : word('listing.latest', 'Latest posts');

  return {
    heading: labelled,
    // The description feeds the meta description and the Open Graph copy, so it is the
    // same resolved string rather than a second English literal to keep in step.
    description: labelled,
    posts: postSummaries(posts, currentTag, { base }),
    emptyMessage:
      emptyMessage === 'untagged'
        ? word('listing.emptyTagged', 'No posts with that tag.')
        : word('listing.empty', 'No posts yet. Add a Markdown file to content/posts/.'),
  };
}

/**
 * A single post, with the rendered body attached.
 *
 * `content` is the component the page got from `render()`, not a string. Handing
 * over HTML would invite `set:html`, which ARCHITECTURE.md D4 forbids and
 * `make arch` rejects anywhere in the app.
 */
/**
 * A single post.
 *
 * `decorative: false` is the one thing this adds to the summary it is built from. In
 * a listing the cover sits beside the title it repeats, so an empty alt is right
 * (§135); here it is the only representation of the image on the page, and a screen
 * reader that skips it entirely learns nothing about what the post is illustrated
 * with. The title is the fallback because the frontmatter carries a URL and nothing
 * else — Markdown's own alt is not available to a cover.
 */
export function postView(
  post: PostEntry,
  content: unknown,
  currentTag?: string,
  options: { base?: URL } = {},
): PostView {
  return {
    ...postSummary(post, currentTag, {
      base: options.base,
      lazy: false,
      decorative: false,
    }),
    content,
  };
}

/** A standalone page. */
export function pageView(
  page: PageEntry,
  content: unknown,
  options: { base?: URL } = {},
): PageView {
  return {
    slug: page.id,
    href: `/${page.id}`,
    title: page.data.title,
    description: page.data.description,
    tags: tagRefs(page.data.tags),
    draft: page.data.draft === true,
    cover: coverRef(page.data.cover, {
      decorative: false,
      title: page.data.title,
      // §132: a page's cover is above the fold on a page whose only content is that
      // page, so it must not be lazy — it is usually the LCP element.
      lazy: false,
      base: options.base,
    }),
    content,
  };
}

/** Comments as plain data. The bodies stay strings — never markup. */
export function commentsView(options: {
  postSlug: string;
  comments: readonly PublicComment[];
  enabled: boolean;
  autoModerate: boolean;
}): CommentsView {
  const { postSlug, comments, enabled, autoModerate } = options;
  return {
    postSlug,
    comments: comments.map((comment) => ({
      nickname: comment.nickname,
      content: comment.content,
      createdAt: comment.createdAt,
    })),
    enabled,
    autoModerate,
  };
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

/**
 * The admin navigation.
 *
 * These hrefs are the framework's URL contract, not a theme's choice
 * (ARCHITECTURE.md §21). A theme renders them; it cannot add a route, remove one,
 * or point one elsewhere.
 */
const ADMIN_NAV: { href: string; label: string; key: string }[] = [
  { href: '/admin', label: 'Dashboard', key: 'dashboard' },
  { href: '/admin/posts', label: 'Posts', key: 'posts' },
  { href: '/admin/pages', label: 'Pages', key: 'pages' },
  { href: '/admin/comments', label: 'Comments', key: 'comments' },
  { href: '/admin/media', label: 'Media', key: 'media' },
  { href: '/admin/custom-code', label: 'Custom code', key: 'custom-code' },
  { href: '/admin/settings', label: 'Settings', key: 'settings' },
];

/**
 * The admin sidebar.
 *
 * The labels come from the same `adminTitles` bag the browser tab uses, because they
 * are the same words: "Dashboard" is a screen name in both places. One bag means a
 * translated admin cannot end up with a Chinese tab above an English sidebar.
 */
export function adminNavLinks(active?: string, titles: AdminTitles = {}): AdminNavLink[] {
  return ADMIN_NAV.map((item) => ({
    href: item.href,
    label: titles[item.label] ?? item.label,
    key: item.key,
    current: item.key === active,
  }));
}

/** The installed themes, with the active one flagged for the settings screen. */
export function installedThemes(
  currentId: string,
): { id: string; name: string; current: boolean }[] {
  return listThemes().map((theme) => ({
    id: theme.id,
    name: theme.name,
    current: theme.id === currentId,
  }));
}

/** Shell context for every admin screen. */
export function adminShell(options: {
  title: string;
  active?: string;
  username: string;
  themeId: string;
  themeName: string;
  siteTitle: string;
  /** Language for the shared core scripts. See ThemeNotices. */
  notices: ThemeNotices;
  /** Per-screen names, shared with the sidebar. See AdminTitles. */
  titles: AdminTitles;
}): AdminShellView {
  const { title, active, username, themeId, themeName, siteTitle, notices, titles } = options;
  return {
    title,
    siteTitle,
    active,
    nav: adminNavLinks(active, titles),
    username,
    themeId,
    themeName,
    installedThemes: installedThemes(themeId),
    notices,
  };
}

/** Re-exported so a page can type its editor view without reaching into lib/. */
export type { PageData };
