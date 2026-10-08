/**
 * The Core JS Contract.
 *
 * ARCHITECTURE.md §45: JavaScript is *not* part of a theme. It ships from
 * `astro/public/` at fixed URLs, it is identical for every theme, and it binds
 * to markup through the `data-cms-*` attributes below. A theme may restyle a
 * control, move it, wrap it in anything, or rename its CSS classes; as long as
 * the contract attributes and the form field names survive, the behaviour does
 * too.
 *
 * The asymmetry is deliberate. The behaviour layer (API calls, CSRF, sessions,
 * redirects) is core-owned and stable; the presentation layer is theme-owned and
 * replaceable. Neither can break the other by accident.
 *
 * Two properties are mechanically enforced by `make arch`:
 *
 *  - every attribute and URL below must appear in the core scripts, so renaming
 *    one here without updating `astro/public/*.js` fails the build;
 *  - the core scripts may not select elements by CSS class, so a theme cannot
 *    break behaviour by renaming a class.
 *
 * Everything in this module is a constant. It is safe for a theme to import: it
 * performs no I/O, reads no environment and calls no API.
 */

/**
 * Attributes the core scripts read.
 *
 * Everything a theme must emit for a control to keep working is here. Nothing
 * in the contract is a CSS class: a class is a presentation decision, and
 * presentation is exactly what a theme is allowed to change.
 */
