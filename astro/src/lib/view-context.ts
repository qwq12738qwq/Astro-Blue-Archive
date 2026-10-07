/**
 * Site-wide context for a public page.
 *
 * ARCHITECTURE.md §19/§44: the theme is resolved once, in middleware, and reaches
 * every page through `Astro.locals.theme`. This helper is the other half of that
 * rule — it builds the `SiteView` the shell needs (title, subtitle, description,
 * icon, navigation, canonical URL, head metadata) from the content model and the
 * runtime settings, so a theme never has to read the loader or the API to render a
 * header.
 *
 * ARCHITECTURE.md §74: this is the single site-settings resolver. The header, the
 * head, the RSS feed and the admin all read the same cached document through
 * `getSiteSettings()`, so there is exactly one place that knows what the site is
 * called.
 *
 * Pages must not resolve a theme themselves. Doing so is how two components on one
 * page end up disagreeing about which theme is rendering it.
 */
import { getSiteSettings } from './site';
import { getPublishedPages } from './queries';
import { siteView } from '../theme-system/view';
import type { SeoInput } from './seo';
import type { SiteView, ResolvedTheme } from '../theme-system/contract';

/** How the calling page wants itself described. */
export type PageSeo = Omit<SeoInput, 'base' | 'path' | 'settings'>;

/**
 * Build the shell context for the current request.
 *
 * `url` and `site` are passed rather than read from the `Astro` global, because
 * `Astro` only exists inside a component's frontmatter — this is a plain module.
 * The result reads the cached settings, so it costs one map lookup per request
 * after the first.
 */
export async function getSiteView(
  url: URL,
  site: URL | undefined,
  seo: PageSeo,
  /**
   * The theme already resolved for this request.
   *
   * Passed rather than resolved here on purpose: ARCHITECTURE.md §18 gives the
   * middleware the single authority over which theme a request uses, and a second
   * `resolveTheme` call in this module is exactly the kind of thing that eventually
   * disagrees with it.
   */
  theme: ResolvedTheme,
): Promise<SiteView> {
  return siteView({
    settings: await getSiteSettings(),
    url,
    site,
    navPages: (await getPublishedPages()).map((page) => page.data),
    seo,
    notices: theme.notices,
    screenTitles: theme.adminTitles,
  });
}

export type { SiteView };
