/**
 * The Astro Theme Contract.
 *
 * This file is the whole interface between the CMS core and a theme. It is
 * deliberately tiny and deliberately one-directional:
 *
 *     core  ──view models──▶  theme  ──HTML──▶  browser
 *
 * A theme receives plain data (the view models below) and returns markup. It
 * never receives a database row, a loader entry, a `fetch` Response, a session,
 * a CSRF token *for a form it does not own the data of*, or a path it may write
 * to. It never calls the API itself. See ARCHITECTURE.md §44.
 *
 * Two rules make the contract enforceable rather than aspirational:
 *
 *  1. The core never imports a theme component except through the registry
 *     (`theme/registry.ts`). Pages reach a theme through `Astro.locals.theme`.
 *     `make arch` enforces this.
 *  2. A theme may only import from `theme/contract.ts` and `lib/`. It may not
 *     import `theme/registry.ts` (which would let one theme reach another),
 *     any API client, any filesystem writer or any `.md` content.
 *     `make arch` enforces this too.
 */

// `AstroComponentFactory` is what an `.astro` file's default export is. Astro
// exposes it from `astro/runtime/server/index.js` — its own `content.d.ts`
// imports the type from the same place — and there is no shorter public alias,
// so the contract refers to it here once rather than in every theme.
//
// It is a *type* import: nothing from Astro's server runtime is loaded at build
// time, and no internal module is added to the dependency graph.
import type { AstroComponentFactory } from 'astro/runtime/server/index.js';

/** An `.astro` component. Themes ship nothing else. */
export type ThemeComponent = AstroComponentFactory;

// ---------------------------------------------------------------------------
// Shared primitives
// ---------------------------------------------------------------------------

/** A tag, already resolved to its public URL. */
export type TagRef = {
  name: string;
  href: string;
  /** True when the current request is this tag's listing page. */
  current: boolean;
};

/** A link in the site navigation, already resolved and ordered. */
export type NavLink = {
  href: string;
  label: string;
  current: boolean;
};

/**
 * Site-wide context.
 *
 * Every field is CMS settings, not theme settings (ARCHITECTURE.md §40). `title`
 * is the admin-configured site title, `subtitle` and `description` come from the same
 * settings document, and `icon` is resolved by the backend. A theme reads them and
 * defines none of them, so a theme swap cannot rename the site or change its icon.
 *
 * `themeId` is informational: a theme may display which theme is active but must
 * never branch behaviour on it, and core JavaScript never reads it.
 */
export type SiteView = {
  title: string;
  subtitle: string;
  description: string;
  /** Resolved favicon, including any cache-busting token. */
  icon: { href: string; type: string };
  themeId: string;
  /** Ordered navigation: Home, then standalone pages. */
  nav: NavLink[];
  /** Absolute canonical URL of the current request. */
  canonical: string;
  /** Path of the current request, for active-state checks. */
  path: string;
  /** Everything the document head needs, computed once by the core. */
  seo: SeoView;
  /** Language for the shared core scripts. See ThemeNotices. */
  notices: ThemeNotices;
};

/**
 * Document metadata.
 *
 * ARCHITECTURE.md §77: this is computed by `lib/seo.ts` and rendered, never
 * recomputed. A layout that assembled its own `<title>` would be the second place
 * that knows the title format, and the second place is the one that breaks.
 */
export type SeoView = {
  documentTitle: string;
  description: string;
  canonical: string;
  /**
   * The favicon, already resolved and versioned.
   *
   * ARCHITECTURE.md §35/§76: a site icon is a media asset, so it is resolved by the
   * backend and versioned here. A layout renders this and names no icon of its own,
   * which is what keeps two themes from disagreeing about the site's identity.
   */
  icon: { href: string; type: string };
  og: {
    title: string;
    description: string;
    type: string;
    url: string;
    image?: string;
  };
  /**
   * The site's own name, as distinct from `documentTitle` (§39).
   *
   * The two coincide on the home page and differ everywhere else, which is why this
   * is a separate field: a theme that reached for `documentTitle` here produced
   * "Post | Site | Site" on every article and nothing anywhere in the admin to notice.
   */
  siteName: string;
  publishedTime?: string;
  modifiedTime?: string;
  noindex: boolean;
};