export const ATTR = {
  /** Marks a form the core script drives. Value: one of `FORM`. */
  form: 'data-cms-form',
  /** Marks a button the core script drives. Value: one of `ACTION`. */
  action: 'data-cms-action',
  /** Marks a live region the core script updates. Value: `status`. */
  status: 'data-cms-status',
  /**
   * The kind of message currently in the live region.
   *
   * The core script writes this attribute rather than a CSS class. That is the
   * difference between a theme being able to restyle a success message and not:
   * a class name is a presentation decision, and the core has no business making
   * one. A theme styles `[data-cms-notice-kind='error']` in whatever way it likes.
   */
  noticeKind: 'data-cms-notice-kind',
  /** Marks an element shown while a request is in flight. Value: `spinner`. */
  spinner: 'data-cms-spinner',
  /**
   * Which collection a button or form acts on.
   *
   * `post` or `page` for a content delete; `css` or `js` for a custom asset. Both
   * answer the same question — "which of the collections under /admin does this
   * control belong to" — and one attribute for it keeps the contract from growing a
   * second spelling of the same idea. `css` and `js` are the two types the CMS
   * accepts, so the core can build `/api/v1/admin/custom/${kind}` from it and still
   * never reach a handler that was not registered for exactly that type.
   */
  kind: 'data-cms-kind',
  /**
   * Warn before a form's unsaved edits are lost.
   *
   * Marks a form that owns a textarea. Present means "this form has text the admin
   * typed", and the core script then guards navigation and mode switches. It is an
   * opt-in marker rather than a blanket behaviour because most admin forms are
   * filters and checkboxes, where a dirty-state prompt is noise.
   */
  unsavedGuard: 'data-cms-unsaved-guard',
  /**
   * The theme id that was active when this page was rendered.
   *
   * Read after a settings save so the script can reload only when the theme
   * actually changed, rather than on every save.
   */
  initialTheme: 'data-cms-initial-theme',
  /** Confirmation prompt shown before a destructive action runs. */
  confirm: 'data-cms-confirm',
  /** Associates an error message with the named form field. */
  errorFor: 'data-cms-error-for',
  /**
   * The per-session CSRF token, rendered by the core into a form or button.
   *
   * A theme never generates, derives or validates this value; it only places
   * the one the page was given. That is why a theme cannot weaken CSRF.
   */
  csrf: 'data-cms-csrf',
  /** `create` or `update`, for the content editors. */
  mode: 'data-cms-mode',
  /**
   * Marks a region of the page the core script reads or writes as a
   * whole — the Markdown editor's textarea and its preview pane, for
   * example. A region is addressed by role rather than by class, so a
   * theme owns the appearance and the core owns the addressing.
   */
  region: 'data-cms-region',
  /** The slug of the content being edited or posted to. */
  slug: 'data-cms-slug',
  /** Database id of the record a button acts on (comment, media item). */
  id: 'data-cms-id',
  /** The status a moderation button sets. */
  statusValue: 'data-cms-status-value',
  /**
   * The literal text a copy button puts on the clipboard (§91).
   *
   * An attribute rather than a positional argument, and "the element's own text"
   * rather than a URL the core computed, so the theme stays the single authority on
   * what a user is shown and the core only performs the clipboard write.
   */
  copyText: 'data-cms-copy-text',
  /** Post slug the comment form belongs to. */
  post: 'data-cms-post',
  /** Where to send the visitor after a successful sign-in. */
  next: 'data-cms-next',
  /** `true` when the login form is the one-time setup form. */
  setup: 'data-cms-setup',
  /** Container the comment script prepends a newly created comment into. */
  list: 'data-cms-list',
  /**
   * `<template>` holding the markup for one comment, cloned when a comment is
   * accepted without a reload.
   *
   * This is how the theme keeps ownership of the markup: the script clones and
   * fills it with textContent instead of building the element and assigning the
   * theme's class names.
   */
  commentTemplate: 'data-cms-comment-template',
  /**
   * Placeholder inside the template: `nickname`, `created-at` or `content`.
   *
   * The script writes textContent into whichever element carries it, so a theme
   * may reorder the template freely as long as these three exist.
   */
  field: 'data-cms-field',
  /** When the form was opened, in epoch ms. Used for spam timing. */
  startedAt: 'data-cms-started-at',
  /** When the upload form was opened. Same use as `startedAt`. */
  uploadStartedAt: 'data-cms-upload-started-at',
  /** The visitor's colour-scheme choice (`auto` | `light` | `dark`). */
  colorScheme: 'data-color-scheme',
  /**
   * The button that flips between light and dark.
   *
   * It flips the scheme that is *rendered*, not the one that is stored: three
   * values (`auto` included) do not fit a two-value screen, and a cycle through
   * all three has one click in it that changes nothing the reader can see.
   */
  colorSchemeToggle: 'data-color-scheme-toggle',
  /**
   * The button that goes back to following the OS.
   *
   * Separate from the toggle on purpose, so `auto` is reachable without a three
   * value cycle — and so the toggle can stay a straight light/dark switch.
   */
  colorSchemeAuto: 'data-color-scheme-auto',

  // -------------------------------------------------------------------------
  // The media picker
  //
  // ARCHITECTURE.md §120/§122: the picker is markup a theme renders and behaviour
  // the core performs. Choosing an image writes a *media URL* into a named form
  // field; an inline upload goes through the media library and writes the same kind
  // of reference. Nothing here knows about the filesystem, and nothing here uploads
  // bytes into a Markdown file.
  // -------------------------------------------------------------------------

  /**
   * The id of the picker panel a trigger opens.
   *
   * Named rather than implicit so a screen can hold several pickers — a cover, a site
   * icon — and have them open independently without either guessing which one it is.
   */
  picker: 'data-cms-picker',
  /**
   * A picker panel. Its value is the name of the form field the chosen item is
   * written into.
   */
  mediaPicker: 'data-cms-media-picker',
  /**
   * Which half of an item the target field wants: `url` or `id`.
   *
   * A Markdown reference is a URL (ARCHITECTURE.md §120); a settings value names
   * the asset (ARCHITECTURE.md §116). Two kinds of consumer, one picker.
   */
  mediaValue: 'data-cms-media-value',
  /** The picker's filter box. Filtering is presentation, so it happens here. */
  mediaSearch: 'data-cms-media-search',
  /** The container whose items the filter hides. */
  mediaGrid: 'data-cms-media-grid',
  /** One selectable item. */
  mediaItem: 'data-cms-media-item',
  /** The lowercase text the filter matches against. Precomputed by the theme. */
  mediaName: 'data-cms-media-name',
  /**
   * The picker's "current value" label.
   *
   * The script writes the chosen value into it so the screen stops claiming to show
   * the previous one. It is an attribute rather than a class precisely because the
   * core script may not select by class — the label's appearance is the theme's.
   */
  mediaCurrent: 'data-cms-media-current',
  /** The empty-state row, shown when the filter hides everything. */
  mediaEmpty: 'data-cms-media-empty',
  /** The public URL a chosen item carries. */
  mediaUrl: 'data-cms-media-url',
  /** The media id a chosen item carries. */
  mediaId: 'data-cms-media-id',
  /**
   * The button that submits an upload control.
   *
   * A separate control because the upload lives in a plain div when it is nested
   * inside the editor's own form — HTML forbids a nested <form>, and a parser drops
   * the inner tag, which would turn "upload and insert" into "save the post".
   */
  uploadSubmit: 'data-cms-upload-submit',
  /** A form field name to write the uploaded image's URL into. */
  insertInto: 'data-cms-insert-into',
  /**
   * A textarea to insert an uploaded image into, as Markdown.
   *
   * ARCHITECTURE.md §122: the reference is inserted, never the bytes. A base64
   * `data:` image in a Markdown file is content the delivery layer cannot manage,
   * cannot cache and cannot re-encode.
   */
  insertMarkdown: 'data-cms-insert-markdown',
} as const;

