// @ts-check
import { readdir, readFile } from 'node:fs/promises';
import { createReadStream, existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'astro/config';
import node from '@astrojs/node';

/**
 * A theme's static assets — its artwork and cursors — live
 * inside the theme directory (ARCHITECTURE.md §44), not in
 * astro/public/, which is the core's.
 *
 * Their URLs must stay stable: content/system/custom.css is
 * admin-authored content, and content cannot know a build hash. So
 * each theme's `assets/` tree is served at `/themes/<id>/…` — read
 * from disk in dev, emitted at the same path into the client bundle
 * at build time.
 *
 * Webfonts are different: the theme's stylesheet `@import`s the
 * font sheets, so the pipeline inlines their `@font-face` rules
 * and rewrites each woff2 URL to a hashed build asset under
 * `/_astro/`. A font is code the theme compiles with, not a path
 * anything else names — so it takes the build's own URL, not a
 * stable one.
 */
/** url path → content type, for the assets a theme ships.
 *  @type {Record<string, string>} */
const THEME_ASSET_TYPES = {
  '.css': 'text/css; charset=utf-8',
  '.cur': 'application/octet-stream',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
};

/** url path → absolute file, per installed theme. */
async function themeAssetPacks() {
  const root = fileURLToPath(new URL('./src/themes/', import.meta.url));
  if (!existsSync(root)) return [];
  const packs = [];
  for (const entry of await readdir(root, { withFileTypes: true })) {
    if (!entry.isDirectory()) continue;
    const dir = path.join(root, entry.name, 'assets');
    if (!existsSync(dir)) continue;
    const files = new Map();
    /** @param {string} current */
    const walk = async (current) => {
      for (const e of await readdir(current, { withFileTypes: true })) {
        const full = path.join(current, e.name);
        if (e.isDirectory()) await walk(full);
        else files.set(path.relative(dir, full).split(path.sep).join('/'), full);
      }
    };
    await walk(dir);
    packs.push({ id: entry.name, files });
  }
  return packs;
}

/** @returns {import('vite').Plugin} */
function themeAssets() {
  const packs = themeAssetPacks();
  return {
    name: 'cms-theme-assets',
    async buildStart() {
      await packs;
    },
    async generateBundle() {
      // Only the client bundle is served as static files; the server
      // bundle has no use for a cursor.
      if (this.environment?.name !== 'client') return;
      for (const pack of await packs) {
        for (const [rel, file] of pack.files) {
          this.emitFile({
            type: 'asset',
            fileName: `themes/${pack.id}/${rel}`,
            source: await readFile(file),
          });
        }
      }
    },
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        void (async () => {
          const match = /^\/themes\/([^/]+)\/(.+)$/.exec(
            new URL(req.url ?? '/', 'http://localhost').pathname,
          );
          if (!match) return next();
          const [, id = '', rel = ''] = match;
          if (!id || !rel) return next();
          const pack = (await packs).find((p) => p.id === id);
          const file = pack ? pack.files.get(decodeURIComponent(rel)) : undefined;
          if (!file) return next();
          res.setHeader(
            'content-type',
            THEME_ASSET_TYPES[path.extname(file).toLowerCase()] ?? 'application/octet-stream',
          );
          createReadStream(file)
            .on('error', () => {
              res.statusCode = 404;
              res.end();
            })
            .pipe(res);
        })().catch(next);
      });
    },
  };
}

// ARCHITECTURE.md §1: Astro is the only HTML rendering layer.
// ARCHITECTURE.md §4: production is SSR; content changes must appear without a rebuild.
export default defineConfig({
  output: 'server',
  adapter: node({ mode: 'standalone' }),

  server: {
    host: true,
    port: Number(process.env.PORT ?? 4321),
  },

  trailingSlash: 'never',

  // ARCHITECTURE.md §13: Astro's built-in Origin check stays on in every
  // environment. It is defence in depth alongside the application CSRF token,
  // never a replacement for it.
  security: {
    checkOrigin: true,
  },

  build: {
    // Content is read at request time by the live loader, so nothing about
    // content/ should be inlined into the build output.
    inlineStylesheets: 'auto',
  },

  vite: {
    // Serve each theme's assets/ tree at /themes/<id>/… in dev, and
    // emit it at the same path into the client bundle at build time.
    // See themeAssets() above.
    plugins: [themeAssets()],
    build: {
      // Never inline a hoisted script into the document.
      //
      // Astro inlines any script chunk smaller than this limit (4 KB by default)
      // and emits it as `<script type="module">…</script>`. That is invisible in a
      // screenshot and fatal in production: the Content-Security-Policy in
      // src/middleware.ts is `script-src 'self'` with no 'unsafe-inline'
      // (ARCHITECTURE.md §18), so an inlined script is simply refused by the
      // browser. A component `<script>` — the mobile menu in the bluearchive theme
      // is the only one today — would otherwise ship as a page whose only
      // JavaScript silently did nothing, and the only symptom would be a CSP
      // violation in a console nobody reads.
      //
      // Zero means every script is a real same-origin file under /_astro/, which is
      // exactly what 'self' allows. `make arch` asserts this setting, and the
      // full-stack tests assert that the rendered HTML carries no inline script.
      assetsInlineLimit: 0,
    },
  },

  devToolbar: {
    enabled: false,
  },
});