// ---------------------------------------------------------------------------
// Public theme contract
// ---------------------------------------------------------------------------

/** One entry in a listing. Summaries only — never a body. */
export type PostSummary = {
  slug: string;
  href: string;
  title: string;
  description?: string;
  date: Date;
  updated?: Date;
  tags: TagRef[];
  draft: boolean;
  /**
   * The cover image, already resolved to a URL.
   *
   * ARCHITECTURE.md §80/§118: the core resolves it and the theme renders it. A theme
   * never assembles a media URL, so the frontmatter value may be a bare storage path
   * or a hand-written `/media/...` and both keep working.
   */
  cover?: CoverRef;
  /** Repository path, shown in the admin only; empty on the public site. */
  path: string;
};

/**
 * A cover image, with everything needed to render it without a layout shift.
 *
 * ARCHITECTURE.md §133: width and height come from the recorded metadata rather than
 * from the theme, and §132: the loading hint comes from the *position* in the page,
 * which the core knows and the theme does not.
 */
export type CoverRef = {
  url: string;
  alt: string;
  width?: number;
  height?: number;
  /** false for a hero or LCP image, true for one below the fold. */
  lazy: boolean;
};

/** The home page and the tag listing both render a list of summaries. */
export type ListingView = {
  /** Rendered as the page heading, e.g. "Latest posts". */
  heading: string;
  description?: string;
  posts: PostSummary[];
  /** Copy for the empty state; the core owns the wording, not the theme. */
  emptyMessage: string;
};

/** A single post. `content` is the rendered Markdown, passed as a component. */
export type PostView = PostSummary & {
  /** Rendered body, injected by the page as an Astro component. */
  content: unknown;
};

/** A standalone page at `/<slug>`. */
export type PageView = {
  slug: string;
  href: string;
  title: string;
  description?: string;
  tags: TagRef[];
  draft: boolean;
  cover?: CoverRef;
  content: unknown;
};

/**
 * Comments.
 *
 * The theme renders the list and the form. It does not own the endpoint: the
 * form must post through the core contract (`data-cms-form="comment"`), which
 * posts to `/api/v1/comments`. A theme that wants a different comment backend
 * is not implementing this contract.
 */
export type CommentView = {
  nickname: string;
  content: string;
  createdAt: string;
  status?: 'pending' | 'approved' | 'spam' | 'deleted';
};

export type CommentsView = {
  postSlug: string;
  comments: CommentView[];
  enabled: boolean;
  autoModerate: boolean;
};

/**
 * The public theme's required slots.
 *
 * This is the runtime list of what a theme must provide. The interface below is
 * typed from it, so the constant and the type cannot drift: adding a slot here
 * without adding it to the interface is a compile error, and vice versa.
 *
 * The constant exists for `make arch`, which compares every theme's manifest
 * against it. A theme missing a slot would otherwise register cleanly and then
 * resolve to `undefined` at request time, when Astro renders a page with an
 * undefined component.
 */
export const PUBLIC_SLOTS = [
  'Layout',
  'HomeView',
  'ListingView',
  'PostView',
  'PageView',
  'NotFoundView',
  'PostCard',
  'CommentSection',
  'CommentList',
] as const;

/**
 * The public theme.
 *
 * Views render whole screens; layouts own `<html>`; the rest are the units a
 * theme composes its views from. Every slot is required — a theme that cannot
 * render the comment form does not satisfy the contract.
 */
export type PublicThemeComponents = Record<(typeof PUBLIC_SLOTS)[number], ThemeComponent>;

// ---------------------------------------------------------------------------
// Admin theme contract
// ---------------------------------------------------------------------------

/** One entry in the admin navigation. */
export type AdminNavLink = {
  href: string;
  label: string;
  key: string;
  current: boolean;
};

