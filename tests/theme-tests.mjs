#!/usr/bin/env node
/**
 * Theme contract tests.
 *
 * The question every test here answers is the same one: *if I replace the theme,
 * what stays the same, and what is supposed to change?*
 *
 * Everything runs against a real Go backend and a real Astro SSR build, because
 * the properties under test — a CSS bundle boundary, a settings cache, an API
 * contract, a CSP — only exist once the two processes are running. The bootstrap
 * is shared with `fullstack-tests.mjs` via `tests/lib/harness.mjs`.
 *
 * The suite is organised as the sixteen acceptance checks for the theme phase:
 *
 *   §31.1-2   both registered themes render every surface
 *   §31.3     a theme id that is not installed degrades instead of exploding
 *   §31.4-5   each theme is served its own stylesheet, and only its own
 *   §31.6-7   the public site and the admin each render through the theme
 *   §31.8     custom.css loads after the theme, and is served unlayered
 *   §31.9     custom.js is untouched by a theme switch
 *   §31.10    Markdown renders, and a switch does not touch a byte of it
 *   §31.11    a draft stays invisible to the public whatever the theme
 *   §31.12    the layouts are usable at 320px
 *   §31.13-16 the theme's own source: no content writes, no database, no API,
 *             no authentication
 *
 * One real bug is what §31.4-5 mostly guard. Every layout used to import its
 * stylesheet plainly, and because the registry statically imports the theme,
 * registering a second theme would merge both palettes into one file; the
 * theme attribute changed and the page did not. A theme switch that cannot
 * be seen is not a theme switch.
 */
import { readFile, readdir, stat } from "node:fs/promises";
import path from "node:path";

import {
  ASTRO,
  ROOT,
  bootstrapAdmin,
  cleanup,
  createReporter,
  ensureBuilds,
  freePort,
  makeRoots,
  sessionCookieFrom,
  startStack,
} from "./lib/harness.mjs";

const { ok, eq, test, summary } = createReporter("Theme contract");

const THEMES_DIR = path.join(ASTRO, "src", "themes");
const THEME_IDS_TS = path.join(ASTRO, "src", "theme-system", "ids.ts");

/**
 * The registry's ids, read from the source of truth rather than hard-coded here.
 *
 * §31.12 checks that the backend accepts exactly the ids the registry declares. A
 * copy of that list inside the test would let the two drift together and make the
 * check vacuous.
 */
async function registryIds() {
  const text = await readFile(THEME_IDS_TS, "utf-8");
  const block = text.match(/THEME_IDS\s*=\s*\[([^\]]*)\]/)?.[1] ?? "";
  return [...block.matchAll(/'([^']+)'/g)].map((m) => m[1]);
}

/** The id a broken setting is expected to degrade to, read from ids.ts. */
async function defaultThemeId() {
  const text = await readFile(THEME_IDS_TS, "utf-8");
  return text.match(/DEFAULT_THEME_ID\s*=\s*'([^']+)'/)?.[1] ?? null;
}

/**
 * A theme's `--accent`, taken from its own stylesheet.
 *
 * Used as a fingerprint to prove one theme's CSS was not served to the other. An
 * earlier version used the first `--font-sans` family, which is the obvious choice
 * and quietly wrong: both themes list `system-ui` in their stack, so "this page
 * does not contain the other theme's font" was never true and the assertion only
 * passed by accident of the particular pair.
 *
 * A colour is compared in both the long and the minified short form, because the
 * build rewrites `#ffffff` as `#fff`. The caller asserts the two fingerprints are
 * distinct before relying on either.
 */
async function accentOf(themeId) {
  const css = await readFile(
    path.join(THEMES_DIR, themeId, "public", "styles", "public.css"),
    "utf-8",
  );
  return (
    css.match(/--accent:\s*(#[0-9a-fA-F]{3,8})\s*;/)?.[1]?.toLowerCase() ?? null
  );
}

/** Both spellings of a hex colour, so a minified stylesheet still matches. */
function hexForms(hex) {
  const body = hex.slice(1);
  if (body.length !== 6) return [hex];
  const short = `#${body[0]}${body[2]}${body[4]}`;
  return short === hex ? [hex] : [hex, short];
}

function mentionsColour(css, hex) {
  return hexForms(hex).some((form) => css.includes(form));
}

/** Every `/_astro/*.css` a document links, in order. */
function stylesheetHrefs(html) {
  return [
    ...html.matchAll(
      /<link[^>]+rel="stylesheet"[^>]+href="(\/_astro\/[^"]+\.css)"/g,
    ),
  ].map((m) => m[1]);
}

/** The theme id the document declares on `<html>`. */
function declaredTheme(html) {
  return html.match(/<html[^>]*data-cms-theme="([^"]+)"/)?.[1] ?? null;
}

/** Every script the document loads, sorted so order is not significant. */
function scriptSrcs(html) {
  return [
    ...new Set(
      [...html.matchAll(/<script[^>]*src="([^"]+)"/g)].map((m) => m[1]),
    ),
  ].sort();
}

/**
 * Only the core scripts: the fixed URLs a theme neither chooses nor replaces.
 *
 * A theme may ship a hashed asset of its own — the bluearchive mobile menu is one —
 * so comparing the whole list would fail for the right reason. What must be
 * identical under every theme is the core set, and a theme may only ever *add* to
 * it.
 */
const CORE_SCRIPTS = [
  "/cms.js",
  "/color-scheme.js",
  "/comments.js",
  "/custom.js",
];

function coreScriptSrcs(html) {
  return scriptSrcs(html).filter((src) => CORE_SCRIPTS.includes(src));
}

/** Every `<script>` in the document that has no `src`: an inline script. */
function inlineScripts(html) {
  return [...html.matchAll(/<script\b([^>]*)>/g)]
    .filter(([, attributes]) => !/\bsrc=/.test(attributes))
    .map(([full]) => full);
}

/** A stable digest of every content file, so "the Markdown did not change" is checkable. */
async function contentDigest(root) {
  const parts = [];
  for (const dir of ["posts", "pages"]) {
    const full = path.join(root, dir);
    let names = [];
    try {
      names = (await readdir(full)).sort();
    } catch {
      continue;
    }
    for (const name of names) {
      const bytes = await readFile(path.join(full, name));
      parts.push(`${dir}/${name}:${bytes.length}:${bytes.toString("base64")}`);
    }
  }
  return parts.join("\n");
}

/** Every file under a directory, recursively. */
async function walk(dir) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...(await walk(full)));
    else out.push(full);
  }
  return out;
}

/**
 * §31.13-16: what a theme's own source is allowed to contain.
 *
 * These four checks are static on purpose. A theme that wrote a file, opened the
 * database, called the API or implemented a login would be a second CMS wearing a
 * stylesheet, and the honest test for that is to read its source rather than to
 * hope no request shows up in a log. `make arch` enforces the same rules; running
 * them here as well means `make test-theme` alone is evidence, not a pointer to
 * another gate.
 */
