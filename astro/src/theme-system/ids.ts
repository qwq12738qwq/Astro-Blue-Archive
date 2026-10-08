/**
 * The theme id allowlist.
 *
 * A theme is server code. Choosing between installed themes must therefore be
 * exactly as constrained as choosing between binaries in a `PATH`: a fixed list
 * of identifiers that the build already knows about.
 *
 * This module is deliberately free of `.astro` imports so that anything which
 * only needs to *validate* an id — the settings cache, the admin form, the
 * backend mirror — does not drag the whole component tree in behind it.
 *
 * `make arch` asserts that these ids, the keys of `theme/registry.ts` and the
 * backend's allowlist are the same set, so the three cannot drift apart.
 */

/**
 * The id used when nothing is stored, or when the stored id is not installed.
 *
 * `bluearchive` is the theme this project ships as its own: an Astro blog theme
 * with a light, blue-and-white visual language.
 */
export const DEFAULT_THEME_ID = 'bluearchive';

/**
 * Every installable theme, in the order the admin switcher lists them.
 *
 * Adding a theme means adding a line here *and* a matching entry in
 * `registry.ts` *and* a matching id in the backend's allowlist. The architecture
 * check fails until all three agree.
 */
export const THEME_IDS = ['bluearchive'] as const;

export type ThemeId = (typeof THEME_IDS)[number];

/** True when `value` names a theme this build knows how to render. */
export function isKnownThemeId(value: unknown): value is ThemeId {
  return typeof value === 'string' && (THEME_IDS as readonly string[]).includes(value);
}

/**
 * Coerce anything to a usable theme id.
 *
 * Anything unrecognised — a typo, a theme that was uninstalled after the
 * setting was saved, a missing value — falls back to `default` rather than
 * throwing. A bad setting must never turn into a 500 on every page.
 */
export function normalizeThemeId(value: unknown): ThemeId {
  return isKnownThemeId(value) ? value : DEFAULT_THEME_ID;
}
