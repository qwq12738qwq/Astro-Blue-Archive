#!/usr/bin/env node
/**
 * Architecture compliance checks.
 *
 * These are architecture tests, not style checks. Each one encodes an invariant
 * from ARCHITECTURE.md that a code review could otherwise erode:
 *
 *   §1  Astro is the only HTML renderer; Go returns JSON only
 *   §2  the database never stores article content
 *   §3  V1 has no MDX
 *   §4  publishing never shells out to npm/astro build
 *   §5  the Astro container cannot write content
 *   §6  CONTENT_ROOT comes from the environment, never import.meta.url
 *   §8  the Markdown processor stays isolated in one file
 *   §41 no shortcut architectures
 *   §35 Git backup is a version layer over the filesystem (ID-49 … ID-58):
 *     no remote, no restore, one branch, one inclusion policy, a fixed
 *     commit identity, no second authority
 *
 * Exit code 0 means every invariant holds.
 */
import { readdir, readFile } from "node:fs/promises";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const BACKEND = path.join(ROOT, "backend");
const ASTRO_SRC = path.join(ROOT, "astro", "src");
const ASTRO_DIR = path.join(ROOT, "astro");
const COMPOSE = path.join(ROOT, "docker-compose.yml");

let failures = 0;
let checks = 0;

function pass(name, detail = "") {
  checks++;
  console.log(`  PASS  ${name}${detail ? ` — ${detail}` : ""}`);
}

function fail(name, detail) {
  checks++;
  failures++;
  console.error(
    `  FAIL  ${name}\n        ${detail.replace(/\n/g, "\n        ")}`,
  );
}

async function walk(dir, filter) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...(await walk(full, filter)));
    else if (!filter || filter(full)) out.push(full);
  }
  return out;
}

function section(title) {
  console.log(`\n${title}`);
}

// ---------------------------------------------------------------------------
// §1 / §35 — Go must not produce HTML
// ---------------------------------------------------------------------------

const HTML_FORBIDDEN = [
  { pattern: "text/html", why: "Go must never return an HTML content type" },
  { pattern: "html/template", why: "Go must never use HTML templates" },
  { pattern: "<html", why: "Go must never contain HTML markup" },
  { pattern: "<body", why: "Go must never contain HTML markup" },
  { pattern: "<div", why: "Go must never contain HTML markup" },
  { pattern: "<!DOCTYPE", why: "Go must never contain HTML markup" },
  { pattern: "<script", why: "Go must never contain HTML markup" },
];

section("§1 / §35  Go backend produces no HTML");

const goFiles = await walk(BACKEND, (f) => f.endsWith(".go"));
// Test files legitimately assert on the *absence* of markup, so they are not
// part of the production-code boundary this check protects.
const goProd = goFiles.filter((f) => !f.endsWith("_test.go"));

for (const { pattern, why } of HTML_FORBIDDEN) {
  const re = new RegExp(pattern.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "i");
  const hits = [];
  for (const file of goProd) {
    const text = await readFile(file, "utf-8");
    text.split("\n").forEach((line, i) => {
      if (re.test(line))
        hits.push(`${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`);
    });
  }
  if (hits.length === 0) pass(`no ${pattern}`, why);
  else fail(`found ${pattern}`, hits.join("\n"));
}

// ---------------------------------------------------------------------------
// §4 / §41 — publishing never shells out
// ---------------------------------------------------------------------------

section("§4 / §41  publishing never executes a build");

const GO_EXEC_FORBIDDEN = [
  { pattern: "os/exec", why: "Go must never exec a subprocess" },
  { pattern: "exec.Command", why: "Go must never exec a subprocess" },
];
for (const { pattern, why } of GO_EXEC_FORBIDDEN) {
  const hits = [];
  for (const file of goProd) {
    const text = await readFile(file, "utf-8");
    if (text.includes(pattern)) hits.push(path.relative(ROOT, file));
  }
  if (hits.length === 0) pass(`no ${pattern}`, why);
  else fail(`found ${pattern}`, hits.join("\n"));
}

const ASTRO_EXEC_FORBIDDEN = ["child_process", "execSync", "spawnSync"];
/**
 * Strips Go comments.
 *
 * These checks assert on behaviour, and a comment *discussing* a forbidden thing —
 * "deliberately NOT immutable", for instance — is the opposite of a violation.
 * Reading the comment as the code would make documenting the reason impossible.
 */
