/**
 * The Theme Registry.
 *
 * ARCHITECTURE.md §44/§5: themes are compile-time registered code, selected by a
 * whitelisted id. There is deliberately no dynamic `import()` anywhere near this
 * file — a theme is not a path, a URL or a database row, and no value that
 * crosses the network boundary is ever concatenated into a module specifier.
 *
 * The map below is the complete set of themes that can ever be rendered. An id
 * that is not a key here cannot be turned into markup, at build time or at
 * request time.
 */
import type { ThemeDefinition } from './contract';
import { DEFAULT_THEME_ID, THEME_IDS, type ThemeId } from './ids';

import blueArchiveTheme from '../themes/bluearchive/theme';

/**
 * Every installed theme.
 *
 * This map is the entire mechanism. There is no loader, no discovery, no
 * installer and no runtime `import()` — a theme is a folder in this repository
 * plus one line here, which is what makes "the theme is part of the source"
 * literally true.
 *
 * The keys must match `THEME_IDS` exactly, and the backend's allowlist must match
 * too; `make arch` fails the build if the three lists have drifted apart. That is
 * what makes "whitelisted id" a checked property rather than a comment.
 */
const REGISTRY: Record<ThemeId, ThemeDefinition> = {
  bluearchive: blueArchiveTheme,
};

export type ThemeSummary = { id: string; name: string; version: string };

/** Every installed theme, in registry order. Safe to expose in the admin UI. */
export function listThemes(): ThemeSummary[] {
  return THEME_IDS.map((id) => {
    const theme = REGISTRY[id];
    return { id: theme.id, name: theme.name, version: theme.version };
  });
}

/** Look up a theme by id. Returns undefined for anything not registered. */
export function findTheme(id: unknown): ThemeDefinition | undefined {
  if (typeof id !== 'string') return undefined;
  return Object.hasOwn(REGISTRY, id) ? REGISTRY[id as ThemeId] : undefined;
}

/**
 * Resolve a theme, falling back to the default.
 *
 * An unknown id must never throw. The stored `themeId` can legitimately stop
 * being valid — a theme is uninstalled, the database predates the setting, a row
 * is corrupted — and none of those is a reason for every page to return 500. The
 * backend rejects unknown ids on write, so this path is a safety net for values
 * that were already stored, not the normal route.
 */
export function resolveTheme(id: unknown): ThemeDefinition {
  return findTheme(id) ?? REGISTRY[DEFAULT_THEME_ID];
}

export { DEFAULT_THEME_ID, THEME_IDS };
export type { ThemeId };