/** Shell context: who is signed in, which theme is active, where you are. */
export type AdminShellView = {
  /** The screen name, without the site-name suffix the head adds. */
  title: string;
  /**
   * The admin-configured site title.
   *
   * ARCHITECTURE.md §40/§78: the admin belongs to the site, so the shell brands
   * itself with the site's name rather than with a literal. A literal here is how
   * "Blog admin" ends up under a site called something else entirely.
   */
  siteTitle: string;
  active?: string;
  nav: AdminNavLink[];
  username: string;
  themeId: string;
  themeName: string;
  /** Themes the deployer installed, for the settings screen. */
  installedThemes: { id: string; name: string; current: boolean }[];
  /** Language for the shared core scripts. See ThemeNotices. */
  notices: ThemeNotices;
};

/** A server-side notice rendered above a form. */
export type Notice = {
  kind: 'ok' | 'error' | 'warn' | 'info';
  message: string;
};

/** A content file the API or the loader refused. */
export type SkippedFile = { file: string; reason: string };

/**
 * The editable form of a post. Raw Markdown, never rendered HTML.
 *
 * `media` is the library, supplied so the theme can render a picker (ARCHITECTURE.md
 * §120). The picker is *markup*: choosing an item writes its URL into the `cover`
 * field, and the core script performs the request. A theme therefore never learns a
 * filesystem path, and it cannot choose a URL that is not one of ours.
 */
export type PostEditorView = {
  isNew: boolean;
  slug: string;
  title: string;
  description: string;
  date: string;
  tags: string;
  /** The cover, exactly as stored: a `/media/...` URL or a bare path. */
  cover: string;
  draft: boolean;
  body: string;
  path?: string;
  /** The media library, for the picker and the inline upload. */
  media: MediaItem[];
  /** The upload ceiling, so the form can state it. */
  mediaMaxBytes: number;
};

export type PageEditorView = {
  isNew: boolean;
  slug: string;
  title: string;
  description: string;
  date: string;
  navOrder: string;
  cover: string;
  draft: boolean;
  body: string;
  path?: string;
  media: MediaItem[];
  mediaMaxBytes: number;
};

export type PostsView = {
  items: (PostSummary & { modifiedAt: string })[];
  skipped: string[];
  invalid: SkippedFile[];
};

export type PagesView = {
  items: {
    slug: string;
    href: string;
    title: string;
    draft: boolean;
    navOrder?: number;
    path: string;
  }[];
  skipped: string[];
  invalid: SkippedFile[];
};

export type DashboardView = {
  username: string;
  posts: number;
  postDrafts: number;
  pages: number;
  pageDrafts: number;
  /** -1 means the moderation queue could not be read. */
  pendingComments: number;
  invalidFiles: number;
};

export type CommentsView_ = {
  filter: 'pending' | 'approved' | 'spam' | 'deleted' | 'all';
  /** The search term the filter form holds, so the input can be re-rendered. */
  search: string;
  /** The post slug the list is filtered to, if any. */
  postFilter: string;
  pending: number;
  counts: Record<string, number>;
  items: (CommentView & {
    id: string;
    postSlug: string;
    /** The collapsed preview for the list cell. */
    excerpt: string;
    /** Link to the post this comment belongs to, in the admin. */
    postHref: string;
    /**
     * A short, non-reversible label for the submitter's address.
     *
     * ARCHITECTURE.md §43: enough to spot a spam run, useless for recovering
     * anything. The raw address is never stored and never sent here.
     */
    ipHashIndicator: string;
    /** The submitter's client, reduced to a short label. */
    userAgentSummary: string;
    moderatedAt?: string;
  })[];
};

/** Live counters for the image pipeline, shown on the media screen. */
export type ImageCacheView = {
  /** The configured ceiling and the current use, in bytes. */
  memoryLimitBytes: number;
  memoryUsedBytes: number;
  memoryEntries: number;
  diskEntries: number;
  diskBytes: number;
  hits: number;
  misses: number;
  evictions: number;
  conversions: number;
  failures: number;
  /** The quality a converted representation is produced at. */
  quality: number;
  /** The decoded-pixel ceiling an upload is held to. */
  maxPixels: number;
};

