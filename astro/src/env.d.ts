/// <reference path="../.astro/types.d.ts" />
/// <reference types="astro/client" />

interface ImportMetaEnv {
  /** Absolute path to the content root. Required at runtime, never bundled. */
  readonly CONTENT_ROOT: string;
  /** Base URL of the Go JSON API, e.g. http://backend:8080 */
  readonly API_BASE: string;
  /** Public origin of the site, e.g. https://blog.example */
  readonly PUBLIC_ORIGIN: string;
  /** app_env: prod | dev */
  readonly APP_ENV: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}

declare namespace App {
  interface Locals {
    /** Resolved admin session, or null for anonymous visitors. */
    session: import('./lib/session').SessionInfo | null;
    /**
     * The Cookie header of the incoming request, or null.
     *
     * ARCHITECTURE.md ID-20: there is no cookie jar on the server, so an admin
     * page's own API call has to forward the session cookie explicitly. The
     * middleware reads it once, here, so every server-side call in a request uses
     * the same value.
     */
    cookie: string | null;
    /**
     * The theme for this request.
     *
     * ARCHITECTURE.md §19: set once, by the middleware, from the compile-time
     * registry. Pages and layouts read it and never resolve a theme themselves,
     * which is what keeps "one theme per request" true even for a page that
     * renders several themed components.
     *
     * Only references live here — no data is copied per request.
     */
    theme: import('./theme-system/contract').ResolvedTheme;
  }
}

/**
 * An `.astro` file's default export, for `tsc --noEmit`.
 *
 * Plain TypeScript cannot parse an `.astro` file, so without this declaration
 * `tsc` cannot resolve the one import that makes the theme registry possible:
 * `theme/registry.ts` mapping ids to components. `astro check` understands
 * `.astro` natively and is the gate that actually checks component props; this
 * declaration only exists so the plain-tsc gate sees the same view Astro's own
 * `content.d.ts` takes — a component factory.
 *
 * The type is deliberately the generic factory rather than a per-file interface:
 * inventing one here would mean `tsc` believed it knew every component's props,
 * and `tsc` cannot know that.
 */
declare module '*.astro' {
  const Component: import('astro/runtime/server/index.js').AstroComponentFactory;
  export default Component;
}