/**
 * Attributes that are emitted for humans and for debugging, and that no script
 * reads.
 *
 * ARCHITECTURE.md §33: the behaviour layer must not know which theme is active, and
 * it does not need to — a theme switch changes HTML and CSS, and the core's
 * JavaScript keeps working unchanged. These attributes exist so an operator
 * looking at a page can tell which theme rendered it. If a script ever started
 * reading one of them, that would be the moment the two layers stopped being
 * independent, so the architecture check forbids it by name.
 *
 * Kept separate from `ATTR` rather than merely unused, because "unused" is not a
 * property an audit can check.
 */
export const DOCUMENT_ATTRS = {
  /**
   * The id of the active theme pack, emitted on `<html>` by every theme layout.
   *
   * Informational only. Nothing in `astro/public/` reads it, and no behaviour may
   * branch on it.
   */
  theme: 'data-cms-theme',
} as const;

/** Forms the core scripts bind to. */
export const FORM = {
  login: 'login',
  comment: 'comment',
  post: 'post',
  page: 'page',
  settings: 'settings',
  customCode: 'custom-code',
  /**
   * The custom-asset create/edit form.
   *
   * `data-cms-mode` says which, `data-cms-id` names the file being edited, and
   * `data-cms-kind` says whether it is CSS or JavaScript.
   */
  customAsset: 'custom-asset',
  /**
   * The Markdown style-template create/edit form.
   *
   * `data-cms-mode` says which and `data-cms-id` names the file being
   * edited. The preview textarea beside it is a region
   * (`data-cms-region="markdown-editor"`), not a form: its content is
   * a sandbox and is never saved.
   */
  markdownTemplate: 'markdown-template',
  upload: 'upload',
  /**
   * The alt-text form on one media item.
   *
   * ARCHITECTURE.md §134: alt is presentation metadata an admin may set, and
   * Markdown's own `![alt]` still wins on a page (§135). It lives on the media row,
   * never in the Markdown file, because it is a property of the asset.
   */
  mediaAlt: 'media-alt',
} as const;

/** Buttons the core scripts bind to. */
export const ACTION = {
  deleteContent: 'delete-content',
  setCommentStatus: 'set-comment-status',
  deleteComment: 'delete-comment',
  deleteMedia: 'delete-media',
  /** Opens a picker panel. */
  pickMedia: 'pick-media',
  /** Closes the nearest open picker panel. */
  closeMedia: 'close-media',
  /** Writes an item's value into the picker panel's target field. */
  useMedia: 'use-media',
  /**
   * Empties the derived representation cache (ARCHITECTURE.md §83).
   *
   * Originals are untouched: the next request regenerates what it needs from them.
   */
  clearImageCache: 'clear-image-cache',
  /** Rescans content/ and rebuilds the derived usage index (§51). */
  rebuildMediaUsage: 'rebuild-media-usage',
  /**
   * Enables or disables one custom asset (ID-33).
   *
   * `data-cms-status-value` carries the *target* state, not the current one, so the
   * button is rendered from the asset's own state and cannot be ambiguous: the label
   * a reader sees and the request the core sends come from the same value.
   */
  toggleCustomAsset: 'custom-asset-toggle',
  /**
   * Deletes one custom asset file (ID-33).
   *
   * Carries `data-cms-confirm`, because a delete here removes a file from disk with
   * no undo and no revision history — the audit log records that it happened, not
   * what the file said.
   */
  deleteCustomAsset: 'custom-asset-delete',
  /**
   * Enables or disables one Markdown style template (§34).
   *
   * `data-cms-status-value` carries the *target* state, exactly as
   * `custom-asset-toggle` does, so the button is rendered from the
   * template's own state and cannot be ambiguous.
   */
  toggleMarkdownTemplate: 'markdown-template-toggle',
  /**
   * Deletes one Markdown style template file (§34).
   *
   * Carries `data-cms-confirm`, because a delete here removes a file
   * from disk with no undo and no revision history.
   */
  deleteMarkdownTemplate: 'markdown-template-delete',
  /**
   * Renders the Markdown editor's sandbox through the real render path
   * (§34): a POST to `/api/v1/markdown/preview` whose response is
   * injected into the `markdown-preview` region.
   */
  markdownPreview: 'markdown-preview',
  /**
   * Copies the element's own `data-copy-text` to the clipboard (§91).
   *
   * The value is an attribute rather than a positional argument so the theme decides
   * *what* is copied and the core decides *how* — and so a theme can put the button
   * somewhere other than next to the text without breaking the binding.
   */
  copyText: 'copy-text',
  /**
   * Creates the local Git backup repository and makes the initial
   * commit (Git Backup Phase 1).
   *
   * A local commit only: a remote push is a later phase and is not
   * bound here.
   */
  backupInitialize: 'backup-initialize',
  /**
   * Commits the current source content into the local Git backup
   * repository (Git Backup Phase 1). A local commit only — never a
   * push — and it is refused with "nothing to commit" when the
   * content has not changed since the last backup.
   */
  backupCommit: 'backup-commit',
} as const;