const THEME_SOURCE_FORBIDDEN = [
  {
    check: "§31.13 the theme writes no content (§34)",
    patterns: [
      /\bwriteFile(?:Sync)?\s*\(/,
      /\bappendFile(?:Sync)?\s*\(/,
      /\bcreateWriteStream\s*\(/,
      /\bmkdir(?:Sync)?\s*\(/,
      /\brename(?:Sync)?\s*\(/,
      /\bunlink(?:Sync)?\s*\(/,
      /\brm(?:Sync)?\s*\(/,
      /\bcpSync\s*\(|\brenameSync\s*\(/,
      /\bnode:fs\b|\bnode:fs\/promises\b/,
      /\bexec(?:Sync|File)?\s*\(|\bspawn(?:Sync)?\s*\(/,
      /\bnode:child_process\b/,
    ],
  },
  {
    check: "§31.14 the theme touches no database",
    patterns: [
      /\bsqlite3?\b/,
      /better-sqlite3/,
      /modernc\.org\/sqlite/,
      /database\/sql/,
      /\bSELECT\b[\s\S]{0,60}\bFROM\b/i,
      /\bCREATE TABLE\b/i,
      /\bINSERT INTO\b/i,
    ],
  },
  {
    check: "§31.15 the theme bypasses the core API",
    patterns: [
      /\bfetch\s*\(/,
      /XMLHttpRequest/,
      /sendBeacon/,
      /['"`]\/api\/v1\//,
      /\bnode:http\b/,
      // A remote URL in a theme is a third-party resource,
      // which the architecture refuses (§27) — with one
      // exception the CSP itself makes: `img-src` allowlists
      // gitee.com, the host the bluearchive hero artwork is
      // served from (middleware.ts). The theme may name
      // exactly what the security policy allows, and nothing
      // else.
      /['"`]https?:\/\/(?!127\.0\.0\.1|localhost|gitee\.com)/,
    ],
  },
  {
    // ID-33 / §43 / §44: `/custom.css` and `/custom.js` are the entire contract. A
    // theme that named a managed file, or read content/system/css, would couple itself
    // to one admin's directory layout — and the day a second admin used a different
    // set of filenames, that theme would be silently wrong rather than visibly broken.
    //
    // Patterns require a quoted string so prose and colour literals cannot trip them;
    // comments are stripped by `code()` before these run.
    check: "§31.17 the theme names no custom asset (ID-33)",
    patterns: [
      /content\/system\/(?:css|js|parked)\b/,
      /\b\d{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.(?:css|js)\b/,
      /blogcms:(?:css|js):/,
      /["'`]\/custom\/(?:css|js)\b/,
    ],
  },
  {
    check: "§31.16 the theme implements no authentication",
    patterns: [
      /argon2|bcrypt|scrypt|pbkdf2|GenerateFromPassword|CompareHashAndPassword/i,
      /csrf_secret|GenerateCSRFSecret/,
      /document\.cookie/,
      /Authorization|X-CSRF/i,
      /\bSESSION\b.*\bTOKEN\b/,
      /\blogin\s*\(|\bsignIn\s*\(/,
    ],
  },
];

/** Strip comments so prose about a rule cannot be mistaken for breaking it. */
function code(text) {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .split("\n")
    .filter((line) => !/^\s*(\/\/|\*|\/\*)/.test(line))
    .join("\n");
}

/**
 * Read every theme's source once, and report which rule each file breaks.
 *
 * Returns a map from check name to the offending `path: reason` lines.
 */
async function auditThemeSources() {
  const violations = new Map(
    THEME_SOURCE_FORBIDDEN.map((rule) => [rule.check, []]),
  );
  const ids = await registryIds();

  for (const id of ids) {
    const dir = path.join(THEMES_DIR, id);
    let files = [];
    try {
      files = await walk(dir);
    } catch {
      violations
        .get("§31.13 the theme writes no content (§34)")
        .push(`${id}: missing directory`);
      continue;
    }

    for (const file of files) {
      const rel = path.relative(ROOT, file);

      // §31.13: content/ is the only source of truth for articles and pages. A
      // Markdown file inside a theme would be a second source that the loader
      // never reads and the admin can never edit.
      if (/\.(md|mdx|markdown|mdwn|json)$/i.test(file)) {
        violations
          .get("§31.13 the theme writes no content (§34)")
          .push(`${rel}: a theme carries a content or data file`);
        continue;
      }

      // The rules below match *source code*. A theme's assets/ tree holds
      // bytes — artwork, cursors, webfonts — and any byte sequence can
      // look like `http://…` when read as text, so a binary asset is never
      // scanned: it is shipped, not executed.
      if (!/\.(astro|ts|tsx|js|mjs|css)$/i.test(file)) continue;

      const text = code(await readFile(file, "utf-8"));
      for (const rule of THEME_SOURCE_FORBIDDEN) {
        for (const pattern of rule.patterns) {
          if (pattern.test(text)) {
            violations.get(rule.check).push(`${rel}: matches ${pattern}`);
            break;
          }
        }
      }
    }
  }

  return violations;
}

async function main() {
  console.log("Theme contract tests (one core)");
  await ensureBuilds();

  const ids = await registryIds();
  const [first, second] = ids;
  /** Every registered theme, read from the registry rather than copied here. */
  const themes = ids;
  const fallback = await defaultThemeId();

  if (!first) {
    console.error(
      `\nids.ts declares no themes; ids.ts declares [${ids.join(", ")}].`,
    );
    process.exit(1);
  }
  if (!fallback) {
    console.error(
      "\nids.ts does not declare DEFAULT_THEME_ID, so the fallback test cannot run.",
    );
    process.exit(1);
  }

  const roots = await makeRoots();
  let stack = await startStack(roots, await freePort(), await freePort());
  let base = stack.base;
  let api = stack.api;

  const { cookie, csrfToken } = await bootstrapAdmin(api, base);

  /**
   * The live session, re-minted whenever a login invalidates the previous one.
   *
   * ARCHITECTURE.md §12 rotates on login and revokes every other session, so a
   * cookie captured at startup stops working the first time any test signs in
   * again. Rather than have each test keep its own pair — and quietly use a stale
   * one — there is one session here and one function that renews it on demand.
   */
  let liveCookie = cookie;
  let liveCsrf = csrfToken;

  async function liveSession({ force = false } = {}) {
    if (!force && liveCookie && liveCsrf)
      return { cookie: liveCookie, csrf: liveCsrf };
    const session = await fetch(`${base}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: base },
      body: JSON.stringify({ username: "owner", password: "a-good-password" }),
    });
    liveCookie = sessionCookieFrom(session);
    liveCsrf = (await session.json()).csrfToken;
    return { cookie: liveCookie, csrf: liveCsrf };
  }

  /**
   * An authenticated admin request through the same-origin proxy.
   *
   * A 401 triggers one re-authentication and one retry. Signing in revokes every
   * earlier session, so a cookie can die between two tests without anything being
   * wrong — and a suite that hard-fails on that would be testing the test's own
   * bookkeeping rather than the CMS. It retries once and never more, so a genuinely
   * revoked session still surfaces as a 401.
   */
  async function asAdmin(target, init = {}) {
    const send = async (session) =>
      fetch(`${base}${target}`, {
        ...init,
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${session.cookie}`,
          "X-CSRF-Token": session.csrf,
          ...(init.headers ?? {}),
        },
      });

    const first = await send(await liveSession());
    if (first.status !== 401) return first;
    return send(await liveSession({ force: true }));
  }

  /** Write settings as the signed-in admin. */
  const putSettings = (patch) =>
    asAdmin("/api/v1/admin/settings", {
      method: "PUT",
      body: JSON.stringify(patch),
    });

  /** Admin mutations go through the Astro proxy, exactly as a browser would. */
  const adminFetch = (target, init = {}) =>
    fetch(`${base}${target}`, {
      ...init,
      headers: {
        "Content-Type": "application/json",
        Origin: base,
        Cookie: `blog_session=${cookie}`,
        "X-CSRF-Token": csrfToken,
        ...(init.headers ?? {}),
      },
    });

  /**
   * Switch the active theme, through the API and the same-origin proxy.
   *
   * It goes through the live session rather than the bootstrap cookie: signing in
   * revokes every other session, so by the time the later tests run the startup
   * cookie no longer authenticates and every switch would answer 401.
   */
  async function setTheme(themeId) {
    const res = await asAdmin("/api/v1/admin/settings", {
      method: "PUT",
      body: JSON.stringify({
        siteTitle: "Theme Contract",
        commentsEnabled: true,
        commentAutoModerate: true,
        themeId,
      }),
    });
    if (res.status !== 200) {
      throw new Error(
        `switching to ${themeId} failed: ${res.status} ${await res.text()}`,
      );
    }
  }

  /** Public and admin HTML plus the CSS they actually serve. */
  async function render() {
    const home = await fetch(`${base}/`);
    const homeHtml = await home.text();
    const admin = await adminFetch("/admin");
    const adminHtml = await admin.text();

    const cssFor = async (html) => {
      const hrefs = stylesheetHrefs(html);
      const bodies = await Promise.all(
        hrefs.map((h) => fetch(base + h).then((r) => r.text())),
      );
      return { hrefs, text: bodies.join("\n") };
    };

    return {
      home,
      homeHtml,
      admin,
      adminHtml,
      homeCss: await cssFor(homeHtml),
      adminCss: await cssFor(adminHtml),
    };
  }

  // Real content, written through the API so the live loader has something to
  // serve and the tests exercise the production path.
  const postRes = await adminFetch("/api/v1/admin/posts", {
    method: "POST",
    body: JSON.stringify({
      title: "Themed Content",
      slug: "themed-content",
      description:
        "A post used to prove a theme switch changes nothing but markup.",
      date: "2026-10-04",
      tags: "themes, contract",
      draft: false,
      body: "# Heading\n\nA paragraph with **bold** text.\n",
    }),
  });
  if (postRes.status !== 201) {
    throw new Error(
      `could not create the fixture post: ${postRes.status} ${await postRes.text()}`,
    );
  }

  const draftRes = await adminFetch("/api/v1/admin/posts", {
    method: "POST",
    body: JSON.stringify({
      title: "Unfinished Thoughts",
      slug: "unfinished-thoughts",
      date: "2026-10-04",
      draft: true,
      body: "Not ready for readers.\n",
    }),
  });
  if (draftRes.status !== 201) {
    throw new Error(
      `could not create the fixture draft: ${draftRes.status} ${await draftRes.text()}`,
    );
  }

  const pageRes = await adminFetch("/api/v1/admin/pages", {
    method: "POST",
    body: JSON.stringify({
      title: "About",
      slug: "about",
      description: "A page used to prove the same thing for pages.",
      date: "2026-10-04",
      draft: false,
      body: "About this site.\n",
    }),
  });
  if (pageRes.status !== 201) {
    throw new Error(
      `could not create the fixture page: ${pageRes.status} ${await pageRes.text()}`,
    );
  }

  try {
    // -----------------------------------------------------------------------
    // §31.1 — the default theme renders every surface.
    // -----------------------------------------------------------------------
    await test("§31.1 the default theme renders the public site", async () => {
      await setTheme(first);
      const { home, homeHtml } = await render();

      eq(home.status, 200, "GET / returns 200");
      eq(
        declaredTheme(homeHtml),
        first,
        "the document declares the default theme",
      );
      eq(
        fallback,
        first,
        "the default theme is the one ids.ts names as the fallback",
      );
      ok(homeHtml.includes("Themed Content"), "the post title is rendered");
      ok(
        homeHtml.includes("/posts/themed-content"),
        "the post links to its themed URL",
      );
      ok(
        homeHtml.includes('href="/about"'),
        "the standalone page appears in the navigation",
      );
      ok(
        homeHtml.includes('<main id="main"'),
        "the theme owns the document shell",
      );

      const post = await fetch(`${base}/posts/themed-content`);
      const postHtml = await post.text();
      eq(post.status, 200, "GET /posts/<slug> returns 200");
      ok(
        /<h1[^>]*>Heading<\/h1>/.test(postHtml),
        "the Markdown body is rendered by the theme",
      );
      ok(
        postHtml.includes("<strong>bold</strong>"),
        "inline Markdown is rendered",
      );

      // ARCHITECTURE.md ID-21: a rendered page carries no inline style at all, so
      // the CSP needs no 'unsafe-inline' and a theme can still override everything.
      const inlineStyles = [...postHtml.matchAll(/\sstyle="[^"]*"/g)].map((m) =>
        m[0].trim(),
      );
      eq(
        inlineStyles.length,
        0,
        `the post carries no inline style${inlineStyles.length ? `: ${inlineStyles[0]}` : ""}`,
      );
      ok(
        postHtml.includes('data-cms-form="comment"'),
        "the comment form is theme-owned markup",
      );

      const page = await fetch(`${base}/about`);
      eq(page.status, 200, "GET /<slug> returns 200");
      ok(
        (await page.text()).includes("About this site."),
        "the page body is rendered",
      );
    });

    await test("§31.2 the default theme renders the admin and its login", async () => {
      const dash = await adminFetch("/admin");
      const dashHtml = await dash.text();
      eq(dash.status, 200, "GET /admin returns 200 for a signed-in admin");
      eq(
        declaredTheme(dashHtml),
        first,
        "the admin is rendered by the default theme",
      );
      ok(
        dashHtml.includes('aria-label="后台导航"') ||
          dashHtml.includes('aria-label="Admin"'),
        "the admin navigation is theme markup, labelled by the theme",
      );
      ok(
        dashHtml.includes("data-cms-csrf"),
        "the shell carries the core CSRF token",
      );

      // An admin view that owns controls. The dashboard has none, and the moderation
      // and media tables only render buttons per row, so an empty fixture would
      // make this assertion pass vacuously — the post editor's form contract is
      // unconditional.
      const editor = await adminFetch("/admin/posts/new");
      const editorHtml = await editor.text();
      eq(editor.status, 200, "GET /admin/posts/new returns 200");
      eq(
        declaredTheme(editorHtml),
        first,
        "admin editors are rendered by the theme",
      );
      ok(
        editorHtml.includes('data-cms-form="post"'),
        "admin forms carry the core JS contract",
      );
      ok(
        editorHtml.includes('data-cms-mode="create"'),
        "the editor declares its mode",
      );
      ok(
        editorHtml.includes("data-cms-csrf"),
        "the editor carries the core CSRF token",
      );

      const login = await fetch(`${base}/admin/login`);
      const loginHtml = await login.text();
      eq(login.status, 200, "GET /admin/login returns 200");
      eq(
        declaredTheme(loginHtml),
        first,
        "the login screen is theme-owned too",
      );
      ok(
        loginHtml.includes('data-cms-form="login"'),
        "the login form is theme-owned markup",
      );
    });

    // -----------------------------------------------------------------------
    // §31.4-5 — the CSS boundary, which is the bug this suite exists for.
    // -----------------------------------------------------------------------
    await setTheme(first);
    const before = await render();
    const digestBefore = await contentDigest(roots.content);

    if (second) await setTheme(second);
    const after = await render();
    const digestAfter = await contentDigest(roots.content);

    // Per-theme snapshots: both themes when two are registered, one
    // otherwise, so the loops below stay meaningful either way.
    const pairs = second
      ? [
          [first, before],
          [second, after],
        ]
      : [[first, before]];

    /** Each theme's palette fingerprint, for the cross-contamination checks. */
    const accents = new Map(
      await Promise.all(ids.map(async (id) => [id, await accentOf(id)])),
    );
    const [accentFirst, accentSecond] = ids.map((id) => accents.get(id));
    const fingerprintsDiffer =
      Boolean(accentFirst) &&
      Boolean(accentSecond) &&
      !hexForms(accentFirst).some((form) =>
        hexForms(accentSecond).includes(form),
      );

    await test("§31.4 each theme is served its own stylesheet", async () => {
      eq(declaredTheme(before.homeHtml), first, "the first theme is active");
      if (!second) {
        ok(true, "only one theme is registered, so a swap is not observable");
      } else {
        eq(
          declaredTheme(after.homeHtml),
          second,
          "after: the second theme is active",
        );

        ok(before.homeHtml !== after.homeHtml, "the public HTML changed");

        // The regression this suite exists for: the attribute changed,
        // the CSS did not, so the swap was invisible.
        ok(
          JSON.stringify(before.homeCss.hrefs) !==
            JSON.stringify(after.homeCss.hrefs),
          "each theme is served its own stylesheet",
        );
        ok(
          before.homeCss.text !== after.homeCss.text,
          "the CSS the browser receives changed",
        );
      }

      if (fingerprintsDiffer) {
        ok(
          mentionsColour(before.homeCss.text, accentFirst) &&
            !mentionsColour(before.homeCss.text, accentSecond),
          `the ${first} page carries only the ${first} theme's tokens`,
        );
        ok(
          mentionsColour(after.homeCss.text, accentSecond) &&
            !mentionsColour(after.homeCss.text, accentFirst),
          `the ${second} page carries only the ${second} theme's tokens`,
        );
      } else {
        ok(
          true,
          "the themes share a palette, so token isolation is not asserted here",
        );
      }

      ok(
        before.homeCss.text.includes("@layer theme"),
        "theme CSS stays inside @layer theme",
      );
      eq(
        (before.homeHtml.match(/<style/g) ?? []).length,
        0,
        "no inline <style> shadows the theme",
      );
      eq(
        (before.adminHtml.match(/<style/g) ?? []).length,
        0,
        "and none in the admin either",
      );

      if (second) {
        ok(
          JSON.stringify(before.adminCss.hrefs) !==
            JSON.stringify(after.adminCss.hrefs),
          "each theme is served its own admin stylesheet",
        );
        ok(
          before.adminCss.text !== after.adminCss.text,
          "the admin CSS the browser receives changed",
        );
      }
    });

    await test("§31.5 a page never loads another theme's CSS", async () => {
      for (const [id, snapshot] of pairs) {
        const mine = accents.get(id);
        const theirs = accents.get(id === first ? second : first);

        eq(
          snapshot.homeCss.hrefs.length,
          1,
          `the ${id} public page links exactly one stylesheet`,
        );
        eq(
          snapshot.adminCss.hrefs.length,
          1,
          `the ${id} admin page links exactly one stylesheet`,
        );

        if (mine && theirs && mine !== theirs) {
          ok(
            !mentionsColour(snapshot.homeCss.text, theirs) &&
              !mentionsColour(snapshot.adminCss.text, theirs),
            `the ${id} page does not carry the other theme's tokens`,
          );
          ok(
            mentionsColour(snapshot.homeCss.text, mine),
            `the ${id} page carries its own tokens on the public side`,
          );
          ok(
            mentionsColour(snapshot.adminCss.text, mine),
            `the ${id} page carries its own tokens on the admin side`,
          );
        }

        // Every linked stylesheet has to exist: a `?url` import that was reverted to a
        // plain import would still link one file, but it would be the wrong one.
        for (const href of [
          ...snapshot.homeCss.hrefs,
          ...snapshot.adminCss.hrefs,
        ]) {
          eq(
            (await fetch(base + href)).status,
            200,
            `${id}: ${href} is served`,
          );
        }
      }
    });

    // -----------------------------------------------------------------------
    // §31.6-7 — which layout renders which area.
    // -----------------------------------------------------------------------
    await test("§31.6 the public site renders through the theme layout", async () => {
      await setTheme(first);
      for (const url of [
        "/",
        "/posts/themed-content",
        "/about",
        "/tags/themes",
        "/no-such-page",
      ]) {
        const res = await fetch(`${base}${url}`);
        const html = await res.text();
        eq(declaredTheme(html), first, `${url} declares the active theme`);
        ok(
          html.includes('class="skip-link"'),
          `${url} renders the theme's own shell`,
        );
      }
    });

    await test("§31.7 the admin renders through the theme admin layout", async () => {
      await setTheme(first);
      const shell = await adminFetch("/admin");
      const shellHtml = await shell.text();

      eq(
        declaredTheme(shellHtml),
        first,
        "the admin declares the active theme",
      );
      ok(
        shellHtml.includes('class="admin-shell"'),
        "the admin layout owns the shell",
      );
      ok(
        shellHtml.includes('aria-label="后台导航"') ||
          shellHtml.includes('aria-label="Admin"'),
        "the sidebar navigation is theme markup, labelled by the theme",
      );
      ok(
        !shellHtml.includes('class="hero"'),
        "the admin is not the public home page in disguise",
      );

      // Every admin screen has to come out of the theme's admin tree, not fall back
      // to the public one.
      for (const url of [
        "/admin/posts",
        "/admin/posts/new",
        "/admin/pages",
        "/admin/pages/new",
        "/admin/comments",
        "/admin/media",
        "/admin/settings",
        "/admin/custom-code",
      ]) {
        const res = await adminFetch(url);
        const html = await res.text();
        eq(res.status, 200, `${url} returns 200`);
        eq(
          declaredTheme(html),
          first,
          `${url} is rendered by the active theme`,
        );
        ok(
          html.includes('class="admin-shell"'),
          `${url} uses the admin layout`,
        );
      }
    });

    // -----------------------------------------------------------------------
    // §31.8-9 — custom code belongs to the admin, not to the theme.
    // -----------------------------------------------------------------------
    await test("§31.8 custom.css loads after the theme and is served unlayered", async () => {
      // Author something through the admin API first, so "custom.css is served" and
      // "custom.css is unlayered" are statements about the admin's real file rather
      // than about an empty response.
      const saved = await adminFetch("/api/v1/admin/custom-code", {
        method: "PUT",
        body: JSON.stringify({
          css: ".post-card { border-left-color: #ff00ff; }",
          js: "window.__customCodeRan = true;",
        }),
      });
      eq(saved.status, 200, `custom code saved (${await saved.text()})`);

      for (const [id, snapshot] of pairs) {
        const hrefs = [
          ...snapshot.homeHtml.matchAll(
            /<link[^>]+rel="stylesheet"[^>]+href="([^"]+)"/g,
          ),
        ]
          .map((m) => m[1])
          .filter((href) => href.endsWith(".css") || href === "/custom.css");

        eq(
          hrefs.length,
          3,
          `the ${id} public page links a theme stylesheet, the Markdown stylesheet and custom.css`,
        );
        eq(
          hrefs[1],
          "/markdown.css",
          `the Markdown presentation layer sits between the theme and the admin's override (${id})`,
        );
        ok(
          hrefs[hrefs.length - 1] === "/custom.css",
          `custom.css is last, so it wins on source order too (${id})`,
        );
        ok(
          hrefs[0].startsWith("/_astro/"),
          `the theme stylesheet is an emitted asset (${id})`,
        );

        const adminHrefs = [
          ...snapshot.adminHtml.matchAll(
            /<link[^>]+rel="stylesheet"[^>]+href="([^"]+)"/g,
          ),
        ].map((m) => m[1]);
        eq(
          adminHrefs[adminHrefs.length - 1],
          "/custom.css",
          `the ${id} admin page links custom.css last too`,
        );

        const custom = await fetch(`${base}/custom.css`);
        eq(custom.status, 200, "/custom.css is served");
        ok(
          custom.headers.get("content-type")?.includes("text/css"),
          "/custom.css is served as CSS, by Astro",
        );
        const body = await custom.text();
        ok(
          body.includes("ff00ff"),
          "the admin-authored rule is what is served",
        );
        ok(
          !/@layer\s+theme/.test(body),
          "custom.css is not in the theme layer, which is what lets it override the theme",
        );
      }
    });

    await test("§31.9 switching the theme does not change custom.js or the core scripts", async () => {
      // custom.js is the admin's, not the theme's. Read it back under each theme.
      await setTheme(first);
      const underFirst = await (await fetch(`${base}/custom.js`)).text();
      if (second) await setTheme(second);
      const underSecond = await (await fetch(`${base}/custom.js`)).text();

      ok(
        underFirst.includes("__customCodeRan"),
        "/custom.js serves the admin-authored file",
      );
      eq(
        underSecond,
        underFirst,
        "custom.js is byte-identical whichever theme is active",
      );

      eq(
        JSON.stringify(coreScriptSrcs(after.homeHtml)),
        JSON.stringify(coreScriptSrcs(before.homeHtml)),
        "the public page loads exactly the same core scripts under either theme",
      );
      eq(
        JSON.stringify(coreScriptSrcs(after.adminHtml)),
        JSON.stringify(coreScriptSrcs(before.adminHtml)),
        "the admin loads exactly the same core scripts under either theme",
      );

      // A theme may add an asset of its own; it may not take over a core URL, and
      // anything it does add has to be a hashed build artifact rather than a
      // predictable path.
      for (const [id, snapshot] of pairs) {
        for (const src of scriptSrcs(snapshot.homeHtml).filter(
          (s) => !CORE_SCRIPTS.includes(s),
        )) {
          ok(
            src.startsWith("/_astro/"),
            `the ${id} theme's own script is a hashed asset, not a fixed URL (${src})`,
          );
        }
      }

      // JavaScript is core, so it is served from a fixed path and never from a
      // theme directory. The check is on script sources alone: an artwork path
      // may legitimately contain the word (a WordPress tree is one such path),
      // but no script may come out of one.
      const loadsThemeScript = (html) =>
        scriptSrcs(html).some((src) => src.includes("themes/"));
      ok(
        !loadsThemeScript(before.homeHtml) &&
          !loadsThemeScript(after.homeHtml),
        "no script is ever loaded from a theme directory",
      );
      ok(
        after.homeHtml.includes('src="/cms.js"'),
        "the core behaviour script is still /cms.js",
      );
      ok(
        after.homeHtml.includes('src="/comments.js"'),
        "the comment script is still /comments.js",
      );
      ok(
        after.homeHtml.includes('src="/custom.js"'),
        "the admin-authored custom.js is still served",
      );

      // CSP is `script-src 'self'` with no 'unsafe-inline', so an inline script is
      // silently refused by the browser. Astro inlines small script chunks by
      // default, which is how a theme's own script can end up shipped as dead code.
      for (const [id, snapshot] of pairs) {
        eq(
          inlineScripts(snapshot.homeHtml).length,
          0,
          `the ${id} public page has no inline script, so the CSP cannot block it`,
        );
        eq(
          inlineScripts(snapshot.adminHtml).length,
          0,
          `the ${id} admin page has no inline script either`,
        );
      }
    });

    // -----------------------------------------------------------------------
    // ID-33 / §75 — managed custom assets, seen from under both themes.
    //
    // The claim being tested is narrow and absolute: a theme knows there are two
    // custom-code URLs and nothing else. It never learns a filename, never reads a
    // directory, and cannot gain or lose an asset by being switched.
    // -----------------------------------------------------------------------
    await test("§75 a theme switch cannot change which custom assets load", async () => {
      const write = (method, kind, id, body) =>
        asAdmin(
          id === null
            ? `/api/v1/admin/custom/${kind}`
            : `/api/v1/admin/custom/${kind}/${encodeURIComponent(id)}`,
          { method, body: JSON.stringify(body) },
        );

      const created = [];
      for (const name of ["001-theme-test.css", "020-theme-test.css"]) {
        const res = await write("POST", "css", null, {
          filename: name,
          content: `.${name.replace(".css", "")} { color: #00ffcc; }\n`,
        });
        eq(res.status, 201, `${name} created (${await res.text()})`);
        created.push(`css/${name}`);
      }
      const js = await write("POST", "js", null, {
        filename: "001-theme-test.js",
        content: "window.__themeCustomAsset = true;\n",
      });
      eq(js.status, 201, "the JavaScript asset was created");
      created.push("js/001-theme-test.js");

      // One disabled asset, so "enabled" is part of what a switch must not disturb.
      await write("PUT", "css", "020-theme-test.css", { enabled: false });

      const read = async () => {
        await setTheme(first);
        const a = await render();
        const cssA = await fetch(`${base}/custom.css`);
        const jsA = await fetch(`${base}/custom.js`);
        if (second) await setTheme(second);
        const b = await render();
        const cssB = await fetch(`${base}/custom.css`);
        const jsB = await fetch(`${base}/custom.js`);
        return {
          first: { page: a, css: await cssA.text(), js: await jsA.text() },
          second: { page: b, css: await cssB.text(), js: await jsB.text() },
        };
      };

      const seen = await read();

      // The aggregate is the same bytes under either theme, and it carries exactly the
      // assets that are enabled — in filename order, with the legacy file first.
      for (const [id, snapshot] of second
        ? [
            [first, seen.first],
            [second, seen.second],
          ]
        : [[first, seen.first]]) {
        ok(
          snapshot.css.includes("#00ffcc"),
          `${id}: the enabled managed CSS is in /custom.css`,
        );
        ok(
          !snapshot.css.includes("020-theme-test.css"),
          `${id}: the disabled asset is not in /custom.css`,
        );
        ok(
          snapshot.js.includes("__themeCustomAsset"),
          `${id}: the managed JavaScript is in /custom.js`,
        );
        const order = ["custom.css", "blogcms:css:001-theme-test.css"].map(
          (marker) =>
            snapshot.css.indexOf(marker === "custom.css" ? "ff00ff" : marker),
        );
        ok(
          order[0] !== -1 && order[1] !== -1 && order[0] < order[1],
          `${id}: the legacy file still loads before the managed assets`,
        );
      }
      eq(
        seen.second.css,
        seen.first.css,
        "/custom.css is byte-identical under either theme",
      );
      eq(
        seen.second.js,
        seen.first.js,
        "/custom.js is byte-identical under either theme",
      );

      // The markup contract is unchanged: two stylesheets, the second one being
      // /custom.css, and exactly one custom script.
      for (const [id, snapshot] of second
        ? [
            [first, seen.first.page],
            [second, seen.second.page],
          ]
        : [[first, seen.first.page]]) {
        // stylesheetHrefs() deliberately matches only /_astro/*.css — the theme
        // own asset — so /custom.css has to be picked up with the regex §31.8 uses.
        const hrefs = [
          ...snapshot.homeHtml.matchAll(
            /<link[^>]+rel="stylesheet"[^>]+href="([^"]+)"/g,
          ),
        ]
          .map((m) => m[1])
          .filter(
            (href) => href === "/custom.css" || href.startsWith("/_astro/"),
          );
        eq(
          hrefs.length,
          2,
          `${id}: the page links a theme stylesheet and /custom.css`,
        );
        eq(hrefs[1], "/custom.css", `${id}: and /custom.css is last`);
        eq(
          scriptSrcs(snapshot.homeHtml).filter((src) => src === "/custom.js")
            .length,
          1,
          `${id}: /custom.js is loaded exactly once`,
        );
        ok(
          !snapshot.homeHtml.includes("theme-test"),
          `${id}: no asset filename appears in the markup`,
        );
        // §39: a theme's own script and the admin's custom script are separate. One
        // does not replace the other, and neither knows the other exists.
        // A theme may ship a hashed asset of its own; what it may not do is add a
        // script at a fixed URL, which would be a core script by another name.
        for (const src of scriptSrcs(snapshot.homeHtml).filter(
          (x) => !CORE_SCRIPTS.includes(x),
        )) {
          ok(
            src.startsWith("/_astro/"),
            `${id}: any script the theme adds is its own hashed asset (${src})`,
          );
        }
      }

      // §31.17, read from the source: a theme that named an asset, or reached into
      // the asset directory, would be coupling itself to one admin's file layout.
      const violations = await auditThemeSources();
      for (const [rule, hits] of violations) {
        if (rule.includes("custom asset")) {
          eq(
            hits.length,
            0,
            `${rule}: ${hits.join("; ") || "no theme names a custom asset"}`,
          );
        }
      }

      for (const path of created) {
        const kind = path.split("/")[0];
        const name = path.split("/")[1];
        const res = await write("DELETE", kind, name);
        eq(res.status, 204, `${path} deleted`);
      }
      const after = await render();
      const cleared = await (await fetch(`${base}/custom.css`)).text();
      ok(
        !cleared.includes("theme-test"),
        "and the assets are gone from /custom.css",
      );
      ok(
        after.homeHtml.includes('href="/custom.css"'),
        "the page still links /custom.css after the assets are removed",
      );
    });

    // -----------------------------------------------------------------------
    // §31.10 — content is the filesystem's business, not the theme's.
    // -----------------------------------------------------------------------
    await test("§31.10 switching the theme does not touch the Markdown", async () => {
      eq(
        digestAfter,
        digestBefore,
        "every file in content/posts and content/pages is byte-identical",
      );

      const post = await (await fetch(`${base}/posts/themed-content`)).text();
      ok(
        post.includes("Themed Content"),
        "the same post still renders from the same file",
      );
      ok(
        /<h1[^>]*>Heading<\/h1>/.test(post),
        "the same Markdown body still renders",
      );
      eq(
        (before.homeHtml.match(/<style/g) ?? []).length,
        0,
        "the theme still ships no inline CSS",
      );

      // And writing through the API still produces a file the loader picks up
      // without a rebuild.
      const created = await adminFetch("/api/v1/admin/posts", {
        method: "POST",
        body: JSON.stringify({
          title: "Written Under The Other Theme",
          slug: "written-under-the-other-theme",
          date: "2026-10-04",
          draft: false,
          body: "Still a file on disk.\n",
        }),
      });
      eq(
        created.status,
        201,
        "creating a post works with the other theme active",
      );
      const rendered = await (
        await fetch(`${base}/posts/written-under-the-other-theme`)
      ).text();
      ok(
        rendered.includes("Written Under The Other Theme"),
        "the new post renders with no rebuild",
      );
    });

    // -----------------------------------------------------------------------
    // §31.11 — a theme must not become a way to see a draft.
    // -----------------------------------------------------------------------
    await test("§31.11 a draft stays invisible to the public under either theme", async () => {
      for (const id of ids) {
        await setTheme(id);

        const direct = await fetch(`${base}/posts/unfinished-thoughts`);
        eq(
          direct.status,
          404,
          `GET /posts/unfinished-thoughts is 404 under ${id}`,
        );

        const home = await (await fetch(`${base}/`)).text();
        ok(
          !home.includes("Unfinished Thoughts"),
          `the ${id} home page does not list the draft`,
        );

        const tagListing = await fetch(`${base}/tags/themes`);
        eq(tagListing.status, 200, "the tag listing still renders");
        ok(
          !(await tagListing.text()).includes("Unfinished Thoughts"),
          `the ${id} tag listing does not list the draft either`,
        );

        const feed = await (await fetch(`${base}/rss.xml`)).text();
        ok(
          !feed.includes("Unfinished Thoughts"),
          `the ${id} feed does not publish the draft`,
        );

        // And the admin, which is the only place a draft is supposed to appear.
        const adminPosts = await adminFetch("/admin/posts");
        ok(
          (await adminPosts.text()).includes("Unfinished Thoughts"),
          `the ${id} admin can still see the draft`,
        );
      }
    });

    // -----------------------------------------------------------------------
    // §31.12 — the layout has to survive a 320px phone.
    //
    // There is no browser here, so this asserts what can be asserted statically
    // and honestly: a viewport the layout actually declares, responsive rules that
    // exist, and no fixed minimum width that would force a horizontal scrollbar on
    // the narrowest phone still in use.
    // -----------------------------------------------------------------------
    await test("§31.12 the layouts declare a mobile viewport and no fixed minimum width", async () => {
      for (const id of ids) {
        for (const area of ["public", "admin"]) {
          const file = path.join(THEMES_DIR, id, area, "styles", `${area}.css`);
          const css = await readFile(file, "utf-8");
          ok(
            (await stat(file)).size > 0,
            `the ${id} ${area} stylesheet exists and is not empty`,
          );
          ok(
            /@media/.test(css),
            `the ${id} ${area} stylesheet has responsive rules`,
          );
          ok(
            !/^\s*(body|html|\.container|\.admin-shell)[^{]*\{[^}]*min-width\s*:\s*\d+px/ms.test(
              css,
            ),
            `the ${id} ${area} stylesheet sets no fixed pixel min-width on the shell`,
          );
          ok(
            /overflow-x\s*:\s*auto/.test(css),
            `the ${id} ${area} stylesheet scrolls wide content`,
          );
        }
      }

      for (const snapshot of [before, after]) {
        ok(
          snapshot.homeHtml.includes(
            'name="viewport" content="width=device-width, initial-scale=1"',
          ),
          "the public layout declares a device-width viewport",
        );
        ok(
          snapshot.adminHtml.includes(
            'name="viewport" content="width=device-width, initial-scale=1"',
          ),
          "the admin layout declares a device-width viewport",
        );
      }

      // The theme's own responsive chrome has to be in the document, not implied.
      ok(
        before.homeHtml.includes("data-nav-toggle"),
        "the theme ships its mobile menu control",
      );
      ok(
        before.homeHtml.includes('aria-expanded="false"'),
        "and it is announced as collapsed to a screen reader",
      );
      ok(
        before.adminHtml.includes('class="admin-nav"'),
        "the admin ships a navigation landmark",
      );
    });

    // -----------------------------------------------------------------------
    // §31.13-16 — the theme's source, read rather than inferred.
    // -----------------------------------------------------------------------
    await test("§31.13-16 a theme contains no core logic", async () => {
      const violations = await auditThemeSources();
      for (const [check, hits] of violations) {
        eq(hits.length, 0, check + (hits.length ? `: ${hits.join("; ")}` : ""));
      }
    });

    // -----------------------------------------------------------------------
    // §31.3 — the theme id is validated everywhere it can be reached.
    // -----------------------------------------------------------------------
    await test("§31.3 a stored theme that is not installed falls back to the default", async () => {
      await stack.stop();

      // The backend refuses to *store* an unknown id, so the only way to reach this
      // state is a row that was already stored — an uninstalled theme, a restored
      // backup, a hand-edited database. It is planted with sqlite3 for exactly that
      // reason, on a freshly started stack so the settings cache cannot mask it.
      const db = path.join(roots.data, "blog.db");
      const planted = `${first}-removed-in-a-future-deploy`;
      const { execFileSync } = await import("node:child_process");
      execFileSync("sqlite3", [
        db,
        `UPDATE setting SET value='${planted}' WHERE key='themeId';`,
      ]);
      const stored = execFileSync("sqlite3", [
        db,
        `SELECT value FROM setting WHERE key='themeId';`,
      ])
        .toString()
        .trim();
      eq(stored, planted, "the unknown theme id is now the stored value");

      stack = await startStack(roots, await freePort(), await freePort());
      base = stack.base;
      api = stack.api;

      const home = await fetch(`${base}/`);
      const homeHtml = await home.text();
      eq(
        home.status,
        200,
        "the public site still returns 200 rather than failing",
      );
      eq(
        declaredTheme(homeHtml),
        fallback,
        "the site falls back to the default theme",
      );
      ok(
        homeHtml.includes("Themed Content"),
        "the site still renders its content",
      );

      eq(
        (await fetch(`${base}/posts/themed-content`)).status,
        200,
        "posts still render",
      );
      eq((await fetch(`${base}/about`)).status, 200, "pages still render");
      eq(
        (await fetch(`${base}/admin/login`)).status,
        200,
        "the admin login screen still renders",
      );

      const anon = await fetch(`${base}/admin`, { redirect: "manual" });
      eq(anon.status, 303, "the admin is still protected");
    });

    await test("§31.3b the stored themeId can only ever be a registered theme", async () => {
      const login = await fetch(`${base}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const liveCookie = sessionCookieFrom(login);
      const { csrfToken: liveCsrf } = await login.json();

      const put = (themeId) =>
        fetch(`${base}/api/v1/admin/settings`, {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${liveCookie}`,
            "X-CSRF-Token": liveCsrf,
          },
          body: JSON.stringify({
            siteTitle: "Theme Contract",
            commentsEnabled: true,
            commentAutoModerate: true,
            themeId,
          }),
        });

      for (const id of ids) {
        eq(
          (await put(id)).status,
          200,
          `the registered theme "${id}" is accepted`,
        );
      }

      const rejected = [
        "not-a-theme",
        "../../etc/passwd",
        `/themes/${first}/public.css`,
        "constructor",
        "__proto__",
        first.toUpperCase(),
        "",
      ];
      for (const id of rejected) {
        const res = await put(id);
        ok(
          res.status === 422 || res.status === 400,
          `"${id}" is refused (got ${res.status})`,
        );
        if (res.status === 422) {
          const envelope = await res.json();
          ok(
            Boolean(envelope?.error?.fields?.themeId),
            `"${id}" names themeId in the error envelope`,
          );
        }
      }

      // After all that, the stored value is still a registered id: the endpoint
      // reports what was saved, not what was asked for.
      const site = await (await fetch(`${base}/api/v1/site`)).json();
      ok(
        ids.includes(site.themeId),
        `the stored themeId "${site.themeId}" is a registered id`,
      );
    });

    // -----------------------------------------------------------------------
    // The behaviour layer is core, whatever the theme.
    // -----------------------------------------------------------------------
    await test("a theme switch changes no API contract, session or CSRF rule", async () => {
      // The suite signed in again after the stack restart (signing in revokes every
      // other session, ID-4), so the original cookie is gone. Sign in once more and
      // use those credentials from here on.
      const session = await liveSession({ force: true });
      const { cookie: liveCookie, csrf: liveCsrf } = session;
      ok(Boolean(liveCookie && liveCsrf), "re-authenticated after the restart");

      const put = (themeId) =>
        asAdmin("/api/v1/admin/settings", {
          method: "PUT",
          body: JSON.stringify({
            siteTitle: "Theme Contract",
            commentsEnabled: true,
            commentAutoModerate: true,
            themeId,
          }),
        });

      eq(
        (await put(first)).status,
        200,
        "the first theme can be selected again",
      );

      const endpoints = [
        "/api/v1/healthz",
        "/api/v1/readyz",
        "/api/v1/site",
        "/api/v1/admin/settings",
        "/api/v1/admin/posts",
        "/api/v1/admin/pages",
        "/api/v1/admin/comments",
        "/api/v1/admin/media",
      ];

      const statuses = {};
      for (const target of endpoints)
        statuses[target] = (await asAdmin(target)).status;

      if (second) {
        eq((await put(second)).status, 200, "the second theme can be selected");
      }
      const withSecond = {};
      for (const target of endpoints)
        withSecond[target] = (await asAdmin(target)).status;

      eq(
        JSON.stringify(statuses),
        JSON.stringify(withSecond),
        "every endpoint answers identically whichever theme is active",
      );

      // Unauthenticated admin access is still refused, and lands on the same page.
      const anon = await fetch(`${base}/admin`, { redirect: "manual" });
      eq(anon.status, 303, "GET /admin without a session still redirects");
      ok(
        (anon.headers.get("location") ?? "").startsWith("/admin/login"),
        "it still redirects to the login page",
      );

      // A mutation with a valid session but no CSRF token is still refused. The
      // cookie is included deliberately: without it the request would be rejected as
      // unauthenticated, which would prove nothing about CSRF.
      const noCsrf = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
        },
        body: JSON.stringify({
          siteTitle: "Hijacked",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: first,
        }),
      });
      eq(
        noCsrf.status,
        403,
        "a mutation without a CSRF token is still refused",
      );

      // A foreign Origin is still refused.
      const foreign = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: "https://elsewhere.example",
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({
          siteTitle: "Hijacked",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: first,
        }),
      });
      eq(foreign.status, 403, "a foreign Origin is still refused");

      // The cookie this session already holds still works, so the theme switch
      // neither rotated nor invalidated it.
      eq(
        (await asAdmin("/api/v1/admin/settings")).status,
        200,
        "the session cookie still authenticates",
      );

      // And a fresh sign-in still mints a working cookie and CSRF token.
      const login = await fetch(`${base}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const fresh = sessionCookieFrom(login);
      const { csrfToken: freshCsrf } = await login.json();
      ok(
        Boolean(fresh && freshCsrf),
        "sign-in still returns a cookie and a CSRF token",
      );

      const withFresh = await fetch(`${base}/api/v1/admin/settings`, {
        headers: { Cookie: `blog_session=${fresh}` },
      });
      eq(withFresh.status, 200, "the new cookie authenticates");
    });
    // =======================================================================
    // Media, site metadata and RSS — under *either* theme.
    //
    // The point of these checks is §79: swapping the theme must change presentation
    // and nothing else. A theme that quietly dropped the favicon, the media
    // URLs or the comment list would satisfy every check above and
    // still be a worse theme.
    // =======================================================================

    await test("§79/§104 site settings reach every page under either theme", async () => {
      const identify = (themeId) =>
        putSettings({
          siteTitle: "Field Notes",
          siteSubtitle: "Notes from the edge of the map",
          siteDescription: "A small blog about maps, data and walking.",
          themeId,
        });

      // The title is written *after* the switch, because switching the theme writes
      // a site title of its own; doing it the other way round would prove nothing.
      await setTheme(first);
      const put = await identify(first);
      eq(
        put.status,
        200,
        `the settings write succeeds (${await put.clone().text()})`,
      );

      for (const theme of themes) {
        await setTheme(theme);
        await identify(theme);

        const { homeHtml } = await render();
        ok(
          homeHtml.includes("<title>Field Notes</title>"),
          `${theme}: the browser tab carries the configured site title`,
        );
        ok(
          homeHtml.includes(
            'name="description" content="A small blog about maps, data and walking."',
          ),
          `${theme}: the meta description comes from the settings`,
        );
        ok(
          homeHtml.includes('property="og:title" content="Field Notes"'),
          `${theme}: Open Graph title comes from the settings`,
        );
        ok(
          /<meta property="og:url" content="http[^"]+"/.test(homeHtml),
          `${theme}: Open Graph carries an absolute URL`,
        );
        ok(
          homeHtml.includes('property="og:site_name" content="Field Notes"'),
          `${theme}: Open Graph names the site, by its configured title`,
        );
        ok(
          homeHtml.includes("Notes from the edge of the map") ||
            homeHtml.includes("Field Notes"),
          `${theme}: the configured identity is visible on the page`,
        );

        // §38: the post title format is a CMS decision.
        const post = await fetch(`${base}/posts/themed-content`);
        const postHtml = await post.text();
        eq(post.status, 200, `${theme}: the fixture post is served`);
        const postTitle = postHtml.match(/<title>([^<]*)<\/title>/)?.[1] ?? "";
        ok(
          postTitle === "Themed Content | Field Notes",
          `${theme}: a post is titled "{post} | {site}" (got "${postTitle}")`,
        );
        // §39: og:site_name is the site, never the document. On the home page the two
        // are the same string, so a presence check passed while the post page carried
        // "Themed Content | Field Notes | Field Notes" — the defect only exists
        // *because* the value came from documentTitle.
        ok(
          postHtml.includes('property="og:site_name" content="Field Notes"'),
          `${theme}: a post's og:site_name is the site, not its own title`,
        );
        ok(
          !postHtml.includes(`property="og:site_name" content="${postTitle}"`),
          `${theme}: a post's og:site_name does not repeat the document title`,
        );
        ok(
          postHtml.includes('property="article:published_time"'),
          `${theme}: a post carries its published time for Open Graph`,
        );

        // §78: the admin tab is distinguishable.
        const admin = await asAdmin("/admin");
        const adminHtml = await admin.text();
        eq(admin.status, 200, `${theme}: the admin renders`);
        // §78 decides the format — "{screen} — {site} {suffix}" — and the theme now
        // supplies both words in it, so the two themes are asserted separately rather
        // than against a literal. What must hold for both is that the tab names the
        // screen, the site and the admin area; a bare "Admin" would satisfy neither.
        const adminTitle =
          adminHtml.match(/<title>([^<]*)<\/title>/)?.[1] ?? "";
        ok(
          adminTitle.endsWith("— Field Notes 后台") ||
            adminTitle.endsWith("— Field Notes Admin"),
          `${theme}: the admin tab reads "{screen} — {site} {admin}" (got "${adminTitle}")`,
        );
        ok(
          !/^Admin$/.test(adminTitle),
          `${theme}: the admin tab is not just "Admin" (§78)`,
        );
        ok(
          !adminHtml.includes("<title>Admin</title>"),
          `${theme}: the admin tab is not the bare word "Admin"`,
        );
      }
    });

    await test("§35/§76 the site icon is a media asset, versioned and dynamic", async () => {
      // A real PNG, read from the committed fixture rather than pasted as base64: an
      // icon is a genuine image, and a hand-written blob that does not decode would
      // quietly exercise the fallback path instead of the happy one.
      const png = await readFile(
        path.join(ROOT, "tests", "fixtures", "sample.png"),
      );
      const form = new FormData();
      form.append("file", new Blob([png], { type: "image/png" }), "icon.png");
      const uploaded = await fetch(`${base}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: form,
      });
      const uploadedText = await uploaded.text();
      eq(
        uploaded.status,
        201,
        `the icon uploads (${uploadedText.slice(0, 120)})`,
      );
      const icon = JSON.parse(uploadedText);

      const setIcon = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({ siteIconMediaId: icon.id }),
      });
      eq(
        setIcon.status,
        200,
        `the icon can be selected (${await setIcon.clone().text()})`,
      );

      for (const theme of themes) {
        await setTheme(theme);
        const { homeHtml } = await render();
        const m = homeHtml.match(/<link rel="icon"[^>]*>/);
        ok(m, `${theme}: the document still links an icon`);
        ok(
          m[0].includes(icon.url.split("/").pop()),
          `${theme}: the icon is the configured media asset (${m[0]})`,
        );
        ok(
          /[?&]v=[0-9a-f]{16}/.test(m[0]),
          `${theme}: the icon URL carries a cache-busting version (§76)`,
        );
        ok(
          !m[0].includes("/favicon.png"),
          `${theme}: the theme's own default is not used once an icon is configured`,
        );

        // The icon is served by the delivery layer, so a WebP-capable client gets
        // the converted representation of the *same* URL.
        const fetched = await fetch(`${base}${icon.url}`, {
          headers: { Accept: "image/webp" },
        });
        eq(
          fetched.status,
          200,
          `${theme}: the icon is served from the media URL`,
        );
        eq(
          fetched.headers.get("content-type"),
          "image/webp",
          `${theme}: the icon is negotiated like any other image`,
        );
      }

      // Clearing it returns to the core default, and no theme decides that.
      const cleared = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({ siteIconMediaId: "" }),
      });
      eq(cleared.status, 200, "the icon can be cleared");
      const afterClear = await render();
      const clearedLink = afterClear.homeHtml.match(/<link rel="icon"[^>]*>/);
      ok(
        clearedLink,
        "with no icon configured, a link element is still rendered",
      );
      ok(
        clearedLink[0].includes("/favicon.png"),
        `with no icon configured, the core default is used (${clearedLink[0]})`,
      );
      ok(
        !/[?&]v=/.test(clearedLink[0]),
        `the core default is a static core asset and carries no media version token (${clearedLink[0]})`,
      );
      // The declared type has to match what the browser is actually handed, or a
      // browser may refuse the icon for a reason no page render would show.
      const declaredType = clearedLink[0].match(/type="([^"]+)"/)?.[1];
      const defaultIcon = await fetch(
        `${base}${clearedLink[0].match(/href="([^"]+)"/)?.[1]}`,
      );
      eq(defaultIcon.status, 200, "the core default icon resolves");
      eq(
        defaultIcon.headers.get("content-type"),
        declaredType,
        `the declared icon type matches the served Content-Type (declared ${declaredType})`,
      );
    });

    await test("§65/§66/§71/§105 the feed is core, settings-driven and theme-independent", async () => {
      const readFeed = async () => {
        const res = await fetch(`${base}/rss.xml`);
        return {
          status: res.status,
          type: res.headers.get("content-type"),
          body: await res.text(),
        };
      };

      const perTheme = [];
      for (const theme of themes) {
        await setTheme(theme);
        // setTheme writes a site title of its own, so the identity is re-applied
        // after the switch rather than before it.
        await putSettings({
          siteTitle: "Field Notes",
          siteDescription: "A small blog.",
        });
        const feed = await readFeed();
        eq(feed.status, 200, `${theme}: /rss.xml returns 200`);
        ok(
          (feed.type ?? "").startsWith("application/rss+xml"),
          `${theme}: the feed carries an RSS content type (${feed.type})`,
        );
        ok(
          feed.body.includes('<rss version="2.0"'),
          `${theme}: the document is RSS 2.0`,
        );
        ok(
          feed.body.includes("<title>Field Notes</title>"),
          `${theme}: the channel title is the configured site title`,
        );
        ok(
          feed.body.includes(
            "<link>" +
              base.replace("http://127.0.0.1", "http://127.0.0.1") +
              "</link>",
          ),
          `${theme}: the channel link is absolute`,
        );
        perTheme.push(feed.body);
      }
      // lastBuildDate is the moment the request was served, so it differs between
      // two fetches of the same feed. Everything else must be identical, which is
      // what "the theme does not own the feed" means in practice.
      const withoutTimestamp = (xml) =>
        xml.replace(/<lastBuildDate>[^<]*<\/lastBuildDate>/g, "");
      eq(
        withoutTimestamp(perTheme[0]),
        withoutTimestamp(perTheme[1] ?? perTheme[0]),
        "the feed is byte-identical under either theme (§62, §73)",
      );

      // §67: an explicit description wins; §66: an explicit title wins.
      const customised = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({
          rssTitle: "Field Notes — the feed",
          rssDescription: "Everything, in order.",
          rssItemLimit: 1,
        }),
      });
      eq(customised.status, 200, "the feed can be customised");

      const limited = await readFeed();
      const items = (limited.body.match(/<item>/g) ?? []).length;
      eq(items, 1, `the item limit is honoured (§68), got ${items}`);
      ok(
        limited.body.includes("<title>Field Notes — the feed</title>"),
        "the configured feed title wins over the site title",
      );
      ok(
        limited.body.includes(
          "<description>Everything, in order.</description>",
        ),
        "the configured feed description wins over the site description",
      );
      ok(
        !limited.body.includes("Unfinished Thoughts"),
        "a draft never appears in the feed (§107)",
      );

      // §71: the pages never advertised the feed, and switching the
      // feed off still turns it into a 404 for a reader who knows
      // the address.
      await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({ rssEnabled: false }),
      });
      const off = await readFeed();
      eq(off.status, 404, "a disabled feed is a 404, not an empty channel");
      const { homeHtml } = await render();
      ok(
        !homeHtml.includes('type="application/rss+xml"'),
        "the public pages carry no RSS markup of their own",
      );

      await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({
          rssEnabled: true,
          rssTitle: "",
          rssDescription: "",
          rssItemLimit: 20,
        }),
      });
      await setTheme(first);
      await putSettings({ siteTitle: "Field Notes" });
      const back = await readFeed();
      eq(back.status, 200, "the feed returns when it is switched back on");
      ok(
        back.body.includes("<title>Field Notes</title>"),
        "an empty feed title falls back to the site title (§66)",
      );
    });

    await test("§11/§55/§93 the media library works under either theme", async () => {
      // Read the core script and the contract from disk rather than from the network:
      // the assertion below is about which *names* they agree on, and a served body
      // would let a minifier rename something the check is meant to pin.
      const cmsSource = await readFile(
        path.join(ASTRO, "public", "cms.js"),
        "utf-8",
      );
      const contractSource = await readFile(
        path.join(ASTRO, "src", "theme-system", "js-contract.ts"),
        "utf-8",
      );

      const png = Uint8Array.from(
        atob(
          "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
        ),
        (c) => c.charCodeAt(0),
      );
      const form = new FormData();
      form.append(
        "file",
        new Blob([png], { type: "image/png" }),
        "library.png",
      );
      const uploaded = await fetch(`${base}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: form,
      });
      const image = JSON.parse(await uploaded.text());
      eq(uploaded.status, 201, "a library image uploads");

      for (const theme of themes) {
        await setTheme(theme);
        const media = await asAdmin("/admin/media");
        const html = await media.text();
        eq(media.status, 200, `${theme}: the media library renders`);

        ok(
          html.includes(image.url),
          `${theme}: the library shows the delivery URL, not a path (§56)`,
        );
        ok(
          html.includes('data-cms-form="upload"'),
          `${theme}: the upload form is wired to the core script`,
        );
        // The bound action, not the button's words. The words are the
        // theme's (§31) — bluearchive says 清空图片缓存 — so what
        // has to hold is that the control exists and carries the
        // action the core script binds, and that is what is asserted.
        ok(
          html.includes('data-cms-action="clear-image-cache"'),
          `${theme}: the derived cache can be cleared from the UI (§83)`,
        );
        ok(
          html.includes('data-cms-action="rebuild-media-usage"'),
          `${theme}: the derived usage index can be rebuilt from the UI (§51)`,
        );
        ok(
          html.includes('name="unused"'),
          `${theme}: the unused filter is offered, under the name the API expects (§93)`,
        );

        // §91: the detail offers a copy button, and it copies the *public URL* rather
        // than the stored path. A button that copied the storage path would produce a
        // Markdown reference that only resolves on this deployment. The detail is a
        // disclosure on the library page rather than a route of its own, so this
        // asserts against the page the admin is actually standing on.
        ok(
          html.includes('data-cms-action="copy-text"'),
          `${theme}: the media detail has a copy button (§91)`,
        );
        ok(
          html.includes(`data-cms-copy-text="${image.url}"`),
          `${theme}: the copy button carries the public media URL, not the stored path`,
        );
        ok(
          !html.includes(`data-cms-copy-text="${image.path}"`),
          `${theme}: the stored path is never what a button offers to copy`,
        );

        // §19/§91: the behaviour is core, so the button is bound through the shared
        // contract rather than through anything a theme invented. A theme that shipped
        // its own copy handler would be a control the core never binds — present,
        // clickable, and inert.
        ok(
          cmsSource.includes('"copy-text"') &&
            cmsSource.includes("data-cms-copy-text"),
          "the core script binds the copy button through the contract",
        );
        ok(
          contractSource.includes("copyText: 'copy-text'") &&
            contractSource.includes("'data-cms-copy-text'"),
          "the copy button is declared in the JS contract, not in a theme",
        );

        // §91: the copy button offers the public URL.
        const settings = await asAdmin("/admin/settings");
        const settingsHtml = await settings.text();
        ok(
          settingsHtml.includes('name="siteIconMediaId"'),
          `${theme}: the site icon is chosen from the library, not typed as a path`,
        );
        ok(
          settingsHtml.includes('data-cms-media-value="id"'),
          `${theme}: the icon picker writes the media id`,
        );
        ok(
          settingsHtml.includes("data-cms-media-picker"),
          `${theme}: the icon picker is present on the settings screen`,
        );

        // §120/§122: the editor picks a cover and can upload inline, and neither
        // writes bytes into the Markdown.
        const editor = await asAdmin("/admin/posts/themed-content");
        const editorHtml = await editor.text();
        eq(editor.status, 200, `${theme}: the post editor renders`);
        ok(
          editorHtml.includes('name="cover"'),
          `${theme}: the editor has a cover field`,
        );
        ok(
          editorHtml.includes('data-cms-media-value="url"'),
          `${theme}: the cover picker writes a URL (§81)`,
        );
        ok(
          editorHtml.includes('data-cms-insert-markdown="body"'),
          `${theme}: the editor can upload an image into the body (§122)`,
        );
        ok(
          !editorHtml.includes("data:image"),
          `${theme}: nothing inlines image bytes (§121)`,
        );
      }
    });

    await test("§42/§43/§46 the comment queue works under either theme", async () => {
      // A real pending comment, so the queue has something in it.
      await fetch(`${base}/api/v1/comments`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          postSlug: "themed-content",
          nickname: "Reader One",
          content:
            "This is a <script>alert(1)</script> payload and some ordinary text.",
          startedAt: Date.now() - 30_000,
        }),
      });

      for (const theme of themes) {
        await setTheme(theme);
        const queue = await asAdmin("/admin/comments");
        const html = await queue.text();
        eq(queue.status, 200, `${theme}: the moderation queue renders`);

        ok(
          html.includes('data-cms-action="set-comment-status"'),
          `${theme}: moderation actions are wired to the core script`,
        );
        ok(
          html.includes('data-cms-status-value="approved"'),
          `${theme}: approve is offered`,
        );
        ok(
          html.includes('data-cms-status-value="spam"'),
          `${theme}: mark-as-spam is offered`,
        );
        ok(
          html.includes('data-cms-action="delete-comment"'),
          `${theme}: erase is offered`,
        );
        ok(
          html.includes('name="q"'),
          `${theme}: the queue can be searched (§42)`,
        );

        // §46: the body is plain text. Astro escapes it, so a payload is inert.
        ok(
          !html.includes("<script>alert(1)</script>"),
          `${theme}: a comment payload is never emitted as markup`,
        );
        ok(
          html.includes("&lt;script&gt;") ||
            html.includes("This is a &lt;script&gt;"),
          `${theme}: the payload appears escaped instead`,
        );

        // §43: an indicator, never an address.
        ok(
          html.includes("ipHashIndicator") ||
            /#[0-9a-f]{4,8}/.test(html) ||
            html.includes("unknown"),
          `${theme}: the queue shows an address indicator`,
        );
        ok(
          !/127\.0\.0\.1/.test(html.split("Moderation")[1] ?? html),
          `${theme}: no raw address is shown in the queue`,
        );
      }

      // §44: moderating is a JSON API call, not a theme-rendered form action.
      const approved = await fetch(
        `${base}/api/v1/admin/comments?status=pending&limit=10`,
        {
          headers: { Cookie: `blog_session=${liveCookie}` },
        },
      );
      const pending = await approved.json();
      const target = (pending.items ?? [])[0];
      ok(target, "there is a pending comment to moderate");
      const moderated = await fetch(
        `${base}/api/v1/admin/comments/${target.id}`,
        {
          method: "PATCH",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${liveCookie}`,
            "X-CSRF-Token": liveCsrf,
          },
          body: JSON.stringify({ status: "approved" }),
        },
      );
      eq(moderated.status, 200, "approve succeeds through the JSON API");

      // §45: it is still CSRF-protected and still rate limited.
      const noCsrf = await fetch(`${base}/api/v1/admin/comments/${target.id}`, {
        method: "PATCH",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
        },
        body: JSON.stringify({ status: "spam" }),
      });
      eq(
        noCsrf.status,
        403,
        "moderating without a CSRF token is still refused",
      );

      const foreign = await fetch(
        `${base}/api/v1/admin/comments/${target.id}`,
        {
          method: "PATCH",
          headers: {
            "Content-Type": "application/json",
            Origin: "https://elsewhere.example",
            Cookie: `blog_session=${liveCookie}`,
            "X-CSRF-Token": liveCsrf,
          },
          body: JSON.stringify({ status: "spam" }),
        },
      );
      eq(
        foreign.status,
        403,
        "moderating from a foreign Origin is still refused",
      );

      // §46/D4: the approved comment reaches the public page as text.
      const post = await fetch(`${base}/posts/themed-content`);
      const postHtml = await post.text();
      ok(
        postHtml.includes("Reader One"),
        "the approved comment is visible to visitors",
      );
    });

    await test("§80/§118/§119 a cover is a media URL that survives a theme switch", async () => {
      const png = Uint8Array.from(
        atob(
          "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
        ),
        (c) => c.charCodeAt(0),
      );
      const form = new FormData();
      form.append("file", new Blob([png], { type: "image/png" }), "cover.png");
      const image = JSON.parse(
        await (
          await fetch(`${base}/api/v1/admin/media`, {
            method: "POST",
            headers: {
              Origin: base,
              Cookie: `blog_session=${liveCookie}`,
              "X-CSRF-Token": liveCsrf,
            },
            body: form,
          })
        ).text(),
      );

      // §81: the frontmatter keeps the original URL. No `.webp` appears anywhere.
      const updated = await fetch(`${base}/api/v1/admin/posts/themed-content`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({
          title: "A Theme Switch Changes Markup",
          slug: "themed-content",
          description:
            "A post used to prove a theme switch changes nothing but markup.",
          date: "2026-10-04",
          tags: "themes, contract",
          cover: image.url,
          draft: false,
          body: "# Heading\n\nA paragraph with **bold** text.\n",
        }),
      });
      eq(
        updated.status,
        200,
        `the cover is saved (${await updated.clone().text()})`,
      );

      const stored = await readFile(
        path.join(roots.content, "posts", "themed-content.md"),
        "utf-8",
      );
      ok(
        stored.includes(`cover: ${image.url}`),
        "the frontmatter holds the media URL",
      );
      ok(
        !stored.includes(".webp"),
        "the frontmatter was never rewritten to a WebP URL (§81)",
      );

      const seen = [];
      for (const theme of themes) {
        await setTheme(theme);
        const post = await fetch(`${base}/posts/themed-content`);
        const html = await post.text();
        eq(post.status, 200, `${theme}: the post with a cover renders`);
        ok(
          html.includes(image.url),
          `${theme}: the cover uses the stable media URL, whatever the browser asks for`,
        );
        ok(
          /<meta property="og:image" content="[^"]*\/media\//.test(html),
          `${theme}: the cover is offered as the Open Graph image (§39)`,
        );
        ok(
          !html.includes(`${image.url}.webp`),
          `${theme}: no .webp URL was invented`,
        );
        seen.push(html.includes(image.url));
      }
      ok(seen.every(Boolean), "the cover survives every theme switch (§79)");

      // Put the fixture back the way it was, so later checks are unaffected.
      await fetch(`${base}/api/v1/admin/posts/themed-content`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${liveCookie}`,
          "X-CSRF-Token": liveCsrf,
        },
        body: JSON.stringify({
          title: "A Theme Switch Changes Markup",
          slug: "themed-content",
          description:
            "A post used to prove a theme switch changes nothing but markup.",
          date: "2026-10-04",
          tags: "themes, contract",
          cover: "",
          draft: false,
          body: "# Heading\n\nA paragraph with **bold** text.\n",
        }),
      });
    });
  } finally {
    await stack.stop();
    await cleanup(roots);
  }

  process.exit(summary() ? 0 : 1);
}

main().catch((err) => {
  console.error(`\ntheme tests could not run: ${err?.stack ?? err}`);
  process.exit(1);
});