export type MediaItem = {
  id: string;
  /** The admin's own filename, kept only as a label. */
  filename: string;
  /** Server-generated storage path, shown for support and debugging. */
  path: string;
  /**
   * The public, delivery-layer URL.
   *
   * ARCHITECTURE.md §56/§91: the admin preview and the copy button both use this, so
   * neither ever needs a filesystem path and neither can be wrong about which
   * representation a browser will receive.
   */
  url: string;
  mime: string;
  size: number;
  sizeLabel: string;
  width?: number;
  height?: number;
  /** Admin-authored fallback alt text (ARCHITECTURE.md §134). */
  alt: string;
  createdAt: string;
  /** How many times content references this asset. Derived, never stored. */
  usageCount: number;
  /** True when the file is gone but the row remains (ARCHITECTURE.md §5). */
  missing: boolean;
  /** A role the asset plays beyond content references, e.g. the site icon. */
  usedAs?: string;
  usedBy: { kind: string; slug: string; href: string; referenceCount: number }[];
};

export type MediaView = {
  items: MediaItem[];
  /** The upload ceiling, so the form can state it. */
  maxBytes: number;
  /** The decoded-pixel ceiling, so the form can state it. */
  maxPixels: number;
  cache: ImageCacheView;
  /** The active filters, so the controls render with their state. */
  filters: {
    search: string;
    mime: string;
    unused: boolean;
    missing: boolean;
  };
  /** MIME types the delivery layer serves, for the type filter. */
  types: string[];
};

export type SettingsView = {
  siteTitle: string;
  siteSubtitle: string;
  siteDescription: string;
  /** The media id currently used as the site icon, or ''. */
  siteIconMediaId: string;
  /** Its resolved public URL, for the preview. */
  siteIconUrl: string;
  /** The site's image cache counters, so the Media section can show them too. */
  webpQuality: number;
  imageMemoryCacheMB: number;
  rssEnabled: boolean;
  rssTitle: string;
  rssDescription: string;
  rssItemLimit: number;
  commentsEnabled: boolean;
  commentAutoModerate: boolean;
  /** The registry's ids, so a theme can render the switcher. */
  themeId: string;
  installedThemes: { id: string; name: string; current: boolean }[];
  /**
   * The media library, for the site-icon picker.
   *
   * ARCHITECTURE.md §116: the icon is an ordinary media asset, so it is chosen from
   * the library rather than by pasting a path. A theme renders the choice; the core
   * script performs the request.
   */
  media: MediaItem[];
  mediaMaxBytes: number;
};

export type CustomCodeView = {
  css: string;
  js: string;
  /**
   * The managed CSS/JS asset collections, plus the file the editor is
   * showing.
   *
   * The managed assets are the second half of this screen: the legacy
   * pair and the managed files are one custom-code manager (the
   * /admin/custom-assets screen was merged into this one), so the
   * screen's view model carries both halves.
   */
  assets: CustomAssetsView;
};

/**
 * One custom CSS or JavaScript file, as the admin needs to see it.
 *
 * ARCHITECTURE.md ID-33: `filename` is the identity — there is no opaque id and no
 * path — and it is also the sort key, so `order` is derived from its numeric prefix
 * rather than stored separately (ID-35).
 */
export type CustomAssetItem = {
  id: string;
  filename: string;
  type: 'css' | 'js';
  enabled: boolean;
  /** True for `custom.css`/`custom.js`: editable, never deletable, always first. */
  legacy: boolean;
  order: number;
  size: number;
  updatedAt: string;
  /** `ok`, `missing` or `invalid`. Anything but `ok` is not served. */
  status: 'ok' | 'missing' | 'invalid';
  /** Why it is not `ok`. Absent when there is nothing to explain. */
  problem?: string;
};

/**
 * The managed CSS/JS asset collections: both kinds, plus the
 * file the editor is showing.
 *
 * `editor` is null when the screen is listing rather than editing, which is
 * the default — the create form is always rendered, and an edit form appears
 * beside it only when the page was asked for one specific file.
 *
 * This is the second half of the custom-code screen: the legacy
 * pair has its own editors above it, and a legacy file opened
 * through `?name=` is answered with a note rather than an editor
 * form, because its save belongs to the legacy editors (ID-34).
 */
export type CustomAssetsView = {
  css: CustomAssetItem[];
  js: CustomAssetItem[];
  /** The per-file size ceiling, reported by the server rather than guessed here. */
  maxBytes: number;
  editor: CustomAssetItem | null;
  /** The editor file's text. Empty when `editor` is null. */
  editorContent: string;
};