/**
 * Core script URLs.
 *
 * Every theme loads exactly these. They do not change when the theme does, which
 * is the observable form of "JS is not part of the theme": the browser caches
 * one set of scripts no matter which theme is active.
 *
 * Each script is a no-op when its target markup is absent, which is why loading
 * all of them is safe.
 *
 * `/cms.js` appears in both lists on purpose. The sign-in form is rendered
 * inside the *public* layout, so a public page can legitimately carry core
 * behaviour. Deciding that per page would mean either a theme choosing (which
 * would let a theme drop the script its own markup needs) or a page choosing
 * (which would make the set depend on the route rather than on the core). One
 * fixed set per layout keeps both impossible.
 */
export const CORE_SCRIPT = {
  /** Applies the stored colour scheme before first paint. */
  colorScheme: '/color-scheme.js',
  /** All admin behaviour. */
  cms: '/cms.js',
  /** The public comment form. */
  comments: '/comments.js',
  /** content/system/custom.js, served as an external same-origin resource. */
  custom: '/custom.js',
} as const;

/**
 * Scripts that must run before the first paint.
 *
 * Loaded in `<head>` without `defer`, because deferring them would mean the
 * document renders once in the wrong colour scheme and corrects itself — a
 * visible flash, and the reason this file is a head script rather than a body
 * script in the first place.
 */
export const HEAD_SCRIPTS: readonly string[] = [CORE_SCRIPT.colorScheme];

/** Deferred scripts for visitor-facing pages. */
export const PUBLIC_BODY_SCRIPTS: readonly string[] = [
  CORE_SCRIPT.comments,
  CORE_SCRIPT.custom,
  CORE_SCRIPT.cms,
];

/** Deferred scripts for the admin area. */
export const ADMIN_BODY_SCRIPTS: readonly string[] = [CORE_SCRIPT.cms];

/** The complete set a public layout loads, head and body together. */
export const PUBLIC_SCRIPTS: readonly string[] = [...HEAD_SCRIPTS, ...PUBLIC_BODY_SCRIPTS];

/** The complete set an admin layout loads, head and body together. */
export const ADMIN_SCRIPTS: readonly string[] = [...HEAD_SCRIPTS, ...ADMIN_BODY_SCRIPTS];

/**
 * content/system/custom.css.
 *
 * Not a theme file and not core CSS: it is an admin-authored
 * override. A fresh install has no such file, and the
 * endpoint answers 200 with an empty body — the link is
 * always present, so an admin's first save needs no layout
 * change. It is deliberately *not* in the theme layer, so
 * whatever it says wins the cascade over the theme.
 */
export const CUSTOM_STYLESHEET = '/custom.css';

/**
 * The Markdown content stylesheet.
 *
 * The presentation layer for rendered Markdown content (ARCHITECTURE.md
 * §34): every enabled template in `content/system/markdown/`, aggregated
 * per request. Public layouts link it after the theme stylesheet and
 * before `/custom.css`, so the theme owns the page, the templates own
 * the article, and the admin's own overrides still win both.
 */
export const MARKDOWN_STYLESHEET = '/markdown.css';

/** Every core script URL, for the architecture check to verify. */
export const ALL_CORE_SCRIPTS: readonly string[] = [
  ...HEAD_SCRIPTS,
  ...PUBLIC_BODY_SCRIPTS,
  ...ADMIN_BODY_SCRIPTS,
  CORE_SCRIPT.custom,
];