function stripGoComments(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

function readFileSyncSafe(f) {
  try {
    return readFileSync(f, "utf-8");
  } catch {
    return "";
  }
}

const astroFiles = await walk(ASTRO_SRC);
// Scripts under astro/public are served verbatim to the browser, so they are just
// as capable of bypassing the theme layer as anything under src.
const publicFiles = await walk(path.join(ASTRO_DIR, "public"));
for (const pattern of ASTRO_EXEC_FORBIDDEN) {
  const hits = [];
  for (const file of astroFiles) {
    const text = await readFile(file, "utf-8");
    if (text.includes(pattern)) hits.push(path.relative(ROOT, file));
  }
  if (hits.length === 0) pass(`no ${pattern} in astro/src`);
  else fail(`found ${pattern}`, hits.join("\n"));
}

// ---------------------------------------------------------------------------
// §2 / §36 — the database holds no article content
// ---------------------------------------------------------------------------

section("§2 / §36  SQLite stores no article content");

const FORBIDDEN_TABLES = [
  "posts",
  "pages",
  "content_cache",
  "search_index",
  "articles",
  "post",
  "page",
];

// comment.post_slug and media_usage.content_slug are *references* to a filesystem
// slug, explicitly allowed by ARCHITECTURE.md §3 and §50. Neither table stores an
// article: media_usage is a derived index over `content/` and is rebuilt by rescanning
// it, so losing the row costs nothing that is not already in the Markdown files.
// These are the only slug-shaped columns permitted.
const FORBIDDEN_COLUMNS = [
  "title",
  "body",
  "content_body",
  "description",
  "tags",
  "draft",
  "markdown",
];
const ALLOWED_COLUMN_EXCEPTIONS = new Set([
  "comment.post_slug",
  "media_usage.content_slug",
]);

const storeFiles = (await walk(BACKEND, (f) => f.endsWith(".go"))).filter((f) =>
  /store\//.test(f),
);
const ddlText = (
  await Promise.all(storeFiles.map((f) => readFile(f, "utf-8")))
).join("\n");

const tableNames = [
  ...ddlText.matchAll(/CREATE TABLE(?: IF NOT EXISTS)?\s+"?(\w+)"?/gi),
].map((m) => m[1]);

if (tableNames.length === 0) {
  fail(
    "could not find any CREATE TABLE statement",
    "store package may have moved",
  );
} else {
  const badTables = tableNames.filter((t) =>
    FORBIDDEN_TABLES.includes(t.toLowerCase()),
  );
  if (badTables.length === 0)
    pass("no content tables", `tables: ${tableNames.join(", ")}`);
  else fail("forbidden tables present", badTables.join(", "));

  const columns = [];
  const tableRe =
    /CREATE TABLE(?: IF NOT EXISTS)?\s+"?(\w+)"?\s*\(([\s\S]*?)\n\);/gi;
  let m;
  while ((m = tableRe.exec(ddlText)) !== null) {
    const [, table, body] = m;
    for (const line of body.split("\n")) {
      const cm = line
        .trim()
        .match(/^"?(\w+)"?\s+(?:[A-Z]+|TEXT|INTEGER|REAL|BLOB|NUMERIC|ANY)/i);
      if (cm) columns.push({ table, column: cm[1] });
    }
  }

  const badColumns = columns
    .filter(({ table, column }) => {
      const key = `${table}.${column}`.toLowerCase();
      if (ALLOWED_COLUMN_EXCEPTIONS.has(key)) return false;
      return FORBIDDEN_COLUMNS.includes(column.toLowerCase());
    })
    .map((c) => `${c.table}.${c.column}`);

  if (badColumns.length === 0) {
    pass("no article content columns", `${columns.length} columns checked`);
  } else {
    fail("article content columns present in schema", badColumns.join(", "));
  }

  const slugCols = columns.filter(({ column }) =>
    column.toLowerCase().endsWith("slug"),
  );
  const unexpected = slugCols.filter(
    (c) =>
      !ALLOWED_COLUMN_EXCEPTIONS.has(`${c.table}.${c.column}`.toLowerCase()),
  );
  if (unexpected.length === 0)
    pass(
      "slug only as a reference",
      [...ALLOWED_COLUMN_EXCEPTIONS].sort().join(", "),
    );
  else
    fail(
      "unexpected slug column",
      unexpected.map((c) => `${c.table}.${c.column}`).join(", "),
    );
}

// ---------------------------------------------------------------------------
// §3 — no MDX
// ---------------------------------------------------------------------------

section("§3  V1 has no MDX");

const mdxHits = [];
for (const file of await walk(ASTRO_SRC)) {
  const text = await readFile(file, "utf-8");
  if (/from ['"][^'"]*mdx[^'"]*['"]/i.test(text))
    mdxHits.push(path.relative(ROOT, file));
}
if (mdxHits.length === 0) pass("no MDX imports in astro/src");
else fail("MDX import found", mdxHits.join("\n"));

// ---------------------------------------------------------------------------
// §8 / R5 — the internal Markdown processor stays isolated
// ---------------------------------------------------------------------------

section("§8 / R5  internal Markdown processor is isolated");

const INTERNAL_MARKDOWN = "@astrojs/markdown-satteri";
const internalHits = [];
for (const file of await walk(ASTRO_SRC)) {
  if (file.endsWith(path.join("lib", "markdown.ts"))) continue;
  const text = await readFile(file, "utf-8");
  if (text.includes(INTERNAL_MARKDOWN))
    internalHits.push(path.relative(ROOT, file));
}
if (internalHits.length === 0)
  pass(`${INTERNAL_MARKDOWN} imported only by src/lib/markdown.ts`);
else
  fail(
    `${INTERNAL_MARKDOWN} leaked outside src/lib/markdown.ts`,
    internalHits.join("\n"),
  );

// ---------------------------------------------------------------------------
// §6 — CONTENT_ROOT comes from the environment
// ---------------------------------------------------------------------------

section("§6  CONTENT_ROOT is resolved from the environment");

const contentTs = await readFile(
  path.join(ASTRO_SRC, "lib", "content.ts"),
  "utf-8",
);
if (!contentTs.includes("process.env.CONTENT_ROOT")) {
  fail(
    "lib/content.ts does not read process.env.CONTENT_ROOT",
    "CONTENT_ROOT must be the only source",
  );
} else {
  pass("lib/content.ts reads process.env.CONTENT_ROOT");
}

const importMetaHits = [];
for (const file of astroFiles) {
  const text = await readFile(file, "utf-8");
  for (const line of text.split("\n")) {
    // Prose in a comment explaining *why* import.meta.url is banned is fine;
    // only executable code matters.
    const isComment = /^\s*(\/\/|\*|\/\*)/.test(line);
    if (isComment) continue;
    if (/import\.meta\.url/.test(line) && /content/i.test(line)) {
      importMetaHits.push(`${path.relative(ROOT, file)}: ${line.trim()}`);
    }
  }
}
if (importMetaHits.length === 0) pass("no import.meta.url content path");
else fail("import.meta.url used to locate content", importMetaHits.join("\n"));

// ---------------------------------------------------------------------------
// §2 / D4 — anonymous input must never be interpreted as markup
// ---------------------------------------------------------------------------

section("§2 / D4  anonymous input is never rendered as markup");

/** Strip comment-only lines so prose explaining a rule does not trip the check. */
function codeLines(text) {
  return text
    .split("\n")
    .map((line) => line.replace(/\/\*.*?\*\//g, ""))
    .filter((line) => !/^\s*(\/\/|\*|\/\*|<!--)/.test(line))
    .join("\n");
}

const MARKUP_FORBIDDEN = [
  { pattern: "set:html", why: "untrusted input must not be injected as HTML" },
  {
    pattern: "innerHTML",
    why: "untrusted input must be inserted with textContent",
  },
  { pattern: "outerHTML", why: "untrusted input must not be injected as HTML" },
  {
    pattern: "insertAdjacentHTML",
    why: "untrusted input must be inserted with textContent",
  },
  { pattern: "dangerouslySet", why: "no React-style raw HTML rendering" },
];

for (const { pattern, why } of MARKUP_FORBIDDEN) {
  const hits = [];
  for (const file of astroFiles) {
    const text = codeLines(await readFile(file, "utf-8"));
    if (text.includes(pattern)) hits.push(path.relative(ROOT, file));
  }
  if (hits.length === 0) pass(`no ${pattern}`, why);
  else fail(`found ${pattern}`, hits.join("\n"));
}

// ---------------------------------------------------------------------------
// §18 (revised) — Astro is the single origin and owns the security headers
// ---------------------------------------------------------------------------

section("§18  Astro is the single origin and owns the headers");

// middleware.ts is the single authority for documents. The media route may add a
// second, narrower policy on the inert-download branch, because that response is a
// sandboxed subresource rather than a page.
const cspAllowlist = new Set([
  path.join(ASTRO_SRC, "middleware.ts"),
  path.join(ASTRO_SRC, "pages", "media", "[...path].ts"),
]);
const cspHits = [];
for (const file of astroFiles) {
  const text = codeLines(await readFile(file, "utf-8"));
  if (/content-security-policy/i.test(text) && !cspAllowlist.has(file)) {
    cspHits.push(path.relative(ROOT, file));
  }
}
if (cspHits.length === 0) {
  pass("CSP is declared only in src/middleware.ts and the media sandbox");
} else {
  fail("a competing CSP header was found", cspHits.join("\n"));
}

const middlewareText = await readFile(
  path.join(ASTRO_SRC, "middleware.ts"),
  "utf-8",
);
if (/script-src 'self'/.test(middlewareText)) {
  pass("Astro sets script-src 'self'");
} else {
  fail(
    "Astro does not set script-src 'self'",
    "the policy must forbid inline scripts",
  );
}
if (/default-src 'self'/.test(middlewareText)) {
  pass("Astro sets default-src 'self'");
} else {
  fail("Astro sets no default-src", "the policy must have a default");
}
if (/unsafe-inline/.test(codeLines(middlewareText))) {
  fail(
    "Astro contains 'unsafe-inline'",
    "ARCHITECTURE.md §18 forbids unsafe-inline",
  );
} else {
  pass("no unsafe-inline in Astro's policy");
}

// The policy and the build have to agree, or a theme's script becomes dead code.
//
// Astro inlines any hoisted script chunk under `build.assetsInlineLimit` (4 KB by
// default) and emits it as `<script type="module">…</script>`. `script-src 'self'`
// without 'unsafe-inline' refuses exactly that, and the symptom is a page whose
// only JavaScript silently does nothing — no build error, no 500, and a CSP
// violation only in a console nobody reads. The bluearchive theme's mobile menu is
// the first script small enough to be caught by this, and it was.
const astroConfig = await readFile(
  path.join(ASTRO_DIR, "astro.config.mjs"),
  "utf-8",
);
if (/assetsInlineLimit:\s*0\b/.test(astroConfig)) {
  pass("scripts are never inlined, so every one of them is a same-origin file");
} else {
  fail(
    "astro.config.mjs does not set vite.build.assetsInlineLimit: 0",
    "A small <script> would be inlined into the document and refused by CSP (script-src 'self').",
  );
}
for (const header of [
  "X-Content-Type-Options",
  "X-Frame-Options",
  "Referrer-Policy",
]) {
  if (middlewareText.includes(header)) pass(`Astro sets ${header}`);
  else
    fail(
      `Astro does not set ${header}`,
      "the header is part of the single-origin policy",
    );
}

// Routing that a reverse proxy used to provide now lives in Astro.
if (existsSync(path.join(ASTRO_SRC, "pages", "api"))) {
  pass("Astro proxies /api/* to the Go backend");
} else {
  fail(
    "no Astro API proxy route",
    "Astro is the single origin and must forward /api/*",
  );
}
if (existsSync(path.join(ASTRO_SRC, "pages", "media"))) {
  pass("Astro serves /media/*");
} else {
  fail(
    "no Astro media route",
    "Astro is the single origin and must serve /media/*",
  );
}

// No reverse proxy may reappear: two origins would mean two cookie scopes and
// two chances to disagree about security headers.
for (const name of [
  "Caddyfile",
  "Caddyfile.dev",
  "docker-compose.yml",
  "nginx.conf",
]) {
  if (existsSync(path.join(ROOT, name))) {
    fail(
      `${name} exists`,
      "the deployment is a single Astro process; there is no proxy",
    );
  } else {
    pass(`no ${name}`);
  }
}

// ---------------------------------------------------------------------------
// §5 (revised) — Astro never writes content
// ---------------------------------------------------------------------------

section("§5  Astro never writes content");

// The loader and the query helpers are read-only by construction; make sure no
// write primitive crept into the frontend.
// Call-shaped patterns: a bare substring would also match 'trim('.
const WRITE_FORBIDDEN = [
  /\bwriteFile(?:Sync)?\s*\(/,
  /\bappendFile(?:Sync)?\s*\(/,
  /\bmkdir(?:Sync)?\s*\(/,
  /\bunlink(?:Sync)?\s*\(/,
  /\brm(?:Sync)?\s*\(/,
  /\brename(?:Sync)?\s*\(/,
  /\bcopyFile(?:Sync)?\s*\(/,
  /\bcreateWriteStream\s*\(/,
];
for (const pattern of WRITE_FORBIDDEN) {
  const hits = [];
  for (const file of astroFiles) {
    const text = codeLines(await readFile(file, "utf-8"));
    if (pattern.test(text)) hits.push(path.relative(ROOT, file));
  }
  if (hits.length === 0) pass(`no ${pattern.source} in astro/src`);
  else fail(`found ${pattern.source} in astro/src`, hits.join("\n"));
}

// ---------------------------------------------------------------------------
// §30 — secrets and comment bodies must not reach the logs
// ---------------------------------------------------------------------------

section("§30  secrets and comment bodies stay out of the logs");

for (const pattern of ["password", "session token"]) {
  const hits = [];
  for (const file of goProd) {
    const text = await readFile(file, "utf-8");
    text.split("\n").forEach((line, i) => {
      if (/^\s*(\/\/|\*)/.test(line)) return;
      if (
        /slog\.(Info|Warn|Error|Debug|Log)\(/.test(line) &&
        line.includes(pattern)
      ) {
        hits.push(`${path.relative(ROOT, file)}:${i + 1}`);
      }
    });
  }
  if (hits.length === 0) pass(`no logging of ${pattern}`);
  else fail(`${pattern} may reach the logs`, hits.join("\n"));
}

// ---------------------------------------------------------------------------
// public/*.js is served straight to the browser, so it must be plain JavaScript
// ---------------------------------------------------------------------------

section("public/*.js is plain JavaScript");

const PUBLIC_DIR = path.join(ASTRO_DIR, "public");

if (!existsSync(PUBLIC_DIR)) {
  fail("astro/public is missing", "admin scripts live there");
} else {
  const scripts = (await readdir(PUBLIC_DIR)).filter((n) => n.endsWith(".js"));
  if (scripts.length === 0) {
    fail("no scripts in astro/public", "admin behaviour would be unreachable");
  }

  const { execFile } = await import("node:child_process");
  const { promisify } = await import("node:util");
  const run = promisify(execFile);

  for (const name of scripts) {
    const file = path.join(PUBLIC_DIR, name);
    try {
      // `node --check` parses without executing: TypeScript syntax such as
      // `x as T` is a SyntaxError here and in the browser.
      await run(process.execPath, ["--check", file]);
      pass(`${name} parses as plain JavaScript`);
    } catch (err) {
      const detail = String(err.stderr || err.message)
        .split("\n")
        .slice(0, 4)
        .join("\n");
      fail(`${name} is not valid browser JavaScript`, detail);
    }
  }

  /*
   * Every one of these files is a *classic* script, served from a fixed URL and
   * loaded on the same page, so they share one global lexical scope.
   *
   * That makes a top-level `const` a cross-file hazard rather than a local one.
   * `cms.js` and `comments.js` both mirrored `js-contract.ts` with
   * `const ATTR_FORM = …`; the second file to load was not a name clash the
   * browser resolves, it was `SyntaxError: Identifier 'ATTR_FORM' has already been
   * declared` and the entire file never executed. Nothing on the server could see
   * it: the page rendered, the API tests passed, and the buttons silently did
   * nothing — including the sign-in form, which is why it was found by loading a
   * page in a browser rather than by a test.
   *
   * The fix is a per-file scope (an IIFE), and the rule that keeps it is this one:
   * no core script may declare anything at column 0.
   */
  const leaking = [];
  for (const name of scripts) {
    const text = codeLines(
      await readFile(path.join(PUBLIC_DIR, name), "utf-8"),
    );
    text.split("\n").forEach((line, i) => {
      const m = line.match(
        /^(?:const|let|var|class|function|async)\s+([A-Za-z_$][\w$]*)/,
      );
      if (m) leaking.push(`${name}:${i + 1}: ${m[1]}`);
    });
  }
  if (leaking.length === 0) {
    pass(
      `every core script keeps its declarations to itself (${scripts.length} files; classic scripts share one global scope)`,
    );
  } else {
    fail(
      "a core script declares a top-level name",
      leaking.join("\n") +
        "\n        Two classic scripts sharing one global scope cannot both declare the same const: the\n        second file is a SyntaxError and never runs. Wrap the file in an IIFE.",
    );
  }
}

// ---------------------------------------------------------------------------
// ID-13 — the theme must be swappable, and custom.css must actually win
// ---------------------------------------------------------------------------

section("ID-13  the theme is swappable");

/**
 * Theme stylesheets are enumerated rather than named.
 *
 * ARCHITECTURE.md §44: CSS is theme-owned, so a new theme brings its own files and
 * the check has to follow. Hard-coding `base.css`/`admin.css` here would have made
 * a second theme's stylesheet unchecked by construction — which is exactly the
 * class of bug these checks exist to catch.
 */
const THEMES_DIR = path.join(ASTRO_SRC, "themes");
const themeDirs = existsSync(THEMES_DIR)
  ? (await readdir(THEMES_DIR, { withFileTypes: true }))
      .filter((entry) => entry.isDirectory())
      .map((entry) => ({
        name: entry.name,
        dir: path.join(THEMES_DIR, entry.name),
      }))
  : [];

/**
 * A theme's `assets/` tree is not a stylesheet: it holds the theme's
 * static files — webfonts, artwork, cursors. The only CSS that
 * belongs there is a vendored `@font-face` sheet, and `@font-face`
 * does not participate in the cascade, so it cannot beat
 * content/system/custom.css no matter how it is layered.
 *
 * What it may never contain is a style rule: one selector smuggled
 * into an assets sheet would ride custom.css's `@import` into the
 * unlayered origin and beat every `@layer theme` rule. So an assets
 * stylesheet is checked the opposite way — strip `@font-face` blocks
 * and comments, and require that nothing is left.
 */
const inThemeAssets = (file, themeDir) =>
  file.startsWith(path.join(themeDir, "assets") + path.sep);

const fontFaceOnly = (css) =>
  !/\{/.test(
    css
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/@font-face\s*\{[^{}]*\}/g, ""),
  );

if (themeDirs.length === 0) {
  fail(
    "no themes under astro/src/themes",
    "the site has nothing to render with",
  );
} else {
  pass(
    `${themeDirs.length} theme(s) under astro/src/themes: ${themeDirs.map((t) => t.name).join(", ")}`,
  );

  for (const { name, dir } of themeDirs) {
    const cssFiles = (await walk(dir)).filter((f) => f.endsWith(".css"));
    const stylesheets = cssFiles.filter((f) => !inThemeAssets(f, dir));
    if (stylesheets.length === 0) {
      fail(
        `the ${name} theme has no stylesheet`,
        "a theme must bring its own CSS",
      );
      continue;
    }
    const unlayered = [];
    const smuggled = [];
    for (const file of cssFiles) {
      const css = await readFile(file, "utf-8");
      if (inThemeAssets(file, dir)) {
        if (!fontFaceOnly(css)) smuggled.push(path.relative(ROOT, file));
        continue;
      }
      if (!/@layer\s+theme\s*\{/.test(css)) {
        unlayered.push(path.relative(ROOT, file));
      }
    }
    if (smuggled.length > 0) {
      fail(
        `the ${name} theme carries a style rule in its assets`,
        smuggled.join("\n") +
          "\n        A vendored assets sheet may hold @font-face only: a style\n        rule there rides custom.css's @import into the unlayered origin.",
      );
    } else if (unlayered.length === 0) {
      pass(
        `every ${name} stylesheet is inside @layer theme (${stylesheets.length} checked)`,
      );
    } else {
      fail(
        `the ${name} theme has unlayered stylesheets`,
        unlayered.join("\n") +
          "\n        Unlayered theme rules would beat content/system/custom.css.",
      );
    }
  }
}

// A theme must not carry content. ARCHITECTURE.md §7: content/ is the only source
// of truth for articles and pages. A Markdown file under a theme would be a second
// source that the loader never reads and the admin can never edit.
const themeContent = [];
for (const { name, dir } of themeDirs) {
  for (const file of await walk(dir)) {
    if (/\.(md|mdx|markdown|mdwn)$/i.test(file)) {
      themeContent.push(`${name}: ${path.relative(ROOT, file)}`);
    }
  }
}
if (themeContent.length === 0) {
  pass("no content files inside a theme");
} else {
  fail("a theme contains content files", themeContent.join("\n"));
}

// An inline style attribute cannot be overridden by a theme without !important.
const inlineStyleHits = [];
for (const file of astroFiles) {
  if (!file.endsWith(".astro")) continue;
  const text = await readFile(file, "utf-8");
  text.split("\n").forEach((line, i) => {
    if (/ style="/.test(line)) {
      inlineStyleHits.push(
        `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`,
      );
    }
  });
}
if (inlineStyleHits.length === 0) {
  pass("no inline style attributes in any Astro component");
} else {
  fail(
    "inline style attributes found (a theme cannot override these)",
    inlineStyleHits.join("\n"),
  );
}

// The colour-scheme hook is a per-visitor display preference, entirely separate
// from the theme pack. Both are declared on <html>; neither is derived from the
// other.
const middleware = await readFile(
  path.join(ASTRO_SRC, "middleware.ts"),
  "utf-8",
);

// Every layout must honour the whole contract. Admin once linked custom.css but
// loaded neither the colour-scheme script nor the attribute, so an explicit
// light/dark choice was silently dropped the moment you opened the admin. Naming
// one layout would not have caught that, so this walks every theme's layouts.
//
// A layout may satisfy a requirement either by naming the URL literally or by
// rendering one of the exported lists from theme-system/js-contract.ts. The second form
// is preferred — it cannot drift — and both are accepted, because the requirement
// is "this resource is loaded", not "this file spells it a particular way".
const layouts = (await walk(THEMES_DIR)).filter(
  (f) => f.endsWith(".astro") && f.includes(`${path.sep}layouts${path.sep}`),
);
if (layouts.length === 0) {
  fail("no theme layouts found", "a theme must own the <html> document shell");
}

for (const layout of layouts) {
  const rel = path.relative(ROOT, layout);
  const text = await readFile(layout, "utf-8");
  const admin =
    text.includes("admin-shell") || text.includes("ADMIN_BODY_SCRIPTS");

  const problems = [];
  if (!/data-color-scheme=/.test(text)) {
    problems.push("no data-color-scheme attribute on <html>");
  }
  if (!/data-cms-theme=/.test(text)) {
    problems.push(
      "no data-cms-theme attribute on <html>, so the active theme is invisible",
    );
  }
  if (!/color-scheme\.js|HEAD_SCRIPTS/.test(text)) {
    problems.push(
      "color-scheme.js is not loaded, so the stored choice is ignored",
    );
  }
  if (!/custom\.css|CUSTOM_STYLESHEET/.test(text)) {
    problems.push(
      "custom.css is not linked, so admin CSS overrides cannot work",
    );
  }
  // A layout must load exactly the core script set for its area: the admin one for
  // the admin shell, the public one otherwise. This is where "JS is not part of
  // the theme" is enforced rather than asserted — a theme that dropped /cms.js
  // would ship an admin whose buttons do nothing.
  const wanted = admin ? "ADMIN_BODY_SCRIPTS" : "PUBLIC_BODY_SCRIPTS";
  if (!text.includes(wanted)) {
    problems.push(
      `the ${wanted} list is not rendered, so the core scripts may be missing`,
    );
  }

  if (problems.length === 0) {
    pass(`${rel} honours the colour-scheme and theme contract`);
  } else {
    fail(`${rel} does not honour the contract`, problems.join("; "));
  }
}

if (existsSync(path.join(ASTRO_DIR, "public", "color-scheme.js"))) {
  pass("public/color-scheme.js exists and is loaded externally");
} else {
  fail(
    "public/color-scheme.js is missing",
    "the stored colour-scheme choice would not be applied",
  );
}

// Colours must go through custom properties, never a literal in a component.
const colourLiterals = [];
for (const file of astroFiles) {
  if (!file.endsWith(".astro")) continue;
  const text = await readFile(file, "utf-8");
  const hits = text.match(/#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/g);
  if (hits)
    colourLiterals.push(`${path.relative(ROOT, file)}: ${hits.join(", ")}`);
}
if (colourLiterals.length === 0) {
  pass(
    "no colour literals in Astro components (all go through custom properties)",
  );
} else {
  fail("colour literals bypass the theme tokens", colourLiterals.join("\n"));
}

// The public site must not fetch settings per request. A naive fetch made every
// anonymous request open its own connection and block for the full timeout, so a
// burst of anonymous requests aimed at an already-struggling backend. lib/site.ts
// caches, coalesces in-flight calls and opens a circuit breaker; verify all three
// are still there rather than trusting the comment.
const siteLib = await readFile(path.join(ASTRO_SRC, "lib", "site.ts"), "utf-8");
const viewContext = await readFile(
  path.join(ASTRO_SRC, "lib", "view-context.ts"),
  "utf-8",
);
const goRss = await readFile(
  path.join(ROOT, "backend", "internal", "api", "rss.go"),
  "utf-8",
);
const guards = [
  ["TTL_MS", /const TTL_MS\s*=/, "caches the value"],
  [
    "BREAKER_MS",
    /const BREAKER_MS\s*=/,
    "opens a circuit breaker after a failure",
  ],
  ["TIMEOUT_MS", /const TIMEOUT_MS\s*=/, "bounds the wait on the backend"],
  [
    "inFlight",
    /if \(inFlight\) return inFlight/,
    "coalesces concurrent callers into one request",
  ],
  [
    "stale value",
    /return cached/,
    "serves the last good value during an outage",
  ],
];
for (const [name, re, why] of guards) {
  if (re.test(siteLib)) pass(`lib/site.ts ${why} (${name})`);
  else
    fail(
      `lib/site.ts lost its ${name} guard`,
      "the public site would depend on the backend per request",
    );
}
if (siteLib.match(/const TIMEOUT_MS\s*=\s*(\d[\d_]*)/)?.[1]) {
  const t = Number(
    siteLib.match(/const TIMEOUT_MS\s*=\s*(\d[\d_]*)/)[1].replace(/_/g, ""),
  );
  if (t <= 1000)
    pass(
      `the settings timeout is ${t}ms, so a page cannot stall on the backend`,
    );
  else
    fail(
      `the settings timeout is ${t}ms`,
      "a page render holds for that long when the backend is wedged",
    );
}

// Both consumers must go through the cached helper, not fetch for themselves.
// The feed is one of them, but it lives in the Go backend now (§28), where
// every request already reads settings through the backend's own loader —
// so the cached-helper rule is asserted for the Astro-side consumer alone.
for (const rel of ["lib/view-context.ts"]) {
  const text = await readFile(path.join(ASTRO_SRC, rel), "utf-8");
  if (/getSiteSettings/.test(text) && !/AbortSignal\.timeout/.test(text)) {
    pass(`${rel} reads settings through the cached helper`);
  } else {
    fail(
      `${rel} bypasses the settings cache`,
      "use getSiteSettings() from lib/site.ts",
    );
  }
}

// The custom-code editor is pointless if the site title is hardcoded. A
// fallback literal is fine — the backend may be down — so the invariant is that
// the value is actually fetched, not that no literal exists.
// Both Base.astro and the feed used to hardcode 'Blog', so the admin setting
// had no visible effect. The fetch now lives in lib/site.ts (see the cache checks
// above), so follow the indirection rather than grepping for the endpoint.
if (siteLib.includes("/api/v1/site")) {
  pass("the site title is fetched from the API by lib/site.ts");
} else {
  fail(
    "lib/site.ts does not request /api/v1/site",
    "the admin setting for the title would have no visible effect",
  );
}
if (
  viewContext.includes("getSiteSettings") &&
  goRss.includes("loadSettings") &&
  goRss.includes("RSSTitle")
) {
  pass("both the page header and the RSS feed render the configured title");
} else {
  fail(
    "the configured title is not used by both the header and the feed",
    "one of them is still showing a hardcoded default",
  );
}

// A class used in markup but defined in no stylesheet is invisible styling that
// used to live in an inline attribute. Removing the attribute without adding the
// rule silently changes the page, which is exactly what happened to .honeypot.
const usedClasses = new Map();
for (const file of astroFiles) {
  if (!file.endsWith(".astro")) continue;
  const text = await readFile(file, "utf-8");
  for (const m of text.matchAll(/\sclass="([^"{}]+)"/g)) {
    // Strip Astro's template expressions; only literal class names are checkable.
    for (const raw of m[1].split(/\s+/)) {
      const name = raw.trim();
      if (
        !name ||
        name.includes("{") ||
        name.includes("}") ||
        name.includes("$")
      )
        continue;
      if (!usedClasses.has(name)) usedClasses.set(name, []);
      usedClasses.get(name).push(path.relative(ROOT, file));
    }
  }
}

const defined = new Set();
for (const file of [
  ...astroFiles.filter((f) => f.endsWith(".css")),
  ...astroFiles.filter((f) => f.endsWith(".astro")),
]) {
  const text = await readFile(file, "utf-8");
  for (const m of text.matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)) defined.add(m[1]);
}
// third-party / runtime classes that legitimately have no rule here
for (const name of ["astro-transition-swap", "view-transition", "list-item"])
  defined.add(name);

const orphans = [...usedClasses.entries()]
  .filter(([name]) => !defined.has(name))
  .map(([name, files]) => `  .${name}  <- ${files[0]}`);

if (orphans.length === 0) {
  pass(
    `every class used in markup is defined in a stylesheet (${usedClasses.size} checked)`,
  );
} else {
  fail(
    "classes used in markup but defined in no stylesheet",
    orphans.join("\n"),
  );
}

// Every rule the theme owns must live in src/styles/*.css inside @layer theme.
// A component <style> block is compiled by Astro into an *unlayered* stylesheet
// with an extra attribute selector for specificity, so it becomes a second source
// of styling the theme cannot control and custom.css cannot reliably beat.
const scopedBlocks = [];
for (const file of astroFiles) {
  if (!file.endsWith(".astro")) continue;
  const text = await readFile(file, "utf-8");
  if (/<style[\s>]/.test(text)) scopedBlocks.push(path.relative(ROOT, file));
}
if (scopedBlocks.length === 0) {
  pass(
    "no <style> blocks in any .astro file (all CSS is layered and theme-owned)",
  );
} else {
  fail(
    "component <style> blocks found",
    "Astro inlines them unlayered, which puts styling outside the theme:\n" +
      scopedBlocks.map((f) => `  ${f}`).join("\n"),
  );
}

// Sanity: everything the theme needs is actually present in the layer. These rules
// used to live in component <style> blocks and were migrated into the stylesheet;
// losing one during the move silently changes the page.
const movedRules = [
  ".post-card h2",
  ".comment .body",
  ".error-page h1",
  ".comment-list",
];
for (const { name, dir } of themeDirs) {
  const themeCss = await readFile(
    path.join(dir, "public", "styles", "public.css"),
    "utf-8",
  );
  const missing = movedRules.filter((r) => !themeCss.includes(r));
  if (missing.length === 0) {
    pass(
      `the rules migrated out of the removed <style> blocks are in the ${name} theme`,
    );
  } else {
    fail(`rules lost in the migration (${name})`, missing.join(", "));
  }
}

// The remaining ways styling can escape the theme layer. Each one was a real hole
// in this project's life, so each is now a build failure. The rendered-output
// halves of these live in tests/fullstack-tests.mjs, where a server exists.
section("ID-13  no route around the theme layer");

// 1. Every stylesheet Astro ships must be layered, not just the ones we know about.
//    A theme's assets/ tree is exempt the same way as above: it may hold a
//    vendored @font-face sheet, which the cascade cannot see, and nothing else.
const cssFiles = astroFiles.filter((f) => f.endsWith(".css"));
const unlayered = [];
const smuggled = [];
for (const file of cssFiles) {
  const css = await readFile(file, "utf-8");
  const assetOf = themeDirs.find(({ dir }) => inThemeAssets(file, dir));
  if (assetOf) {
    if (!fontFaceOnly(css)) smuggled.push(path.relative(ROOT, file));
    continue;
  }
  if (!/@layer\s+theme\s*\{/.test(css))
    unlayered.push(path.relative(ROOT, file));
}
if (smuggled.length > 0) {
  fail(
    "a theme asset stylesheet carries a style rule",
    smuggled.join("\n") +
      "\n        A vendored assets sheet may hold @font-face only.",
  );
} else if (unlayered.length === 0) {
  pass(
    `every stylesheet under astro/src is inside @layer theme (${cssFiles.length} checked)`,
  );
} else {
  fail("unlayered stylesheets", unlayered.join("\n"));
}

// 2. Script must not reintroduce inline styles. A theme cannot override a value
//    the browser computed from a style attribute, whatever wrote it.
const jsStyleWrites = [];
for (const file of [...astroFiles, ...publicFiles]) {
  if (!/\.(js|mjs|ts)$/.test(file)) continue;
  const text = await readFile(file, "utf-8");
  text.split("\n").forEach((line, i) => {
    if (
      /\.style\.\s*[A-Za-z-]+\s*=/.test(line) ||
      /setAttribute\(\s*['"`]style['"`]/.test(line)
    ) {
      jsStyleWrites.push(
        `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`,
      );
    }
  });
}
if (jsStyleWrites.length === 0) {
  pass(
    `no script writes an inline style, in src or public (a theme could not override it)`,
  );
} else {
  fail("script writes inline styles", jsStyleWrites.join("\n"));
}

// A setting that is stored, validated and editable in the admin UI but read by
// nothing is worse than no setting: the admin changes it and nothing happens.
// `postsPerPage` sat in that state for the whole project — the index renders every
// post. Require every stored key to be consumed by the rendering path.
section("settings must be consumed by something that renders");

const settingsGo = await readFile(
  path.join(ROOT, "backend", "internal", "api", "settings_handlers.go"),
  "utf-8",
);
const keys = [...settingsGo.matchAll(/setting[A-Za-z]+\s*=\s*"([^"]+)"/g)].map(
  (m) => m[1],
);
const unique = [...new Set(keys)].sort();

// The admin form necessarily mentions every key, so it cannot count as a consumer.
const FORM = path.join(ASTRO_SRC, "pages", "admin", "settings.astro");
const consumers = (await walk(ASTRO_SRC))
  .filter((f) => f.endsWith(".astro") || f.endsWith(".ts"))
  .filter((f) => path.resolve(f) !== FORM);

const dead = [];
for (const key of unique) {
  const used = consumers.some((f) => readFileSyncSafe(f).includes(key));
  if (!used) dead.push(key);
}

if (dead.length === 0) {
  pass(
    `every stored setting is consumed by the rendering path (${unique.length} checked: ${unique.join(", ")})`,
  );
} else {
  fail(
    "settings that nothing on the site reads",
    `${dead.join(", ")}\n  Editable in the admin UI, but no page consumes the value.`,
  );
}

// Rate limiting identifies clients by the real peer address. Astro proxies every
// request, so Go would otherwise see only 127.0.0.1 and every client would share
// one bucket: five bad logins from anyone locked the admin out, repeatable every
// minute. Both halves of the fix are load-bearing, so both are enforced here.
section("rate limiting identifies the real client");

const proxySrc = await readFile(
  path.join(ASTRO_SRC, "pages", "api", "[...path].ts"),
  "utf-8",
);
const ipGo = await readFile(
  path.join(ROOT, "backend", "internal", "httpx", "middleware.go"),
  "utf-8",
);

// Astro must take the address from the socket, never from a request header.
if (
  /clientAddress/.test(proxySrc) &&
  /headers\.set\(\s*CLIENT_IP_HEADER/.test(proxySrc)
) {
  pass(
    "the proxy sets the client IP from Astro's clientAddress (the TCP peer)",
  );
} else {
  fail(
    "the proxy does not set the client IP from clientAddress",
    "rate limiting cannot see the real client",
  );
}

// It must also drop whatever the client sent under that name, or a caller could
// pick its own bucket.
if (/lower === CLIENT_IP_HEADER\) continue/.test(proxySrc)) {
  pass(
    "the proxy strips any client-supplied client-IP header before setting its own",
  );
} else {
  fail(
    "the proxy forwards the client-supplied client-IP header",
    "a caller could choose its own rate-limit bucket",
  );
}

// Go must honour the header only from loopback.
if (/IsLoopback\(\)/.test(ipGo) && /ClientIPHeader/.test(ipGo)) {
  pass("Go trusts the forwarded client IP only from a loopback peer");
} else {
  fail(
    "Go trusts the client-IP header without checking the peer",
    "any client could forge its address",
  );
}

// The header name must match on both sides or the feature silently does nothing.
const name = proxySrc.match(/CLIENT_IP_HEADER\s*=\s*'([^']+)'/)?.[1];
const goName = ipGo.match(/ClientIPHeader\s*=\s*"([^"]+)"/)?.[1];
if (name && goName && name.toLowerCase() === goName.toLowerCase()) {
  pass(`the header name matches on both sides (${name})`);
} else {
  fail(
    "the client-IP header names differ between Astro and Go",
    `${name} vs ${goName}`,
  );
}

// The design-token contract is a set of custom properties. A token that one theme
// does not declare is a token that theme cannot be given a value for: the dark
// variants shipped ten of the fourteen, so a theme setting --radius or a font
// stack was silently ignored in dark mode. Enforce exact parity for every theme,
// not just the first one — a second theme that skipped a token would otherwise be
// a build-time surprise rather than a caught mistake.
section("ID-13 / §17  every theme declares the same design tokens");

const TOKEN_ROOT = [
  "--bg",
  "--fg",
  "--muted",
  "--border",
  "--accent",
  "--accent-fg",
  "--code-bg",
  "--danger",
  "--ok",
  "--warn",
  "--radius",
  "--measure",
  "--font-sans",
  "--font-mono",
  /*
   * Motion tokens. ARCHITECTURE.md §11a: the theme expresses "stop animating the
   * page" as a token remap rather than as `!important`, so these are part of the
   * contract too — and they are the only way the preloader's two halves can be
   * guaranteed to stay the same length (§33).
   */
  "--motion-fast",
  "--motion",
  "--motion-preloader",
];
const VARIANTS = [
  ["the OS dark preference", ":root:not([data-color-scheme='light'])"],
  ["the explicit dark choice", ":root[data-color-scheme='dark']"],
];

/** Custom properties declared inside the block for a given selector. */
function tokensFor(css, selector) {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const re = new RegExp(`${escaped}\\s*\\{([^{}]*)\\}`, "g");
  const found = new Set();
  for (const m of css.matchAll(re)) {
    for (const p of m[1].matchAll(/(--[\w-]+)\s*:/g)) found.add(p[1]);
  }
  return found;
}

for (const { name, dir } of themeDirs) {
  // A theme's tokens may live in its public or admin stylesheet, or be split
  // between them, so the union is what matters.
  const css = (
    await Promise.all(
      (await walk(dir))
        .filter((f) => f.endsWith(".css"))
        .map((f) => readFile(f, "utf-8")),
    )
  ).join("\n");

  const light = tokensFor(css, ":root");
  if (light.size === 0) {
    fail(
      `the ${name} theme declares no tokens`,
      "the token check needs a real :root block",
    );
    continue;
  }

  const undocumented = [...light].filter((t) => !TOKEN_ROOT.includes(t));
  if (undocumented.length === 0) {
    pass(`the ${name} theme declares the ${light.size} contract tokens`);
  } else {
    fail(
      `the ${name} theme declares tokens outside the contract`,
      `${undocumented.join(" ")}\n  Add it to TOKEN_ROOT in this file and to the contract in AGENTS.md, or remove it.`,
    );
  }

  for (const [label, selector] of VARIANTS) {
    const set = tokensFor(css, selector);
    const gone = [...light].filter((t) => !set.has(t));
    const extra = [...set].filter((t) => !light.has(t));
    if (gone.length === 0 && extra.length === 0) {
      pass(`the ${name} theme: ${label} declares the same ${set.size} tokens`);
    } else {
      const parts = [];
      if (gone.length) parts.push(`missing ${gone.join(" ")}`);
      if (extra.length) parts.push(`not in light: ${extra.join(" ")}`);
      fail(
        `the ${name} theme: ${label} disagrees with its light tokens`,
        parts.join("; "),
      );
    }
  }

  // The two dark blocks must not drift apart; they can only differ by selector.
  const a = [...tokensFor(css, ":root:not([data-color-scheme='light'])")]
    .sort()
    .join(" ");
  const b = [...tokensFor(css, ":root[data-color-scheme='dark']")]
    .sort()
    .join(" ");
  if (a === b && a.length > 0) {
    pass(
      `the ${name} theme: both dark variants declare an identical token set`,
    );
  } else {
    fail(
      `the ${name} theme: the two dark variants disagree about tokens`,
      "an explicit dark choice would differ from the OS preference",
    );
  }

  // Every theme's *admin* stylesheet has to stand on its own.
  //
  // The admin layout links only the admin stylesheet (ID-18), so a theme that
  // declared its tokens in public.css alone rendered an admin with no --bg, no
  // --border and no --accent. Every `var()` in that file then resolved to nothing:
  // it still looked plausible enough not to be reported, and no test caught it
  // because the public page — the one with the screenshot — was perfect.
  const adminCss = await readFile(
    path.join(dir, "admin", "styles", "admin.css"),
    "utf-8",
  ).catch(() => "");
  if (adminCss.length === 0) {
    fail(
      `the ${name} theme has no admin/styles/admin.css`,
      "an admin page needs its own styles",
    );
  } else {
    const adminTokens = tokensFor(adminCss, ":root");
    const absent = TOKEN_ROOT.filter((token) => !adminTokens.has(token));
    if (absent.length === 0) {
      pass(
        `the ${name} theme declares all ${TOKEN_ROOT.length} tokens in its admin stylesheet too`,
      );
    } else {
      fail(
        `the ${name} theme's admin stylesheet is missing tokens`,
        absent.join(" ") +
          "\n        The admin layout links only the admin stylesheet, so it needs its own.",
      );
    }
  }
}

// ---------------------------------------------------------------------------
// §44 / §45 / §26 — the Theme Contract
//
// The theme system is only real if its boundaries hold. Each check below encodes
// one way the boundary could be crossed, and the failure message says what would
// break if it were.
// ---------------------------------------------------------------------------

section("§44 / §45  the Theme Contract holds");

const THEME_CONTRACT = path.join(ASTRO_SRC, "theme-system", "contract.ts");
const THEME_IDS_TS = path.join(ASTRO_SRC, "theme-system", "ids.ts");
const THEME_REGISTRY = path.join(ASTRO_SRC, "theme-system", "registry.ts");
const JS_CONTRACT = path.join(ASTRO_SRC, "theme-system", "js-contract.ts");

/**
 * The theme id allowlist, read once.
 *
 * Several checks below need it — the id-agreement check in section 6, and the
 * "no script branches on a theme id" rules in sections 4b and 7 — so it is read
 * here rather than three times.
 */
const idsText = await readFile(THEME_IDS_TS, "utf-8").catch(() => "");
const frontendIds = [
  ...(idsText.match(/THEME_IDS\s*=\s*\[([^\]]*)\]/)?.[1] ?? "").matchAll(
    /'([^']+)'/g,
  ),
].map((m) => m[1]);

// --- 1. The three files the whole mechanism rests on must exist --------------

for (const [label, file] of [
  ["theme-system/contract.ts", THEME_CONTRACT],
  ["theme-system/ids.ts", THEME_IDS_TS],
  ["theme-system/registry.ts", THEME_REGISTRY],
  ["theme-system/js-contract.ts", JS_CONTRACT],
]) {
  if (existsSync(file)) pass(`${label} exists`);
  else fail(`${label} is missing`, "the theme system cannot work without it");
}

// --- 2. Only the registry may import a theme -------------------------------
//
// A page that imports a theme directly is a page pinned to that theme: the moment
// there are two, half the site ignores the switch. This is the single check that
// makes "swap the theme without touching the CMS" true rather than aspirational.

const registryText = existsSync(THEME_REGISTRY)
  ? await readFile(THEME_REGISTRY, "utf-8")
  : "";
const themeImportHits = [];
for (const file of astroFiles) {
  if (file === THEME_REGISTRY) continue;
  const inThemes = file.startsWith(`${THEMES_DIR}${path.sep}`);
  const text = await readFile(file, "utf-8");
  for (const [i, line] of text.split("\n").entries()) {
    if (/^\s*(\/\/|\*|\/\*)/.test(line)) continue;
    const specifier = line.match(/from\s+'([^']*themes\/[^']*)'/);
    if (!specifier) continue;
    // A theme importing a *sibling* theme is also a boundary crossing.
    if (!inThemes) {
      themeImportHits.push(
        `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}\n` +
          "          Reach a theme through Astro.locals.theme, which the middleware resolved.",
      );
    } else if (!file.endsWith(`${path.sep}theme.ts`)) {
      themeImportHits.push(
        `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}\n` +
          "          One theme may not import another; only each theme's theme.ts may name its own files.",
      );
    }
  }
}
if (themeImportHits.length === 0) {
  pass(
    "no module imports a theme except the registry (and each theme's own manifest)",
  );
} else {
  fail(
    "a theme was imported directly, bypassing the registry",
    themeImportHits.join("\n"),
  );
}

// --- 3. The middleware is the single theme resolver -------------------------

const resolverText = await readFile(
  path.join(ASTRO_SRC, "theme-system", "resolve.ts"),
  "utf-8",
).catch(() => "");
if (/locals\.theme\s*=\s*await resolveRequestTheme\(\)/.test(middlewareText)) {
  pass(
    "middleware resolves the theme once per request into Astro.locals.theme",
  );
} else {
  fail(
    "middleware does not assign locals.theme",
    "ARCHITECTURE.md §19: there must be exactly one place a request is bound to a theme",
  );
}
if (
  /Astro\.locals\.theme|locals\.theme/.test(resolverText) ||
  resolverText.length > 0
) {
  pass("theme-system/resolve.ts is the resolver middleware calls");
} else {
  fail(
    "theme-system/resolve.ts is missing",
    "the resolver is what makes a theme switchable per request",
  );
}

// No page may resolve its own theme.
const pageResolvers = [];
for (const file of astroFiles) {
  if (file.startsWith(path.join(ASTRO_SRC, "pages"))) {
    const text = codeLines(await readFile(file, "utf-8"));
    if (/resolveTheme\(|resolveRequestTheme\(|findTheme\(/.test(text)) {
      pageResolvers.push(path.relative(ROOT, file));
    }
  }
}
if (pageResolvers.length === 0) {
  pass("no page resolves a theme itself");
} else {
  fail(
    "pages resolve their own theme",
    `${pageResolvers.join(", ")}\n  Read Astro.locals.theme instead; two resolutions can disagree.`,
  );
}

// --- 4. A theme may not contain business logic or reach a back end -----------

const THEME_FORBIDDEN = [
  {
    pattern:
      /\bfrom\s+['"][^'"]*lib\/(api|admin-api|comment-api|session|system|queries)['"]/,
    why: "an API client, the session or the filesystem loader",
  },
  { pattern: /\bfetch\s*\(/, why: "a network call" },
  { pattern: /\bprocess\.env\b/, why: "environment access" },
  { pattern: /\bnode:fs\b/, why: "filesystem access" },
  { pattern: /\bnode:child_process\b/, why: "subprocess execution" },
  { pattern: /\bwriteFile(?:Sync)?\s*\(/, why: "a filesystem write" },
  { pattern: /\bcreateWriteStream\s*\(/, why: "a filesystem write" },
  { pattern: /\bmkdir(?:Sync)?\s*\(/, why: "a filesystem write" },
  { pattern: /\brename(?:Sync)?\s*\(/, why: "a filesystem write" },
  { pattern: /\bunlink(?:Sync)?\s*\(/, why: "a filesystem write" },
  { pattern: /\brm(?:Sync)?\s*\(/, why: "a filesystem write" },
  // Credential and database *implementation*, not the words. A login form has a
  // field called "password" and prose may mention SQLite; neither is handling a
  // credential. What would be handling one is a KDF, a driver or a query.
  {
    pattern:
      /\bdatabase\/sql\b|\bsqlite3?\b|\bmodernc\.org\/sqlite\b|\bSELECT\b[\s\S]{0,40}\bFROM\b|\bCREATE TABLE\b/i,
    why: "database access",
  },
  {
    pattern:
      /\bargon2\b|\bbcrypt\b|\bscrypt\b|\bpbkdf2\b|\bGenerateFromPassword\b|\bCompareHashAndPassword\b/,
    why: "password hashing",
  },
  {
    pattern: /\bcsrf_secret\b|\bGenerateCSRFSecret\b/,
    why: "CSRF secret generation",
  },
  {
    pattern: /from\s+['"][^'"]*theme(?:-system)?\/registry['"]/,
    why: "the registry (a theme may not reach another)",
  },
  {
    pattern: /getSiteSettings|listThemes|resolveTheme/,
    why: "theme resolution",
  },
];

const themeLogicHits = [];
const themeFileList = [];
for (const { name, dir } of themeDirs) {
  for (const file of await walk(dir)) {
    if (!/\.(astro|ts|css)$/.test(file)) continue;
    themeFileList.push(file);
    const text = codeLines(await readFile(file, "utf-8"));
    for (const { pattern, why } of THEME_FORBIDDEN) {
      if (pattern.test(text)) {
        themeLogicHits.push(`${path.relative(ROOT, file)}: ${why}`);
      }
    }
  }
}
if (themeLogicHits.length === 0) {
  pass(
    `a theme contains no API, session, filesystem, database or theme-resolution logic (${themeFileList.length} files)`,
  );
} else {
  fail(
    "a theme contains logic that belongs to the core",
    themeLogicHits.join("\n") +
      "\n        ARCHITECTURE.md §44: a theme renders markup. Everything above is core.",
  );
}

// Both colour-scheme controls, in every theme.
//
// ARCHITECTURE.md ID-13 gives the interface three values — auto, light, dark — and
// there are only two things a screen can be. So the light/dark switch cannot be a
// three-value cycle: one click in three would change nothing the reader can see,
// which is a control that looks broken rather than one that follows the system.
// `auto` therefore needs a control of its own, and a theme that rendered only the
// switch would leave the one value it cannot express unreachable.
const SCHEME_CONTROLS = ["data-color-scheme-toggle", "data-color-scheme-auto"];
const missingControls = [];
for (const { name, dir } of themeDirs) {
  const markup = (await walk(dir))
    .filter((f) => f.endsWith(".astro"))
    .map((f) => readFileSyncSafe(f) ?? "")
    .join("\n");
  for (const attr of SCHEME_CONTROLS) {
    if (!markup.includes(attr)) missingControls.push(`${name}: ${attr}`);
  }
}
if (missingControls.length === 0) {
  pass(
    `every theme renders both colour-scheme controls (${SCHEME_CONTROLS.join(", ")})`,
  );
} else {
  fail(
    "a theme does not render both colour-scheme controls",
    missingControls.join("\n") +
      "\n        Without the auto button, `auto` is a value a reader can never choose.",
  );
}

// The opening animation is a loop, and a loop has two halves.
//
// ARCHITECTURE.md ID-19 lets a theme carry the one script its own markup needs.
// `bluearchive`'s preloader is that script: /custom.js opens the overlay when the
// arriving document has loaded, and the component closes it before the page is left.
// Three things silently break that loop, and none of them is visible in a page
// render, so they are checked here:
//
//  1. The component reads the overlay by id. Rename the id in the markup and the
//     script finds nothing: the curtain never closes, and every navigation becomes
//     an instant cut. Nothing else fails.
//  2. The script navigates on the closing animation's `animationend`, so the two
//     durations have to be the same number. Drift means the script waits for an
//     animation that has already ended — it falls back to its timer, which is a
//     visible hitch — or navigates before the curtain has shut, which is a cut.
//  3. `.is-closing` must be declared *after* `.is-done`. Same specificity, so the last
//     declaration wins, and each half has to be able to replace the other's
//     `forwards` state. Move the block up and the overlay fades to nothing and
//     stays there.
const preloader = path.join(
  THEMES_DIR,
  "bluearchive",
  "public",
  "components",
  "Preloader.astro",
);
if (!existsSync(preloader)) {
  pass(
    "no bluearchive preloader component, so no opening-animation loop to check",
  );
} else {
  const preloaderText = await readFile(preloader, "utf-8");
  const publicCssPath = path.join(
    THEMES_DIR,
    "bluearchive",
    "public",
    "styles",
    "public.css",
  );
  const publicCss = existsSync(publicCssPath)
    ? await readFile(publicCssPath, "utf-8")
    : "";

  const overlayId = preloaderText.match(
    /getElementById\(['"]([\w-]+)['"]\)/,
  )?.[1];
  const problems = [];

  if (!overlayId) {
    problems.push("the component's script reads no overlay id");
  } else if (!new RegExp(`id=["']${overlayId}["']`).test(preloaderText)) {
    problems.push(
      `the script reads #${overlayId}, which the markup does not render: the curtain would never close`,
    );
  }

  const scriptMs = preloaderText.match(/CLOSE_MS\s*=\s*(\d+)/)?.[1];

  /*
   * The two halves of the loop must be the same length, and that length must be
   * the one the script waits for.
   *
   * They are checked as a *token*, not as two numbers, because the failure this
   * exists for is a reader's: a 700 ms lid falling into a 1000 ms lid lifting
   * reads as a cut-and-paste rather than a carried page, and the only way that
   * cannot happen is for both halves to be one declaration. Two numbers drift; a
   * token cannot.
   */
  const token = publicCss.match(/--motion-preloader:\s*([\d.]+)ms/)?.[1];
  const half = (selector) =>
    publicCss.match(new RegExp(`${selector}\\s*\\{([^}]*)\\}`))?.[1] ?? "";
  const usesToken = (block) =>
    /animation:[^;]*var\(--motion-preloader\)/.test(block);

  if (!token) {
    problems.push(
      "no --motion-preloader token, so the two halves of the loop can drift apart",
    );
  } else if (!usesToken(half(".cms-preloader.is-done"))) {
    problems.push(
      "the opening half does not use --motion-preloader, so it can drift from the closing half",
    );
  } else if (!usesToken(half(".cms-preloader.is-closing"))) {
    problems.push(
      "the closing half does not use --motion-preloader, so it can drift from the opening half",
    );
  } else if (!scriptMs) {
    problems.push("the component's script declares no CLOSE_MS");
  } else if (Number(token) !== Number(scriptMs)) {
    problems.push(
      `the script waits ${scriptMs}ms for a ${token}ms loop, so it navigates before the lid has seated`,
    );
  }

  const doneAt = publicCss.indexOf(".cms-preloader.is-done {");
  const closingAt = publicCss.indexOf(".cms-preloader.is-closing {");
  if (doneAt === -1 || closingAt === -1) {
    problems.push(
      "one half of the opening animation is missing from public.css",
    );
  } else if (closingAt < doneAt) {
    problems.push(
      ".is-closing is declared before .is-done, so the closing animation can never replace the dismissal",
    );
  }

  if (problems.length === 0) {
    pass(
      `the opening animation closes before navigating and reopens on arrival (${token}ms, both halves one token)`,
    );
  } else {
    fail(
      "the opening-animation loop cannot close",
      problems.join("\n") +
        "\n        ARCHITECTURE.md ID-19: the theme owns this effect, so its two halves have to agree.",
    );
  }
}

// `!important` in a theme stylesheet is a maintenance debt, and it is now a
// documented exception rather than a habit.
//
// ARCHITECTURE.md §11a: an unlayered `!important` is the *weakest* important
// origin, so it loses to a layered one — which is why the theme used to need six
// of them to keep its reduced-motion rules, and why `custom.css` could only reach
// them by joining the theme's layer. The theme now expresses the same thing with
// motion tokens and a zero-specificity `:where(*)` catch-all, so it needs none.
//
// What is left is one idiom, repeated where a stylesheet has a `[hidden]` element:
// the HTML `hidden` attribute has to beat any `display` the theme sets, and there
// is no specificity that does that without it. `make arch` allows exactly this
// shape and nothing else, so a seventh `!important` is a failure rather than a
// decision someone has to remember.
const IMPORTANT_ALLOWLIST = /^\s*display:\s*none\s*!important;\s*$/;
const importantHits = [];
for (const { dir } of themeDirs) {
  for (const file of [
    path.join(dir, "public", "styles", "public.css"),
    path.join(dir, "admin", "styles", "admin.css"),
  ]) {
    if (!existsSync(file)) continue;
    const rel = path.relative(ROOT, file);
    const lines = (await readFile(file, "utf8")).split("\n");
    for (const [i, line] of lines.entries()) {
      if (!line.includes("!important")) continue;
      // A comment explaining why `!important` is gone is not a declaration.
      if (/^\s*(\/\/|\*|\/\*)/.test(line)) continue;
      // The one allowed shape is the declaration inside a `[hidden]` rule,
      // which may be written on one line or with the selector on the
      // line above.
      let selector = "";
      for (let j = i - 1; j >= 0; j--) {
        if (lines[j].includes("{")) {
          selector = lines[j];
          break;
        }
        if (lines[j].trim() === "") continue;
        break;
      }
      if (IMPORTANT_ALLOWLIST.test(line) && selector.includes("[hidden]"))
        continue;
      importantHits.push(`${rel}:${i + 1}: ${line.trim()}`);
    }
  }
}
if (importantHits.length === 0) {
  pass(
    "the theme stylesheets use no !important except the documented [hidden] idiom",
  );
} else {
  fail(
    "a theme stylesheet uses !important outside the documented [hidden] idiom",
    importantHits.join("\n") +
      "\n        ARCHITECTURE.md §11a: use a motion token or :where(*) instead. An unlayered !important is the weakest important origin, so it also cannot be overridden from custom.css.",
  );
}

// A theme may ship no *standalone* script. The walk above filters to
// .astro/.ts/.css so the forbidden-pattern scan reads markup and stylesheets;
// scripts have to be collected separately or this check would be vacuous — there
// would never be a candidate to fail. A component's own `<script>` is handled
// below, where it can be bounded rather than merely banned.
const themeScriptFiles = [];
for (const { name, dir } of themeDirs) {
  for (const file of await walk(dir)) {
    // theme.ts is the manifest: it names components and is not itself shipped to
    // a browser. Everything else script-shaped in a theme would be behaviour.
    if (
      /\.(js|mjs|cjs|jsx|ts|tsx)$/.test(file) &&
      !file.endsWith(`${path.sep}theme.ts`)
    ) {
      themeScriptFiles.push(`${path.relative(ROOT, file)}  (theme ${name})`);
    }
  }
}
if (themeScriptFiles.length === 0) {
  pass("no theme ships a standalone script file (behaviour is core, §45)");
} else {
  fail(
    "a theme ships a script file",
    themeScriptFiles.join("\n") +
      "\n        ARCHITECTURE.md §45: a theme may not provide script that changes core behaviour.",
  );
}

// --- 4b. A theme script is presentation, and only presentation --------------
//
// ARCHITECTURE.md ID-19: a theme *may* have a small script — the mobile menu in
// bluearchive's header — because opening a menu is presentation. What it may not
// do is anything the core owns: make a request, touch a credential, write an
// inline style, load remote code, or branch on which theme is active. "It is only
// a menu" is exactly the kind of claim that becomes "and while we were in there,
// one fetch" six months later, so each half is checked.
//
// `is:inline` is banned because CSP is `script-src 'self'` with no
// 'unsafe-inline' (ARCHITECTURE.md §18): an inline script does not work at all, so
// allowing one would be allowing a silently broken theme.
const THEME_SCRIPT_FORBIDDEN = [
  { pattern: /\bfetch\s*\(/, why: "a network call" },
  { pattern: /XMLHttpRequest|sendBeacon|importScripts/, why: "a network call" },
  { pattern: /['"`]\/api\//, why: "a core API path" },
  { pattern: /document\.cookie/, why: "the session cookie" },
  {
    pattern: /\bcsrf\b|X-CSRF|Authorization|credentials\s*:/i,
    why: "credential handling",
  },
  {
    pattern: /\beval\s*\(|new\s+Function\s*\(/,
    why: "dynamic code evaluation",
  },
  {
    pattern: /innerHTML|outerHTML|insertAdjacentHTML|document\.write/,
    why: "markup injection",
  },
  {
    pattern: /\.style\.\s*[A-Za-z-]+\s*=|setAttribute\(\s*['"`]style['"`]/,
    why: "an inline style",
  },
  { pattern: /['"`]https?:\/\//, why: "a remote resource" },
  {
    pattern: /data-cms-/,
    why: "the core JS contract — core scripts own those attributes",
  },
  { pattern: /data-color-scheme/, why: "the core colour-scheme behaviour" },
  ...frontendIds.map((id) => ({
    pattern: new RegExp(`['"\`]${id}['"\`]`),
    why: `branching on the ${id} theme`,
  })),
];

const themeScriptHits = [];
let themeScriptCount = 0;
for (const { name, dir } of themeDirs) {
  for (const file of await walk(dir)) {
    if (!file.endsWith(".astro")) continue;
    const rel = path.relative(ROOT, file);
    const text = await readFile(file, "utf-8");

    for (const m of text.matchAll(
      /<script\b([^>]*?)(\/?)>([\s\S]*?)<\/script>/g,
    )) {
      const [, attributes, selfClosing, body] = m;
      // A self-closing tag has no body: `<script src … />` renders a
      // core script from the js-contract lists, and the next
      // `</script>` in the file is not its — pairing the two would
      // mistake the markup between them, including a theme script's
      // whole body, for the core script's own.
      if (selfClosing) continue;
      // A `<script src>` is a core script rendered from the js-contract
      // lists; those are checked separately. Anything else with a body
      // is a theme script.
      if (/\bsrc=/.test(attributes) && body.trim().length === 0) continue;

      themeScriptCount += 1;

      if (/is:inline/.test(attributes)) {
        themeScriptHits.push(
          `${rel}: an inline <script> (CSP is script-src 'self')`,
        );
      }

      const code = codeLines(body);
      for (const { pattern, why } of THEME_SCRIPT_FORBIDDEN) {
        if (pattern.test(code)) {
          themeScriptHits.push(`${rel} (theme ${name}): ${why}`);
        }
      }
    }
  }
}
if (themeScriptHits.length === 0) {
  pass(
    themeScriptCount === 0
      ? "no theme script exists at all"
      : `the ${themeScriptCount} theme script(s) are presentation only: no request, no credential, no inline style, no remote code`,
  );
} else {
  fail(
    "a theme script does something that belongs to the core",
    themeScriptHits.join("\n") +
      "\n        ARCHITECTURE.md ID-19: a theme may open its own menu; it may not act.",
  );
}

// --- 5. Themes are compile-time registered, never reached by path ------------

const dynamicImportHits = [];
for (const file of [...astroFiles, ...publicFiles]) {
  if (!/\.(ts|js|mjs|astro)$/.test(file)) continue;
  const text = codeLines(await readFile(file, "utf-8"));
  text.split("\n").forEach((line, i) => {
    // A dynamic import with a template literal or a concatenation is a path built
    // at runtime. That is the difference between "choose a registered theme" and
    // "load whatever this string says".
    if (/\bimport\s*\(/.test(line) && /[`'"]\s*\+|`[^`]*\$\{/.test(line)) {
      dynamicImportHits.push(
        `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`,
      );
    }
    if (/\bimport\.meta\.glob\b/.test(line)) {
      dynamicImportHits.push(
        `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}\n          A theme registry is an explicit map, not a directory scan.`,
      );
    }
  });
}
if (dynamicImportHits.length === 0) {
  pass(
    "themes are reached through the registry, never through a computed path",
  );
} else {
  fail("a module specifier is built at runtime", dynamicImportHits.join("\n"));
}

// --- 6. The id allowlist, the registry and the backend must agree ----------
//
// "Whitelisted theme id" is only a property if the whitelist is real. Three copies
// of the list exist because three layers validate it; nothing but this check stops
// them drifting, and a theme added to one and forgotten in another fails silently:
// the registry never renders it and the admin rejects it.

const registryIds = [
  ...registryText.matchAll(/^\s{2}([A-Za-z][\w]*):\s*\w+Theme,$/gm),
].map((m) => m[1]);

const settingsGoPath = path.join(
  ROOT,
  "backend",
  "internal",
  "api",
  "settings_handlers.go",
);
const settingsGoFile = existsSync(settingsGoPath)
  ? await readFile(settingsGoPath, "utf-8")
  : "";
const backendIds = [
  ...(
    settingsGoFile.match(/knownThemeIDs\s*=\s*\[\]string\{([^}]*)\}/)?.[1] ?? ""
  ).matchAll(/"([^"]+)"/g),
].map((m) => m[1]);

const sameSet = (a, b) =>
  a.length === b.length && [...a].sort().join(" ") === [...b].sort().join(" ");

if (frontendIds.length === 0) {
  fail(
    "could not read THEME_IDS from theme-system/ids.ts",
    "the allowlist is unreadable",
  );
} else if (!sameSet(frontendIds, registryIds)) {
  fail(
    "THEME_IDS and the registry keys disagree",
    `ids.ts: ${frontendIds.join(", ")}\n        registry: ${registryIds.join(", ") || "(none)"}`,
  );
} else if (!sameSet(frontendIds, backendIds)) {
  fail(
    "THEME_IDS and the backend allowlist disagree",
    `ids.ts: ${frontendIds.join(", ")}\n        Go: ${backendIds.join(", ")}`,
  );
} else {
  pass(
    `the theme allowlist agrees across ids.ts, the registry and the backend (${frontendIds.join(", ")})`,
  );
}

// The backend must reject an unknown id, not merely ignore one.
if (
  /!isKnownThemeID\(/.test(settingsGoFile) &&
  /fields\["themeId"\]/.test(settingsGoFile)
) {
  pass("the backend refuses to store an unknown themeId");
} else {
  fail(
    "the backend does not validate themeId on write",
    "ARCHITECTURE.md §26: choosing a theme must be as constrained as choosing a binary",
  );
}

// --- 7. JavaScript is core, and does not depend on the theme ---------------

const componentContractText = existsSync(THEME_CONTRACT)
  ? await readFile(THEME_CONTRACT, "utf-8")
  : "";
const contractText = existsSync(JS_CONTRACT)
  ? await readFile(JS_CONTRACT, "utf-8")
  : "";

// Every attribute the core scripts bind to must actually be read by them.
// public/*.js is served verbatim and cannot import the TypeScript contract, so the
// two are duplicated on purpose — and this is what keeps the duplication honest.
//
// The informational document attributes are excluded deliberately: they exist
// precisely so that nothing reads them, so requiring a reader would invert the
// rule they encode.
const docAttrsBlock =
  contractText.match(/DOCUMENT_ATTRS = \{([\s\S]*?)\n\} as const;/)?.[1] ?? "";
const docAttrValues = [...docAttrsBlock.matchAll(/'(data-cms-[\w-]+)'/g)].map(
  (m) => m[1],
);

const bindingTokens = [
  ...[
    ...contractText.matchAll(
      /^\s{2}\w+:\s*'(data-cms-[\w-]+|data-color-scheme[\w-]*)'/gm,
    ),
  ].map((m) => m[1]),
].filter((t) => !docAttrValues.includes(t));

const scriptTokens = [
  ...contractText.matchAll(/^\s{2}\w+:\s*'\/([\w.-]+\.js)'/gm),
].map((m) => `/${m[1]}`);

const coreScripts = (await walk(path.join(ASTRO_DIR, "public"))).filter((f) =>
  f.endsWith(".js"),
);
const coreScriptText = (
  await Promise.all(coreScripts.map((f) => readFile(f, "utf-8")))
).join("\n");

const unbound = bindingTokens.filter((t) => !coreScriptText.includes(t));
if (unbound.length === 0) {
  pass(
    `every binding attribute appears in the core scripts (${bindingTokens.length} checked)`,
  );
} else {
  fail(
    "the contract defines a binding attribute the core scripts never read",
    `${unbound.join(" ")}\n  Either the contract is stale or a control has been renamed.`,
  );
}

// The informational attributes must appear in the themes (they are the theme's
// own marker) and must NOT appear in any script. Asserting the negative is what
// makes "informational" a property rather than a hope.
const docAttrInThemes = docAttrValues.every((attr) =>
  themeFileList.some((f) => readFileSyncSafe(f).includes(attr)),
);
if (docAttrInThemes) {
  pass(
    `the document attributes are emitted by the themes (${docAttrValues.join(", ")})`,
  );
} else {
  fail(
    "a document attribute is not emitted by any theme",
    docAttrValues.join(", "),
  );
}
const docAttrInJs = docAttrValues.filter((attr) =>
  coreScriptText.includes(attr),
);
if (docAttrInJs.length === 0) {
  pass(
    "no core script reads a document attribute (theme and behaviour stay independent)",
  );
} else {
  fail(
    "a core script reads a document attribute",
    `${docAttrInJs.join(" ")}\n  ARCHITECTURE.md §33: the behaviour layer must not know the theme.`,
  );
}

// Every core script must be servable: either a file in public/, or an Astro
// endpoint. /custom.js is the latter — it streams content/system/custom.js — and
// treating that as a missing file would have been a check that could only be
// satisfied by moving admin-authored code out of content/, which is the opposite
// of what ARCHITECTURE.md §16 wants.
const declaredScripts = [...new Set(scriptTokens.map((t) => t.slice(1)))];
const unservable = declaredScripts.filter((name) => {
  if (existsSync(path.join(ASTRO_DIR, "public", name))) return false;
  return (
    !existsSync(path.join(ASTRO_SRC, "pages", name)) &&
    !existsSync(path.join(ASTRO_SRC, "pages", `${name}.ts`))
  );
});
if (unservable.length === 0) {
  pass(
    `every core script the contract declares is servable (${declaredScripts.join(", ")})`,
  );
} else {
  fail(
    "a declared core script is neither in public/ nor served by an endpoint",
    unservable.join(", "),
  );
}

// The behaviour layer must not select by CSS class. A theme that renames a class
// would otherwise silently lose a button's behaviour, which is the whole failure
// mode the contract exists to prevent.
const classSelectorHits = [];
for (const file of coreScripts) {
  const text = await readFile(file, "utf-8");
  text.split("\n").forEach((line, i) => {
    if (/^\s*(\/\/|\*|\/\*)/.test(line)) return;
    for (const m of line.matchAll(
      /(?:querySelectorAll|querySelector|closest|getElementsByClassName|getElementById)\s*\(\s*['"]([^'"]*)['"]/g,
    )) {
      const selector = m[1];
      // A class, an id or a tag selector. Attribute, type and form-name selectors
      // are the contract; those are allowed.
      if (/^[.#]/.test(selector)) {
        classSelectorHits.push(
          `${path.relative(ROOT, file)}:${i + 1}: '${selector}'`,
        );
      }
    }
  });
}
if (classSelectorHits.length === 0) {
  pass("the core scripts select by contract attribute, never by class or id");
} else {
  fail(
    "the core scripts select by CSS class or id",
    classSelectorHits.join("\n") +
      "\n        A theme may rename its classes; behaviour must not depend on them.",
  );
}

// Nor may the behaviour layer depend on which theme is active.
//
// The `themeId` *form field* on the settings screen is not a violation: that is a
// form-field name, part of the same contract as `siteTitle`, and it is how a theme
// selection is submitted. What must not appear is anything that branches on the
// theme's identity.
const THEME_AWARE_JS = [
  /data-cms-theme/,
  /locals\.theme/,
  /\bresolveTheme\b/,
  /\bfindTheme\b/,
  /\blistThemes\b/,
  /\bTHEME_IDS\b/,
  // A comparison against a literal theme id is the shape a behavioural
  // dependency takes. The ids come from the allowlist rather than a hand-written
  // list, so a new theme is covered automatically.
  ...frontendIds.map((id) => new RegExp(`[!=]==?\\s*['"]${id}['"]`)),
];
const themeAwareJs = [];
for (const file of coreScripts) {
  const text = await readFile(file, "utf-8");
  text.split("\n").forEach((line, i) => {
    if (/^\s*(\/\/|\*|\/\*)/.test(line)) return;
    for (const pattern of THEME_AWARE_JS) {
      if (pattern.test(line)) {
        themeAwareJs.push(
          `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`,
        );
        return;
      }
    }
  });
}
if (themeAwareJs.length === 0) {
  pass("no core script branches on which theme is active");
} else {
  fail(
    "a core script depends on which theme is active",
    themeAwareJs.join("\n") +
      "\n        ARCHITECTURE.md §33: JS is the behaviour layer and must not know the theme name.",
  );
}

// --- 8. Both themes must implement the whole contract ----------------------
//
// A theme that shipped a subset would register fine and then render a page that
// crashes at request time, because Astro resolves a missing slot to undefined.
// Comparing the registry's declared keys against each manifest makes that a build
// failure instead.

// The slot lists are exported constants in the contract, and the interfaces are
// typed from them, so this compares two things that cannot drift apart.
const slotList = (name) =>
  [
    ...(
      componentContractText.match(
        new RegExp(`${name} = \\[([^\\]]*)\\]`),
      )?.[1] ?? ""
    ).matchAll(/'([^']+)'/g),
  ].map((m) => m[1]);
const contractPublic = slotList("PUBLIC_SLOTS");
const contractAdmin = slotList("ADMIN_SLOTS");

if (contractPublic.length === 0 || contractAdmin.length === 0) {
  fail(
    "could not read the slot lists from theme-system/contract.ts",
    `PUBLIC_SLOTS: ${contractPublic.length}, ADMIN_SLOTS: ${contractAdmin.length}`,
  );
}

for (const { name, dir } of themeDirs) {
  const manifest = await readFile(path.join(dir, "theme.ts"), "utf-8").catch(
    () => "",
  );
  const block = (area) =>
    manifest.match(new RegExp(`${area}:\\s*\\{([\\s\\S]*?)\\n  \\},`))?.[1] ??
    "";

  const missingPublic = contractPublic.filter(
    (slot) => !new RegExp(`\\b${slot}[,:]`).test(block("public")),
  );
  const missingAdmin = contractAdmin.filter(
    (slot) => !new RegExp(`\\b${slot}[,:]`).test(block("admin")),
  );

  if (missingPublic.length === 0 && missingAdmin.length === 0) {
    pass(
      `the ${name} theme implements all ${contractPublic.length} public and ${contractAdmin.length} admin slots`,
    );
  } else {
    fail(
      `the ${name} theme does not implement the whole contract`,
      [
        missingPublic.length ? `  public: ${missingPublic.join(" ")}` : "",
        missingAdmin.length ? `  admin: ${missingAdmin.join(" ")}` : "",
      ]
        .filter(Boolean)
        .join("\n"),
    );
  }
}

// --- 9. A theme's CSS must be its own asset, not a shared bundle ----------
//
// This check exists because of a real bug, not a hypothetical one.
//
// Every layout used to do a plain `import '../styles/public.css'`. The registry
// statically imports *both* themes, so both stylesheets were reachable from the
// server entry and Vite merged them into a single emitted file. Both themes'
// `:root` blocks were then in one document, the later one won on source order,
// and the browser received byte-identical CSS whichever theme was active: the
// `data-cms-theme` attribute changed and nothing else did. "The theme is
// swappable" was true of the attribute and false of the page.
//
// The fix is `?url`, which makes each stylesheet its own emitted asset. This
// asserts that fix is still in place, because reverting it looks like a harmless
// simplification and silently un-swaps the theme.

const plainCssImports = [];
for (const { name, dir } of themeDirs) {
  for (const file of (await walk(dir)).filter((f) => f.endsWith(".astro"))) {
    const text = await readFile(file, "utf-8");
    text.split("\n").forEach((line, i) => {
      if (/^\s*(\/\/|\*|\/\*)/.test(line)) return;
      const m = line.match(/^\s*import\s+'([^']+\.css)'/);
      if (m) {
        plainCssImports.push(
          `${path.relative(ROOT, file)}:${i + 1}: ${m[1]}\n` +
            "          Use `import css from '<file>?url'` and link it, so each theme ships its own stylesheet.",
        );
      }
    });
  }
}
if (plainCssImports.length === 0) {
  pass(
    `every theme stylesheet is emitted as its own asset (${themeDirs.length} themes checked for plain CSS imports)`,
  );
} else {
  fail(
    "a theme imports CSS without ?url, which merges every theme into one stylesheet",
    plainCssImports.join("\n") +
      "\n        A theme swap would change an attribute and nothing a visitor can see.",
  );
}

// ===========================================================================
// Media, site metadata and RSS — the invariants this phase added
// ===========================================================================

section("§12/§13 a media URL is resolved, never assembled");

// The public URL of an asset is one function's output. A theme that writes
// `"/media/" + filename` has coupled itself to the storage layout, which is exactly
// what §13 exists to prevent: the day the layout changes, every theme has to change
// with it.
//
// Hits inside comments are fine; what is forbidden is a *literal* in a line that
// produces a URL.
const mediaUrlHits = [];
for (const file of themeFileList) {
  const rel = path.relative(ROOT, file);
  const stripped = readFileSyncSafe(file)
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");
  stripped.split("\n").forEach((line, i) => {
    if (/["'`]\/media\//.test(line)) {
      mediaUrlHits.push(`${rel}:${i + 1}: ${line.trim()}`);
    }
  });
}
if (mediaUrlHits.length === 0) {
  pass(
    `no theme assembles a media URL (${themeFileList.length} files checked)`,
  );
} else {
  fail(
    "a theme hardcodes a media URL prefix",
    `${mediaUrlHits.join("\n")}\n        Use mediaUrl() from lib/media.ts. The storage layout is not a theme's business.`,
  );
}

// §35: the favicon is a site setting resolved by the core. A theme naming one is how
// two themes end up disagreeing about the site's identity.
const iconHits = [];
for (const { name, dir } of themeDirs) {
  for (const file of (await walk(dir)).filter((f) => f.endsWith(".astro"))) {
    const text = await readFile(file, "utf-8");
    const m = text.match(/<link rel="icon"[^>]*>/);
    if (m && !/\{seo\.icon\.href\}|\{site\.icon\.href\}/.test(m[0])) {
      iconHits.push(`${path.relative(ROOT, file)}: ${m[0]}`);
    }
  }
}
if (iconHits.length === 0) {
  pass("no theme names a favicon of its own; the icon comes from CMS settings");
} else {
  fail(
    "a theme hardcodes the site icon",
    `${iconHits.join("\n")}\n        The icon is a media asset chosen in Settings (ARCHITECTURE.md §35).`,
  );
}

// Every layout must actually render the resolved head, not merely receive it.
const layoutHead = [];
for (const { name, dir } of themeDirs) {
  const targets = [
    ["public", path.join(dir, "public", "layouts", "PublicLayout.astro")],
    ["admin", path.join(dir, "admin", "layouts", "AdminLayout.astro")],
  ];
  for (const [kind, file] of targets) {
    if (!existsSync(file)) {
      layoutHead.push(`${name}/${kind}: the layout is missing`);
      continue;
    }
    const text = await readFile(file, "utf-8");
    if (kind === "public") {
      // The public layout delegates to SeoHead, which renders the title, the icon,
      // Open Graph and the RSS discovery link from one computed block. Checking this
      // file for a literal <title> would push the metadata back into two layouts.
      if (!/<SeoHead\s+seo=\{site\.seo\}/.test(text)) {
        layoutHead.push(
          `${name}/${kind}: does not render the core-resolved SeoHead`,
        );
      }
      continue;
    }
    if (!/<title>\{seo\.documentTitle\}<\/title>/.test(text)) {
      layoutHead.push(
        `${name}/${kind}: does not render the resolved document title`,
      );
    }
    if (!/\{seo\.icon\.href\}/.test(text)) {
      layoutHead.push(`${name}/${kind}: does not render the resolved icon`);
    }
  }
}
if (layoutHead.length === 0) {
  pass(
    `every layout renders the core-resolved head (${themeDirs.length} themes x 2 layouts)`,
  );
} else {
  fail(
    "a layout does not render the metadata the core computed",
    `${layoutHead.join("\n")}\n        §77: one place decides the title format and the icon.`,
  );
}

// §40/§74: one resolver. An admin shell that reads settings separately drifts.
const adminContext = await readFile(
  path.join(ASTRO_SRC, "lib", "admin-context.ts"),
  "utf-8",
);
if (
  adminContext.includes("getSiteSettings") &&
  adminContext.includes("seoView")
) {
  pass("the admin head comes from the same resolver as the public site");
} else {
  fail(
    "the admin builds its own head",
    "use seoView() and getSiteSettings() from lib/, so the admin cannot drift from the site",
  );
}

section("§10–§32 image delivery is negotiated, varied and cached");

const deliveryGo = await readFile(
  path.join(BACKEND, "internal", "api", "media_delivery.go"),
  "utf-8",
);
const pipelineGo = await readFile(
  path.join(BACKEND, "internal", "media", "pipeline.go"),
  "utf-8",
);
const cacheGo = await readFile(
  path.join(BACKEND, "internal", "media", "cache.go"),
  "utf-8",
);
const mediaHandlersGo = stripGoComments(
  await readFile(
    path.join(BACKEND, "internal", "api", "media_handlers.go"),
    "utf-8",
  ),
);

const deliveryChecks = [
  [
    "Vary: Accept",
    /header\.Set\("Vary", "Accept"\)/,
    "the negotiated response varies on Accept (§18)",
  ],
  [
    "Accept header",
    /AcceptsWebP\(r\.Header\.Get\("Accept"\)\)/,
    "the representation is chosen from Accept, never from User-Agent (§17)",
  ],
  [
    "representation ETag",
    /RepresentationETag\("webp-q"/,
    "the ETag names the representation, not just the file (§89)",
  ],
  [
    "conditional request",
    /etagMatches\(r\.Header\.Get\("If-None-Match"\), etag\)/,
    "If-None-Match is honoured (§90)",
  ],
  [
    "HEAD",
    /r\.Method == http\.MethodHead/,
    "HEAD answers with the headers GET would produce (§31)",
  ],
  [
    "nosniff",
    /X-Content-Type-Options", "nosniff"/,
    "nosniff is set on image responses (§29)",
  ],
  [
    "fallback",
    /d\.originalBytes\(rec\)/,
    "a conversion failure falls back to the original (§60)",
  ],
  [
    "traversal guard",
    /strings\.Contains\(rel, "\.\."\)/,
    "a traversal segment is refused before the filesystem is touched (§20)",
  ],
];
for (const [name, re, why] of deliveryChecks) {
  if (re.test(deliveryGo)) pass(`image delivery ${why} (${name})`);
  else fail(`image delivery does not ${why}`, `missing: ${name}`);
}

// §30: `immutable` on a URL that carries no content hash is a lie — the admin can
// replace the file in place and a browser holding an immutable year will not notice.
if (!/immutable/.test(stripGoComments(deliveryGo))) {
  pass("no image response claims to be immutable on a replaceable URL (§30)");
} else {
  fail(
    "an image response claims to be immutable",
    "The URL carries no content hash and the file can be replaced in place.",
  );
}

const pipelineChecks = [
  [
    "single-flight",
    /func \(g \*singleGroup\) Do\(key string/,
    "concurrent conversions of one file collapse into one (§59)",
    pipelineGo,
  ],
  [
    "memory cache ceiling",
    /type MemoryCache struct/,
    "the in-memory cache is a bounded LRU (§23)",
    cacheGo,
  ],
  [
    "LRU order",
    /MoveToFront/,
    "the least-recently-used entry is the one evicted",
    cacheGo,
  ],
  [
    "eviction accounting",
    /Evictions\(\) int64/,
    "evictions are counted for the diagnostics screen (§24)",
    cacheGo,
  ],
  [
    "disk cache",
    /type DiskCache struct/,
    "representations are cached on disk, keyed by content (§19)",
    cacheGo,
  ],
  [
    "key includes quality",
    /strconv\.Itoa\(quality\)/,
    "the cache key includes the encoder quality (§28)",
    pipelineGo,
  ],
  [
    "pixel ceiling from the header",
    /func \(p \*Pipeline\) checkPixels/,
    "the pixel ceiling is checked from the header, before a decode (§26)",
    pipelineGo,
  ],
  [
    "quality is validated",
    /quality < QualityMin \|\| quality > QualityMax/,
    "an out-of-range quality cannot reach the encoder (§27)",
    pipelineGo,
  ],
];
for (const [name, re, why, source] of pipelineChecks) {
  if (re.test(source)) pass(`the image pipeline ${why} (${name})`);
  else fail(`the image pipeline does not ${why}`, `missing: ${name}`);
}

// §82: the representation cache must not live beside the originals. If it did, a
// restore from backup would quietly promote derived bytes into the source of truth.
const configGo = await readFile(
  path.join(BACKEND, "internal", "config", "config.go"),
  "utf-8",
);
if (
  /MediaCacheRoot\s+string/.test(configGo) &&
  /"media-cache"/.test(configGo)
) {
  pass(
    "the representation cache has its own root, separate from MEDIA_ROOT (§82)",
  );
} else {
  fail(
    "the representation cache is not separated from the media root",
    "MEDIA_ROOT must hold originals and nothing else.",
  );
}

// §14: Astro is the only process a browser reaches, so the media route delegates
// rather than re-implementing. A second negotiation implementation is one more place
// for `Vary` to be forgotten.
const mediaRoute = await readFile(
  path.join(ASTRO_SRC, "pages", "media", "[...path].ts"),
  "utf-8",
);
if (/backendBase\(\)\}\/media\//.test(mediaRoute)) {
  pass("Astro's media route forwards to the Go delivery layer (§14, §128)");
} else {
  fail(
    "Astro's media route does not forward to Go",
    "Go owns storage, negotiation and the caches.",
  );
}
if (
  /FORWARD_REQUEST = \[[^\]]*'accept'/.test(mediaRoute) &&
  !/image\/webp/.test(mediaRoute)
) {
  pass("Astro forwards Accept and never decides the format itself (§17)");
} else {
  fail(
    "Astro decides the image format",
    "Content negotiation belongs to exactly one place (§17).",
  );
}
// §18: re-deriving a header here would be a second implementation that could
// disagree with the one that produced the bytes.
if (/PASS_THROUGH = \[[^\]]*'vary'/s.test(mediaRoute)) {
  pass("Astro passes Vary through instead of re-deriving it (§18)");
} else {
  fail(
    "Astro does not forward Vary from the backend",
    "A shared cache would hand a WebP to a client that cannot read one.",
  );
}

section("§64/§73 the RSS feed is core, dynamic and settings-driven");

// §28: the feed moved into the Go backend, which owns both halves
// of it — the settings that shape the channel and the content the
// items come from. Astro keeps exactly one job: the middleware
// rewrite that preserves the public URL.
const goRssFeed = await readFile(
  path.join(ROOT, "backend", "internal", "api", "rss.go"),
  "utf-8",
);
const middlewareSrc = await readFile(
  path.join(ASTRO_SRC, "middleware.ts"),
  "utf-8");
const rssChecks = [
  [
    "live collection",
    /Content\.List\(content\.KindPosts\)/,
    "items come from content, not from a database (§64)",
  ],
  ["rssEnabled", /RSSEnabled/, "whether the feed exists is a setting (§65)"],
  ["rssTitle", /RSSTitle/, "the channel title comes from the settings (§66)"],
  [
    "rssDescription",
    /RSSDescription/,
    "the channel description comes from the settings (§67)",
  ],
  ["item limit", /clampRSSLimit/, "the feed is bounded (§68)"],
  ["absolute links", /PublicOrigin/, "links are absolute (§70)"],
  [
    "content type",
    /application\/rss\+xml/,
    "the response carries an RSS content type (§105)",
  ],
  ["drafts", /\.Draft/, "drafts are excluded (§107)"],
  [
    "control characters",
    /xmlEscape/,
    "a stray control character cannot take the whole feed down",
  ],
];
for (const [name, re, why] of rssChecks) {
  if (re.test(goRssFeed)) pass(`the Go feed ${why} (${name})`);
  else fail(`the Go feed does not ${why}`, `missing: ${name}`);
}
if (
  /url\.pathname === '\/rss\.xml'/.test(middlewareSrc) &&
  /rewrite\('\/api\/v1\/rss'\)/.test(middlewareSrc)
) {
  pass("the public /rss.xml URL is rewritten onto the API proxy (§28)");
} else {
  fail(
    "/rss.xml is not rewritten onto the API proxy",
    "the public URL must stay stable while the feed lives in the backend",
  );
}
if (
  !existsSync(path.join(ASTRO_SRC, "pages", "rss.xml.ts")) &&
  !/getPublishedPosts/.test(middlewareSrc)
) {
  pass("Astro generates no feed of its own (§28)");
} else {
  fail(
    "Astro still generates a feed",
    "the feed is served by the Go backend; Astro only rewrites the URL",
  );
}

// §62/§73: no theme owns the feed.
const themeRss = [];
for (const file of themeFileList) {
  const stripped = readFileSyncSafe(file)
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");
  stripped.split("\n").forEach((line, i) => {
    if (/<rss|<channel>|<item>|<description>|<envelope/i.test(line)) {
      themeRss.push(`${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`);
    }
  });
}
if (themeRss.length === 0) {
  pass("no theme generates RSS markup (§62, §73)");
} else {
  fail(
    "a theme writes RSS markup",
    `${themeRss.join("\n")}\n        The feed is a core endpoint (§62).`,
  );
}

// §71: the feed is not advertised from the rendered pages.
//
// The feed still exists — the backend serves it at /rss.xml for
// any reader who knows the address — but the public pages no
// longer show it: no discovery link in the head, no entry in
// the navigation, no button in the hero. Advertising an
// `alternate` the pages themselves never mention would teach
// readers to ignore the element, so the invariant is now the
// absence of the link from the head.
const firstSeoHead = path.join(
  ASTRO_SRC,
  "themes",
  themeDirs[0].name,
  "public",
  "components",
  "SeoHead.astro",
);
if (
  existsSync(firstSeoHead) &&
  !/rel="alternate"/.test(await readFile(firstSeoHead, "utf-8"))
) {
  pass("the public pages advertise no RSS discovery link (§71)");
} else {
  fail(
    "the public pages still advertise the feed",
    "the feed is served but not shown: no <link rel=alternate> in the head (§71).",
  );
}
// §110: no theme-specific feed copy.
const themeFeedCopy = [];
for (const file of themeFileList) {
  const text = readFileSyncSafe(file);
  if (/Blue ?Archive/i.test(text) && /rss|feed/i.test(text)) {
    themeFeedCopy.push(path.relative(ROOT, file));
  }
}
if (themeFeedCopy.length === 0) {
  pass("no feed copy names a theme (§73)");
} else {
  fail(
    "feed copy names a theme",
    `${themeFeedCopy.join(", ")}\n        RSS text comes from CMS settings and content (§73).`,
  );
}

section("§4/§36/§139 no binary content and no derived content in the database");

// §139: no image bytes in the database. `sha256` is a digest, not the file.
const blobHits = [];
for (const file of storeFiles) {
  const text = await readFile(file, "utf-8");
  const m = text.match(/INSERT INTO\s+media[\s\S]{0,400}?\)\s*VALUES/gi);
  if (!m) continue;
  const insert = text.slice(text.search(/INSERT INTO\s+media/i));
  if (
    /\b(image_bytes|blob|payload|content)\b/i.test(insert.split(");")[0] ?? "")
  ) {
    blobHits.push(
      `${path.relative(ROOT, file)}: ${insert.split(");")[0].trim()}`,
    );
  }
}
if (blobHits.length === 0) {
  pass("media rows hold metadata only; no image bytes are stored (§4, §139)");
} else {
  fail("image bytes appear to be written to the database", blobHits.join("\n"));
}

// §50: the usage index must stay derivable, so its schema may not carry content.
if (!tableNames.includes("media_usage")) {
  pass("no media usage table exists (nothing derived to store yet)");
} else if (/media_usage[\s\S]{0,600}?\btitle\s+TEXT/i.test(ddlText)) {
  fail(
    "the media usage index carries a title",
    "It is derived from content/ and must not become a second source of truth (§50).",
  );
} else {
  pass(
    "the media usage index stores paths and counts only — derived metadata (§50)",
  );
}

// §51: rebuilding must be possible in-process, without shelling out (§4).
if (
  /rebuild-usage/.test(mediaHandlersGo) &&
  /RebuildUsageIndex/.test(mediaHandlersGo)
) {
  pass("the media usage index can be rebuilt through the API (§51)");
} else {
  fail(
    "the media usage index cannot be rebuilt",
    "It is derived, so losing it must cost one rescan and nothing else (§51).",
  );
}

// §54: a referenced asset is protected from deletion.
if (/usage > 0 && !force/.test(mediaHandlersGo)) {
  pass(
    "a referenced image cannot be deleted without an explicit override (§54)",
  );
} else {
  fail(
    "a referenced image can be deleted silently",
    "That leaves a broken image inside a Markdown file the CMS does not own (§54).",
  );
}

// §5: the filesystem is the source of truth, so a row without a file is reported.
if (
  /d\.Media\.Exists\(/.test(mediaHandlersGo) &&
  /Missing/.test(mediaHandlersGo)
) {
  pass(
    "the library reports a row whose file is gone as missing, not fatal (§5)",
  );
} else {
  fail(
    "a missing file is not detected",
    "The filesystem is the source of truth; the admin must be able to see the mismatch (§5).",
  );
}

// ===========================================================================
// ID-33 – ID-39 — the Custom Asset Manager.
//
// Ten invariants, each of which is invisible in a page render and would otherwise
// erode one at a time:
//
//   1. the metadata table has exactly the seven columns it may have, and no body
//   2. Go and TypeScript share one filename grammar
//   3. Go and TypeScript share one size ceiling
//   4. /custom.css and /custom.js are runtime endpoints, not build-time output
//   5. the aggregate ETag is derived from the bytes it serves
//   6. Astro never reads the parked directory — "enabled" *is* the location
//   7. Go is the only writer, and the only caller of the write helpers
//   8. every custom-asset route is behind a session
//   9. a theme names no custom asset and cannot reach the asset directory
//  10. the legacy pair is still in both allowlists, and both URLs still exist
//
// §2 is the one that matters most: the whole file-first argument rests on the
// database never becoming a second copy of the admin's code.
// ===========================================================================

section(
  "ID-33–ID-39 the Custom Asset Manager is file-first and theme-agnostic",
);

const assetGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "content", "assets.go"),
);
const contentGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "content", "content.go"),
);
const contentStoreGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "content", "store.go"),
);
const customCodeHandlersGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "api", "customcode_handlers.go"),
);
const assetHandlersGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "api", "customasset_handlers.go"),
);
const storeGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "store", "store.go"),
);
const systemTs = readFileSyncSafe(path.join(ASTRO_SRC, "lib", "system.ts"));
const customCssRoute = readFileSyncSafe(
  path.join(ASTRO_SRC, "pages", "custom.css.ts"),
);
const customJsRoute = readFileSyncSafe(
  path.join(ASTRO_SRC, "pages", "custom.js.ts"),
);

// -- 1. The metadata table holds metadata only --------------------------------
//
// An exact column list, not a forbidden-column list. A table like `custom_asset`
// is the most likely place in this schema for a `content` column to appear "just
// to avoid reading the file twice", and a forbidden list has to be extended one
// column at a time. This one cannot be extended at all.
const customAssetDDL = storeGo.match(
  /CREATE TABLE custom_asset \(([\s\S]*?)\n\);/i,
);
if (!customAssetDDL) {
  fail(
    "the custom-asset metadata table is missing",
    "The files are the source of truth, but the admin needs somewhere to record what it knows about them (ID-33).",
  );
} else {
  const columns = [
    ...customAssetDDL[1].matchAll(/^\s*(\w+)\s+(?:TEXT|INTEGER|REAL|BLOB)/gm),
  ]
    .map((m) => m[1].toLowerCase())
    .sort();
  const wanted = [
    "checksum",
    "created_at",
    "enabled",
    "filename",
    "size_bytes",
    "type",
    "updated_at",
  ];
  if (columns.join(",") === wanted.join(",")) {
    pass(
      "custom_asset holds metadata only — no body, no content column (ID-33)",
      columns.join(", "),
    );
  } else {
    fail(
      "custom_asset does not hold metadata only",
      `columns are [${columns.join(", ")}]; the only permitted set is [${wanted.join(", ")}]. A body here would make the database a second copy of the admin's code (ID-33).`,
    );
  }
  if (
    /type\s+TEXT\s+NOT NULL\s+CHECK\s*\(type\s+IN\s*\('css','js'\)\)/i.test(
      customAssetDDL[1],
    )
  ) {
    pass("custom_asset.type is a closed set of css and js (ID-33)");
  } else {
    fail(
      "custom_asset.type is not constrained",
      "The manager handles CSS and JavaScript and nothing else; an unconstrained type is how an SVG or a shell script gets in (ID-33).",
    );
  }
}

// -- 2. One filename grammar -------------------------------------------------
//
// Two copies, because Go and TypeScript cannot share a constant. Comparing the
// pattern source catches the failure that matters: one side being loosened to
// accommodate a request, after which the two runtimes disagree about what a
// legal name is and a file written through one is invisible to the other.
const goPattern = assetGo.match(
  /AssetFilenamePattern = regexp\.MustCompile\(`([^`]+)`\)/,
)?.[1];
const tsPattern = systemTs.match(
  /const ASSET_FILENAME = \/([^/\n]+)\/\w*;/,
)?.[1];
if (!goPattern || !tsPattern) {
  fail(
    "the custom-asset filename grammar could not be read",
    "Both runtimes must declare it, or this check cannot compare them (ID-35).",
  );
} else if (
  goPattern.replace(/\\\./g, ".") === tsPattern.replace(/\\\./g, ".")
) {
  pass("Go and TypeScript share one filename grammar (ID-35)", goPattern);
} else {
  fail(
    "the filename grammars have diverged",
    `Go has /${goPattern}/ and TypeScript has /${tsPattern}/. A name one accepts and the other rejects is a file that exists and is never served (ID-35).`,
  );
}

// -- 3. One size ceiling -----------------------------------------------------
const goCeiling = Number(
  contentGo.match(/BodyMaxBytes\s+= (\d+) \* 1024/)?.[1],
);
const tsCeiling = Number(
  systemTs.match(/CUSTOM_ASSET_MAX_BYTES = (\d+) \* 1024/)?.[1],
);
if (goCeiling > 0 && tsCeiling === goCeiling) {
  pass(
    "both runtimes cap one custom asset at the same size",
    `${goCeiling} KiB`,
  );
} else {
  fail(
    "the custom-asset size ceilings differ",
    `Go: ${goCeiling || "?"} KiB, TypeScript: ${tsCeiling || "?"} KiB. The editor refuses what the aggregator would serve, or the other way round (ID-38).`,
  );
}

// -- 4. The public URLs are runtime endpoints ---------------------------------
//
// `prerender = false` is the whole of it. A prerendered /custom.css would be
// frozen into the build output, and the admin would save a file and see nothing
// happen until someone ran a build (ID-45).
for (const [label, source] of [
  ["custom.css", customCssRoute],
  ["custom.js", customJsRoute],
]) {
  const runtime =
    /export const prerender = false/.test(source) &&
    /readCustomAssetBundle/.test(source);
  if (runtime) {
    pass(
      `/${label} is assembled at request time, not baked into the build (ID-34)`,
    );
  } else {
    fail(
      `/${label} is not a runtime endpoint`,
      "It must be prerender = false and read the aggregate per request, or a save will need a rebuild (ID-45).",
    );
  }
}

// -- 5. The aggregate ETag comes from the bytes ------------------------------
//
// A counter or a build stamp would go stale exactly when it matters: the request
// after an admin's save.
if (
  /createHash\(['"]sha256['"]\)/.test(systemTs) &&
  /\.update\(body\)/.test(systemTs)
) {
  pass("the aggregate ETag is a digest of the bytes it serves (ID-34)");
} else {
  fail(
    "the aggregate ETag is not derived from the response body",
    "It has to change when any included file changes and nothing else (ID-34).",
  );
}

// -- 6. Astro reads only the enabled directory -------------------------------
//
// This is the mechanical statement of "enabled is a location". If the aggregator
// ever learned to read `parked/`, a disabled file would be served and the whole
// mechanism would be a flag in a database column that Astro cannot see.
if (!/parked/.test(codeLines(systemTs))) {
  pass(
    "the aggregator reads content/system/<type> and never the parked directory (ID-34)",
  );
} else {
  fail(
    "the aggregator knows about the parked directory",
    "A disabled asset must be invisible to it — 'enabled' is the file being in css/, not a flag Astro would have to be told about (ID-34).",
  );
}
if (
  /ASSET_DIR/.test(systemTs) &&
  /readdir/.test(systemTs) &&
  /\.sort\(/.test(systemTs)
) {
  pass(
    "the aggregator sorts the directory listing rather than trusting it (ID-35)",
  );
} else {
  fail(
    "the aggregator does not sort its directory listing",
    "readdir order is an implementation detail; the load order is the filename prefix (ID-35).",
  );
}

// -- 7. Go is the only writer ------------------------------------------------
const writers = [
  "CreateCustomAsset",
  "WriteCustomAsset",
  "DeleteCustomAsset",
  "SetCustomAssetEnabled",
  "RenameCustomAsset",
];
{
  const callers = [];
  for (const file of goProd) {
    if (file.endsWith(path.join("internal", "content", "assets.go"))) continue;
    if (file.endsWith("_test.go")) continue;
    const text = readFileSyncSafe(file);
    for (const fn of writers) {
      if (new RegExp(`\\.${fn}\\(`).test(text)) {
        callers.push(`${path.relative(ROOT, file)} calls ${fn}`);
      }
    }
  }
  const outsideApi = callers.filter(
    (c) => !c.startsWith("backend/internal/api/"),
  );
  if (outsideApi.length === 0) {
    pass(
      "Go is the only writer of custom assets, and only the API handlers call the write helpers",
      callers.length > 0
        ? `${callers.length} call sites, all in internal/api`
        : "no call sites yet",
    );
  } else {
    fail(
      "a custom asset is written outside the API layer",
      `${outsideApi.join("; ")}. One writer, one place that validates and audits (ID-33).`,
    );
  }

  // And the Astro side writes none of it. §5 forbids every write primitive under
  // astro/src; this names the module, so the reason is visible where someone would
  // be tempted. lib/system.ts is the one file that has to hold this whole feature.
  const systemWrites = [
    "writeFile",
    "appendFile",
    "mkdir",
    "rename",
    "unlink",
    "rm",
    "copyFile",
    "createWriteStream",
  ].filter((fn) => new RegExp(`\\b${fn}\\b`).test(systemTs));
  if (systemWrites.length === 0) {
    pass(
      "the aggregator reads the assets and writes nothing (§5, ID-33)",
      "lib/system.ts",
    );
  } else {
    fail(
      "the aggregator writes to the filesystem",
      `system.ts uses ${systemWrites.join(", ")}. Go is the only writer; Astro reads (ID-33).`,
    );
  }
}

// -- 8. Every custom-asset route is behind a session -------------------------
{
  // The routes are registered from a loop over the two kinds, so the paths are
  // computed and cannot be matched literally. What is checkable — and what matters —
  // is that every one of them is wrapped in the session guard, so read the function
  // rather than the route table.
  const registered = [
    ...assetHandlersGo.matchAll(/mux\.HandleFunc\((.*)\)\n/g),
  ].map((m) => m[1]);
  const unguarded = registered.filter((call) => !/requireSession/.test(call));

  if (registered.length >= 5) {
    if (unguarded.length === 0) {
      pass(
        "every custom-asset route requires a session (§7, ID-33)",
        `${registered.length} routes`,
      );
    } else {
      fail(
        "a custom-asset route is not behind a session",
        `${unguarded.join("; ")}. Reading an asset's metadata discloses an admin's file layout, and writing one executes code in every visitor's browser (ID-33).`,
      );
    }
  } else {
    fail(
      "the custom-asset routes could not be found",
      `found ${registered.length} registrations; expected GET/POST/PUT/DELETE for both css and js.`,
    );
  }
}

// -- 9. A theme names no custom asset ----------------------------------------
{
  const FORBIDDEN_ASSET = [
    {
      pattern: /content\/system\/(?:css|js|parked)\b/,
      why: "it reaches into the asset directory",
    },
    {
      pattern: /\b\d{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.(?:css|js)\b/,
      why: "it names a managed asset",
    },
    {
      pattern: /blogcms:(?:css|js):/,
      why: "it knows the aggregator's internal marker",
    },
    {
      pattern: /["'`]\/custom\/(?:css|js)\b/,
      why: "it invents a per-file custom-code URL",
    },
  ];

  const hits = [];
  for (const file of themeFileList) {
    const text = codeLines(readFileSyncSafe(file));
    for (const rule of FORBIDDEN_ASSET) {
      if (rule.pattern.test(text)) {
        hits.push(`${path.relative(ROOT, file)}: ${rule.why}`);
      }
    }
  }
  if (hits.length === 0) {
    pass(
      "no theme names a custom asset or reaches into the asset directory (ID-38, ID-43, ID-44)",
      `${themeFileList.length} theme files checked`,
    );
  } else {
    fail(
      "a theme names a custom asset",
      `${hits.join("; ")}. /custom.css and /custom.js are the entire contract; a theme that knows a filename is coupled to one admin's layout (ID-38).`,
    );
  }

  // And the core cannot assemble one either: a per-file URL would make the stable
  // entry point optional, and an optional entry point is one some theme would skip.
  const coreHits = [];
  for (const file of astroFiles) {
    const text = codeLines(readFileSyncSafe(file));
    for (const rule of FORBIDDEN_ASSET) {
      if (rule.pattern.test(text)) {
        coreHits.push(`${path.relative(ROOT, file)}: ${rule.why}`);
      }
    }
  }
  if (coreHits.length === 0) {
    pass(
      "no core module names a custom asset either — only the aggregator does",
    );
  } else {
    fail(
      "a core module names a custom asset",
      `${coreHits.join("; ")}. The aggregator owns the list; everything else links the two stable URLs (ID-38).`,
    );
  }
}

// -- 10. The legacy pair is still there --------------------------------------
{
  const legacyOk =
    /name != "custom\.css" && name != "custom\.js"/.test(contentStoreGo) &&
    /system\/custom\.css/.test(customCodeHandlersGo) &&
    /system\/custom\.js/.test(customCodeHandlersGo) &&
    /"custom_css\.updated"/.test(customCodeHandlersGo) &&
    /"custom_js\.updated"/.test(customCodeHandlersGo) &&
    /customCss: 'custom\.css'/.test(systemTs) &&
    /customJs: 'custom\.js'/.test(systemTs);

  if (legacyOk) {
    pass(
      "custom.css and custom.js are still first-class files in both runtimes (ID-34)",
    );
  } else {
    fail(
      "the legacy custom-code files lost their place in the allowlist",
      "They are preserved for backward compatibility: still readable, still writable, still ahead of every managed asset (ID-34).",
    );
  }

  // The legacy files are not deletable through the manager. A delete on the
  // reserved id is a 400 in the handler; this asserts the reservation exists.
  if (
    /legacyAssetID/.test(assetHandlersGo) &&
    /custom-code/.test(assetHandlersGo)
  ) {
    pass(
      "the legacy pair is not addressable — let alone deletable — through the asset API (ID-34)",
    );
  } else {
    fail(
      "the legacy pair has become addressable through the asset API",
      "They must stay editable at /admin/custom-code and immutable from the asset manager (ID-34).",
    );
  }

  // §22: the audit log records the event and the reference, never the text. This
  // is the one place a body would be easiest to add "just for debugging".
  const auditRefs = [
    ...assetHandlersGo.matchAll(/d\.Audit\(r\.Context\(\), (.+?), (.+?)\)\n/g),
  ];
  const leaksBody = auditRefs.filter(([, , ref]) =>
    /req\.Content|\bcontent\b|\bbody\b/i.test(ref),
  );
  // The events are written with a computed noun ("custom_" + kind + ".created"),
  // so the shape to check is the verb: one of the five §16 names, and never a
  // content event under the asset log.
  const verbs = auditRefs
    .map(([, kind]) => kind)
    .filter(
      (expr) => !/\.(created|updated|deleted|enabled|disabled)"/.test(expr),
    );

  if (auditRefs.length >= 3 && leaksBody.length === 0 && verbs.length === 0) {
    pass(
      "every custom-asset audit event names a verb from the §16 set and a reference, never the file's text (ID-33, §22)",
      `${auditRefs.length} call sites`,
    );
  } else {
    fail(
      "a custom-asset audit event is malformed",
      `leaking refs: ${leaksBody.map((m) => m[2]).join("; ") || "none"}; unrecognised event names: ${verbs.join("; ") || "none"}`,
    );
  }
}

// ===========================================================================
// §34 — the Markdown presentation layer is file-first, scoped to
// .markdown-body, and theme-agnostic, with the same properties as the
// custom-asset manager: metadata-only table, one grammar, one ceiling,
// a runtime aggregate, one writer, and no theme coupling.
// ===========================================================================

section("ID-40–ID-48 the Markdown template system is file-first");

const markdownAssetsGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "content", "markdown_assets.go"),
);
const markdownHandlersGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "api", "markdownasset_handlers.go"),
);
const markdownStoreGo = readFileSyncSafe(
  path.join(BACKEND, "internal", "store", "markdownassets.go"),
);
const markdownCssTs = readFileSyncSafe(
  path.join(ASTRO_SRC, "lib", "markdown-css.ts"),
);
const markdownTs = readFileSyncSafe(path.join(ASTRO_SRC, "lib", "markdown.ts"));
const markdownCssRoute = readFileSyncSafe(
  path.join(ASTRO_SRC, "pages", "markdown.css.ts"),
);

// -- 1. The metadata table holds metadata only -----------------------------
//
// The same argument as custom_asset, with the same remedy: an exact
// column list. `markdown_asset` is where a `content` column would
// appear "just to avoid reading the CSS twice", and the database
// becoming a second copy of the templates is the inversion the
// whole architecture exists to prevent.
{
  const ddl = storeGo.match(/CREATE TABLE markdown_asset \(([\s\S]*?)\n\);/i);
  if (!ddl) {
    fail(
      "the markdown-template metadata table is missing",
      "The files are the source of truth, but the admin needs somewhere to record what it knows about them (ID-41).",
    );
  } else {
    const columns = [
      ...ddl[1].matchAll(/^\s*(\w+)\s+(?:TEXT|INTEGER|REAL|BLOB)/gm),
    ]
      .map((m) => m[1].toLowerCase())
      .sort();
    const wanted = [
      "checksum",
      "created_at",
      "enabled",
      "filename",
      "size_bytes",
      "updated_at",
    ];
    if (columns.join(",") === wanted.join(",")) {
      pass(
        "markdown_asset holds metadata only — no body, no content column (ID-41)",
        columns.join(", "),
      );
    } else {
      fail(
        "markdown_asset does not hold metadata only",
        `columns are [${columns.join(", ")}]; the only permitted set is [${wanted.join(", ")}]. A body here would make the database a second copy of the admin's CSS (ID-41).`,
      );
    }
  }
}

// -- 2. One filename grammar ------------------------------------------------
{
  const goPattern = markdownAssetsGo.match(
    /MarkdownTemplatePattern = regexp\.MustCompile\(`([^`]+)`\)/,
  )?.[1];
  const tsPattern = markdownCssTs.match(
    /const MARKDOWN_TEMPLATE_FILENAME = \/([^/\n]+)\/\w*;/,
  )?.[1];
  if (!goPattern || !tsPattern) {
    fail(
      "the markdown-template filename grammar could not be read",
      "Both runtimes must declare it, or this check cannot compare them (ID-35).",
    );
  } else if (
    goPattern.replace(/\\\./g, ".") === tsPattern.replace(/\\\./g, ".")
  ) {
    pass(
      "Go and TypeScript share one markdown-template filename grammar (ID-35)",
      goPattern,
    );
  } else {
    fail(
      "the markdown-template filename grammars have diverged",
      `Go has /${goPattern}/ and TypeScript has /${tsPattern}/. A name one accepts and the other rejects is a file that exists and is never served (ID-35).`,
    );
  }
}

// -- 3. One size ceiling -----------------------------------------------------
{
  const goCeiling = Number(
    contentGo.match(/BodyMaxBytes\s+= (\d+) \* 1024/)?.[1],
  );
  const tsCeiling = Number(
    markdownCssTs.match(/MARKDOWN_TEMPLATE_MAX_BYTES = (\d+) \* 1024/)?.[1],
  );
  if (goCeiling > 0 && tsCeiling === goCeiling) {
    pass(
      "both runtimes cap one markdown template at the same size",
      `${goCeiling} KiB`,
    );
  } else {
    fail(
      "the markdown-template size ceilings differ",
      `Go: ${goCeiling || "?"} KiB, TypeScript: ${tsCeiling || "?"} KiB. The editor refuses what the aggregator would serve, or the other way round (ID-38).`,
    );
  }
}

// -- 4. /markdown.css is a runtime endpoint ---------------------------------
{
  const runtime =
    /export const prerender = false/.test(markdownCssRoute) &&
    /readMarkdownTemplateBundle/.test(markdownCssRoute);
  if (runtime) {
    pass(
      "/markdown.css is assembled at request time, not baked into the build (ID-34)",
    );
  } else {
    fail(
      "/markdown.css is not a runtime endpoint",
      "It must be prerender = false and read the aggregate per request, or a save will need a rebuild (ID-34).",
    );
  }
}

// -- 5. The aggregator reads only the enabled directory --------------------
//
// "Enabled is a location" has to hold here too: if the aggregator ever
// learned to read `parked/`, a disabled template would be served.
{
  const code = codeLines(markdownCssTs);
  if (!/parked/.test(code)) {
    pass(
      "the markdown aggregator reads content/system/markdown and never the parked directory (ID-34)",
    );
  } else {
    fail(
      "the markdown aggregator knows about the parked directory",
      "A disabled template must be invisible to it — 'enabled' is the file being in markdown/, not a flag Astro would have to be told about (ID-34).",
    );
  }
  if (/readdir/.test(markdownCssTs) && /\.sort\(/.test(markdownCssTs)) {
    pass(
      "the markdown aggregator sorts the directory listing rather than trusting it (ID-35)",
    );
  } else {
    fail(
      "the markdown aggregator does not sort its directory listing",
      "readdir order is an implementation detail; the load order is the filename prefix (ID-35).",
    );
  }
}

// -- 6. Go is the only writer ------------------------------------------------
{
  const writers = [
    "CreateMarkdownTemplate",
    "WriteMarkdownTemplate",
    "DeleteMarkdownTemplate",
    "SetMarkdownTemplateEnabled",
    "RenameMarkdownTemplate",
  ];
  const callers = [];
  for (const file of goProd) {
    if (file.endsWith(path.join("internal", "content", "markdown_assets.go"))) {
      continue;
    }
    if (file.endsWith("_test.go")) continue;
    const text = readFileSyncSafe(file);
    for (const fn of writers) {
      if (new RegExp(`\\.${fn}\\(`).test(text)) {
        callers.push(`${path.relative(ROOT, file)} calls ${fn}`);
      }
    }
  }
  const outsideApi = callers.filter(
    (c) => !c.startsWith("backend/internal/api/"),
  );
  if (outsideApi.length === 0) {
    pass(
      "Go is the only writer of markdown templates, and only the API handlers call the write helpers",
      callers.length > 0
        ? `${callers.length} call sites, all in internal/api`
        : "no call sites yet",
    );
  } else {
    fail(
      "a markdown template is written outside the API layer",
      `${outsideApi.join("; ")}. One writer, one place that validates and audits (ID-40).`,
    );
  }

  const systemWrites = [
    "writeFile",
    "appendFile",
    "mkdir",
    "rename",
    "unlink",
    "rm",
    "copyFile",
    "createWriteStream",
  ].filter((fn) => new RegExp(`\\b${fn}\\b`).test(markdownCssTs));
  if (systemWrites.length === 0) {
    pass(
      "the markdown aggregator reads the templates and writes nothing (§5, ID-40)",
    );
  } else {
    fail(
      "the markdown aggregator writes to the filesystem",
      `markdown-css.ts uses ${systemWrites.join(", ")}. Go is the only writer; Astro reads (ID-40).`,
    );
  }
}

// -- 7. Every markdown-template route is behind a session -----------------
{
  const registered = [
    ...markdownHandlersGo.matchAll(/mux\.HandleFunc\((.*)\)\n/g),
  ].map((m) => m[1]);
  const unguarded = registered.filter((call) => !/requireSession/.test(call));
  if (registered.length >= 5) {
    if (unguarded.length === 0) {
      pass(
        "every markdown-template route requires a session (§7, ID-40)",
        `${registered.length} routes`,
      );
    } else {
      fail(
        "a markdown-template route is not behind a session",
        `${unguarded.join("; ")}. Reading a template's metadata discloses an admin's file layout, and writing one executes CSS in every visitor's browser (ID-40).`,
      );
    }
  } else {
    fail(
      "the markdown-template routes could not be found",
      `found ${registered.length} registrations; expected GET/POST/PUT/DELETE for the collection and its items.`,
    );
  }
}

// -- 8. The renderer emits the .markdown-body scope ------------------------
{
  if (/<div class="markdown-body">/.test(markdownTs)) {
    pass(
      "renderMarkdown wraps every body in .markdown-body (ID-47)",
      "the scope every template is written against",
    );
  } else {
    fail(
      "the renderer does not wrap output in .markdown-body",
      "The wrapper is what makes a template theme-independent: it is emitted by the renderer, so a post page, a page and the admin preview cannot disagree about the scope (ID-47).",
    );
  }
}

// -- 9. The audit events are the §16 set, and never the text --------------
{
  const auditRefs = [
    ...markdownHandlersGo.matchAll(
      /d\.Audit\(r\.Context\(\), (.+?), (.+?)\)\n/g,
    ),
  ];
  const leaksBody = auditRefs.filter(([, , ref]) =>
    /req\.Content|\bcontent\b|\bbody\b/i.test(ref),
  );
  const verbs = auditRefs
    .map(([, kind]) => kind)
    .filter(
      (expr) =>
        !/markdown_template\.(created|updated|deleted|enabled|disabled)"/.test(
          expr,
        ),
    );
  if (auditRefs.length >= 3 && leaksBody.length === 0 && verbs.length === 0) {
    pass(
      "every markdown-template audit event names a verb from the §16 set and a reference, never the file's text (ID-40, §22)",
      `${auditRefs.length} call sites`,
    );
  } else {
    fail(
      "a markdown-template audit event is malformed",
      `leaking refs: ${leaksBody.map((m) => m[2]).join("; ") || "none"}; unrecognised event names: ${verbs.join("; ") || "none"}`,
    );
  }
}

// -- 10. No theme reaches into the template directory ----------------------
{
  const FORBIDDEN_MARKDOWN = [
    {
      pattern: /content\/system\/(?:markdown|parked)\b/,
      why: "it reaches into the template directory",
    },
    {
      pattern: /blogcms:markdown:/,
      why: "it knows the aggregator's internal marker",
    },
  ];
  const hits = [];
  for (const file of themeFileList) {
    const text = codeLines(readFileSyncSafe(file));
    for (const rule of FORBIDDEN_MARKDOWN) {
      if (rule.pattern.test(text)) {
        hits.push(`${path.relative(ROOT, file)}: ${rule.why}`);
      }
    }
  }
  if (hits.length === 0) {
    pass(
      "no theme reaches into the markdown template directory or knows its internals (ID-40, ID-48)",
      `${themeFileList.length} theme files checked`,
    );
  } else {
    fail(
      "a theme is coupled to the markdown template system",
      `${hits.join("; ")}. /markdown.css is the entire contract; a theme that knows a filename or the aggregator's marker cannot be swapped (ID-48).`,
    );
  }
}

// ===========================================================================
// Git Backup Phase 1 — Git versions the content, it never becomes the content
// ===========================================================================

section("Git Backup Phase 1 — Git is a version layer over the filesystem");

const BACKUP_DIR = path.join(BACKEND, "internal", "backup");
const backupFiles = await walk(BACKUP_DIR, (f) => f.endsWith(".go"));
const backupProd = backupFiles.filter((f) => !f.endsWith("_test.go"));
const backupCode = new Map(
  backupProd.map((f) => [f, codeLines(readFileSyncSafe(f))]),
);

/**
 * Reports every forbidden call in the backup package, with the line it is on.
 *
 * These rules exist because the properties they protect are invisible in a page
 * render. A backup that quietly began pushing, or that learned to check out an
 * older commit, would still show a perfectly normal admin screen.
 */
function forbidInBackup(name, patterns, why) {
  const hits = [];
  for (const [file, text] of backupCode) {
    text.split("\n").forEach((line, i) => {
      for (const pattern of patterns) {
        if (pattern instanceof RegExp) {
          if (pattern.test(line)) {
            hits.push(
              `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`,
            );
          }
        } else if (line.includes(pattern)) {
          hits.push(`${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`);
        }
      }
    });
  }
  if (hits.length === 0) pass(name, why);
  else fail(name, `${hits.join("\n")}\n        ${why}`);
}

// -- 1. "Backup" means a local commit. Push is a later phase. --------------
//
// A backup must succeed with no network at all. The moment this service can
// reach a remote, "GitHub is down" becomes a reason a local backup fails, and
// the local repository stops being a complete backup.
forbidInBackup(
  "the backup service never reaches a remote (ID-49)",
  [
    "PushContext",
    "PushOptions",
    "FetchContext",
    "PullContext",
    "ListRemotes",
    "CreateRemote",
    "DeleteRemote",
    "Remote(",
    ".Push(",
    ".Fetch(",
    ".Pull(",
    "http://",
    "https://",
  ],
  "Backup = a local commit. Push, fetch and pull belong to Phase 2, where a remote being down must not affect this.",
);

// -- 2. Restore, checkout and rollback are Phase 6. ------------------------
//
// These are the destructive operations. None of them is reachable from any
// route today, and a route is not the only way one could appear.
forbidInBackup(
  "the backup service cannot rewrite the working tree (ID-49)",
  [
    /\bCheckout\b/,
    /\bReset\(/,
    /\bRevert\b/,
    /\bRollback\b/,
    /\bStash\b/,
    /\bClone\(/,
    "PlainClone",
    "RemoveAll",
    "PlainRemove",
  ],
  "Restore is destructive and deliberately absent. Adding it is Phase 6, which amends the architecture first.",
);

// -- 3. One site, one repository, one primary branch, many commits. --------
//
// Branches are workspaces, not content categories. A `posts` branch and a
// `media` branch would make "restore the site as it was on Tuesday" a merge
// rather than a checkout, which is the opposite of what a backup is for.
//
// The count that matters is *where* a branch name is created, not how many call
// sites: Initialize resolves the name from two candidate sources (the CMS
// configuration, then the operator's global Git configuration), and both are
// the one initial branch. What must not exist is any way to make a second one.
{
  const branchOps = [];
  let namedOutsideInitialize = false;
  for (const [file, text] of backupCode) {
    const where = path.relative(ROOT, file);
    // The one function allowed to name a branch.
    const initialize =
      text.match(/func \(s \*Service\) Initialize\([\s\S]*?\n\}/)?.[0] ?? "";
    text.split("\n").forEach((line, i) => {
      if (/\b(CreateBranch|DeleteBranch)\b|\.Branch\(/.test(line)) {
        branchOps.push(`${where}:${i + 1}: ${line.trim()}`);
      }
      if (
        line.includes("plumbing.NewBranchReferenceName") &&
        !initialize.includes(line.trim())
      ) {
        namedOutsideInitialize = true;
      }
    });
  }
  if (branchOps.length === 0 && !namedOutsideInitialize) {
    pass(
      "the repository has one branch and no way to make another (ID-52)",
      "the initial branch is named only by Initialize, from configuration or the operator's Git config",
    );
  } else {
    fail(
      "the backup service creates branches",
      [
        branchOps.join("\n"),
        namedOutsideInitialize
          ? "a branch is named outside Initialize"
          : "",
      ]
        .filter(Boolean)
        .join("\n"),
    );
  }
}

// -- 4. The inclusion policy is the single authority. -----------------------
//
// A path check written inline in the sync loop is how a backup starts
// including something nobody decided to include. `contentSourceDirs` lives in
// policy.go and nowhere else, and the service may not name a content directory
// at all.
{
  const policy = path.join(BACKUP_DIR, "policy.go");
  const strays = [];
  for (const [file, text] of backupCode) {
    if (file === policy) continue;
    text.split("\n").forEach((line, i) => {
      if (/"(posts|pages|system)"|"posts\/|"pages\/|"system\//.test(line)) {
        strays.push(
          `${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`,
        );
      }
    });
  }
  if (strays.length === 0) {
    pass(
      "what enters a backup is decided in one file (policy.go) and nowhere else (ID-51)",
      `${backupProd.length - 1} other production files checked`,
    );
  } else {
    fail(
      "a content directory is named outside the inclusion policy",
      `${strays.join("\n")}\n        A backup that decides for itself what to include cannot be reviewed in one place.`,
    );
  }
}

// -- 5. The commit identity is the CMS, never the admin (§30). --------------
//
// The admin's address is stored in SQLite and is the one piece of personal
// data a repository would carry. Pushing to a public remote later must not
// publish it, so the identity is fixed and cannot be derived from the session.
{
  const imports = [];
  const identities = [];
  for (const [file, text] of backupCode) {
    if (/blogcms\/internal\/(auth|store)/.test(text)) {
      imports.push(path.relative(ROOT, file));
    }
    text.split("\n").forEach((line, i) => {
      // Every signature field written must come from the fixed constants, so
      // there is nowhere for a session's address to enter a commit.
      if (/\b(Email|Name)\s*:/.test(line) && !/\b(commitEmail|commitName)\b/.test(line)) {
        identities.push(`${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`);
      }
    });
  }
  const identity = readFileSyncSafe(path.join(BACKUP_DIR, "service.go"));
  const fixed = /commitName\s*=\s*"[^"]+"/.test(identity) &&
    /commitEmail\s*=\s*"[^"]+"/.test(identity);
  if (imports.length === 0 && identities.length === 0 && fixed) {
    pass(
      "a commit is authored by the CMS, never by the admin's account (ID-53)",
      "the backup package cannot reach the session or the admin row, and every signature field is a constant",
    );
  } else {
    fail(
      "the commit identity can become the administrator's",
      [
        imports.length > 0
          ? `the backup package imports ${imports.join(", ")}`
          : "",
        identities.length > 0
          ? `a signature field is not a constant:\n        ${identities.join("\n        ")}`
          : "",
        fixed ? "" : "service.go no longer declares a fixed commit identity",
      ]
        .filter(Boolean)
        .join("\n") +
        "\n        The admin's address is the one piece of personal data a repository carries; pushing to a public remote must not publish it.",
    );
  }
}

// -- 6. Every backup route requires a session, so CSRF cannot be bypassed --
//
// An internal-looking admin endpoint is still reachable from a stranger's
// browser. The session wrapper is the CSRF gate (§7, §68).
{
  const handlers = readFileSyncSafe(
    path.join(BACKEND, "internal", "api", "backup_handlers.go"),
  );
  const block =
    handlers.match(
      /func registerBackupRoutes\([\s\S]*?\n\}/,
    )?.[0] ?? "";
  const routes = [...block.matchAll(/HandleFunc\("([^"]+)"/g)].map((m) => m[1]);
  const gated = [...block.matchAll(/HandleFunc\("[^"]+",\s*d\.requireSession\(/g)];
  if (routes.length > 0 && routes.length === gated.length) {
    pass(
      "every backup route requires a session (§7, so CSRF cannot be bypassed)",
      `${routes.length} routes, all behind requireSession`,
    );
  } else {
    fail(
      "a backup route is not behind requireSession",
      `${gated.length} of ${routes.length} routes are gated:\n        ${routes.join("\n  ")}`,
    );
  }
}

// -- 7. Git is not the content authority. ----------------------------------
//
// The dangerous direction is the one that would make the repository a source
// of truth: the renderer or the loader reading content out of Git, or the
// frontend being told where the repository is. Neither is needed — a live
// collection reads content/ — so both are failures.
{
  const reads = [];
  for (const file of await walk(ASTRO_SRC)) {
    const text = readFileSyncSafe(file);
    if (/GIT_BACKUP_ROOT|git-backup|internal\/backup/.test(text)) {
      reads.push(path.relative(ROOT, file));
    }
  }
  // The content path must not import the backup service either: a renderer that
  // could fall back to the repository has two authorities.
  const contentImports = [];
  for (const file of await walk(path.join(BACKEND, "internal", "content"), (f) =>
    f.endsWith(".go"),
  )) {
    if (/blogcms\/internal\/backup/.test(readFileSyncSafe(file))) {
      contentImports.push(path.relative(ROOT, file));
    }
  }
  if (reads.length === 0 && contentImports.length === 0) {
    pass(
      "nothing reads content out of the backup repository",
      "the loader and the renderer are unaware the repository exists — the filesystem is the only authority (§2)",
    );
  } else {
    fail(
      "the content path can reach the backup repository",
      [
        reads.length > 0
          ? `astro/src: ${reads.join(", ")}`
          : "",
        contentImports.length > 0
          ? `internal/content: ${contentImports.join(", ")}`
          : "",
      ]
        .filter(Boolean)
        .join("\n"),
    );
  }
}

// -- 8. The CMS keeps no copy of Git's history. ----------------------------
//
// Git is the history authority. A `commit` table would be a second authority
// that could disagree with the repository, and would be the first thing to
// consult after a restore.
{
  const ddl = storeGo.match(/CREATE TABLE (\w+) \(/g) ?? [];
  const tables = ddl.map((m) => m.match(/CREATE TABLE (\w+)/)[1]);
  const suspect = tables.filter((t) =>
    /git|commit|backup|branch|remote/i.test(t),
  );
  if (suspect.length === 0) {
    pass(
      "the database holds no Git history — Git is the history authority (ID-54)",
      `${tables.length} tables, none of them a copy of the repository`,
    );
  } else {
    fail(
      "the database is shadowing Git's history",
      `${suspect.join(", ")}. A commit table and a repository would eventually disagree, and nothing could say which was right (ID-54).`,
    );
  }
}

// -- 9. The repository lives outside the content it versions. ---------------
//
// A repository inside the content root would record its own metadata as
// content, and the walk that finds source content would walk the snapshot.
// This is enforced at startup by config.backupRoot; the source here is that
// the default is a separate directory rather than `content/.git`.
{
  const config = readFileSyncSafe(
    path.join(BACKEND, "internal", "config", "config.go"),
  );
  const defaultIsDataRoot =
    /backupRoot\(g,\s*"GIT_BACKUP_ROOT",\s*cfg\.DataRoot/.test(config);
  const rejects = /must live outside the content it versions/.test(config) &&
    /must never sit inside the repository work tree/.test(config);
  if (defaultIsDataRoot && rejects) {
    pass(
      "the repository path is configuration, defaulting outside the content root (ID-50)",
      "GIT_BACKUP_ROOT, with the containment rules enforced at startup",
    );
  } else {
    fail(
      "the backup repository location is not a validated, separate root",
      `${defaultIsDataRoot ? "" : "GIT_BACKUP_ROOT does not default under DATA_ROOT\n        "}${rejects ? "" : "the containment rules are missing"}`,
    );
  }
}

// -- 10. The audit log records that a backup happened, never what it held --
//
// ID-54: Git is the history authority, so an audit row is not a second copy
// of it. A diff or a file list in the reference would be a body in the one
// table that must never hold one — and it is the table a restore would read.
{
  const handlers = readFileSyncSafe(
    path.join(BACKEND, "internal", "api", "backup_handlers.go"),
  );
  const events = [
    ...handlers.matchAll(
      /d\.Audit\(r\.Context\(\),\s*"([^"]+)",\s*([^)]+)\)/g,
    ),
  ];
  const named = events.filter(
    ([, kind]) => kind === "git_backup.initialize" || kind === "git_backup.commit",
  );
  // The reference is a commit hash (or the branch, for a first
  // initialization with no content). Anything longer is a body.
  const refs = events.map(([, , ref]) => ref.trim());
  const carriesBody = refs.filter((ref) =>
    /patch|diff|change|path|content|body|req\.|res\.Status|res\.ChangedFiles/i.test(ref),
  );
  if (named.length === 2 && events.length === 2 && carriesBody.length === 0) {
    pass(
      "every backup audit event names a decision and a commit hash, never the diff (ID-54)",
      `${events.length} events: ${named.map(([, k]) => k).join(", ")}`,
    );
  } else {
    fail(
      "a backup audit event is malformed",
      [
        named.length === events.length
          ? ""
          : `unrecognised events: ${events.map(([, k]) => k).join(", ")}`,
        carriesBody.length > 0
          ? `the reference carries a diff's own bytes: ${carriesBody.join("; ")}`
          : "",
      ]
        .filter(Boolean)
        .join("\n        "),
    );
  }
}

// -- 11. No theme script subscripts a property that is not always there ----
//
// `changedTouches` exists on a TouchEvent and on nothing else. A MouseEvent has
// no such property, so `(e as TouchEvent).changedTouches[0]` throws
// `Cannot read properties of undefined` — in a document-level listener, on every
// left click in a mouse browser.
//
// It shipped that way: the page rendered, every test passed, and no test could
// see it, because a click is not observable over HTTP. The `if (touch)` guard
// that followed it looked like it handled the case, and the mouse branch below
// was unreachable code.
//
// The rule is the shape, not the property: a value reached through a cast is not
// proven to exist, so it must be assigned first and checked before it is
// subscripted. Assign-then-guard-then-index passes; index-in-place does not.
{
  const UNGUARDED = [
    {
      pattern: /\)\s*\.\s*\w+\s*\[\s*0\s*\]/,
      why: "it subscripts a property of a cast value, which may not exist",
    },
    {
      pattern: /\bas\s+\w+\s*\)?\.\w+\s*\[\s*0\s*\]/,
      why: "it subscripts a cast value, which may not exist",
    },
  ];
  const hits = [];
  // `astroFiles` is every file under astro/src, which already contains the
  // themes — listing both would report each theme file twice.
  for (const file of new Set(astroFiles)) {
    const text = codeLines(readFileSyncSafe(file));
    text.split("\n").forEach((line, i) => {
      // One report per line: the rules overlap deliberately (a cast can be
      // parenthesised or not), and a file that trips both is one defect.
      if (UNGUARDED.some((rule) => rule.pattern.test(line))) {
        hits.push(`${path.relative(ROOT, file)}:${i + 1}: ${line.trim()}`);
      }
    });
  }
  if (hits.length === 0) {
    pass(
      "no theme script subscripts a value it has not proved exists",
      `${new Set(astroFiles).size} files checked, themes included`,
    );
  } else {
    fail(
      "a script indexes an optional event property in place",
      `${hits.join("\n")}\n        An event property that only some events carry (\`changedTouches\`) throws on the rest. Assign it, check it, then index it.`,
    );
  }
}

console.log(
  `\n${failures === 0 ? "ARCHITECTURE CHECKS PASSED" : "ARCHITECTURE CHECKS FAILED"}: ${checks - failures}/${checks} passed`,
);
process.exit(failures === 0 ? 0 : 1);