/**
 * One Markdown style template, as the admin needs to see it.
 *
 * ARCHITECTURE.md §34: `filename` is the identity — there is no opaque
 * id and no path — and it is also the sort key, so `order` is derived
 * from its numeric prefix rather than stored separately (ID-35). Every
 * template is CSS, so there is no `type` field: the collection is
 * closed.
 */
export type MarkdownTemplateItem = {
  id: string;
  filename: string;
  enabled: boolean;
  order: number;
  size: number;
  updatedAt: string;
  /** `ok`, `missing` or `invalid`. Anything but `ok` is not served. */
  status: 'ok' | 'missing' | 'invalid';
  /** Why it is not `ok`. Absent when there is nothing to explain. */
  problem?: string;
};

/**
 * The Markdown style-template screen: the collection only.
 *
 * §33/§34: editing a template moved to the standalone Markdown editor
 * (`MarkdownEditorView`), reached from a row's `编辑` link. This screen
 * lists and creates; it no longer embeds an editor, so nothing squeezes
 * an editor column beside it.
 */
export type MarkdownTemplatesView = {
  templates: MarkdownTemplateItem[];
  /** The per-file size ceiling, reported by the server rather than guessed here. */
  maxBytes: number;
};

/**
 * The standalone Markdown editor (§33/§34): one screen for editing a
 * post's body or a style template's CSS, with a live preview filled by
 * the core script through `/api/v1/markdown/preview` — the same
 * `renderMarkdown()` a post page runs.
 *
 * `kind` selects the target, and the two kinds save differently. A post
 * is written back as a whole document — the PUT is a full replace, so the
 * `fields` the page loaded travel with the body — while a template is a
 * partial `{ content }` update that never touches `enabled`. The screen
 * renders the same core form bindings the old inline editors used
 * (`data-cms-form="post"` / `"markdown-template"`), so the core script
 * performs the request and the theme ships no behaviour.
 */
export type MarkdownEditorView = {
  kind: 'post' | 'template';
  /** The target's identity: a post slug or a template filename. */
  target: string;
  /** A label for the toolbar: the post title, or the template filename. */
  title: string;
  /** The Markdown (post) or CSS (template) source the editor opens with. */
  content: string;
  /**
   * Whether a template participates in `/markdown.css`. Unused for a post.
   *
   * It travels with the form because the shared save handler reads a missing
   * `enabled` as "false" — omitting it would disable the template on a save.
   */
  enabled: boolean;
  /** The post's frontmatter, resent verbatim by a post save. Null for a template. */
  fields: {
    title: string;
    slug: string;
    description: string;
    date: string;
    tags: string;
    cover: string;
    draft: boolean;
  } | null;
  /** The upload ceiling, for the inline image control (post kind). */
  mediaMaxBytes: number;
};

/**
 * The admin theme's required slots. See `PUBLIC_SLOTS` for why this is a constant
 * as well as a type.
 *
 * V1 has no modal and no pagination, so there are no slots for them. A slot for a
 * feature the core cannot route would be a contract a theme could satisfy while
 * still rendering a dead control.
 */
export const ADMIN_SLOTS = [
  'Layout',
  'LoginView',
  'DashboardView',
  'PostsView',
  'PostEditorView',
  'PagesView',
  'PageEditorView',
  'CommentsView',
  'MediaView',
  'SettingsView',
  'CustomCodeView',
  'MarkdownView',
  'MarkdownEditorView',
  'Notice',
  'Table',
] as const;

/**
 * The admin theme.
 *
 * Views render whole screens, `Layout` owns `<html>`, and the units below are the
 * shared admin building blocks. Note what is absent: no API client, no session, no
 * CSRF token, no file path. A form view receives the *values* and emits the core
 * JS contract attributes; the core script performs the request.
 */
export type AdminThemeComponents = Record<(typeof ADMIN_SLOTS)[number], ThemeComponent>;

// ---------------------------------------------------------------------------
// Theme definition
// ---------------------------------------------------------------------------

