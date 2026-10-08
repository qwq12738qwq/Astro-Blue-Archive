/**
 * Admin page context.
 *
 * ARCHITECTURE.md §10/§44: every admin screen needs the same three things — the
 * shell (navigation, identity, installed themes) and the per-session CSRF token —
 * and none of them may be computed by a theme. This builds them from the session
 * the middleware resolved, so a view receives values and never a session object.
 *
 * The token is passed through, not generated. A theme places it in a data attribute
 * and /cms.js sends it; nothing in `src/themes/` can mint one, alter one, or decide
 * whether one is needed.
 *
 * `locals` is a parameter rather than the `Astro` global, because `Astro` only
 * exists inside a component's frontmatter and this is a plain module.
 */
import type { AdminShellView, ResolvedTheme } from '../theme-system/contract';
import { adminShell, installedThemes } from '../theme-system/view';
import { getSiteSettings } from './site';
import { seoView, type SeoView } from './seo';

/** The per-request context these helpers read. */
export type AdminContext = App.Locals;

/** The CSRF token for this request, or an empty string when anonymous. */
export function csrfToken(locals: AdminContext): string {
  return locals.session?.csrfToken ?? '';
}

/** True when the middleware resolved a session. */
export function signedIn(locals: AdminContext): boolean {
  return locals.session !== null;
}

/** The username, or an empty string. */
export function username(locals: AdminContext): string {
  return locals.session?.username ?? '';
}

/**
 * The shell for a screen.
 *
 * `active` is the navigation key, which the theme compares against
 * `AdminShellView.nav[].key`. The keys and hrefs are the framework's URL contract
 * (ARCHITECTURE.md §21): a theme renders the list it is given and cannot add a
 * route.
 */
/**
 * The shell for a screen.
 *
 * `active` is the navigation key, which the theme compares against
 * `AdminShellView.nav[].key`. The keys and hrefs are the framework's URL contract
 * (ARCHITECTURE.md §21): a theme renders the list it is given and cannot add a
 * route.
 *
 * It reads the cached site settings, which is why it is async: the shell brands
 * itself with the site's own name (ARCHITECTURE.md §40), so an admin belonging to a
 * site called "Field Notes" does not say "Blog admin" in the sidebar. The read is a
 * map lookup after the first request.
 */
export async function shell(
  locals: AdminContext,
  title: string,
  active: string,
): Promise<AdminShellView> {
  const { siteTitle } = await getSiteSettings();
  return adminShell({
    title,
    active,
    username: username(locals),
    themeId: locals.theme.id,
    themeName: locals.theme.name,
    siteTitle,
    notices: locals.theme.notices,
    titles: locals.theme.adminTitles,
  });
}

/**
 * The head metadata for an admin screen.
 *
 * ARCHITECTURE.md §35/§77/§78: the admin gets the same resolver as the public site —
 * the same favicon, the same site title, the same format. It is computed by the core
 * and rendered by the theme's layout, so no admin screen can name its own.
 */
export async function adminSeo(
  /**
   * The screen name, in the core's English spelling.
   *
   * `theme.adminTitles` is keyed by exactly this string, so the core keeps one
   * spelling and the theme chooses the word. An unknown screen falls through
   * unchanged rather than rendering an empty tab.
   */
  title: string,
  url: URL,
  theme: ResolvedTheme,
): Promise<SeoView> {
  return seoView({
    kind: 'admin',
    title: theme.adminTitles[title] ?? title,
    adminSuffix: theme.adminTitles.adminSuffix ?? 'Admin',
    base: url,
    path: url.pathname,
    settings: await getSiteSettings(),
    noindex: true,
  });
}

export { installedThemes };
