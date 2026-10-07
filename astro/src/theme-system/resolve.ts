/**
 * The Theme Resolver.
 *
 * ARCHITECTURE.md §19: this is the single place a request turns into a theme.
 * Middleware runs it once and puts the result on `Astro.locals.theme`; every page
 * and endpoint then reads that. A page that resolved its own theme would be a
 * second authority, and the two would eventually disagree.
 *
 * The resolution order is: stored `themeId` setting → registry lookup → default.
 * Only the *reference* is put on locals. Resolving means reading a cached setting,
 * which is the one piece of work a theme switch has to pay for, and paying it once
 * per request rather than once per page keeps a page that renders several themed
 * components consistent with itself.
 */
import type { ResolvedTheme } from './contract';
import { getSiteSettings } from '../lib/site';
import { resolveTheme } from './registry';

/**
 * Resolve the theme for the current request. Never throws.
 *
 * `getSiteSettings()` already falls back, caches, coalesces and opens a circuit
 * breaker, so an unreachable backend yields the last known theme or the default
 * rather than an error page.
 */
export async function resolveRequestTheme(): Promise<ResolvedTheme> {
  const { themeId } = await getSiteSettings();
  const theme = resolveTheme(themeId);
  return {
    id: theme.id,
    name: theme.name,
    notices: theme.notices,
    adminTitles: theme.adminTitles,
    public: theme.public,
    admin: theme.admin,
  };
}