/**
 * A registered theme.
 *
 * `id` is the only value that ever reaches this object from outside the build:
 * it comes from the `themeId` setting, which the backend validates against an
 * allowlist. It selects a map entry; it is never interpolated into a path or a
 * module specifier.
 */
/**
 * The strings `/cms.js` and `/comments.js` show the reader.
 *
 * ARCHITECTURE.md §19 says the core scripts are identical for every theme and bind
 * only through `data-cms-*`. That is true of *behaviour*, and it would be a mistake to
 * extend it to language: a notice is presentation, and hard-coding one language in the
 * shared script means an English word appears in the middle of a Chinese moderation
 * queue — and, worse, that adding a language means editing a file no theme owns.
 *
 * So the core keeps the behaviour and asks the theme for the words. A theme supplies
 * this object; the layout serialises it into a `data-cms-notices` attribute; the script
 * looks a key up and falls back to the key itself, which is why a missing translation
 * is a visible untranslated token rather than a blank status line.
 *
 * The name says "notices" because that is what most of it is, but the bag is really the
 * theme's UI strings: `homeNav` is a navigation label that happens to be produced by
 * core. Splitting it into two dictionaries would mean two attributes and two lookups for
 * no gain.
 *
 * `Partial<Record<NOTICE_KEYS, string>>` on purpose: a theme may translate the subset
 * it cares about, and `make arch` fails if a key is spelled wrong.
 */
export type ThemeNotices = Partial<Record<NoticeKey, string>>;

/** Every notice the core scripts can raise. Adding one here makes it translatable. */
export const NOTICE_KEYS = [
  'saved',
  'settingsSaved',
  'savedReload',
  'selected',
  'uploaded',
  'uploadedInserted',
  'altSaved',
  'copied',
  'copyNeedsHTTPS',
  'copyFailed',
  'nothingToCopy',
  'chooseFile',
  'serverUnreachable',
  'deleted',
  'confirmDelete',
  'confirmClearCache',
  'confirmRebuildUsage',
  // Custom-asset editor (ID-33): the buffer is hand-written text with no autosave and
  // no revision history, so leaving the page with unsaved edits is the one thing this
  // screen must not do quietly.
  'confirmLeaveEditor',
  'commentPosted',
  'commentHeld',
  'saveSettingsFailed',
  'saveAltFailed',
  'clearCacheFailed',
  'rebuildUsageFailed',
  'deleteFailed',
  'updateFailed',
  'homeNav',
  // The two colour-scheme controls. `toLight`/`toDark` name what the toggle will
  // do, which is the only wording that cannot be wrong: from `auto` the next click
  // depends on the OS. `follow` is the stable name of the control that returns to
  // `auto`, whose state it carries in `aria-pressed` instead.
  'colorScheme.toLight',
  'colorScheme.toDark',
  'colorScheme.follow',
  // Listing headings and empty states. `{tag}` is substituted into 'listing.tagged'.
  'listing.latest',
  'listing.empty',
  'listing.tagged',
  'listing.emptyTagged',
] as const;

export type NoticeKey = (typeof NOTICE_KEYS)[number];

/**
 * How an admin screen is named in the browser tab and the `<title>`.
 *
 * Keyed by the English name the core page passes in — `adminSeo('Dashboard', …)` —
 * so the core keeps one spelling and the theme supplies the word. An absent key falls
 * back to the English literal, which is why a missing translation shows an English
 * screen name rather than an empty tab.
 *
 * Separate from ThemeNotices because these are titles, not toasts, and because the
 * screen names live in core pages while the toasts are raised by the shared scripts.
 */
export type AdminTitles = Record<string, string>;

export type ThemeDefinition = {
  id: string;
  name: string;
  version: string;
  /** Language for the shared core scripts. See ThemeNotices. */
  notices: ThemeNotices;
  /** Per-screen names for admin tabs. See AdminTitles. */
  adminTitles: AdminTitles;
  public: PublicThemeComponents;
  admin: AdminThemeComponents;
};

/** What the resolver puts on `Astro.locals.theme`. */
export type ResolvedTheme = {
  id: string;
  name: string;
  notices: ThemeNotices;
  adminTitles: AdminTitles;
  public: PublicThemeComponents;
  admin: AdminThemeComponents;
};
