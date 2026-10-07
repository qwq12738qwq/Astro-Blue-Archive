#!/usr/bin/env node
/**
 * Core integration tests against a real production Astro SSR build.
 *
 * These are the project's acceptance conditions, not unit tests:
 *
 *   §32  content changes appear with NO rebuild (edit / create / delete)
 *   §33  one malformed Markdown file never fails the site (D5)
 *   §10  drafts are invisible on public routes
 *   §16/§17 custom CSS and JS are served as external same-origin resources
 *   §6   a broken CONTENT_ROOT fails loudly instead of serving an empty blog
 *
 * Nothing here mocks the filesystem or the HTTP server: the server under test is
 * `node dist/server/entry.mjs`, exactly what production runs.
 */
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { existsSync, readFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const ASTRO = path.join(ROOT, "astro");
const ENTRY = path.join(ASTRO, "dist", "server", "entry.mjs");
const BACKEND = path.join(ROOT, "backend");
const GO_BINARY = path.join(BACKEND, "bin", "blogcms-server");

/**
 * Allocate a free port.
 *
 * A fixed port is unsafe here: a server left over from a previous run would
 * silently answer the tests with a stale content root, producing failures that
 * look like application bugs.
 */
function freePort() {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

let PORT = Number(process.env.TEST_PORT ?? 0);
let BASE = "";

let passed = 0;
let failed = 0;
const failures = [];
let currentTest = "";

function ok(cond, message) {
  if (cond) {
    passed++;
    console.log(`    PASS  ${message}`);
  } else {
    failed++;
    failures.push(`${currentTest}: ${message}`);
    console.error(`    FAIL  ${message}`);
  }
}

function eq(actual, expected, message) {
  ok(
    actual === expected,
    `${message}${actual === expected ? "" : ` (expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)})`}`,
  );
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitFor(fn, { attempts = 120, interval = 150 } = {}) {
  for (let i = 0; i < attempts; i++) {
    try {
      if (await fn()) return true;
    } catch {
      /* keep polling */
    }
    await sleep(interval);
  }
  return false;
}

async function test(name, fn) {
  currentTest = name;
  console.log(`\n  ${name}`);
  try {
    await fn();
  } catch (err) {
    failed++;
    failures.push(`${name}: threw ${err?.stack ?? err}`);
    console.error(`    FAIL  threw: ${err?.stack ?? err}`);
  }
}

// ---------------------------------------------------------------------------

async function ensureBuild() {
  // The feed is served by the Go backend (ARCHITECTURE.md §28), so the
  // stack under test is both processes, exactly like production. Go is
  // rebuilt unconditionally: a conditional build was how a suite twice
  // reported PASS for a reverted fix.
  await mkdir(path.join(BACKEND, "bin"), { recursive: true });
  await new Promise((resolve, reject) => {
    const p = spawn("go", ["build", "-o", GO_BINARY, "./cmd/server"], {
      cwd: BACKEND,
      stdio: "inherit",
    });
    p.on("exit", (code) =>
      code === 0 ? resolve() : reject(new Error("go build failed")),
    );
    p.on("error", reject);
  });
  if (existsSync(ENTRY)) return;
  console.log("  dist/ missing — building...");
  await new Promise((resolve, reject) => {
    const p = spawn("npx", ["astro", "build"], {
      cwd: ASTRO,
      stdio: "inherit",
    });
    p.on("exit", (code) =>
      code === 0 ? resolve() : reject(new Error("astro build failed")),
    );
    p.on("error", reject);
  });
  if (!existsSync(ENTRY)) {
    throw new Error(`astro build did not produce ${ENTRY}`);
  }
}

async function makeContent() {
  const dir = await mkdtemp(path.join(tmpdir(), "blogcms-content-"));
  await mkdir(path.join(dir, "posts"), { recursive: true });
  await mkdir(path.join(dir, "pages"), { recursive: true });
  await mkdir(path.join(dir, "system"), { recursive: true });

  await writeFile(
    path.join(dir, "posts", "hello-world.md"),
    `---\ntitle: Hello World\nslug: hello-world\ndescription: First post\ndate: 2026-10-03\ntags:\n  - astro\ndraft: false\n---\n\n# Hello\n\nOriginal body text.\n`,
  );
  await writeFile(
    path.join(dir, "pages", "about.md"),
    `---\ntitle: About\nslug: about\nnavOrder: 10\ndraft: false\n---\n\nAbout this blog.\n`,
  );
  await writeFile(
    path.join(dir, "system", "custom.css"),
    `/* integration-test custom css */\n.mark { color: rebeccapurple; }\n`,
  );
  await writeFile(
    path.join(dir, "system", "custom.js"),
    `// integration-test custom js\nwindow.__customJsLoaded = true;\n`,
  );
  return dir;
}

async function startServer(contentRoot, { backend = true } = {}) {
  // /rss.xml is rewritten onto the Go backend's feed route, so a stack
  // that answers it needs the backend too. The one caller that opts out
  // is the broken-CONTENT_ROOT check, which is about Astro's own loud
  // failure and would only see the backend fail to start.
  let apiBase = "http://127.0.0.1:9";
  let backendChild;
  let tempRoots = [];

  if (backend) {
    const dataRoot = await mkdtemp(path.join(tmpdir(), "blogcms-data-"));
    const mediaRoot = await mkdtemp(path.join(tmpdir(), "blogcms-media-"));
    tempRoots = [dataRoot, mediaRoot];
    const apiPort = await freePort();
    apiBase = `http://127.0.0.1:${apiPort}`;

    backendChild = spawn(GO_BINARY, [], {
      cwd: BACKEND,
      env: {
        ...process.env,
        PORT: String(apiPort),
        HOST: "127.0.0.1",
        CONTENT_ROOT: contentRoot,
        MEDIA_ROOT: mediaRoot,
        DATA_ROOT: dataRoot,
        PUBLIC_ORIGIN: BASE,
        SECURE_COOKIES: "false",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    const backendLogs = [];
    backendChild.stdout.on("data", (d) => backendLogs.push(d.toString()));
    backendChild.stderr.on("data", (d) => backendLogs.push(d.toString()));

    const backendUp = await waitFor(async () => {
      try {
        const r = await fetch(`${apiBase}/api/v1/healthz`);
        return r.ok;
      } catch {
        return false;
      }
    });
    if (!backendUp) {
      backendChild.kill("SIGKILL");
      throw new Error(`backend did not start\n${backendLogs.join("")}`);
    }
  }

  const child = spawn(process.execPath, [ENTRY], {
    cwd: ASTRO,
    env: {
      ...process.env,
      CONTENT_ROOT: contentRoot,
      API_BASE: apiBase,
      HOST: "127.0.0.1",
      PORT: String(PORT),
      NODE_ENV: "production",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  // Both streams are captured so a test can assert that the loader logged
  // an error, not merely that it skipped the file (§33).
  const logs = [];
  child.stdout.on("data", (d) => logs.push(d.toString()));
  child.stderr.on("data", (d) => logs.push(d.toString()));
  child.logs = logs;
  child.backend = backendChild;
  child.tempRoots = tempRoots;

  for (let i = 0; i < 80; i++) {
    try {
      await fetch(`${BASE}/`);
      return child;
    } catch {
      await sleep(150);
    }
  }
  child.kill("SIGKILL");
  throw new Error(`server did not become ready\n${logs.join("")}`);
}

async function get(pathname) {
  const res = await fetch(`${BASE}${pathname}`, { redirect: "manual" });
  return { status: res.status, body: await res.text(), headers: res.headers };
}

async function stopServer(child) {
  if (!child || child.exitCode !== null) return;
  const exited = new Promise((r) => child.once("exit", r));
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 5000);
  await exited;
  clearTimeout(timer);
  if (child.backend && child.backend.exitCode === null) {
    const exitedBackend = new Promise((r) => child.backend.once("exit", r));
    child.backend.kill("SIGTERM");
    const backendTimer = setTimeout(
      () => child.backend.kill("SIGKILL"),
      5000,
    );
    await exitedBackend;
    clearTimeout(backendTimer);
  }
  for (const d of child.tempRoots ?? []) {
    await rm(d, { recursive: true, force: true });
  }
}

// ---------------------------------------------------------------------------

async function main() {
  console.log(
    "Core integration tests (production Astro SSR, no rebuild between steps)",
  );
  await ensureBuild();

  if (!PORT) PORT = await freePort();
  BASE = `http://127.0.0.1:${PORT}`;
  console.log(`  using port ${PORT}`);

  const content = await makeContent();
  let server = await startServer(content);
  const logText = () => server.logs.join("");

  try {
    // -----------------------------------------------------------------------
    await test("CORE TEST §32 — content changes appear with NO rebuild", async () => {
      let r = await get("/");
      eq(r.status, 200, "GET / returns 200");
      ok(r.body.includes("Hello World"), "index lists the hello post");

      r = await get("/posts/hello-world");
      eq(r.status, 200, "GET /posts/hello returns 200");
      ok(r.body.includes("Original body text."), "post body rendered");

      // --- EDIT, no rebuild ---
      await writeFile(
        path.join(content, "posts", "hello-world.md"),
        `---\ntitle: Hello World EDITED\nslug: hello-world\ndescription: First post\ndate: 2026-10-03\ndraft: false\n---\n\n# Hello EDITED\n\nBody changed by the admin at runtime.\n`,
      );
      await sleep(120);

      r = await get("/");
      ok(
        r.body.includes("Hello World EDITED"),
        "index reflects the edited title",
      );
      r = await get("/posts/hello-world");
      ok(
        r.body.includes("Body changed by the admin at runtime."),
        "post body reflects the edit",
      );
      ok(!r.body.includes("Original body text."), "old body is gone");

      // --- CREATE, no rebuild ---
      await writeFile(
        path.join(content, "posts", "second.md"),
        `---\ntitle: Second Post\nslug: second\ndate: 2026-10-04\ndraft: false\n---\n\nCreated after the build.\n`,
      );
      await sleep(120);

      r = await get("/");
      ok(r.body.includes("Second Post"), "index lists the new post");
      r = await get("/posts/second");
      eq(r.status, 200, "GET /posts/second returns 200");
      ok(r.body.includes("Created after the build."), "new post body rendered");

      // --- RENAME (slug must track the filename), no rebuild ---
      await rm(path.join(content, "posts", "second.md"));
      await writeFile(
        path.join(content, "posts", "renamed.md"),
        `---\ntitle: Renamed Post\nslug: renamed\ndate: 2026-10-04\ndraft: false\n---\n\nRenamed after the build.\n`,
      );
      await sleep(120);

      r = await get("/posts/renamed");
      eq(r.status, 200, "GET /posts/renamed returns 200");
      r = await get("/posts/second");
      eq(r.status, 404, "the old slug is gone after rename");

      // --- DELETE, no rebuild ---
      await rm(path.join(content, "posts", "renamed.md"));
      await sleep(120);

      r = await get("/posts/renamed");
      eq(r.status, 404, "GET /posts/renamed returns 404 after deletion");
      r = await get("/");
      ok(
        !r.body.includes("Renamed Post"),
        "deleted post is gone from the index",
      );
    });

    // -----------------------------------------------------------------------
    await test("CORE TEST §33 — one malformed file does not break the collection (D5)", async () => {
      // Fixture set: two valid files and one broken file.
      await writeFile(
        path.join(content, "posts", "valid.md"),
        `---\ntitle: Valid Post\nslug: valid\ndate: 2026-10-05\ndraft: false\n---\n\nA perfectly fine post.\n`,
      );
      await writeFile(
        path.join(content, "posts", "valid2.md"),
        `---\ntitle: Another Valid Post\nslug: valid2\ndate: 2026-10-06\ndraft: false\n---\n\nAlso fine.\n`,
      );
      await sleep(120);

      const broken = [
        [
          "malformed YAML",
          "broken-yaml.md",
          `---\ntitle: [unclosed\nslug: broken-yaml\n---\n\nbody\n`,
        ],
        [
          "invalid date",
          "broken-date.md",
          `---\ntitle: Bad Date\nslug: broken-date\ndate: not-a-date\n---\n\nbody\n`,
        ],
        [
          "invalid slug",
          "broken-slug.md",
          `---\ntitle: Bad Slug\nslug: Not A Slug\ndate: 2026-10-05\n---\n\nbody\n`,
        ],
        [
          "filename/slug mismatch",
          "broken-mismatch.md",
          `---\ntitle: Mismatch\nslug: somethingelse\ndate: 2026-10-05\n---\n\nbody\n`,
        ],
        [
          "missing title",
          "broken-notitle.md",
          `---\nslug: broken-notitle\ndate: 2026-10-05\n---\n\nbody\n`,
        ],
        [
          "unknown frontmatter key",
          "broken-unknownkey.md",
          `---\ntitle: Unknown Key\nslug: broken-unknownkey\ndate: 2026-10-05\ndescriptoin: typo\n---\n\nbody\n`,
        ],
      ];

      for (const [label, file, body] of broken) {
        await writeFile(path.join(content, "posts", file), body);
      }
      await sleep(250);

      const r = await get("/");
      eq(
        r.status,
        200,
        "GET / still returns 200 with six malformed files present",
      );
      ok(r.body.includes("Valid Post"), "valid.md is rendered");
      ok(r.body.includes("Another Valid Post"), "valid2.md is rendered");
      for (const [, file] of broken) {
        ok(
          !r.body.includes(`/${file.replace(".md", "")}"`),
          `${file} is not published`,
        );
      }

      // The post pages of broken files must 404 rather than 500.
      for (const [, file] of broken) {
        const slug = file.replace(/\.md$/, "");
        const pr = await get(`/posts/${slug}`);
        ok(
          pr.status === 404 || pr.status === 200,
          `${file} route does not 500 (got ${pr.status})`,
        );
      }

      // §33 also requires an explicit error in the log, not just a silent skip.
      const logs = logText();
      ok(
        logs.includes("content_entry_invalid"),
        "the loader logged a structured error for the malformed files",
      );
      for (const [, file] of broken) {
        ok(logs.includes(file), `the log names ${file}`);
      }
      // The log must be machine-readable, not a bare sentence.
      const line = logs
        .split("\n")
        .find((l) => l.includes("content_entry_invalid"));
      let parsed = null;
      try {
        parsed = JSON.parse(line);
      } catch {
        /* handled below */
      }
      ok(parsed !== null, "the log entry is a JSON object");
      if (parsed) {
        eq(
          parsed.msg,
          "content_entry_invalid",
          "the log entry has a stable message key",
        );
        ok(
          typeof parsed.file === "string" && parsed.file.length > 0,
          "the log entry names the file",
        );
        ok(
          typeof parsed.reason === "string" && parsed.reason.length > 0,
          "the log entry explains why",
        );
        ok(
          !("content" in parsed) && !("body" in parsed),
          "the log entry carries no content body",
        );
      }

      await rm(path.join(content, "posts", "valid.md"));
      await rm(path.join(content, "posts", "valid2.md"));
      for (const [, file] of broken)
        await rm(path.join(content, "posts", file));
      await sleep(150);
    });

    // -----------------------------------------------------------------------
    await test("§9 filename and frontmatter slug must agree", async () => {
      await writeFile(
        path.join(content, "posts", "filename-authority.md"),
        `---\ntitle: Claims A Different Slug\nslug: totally-different\ndate: 2026-10-07\ndraft: false\n---\n\nShould not be reachable under either slug.\n`,
      );
      await sleep(150);

      const byFilename = await get("/posts/filename-authority");
      const byFrontmatter = await get("/posts/totally-different");
      eq(byFilename.status, 404, "the filename does not win");
      eq(byFrontmatter.status, 404, "the frontmatter slug does not win either");

      await rm(path.join(content, "posts", "filename-authority.md"));
      await sleep(120);
    });

    // -----------------------------------------------------------------------
    await test("§10 drafts never leak to public routes", async () => {
      await writeFile(
        path.join(content, "posts", "secret-draft.md"),
        `---\ntitle: Secret Draft\nslug: secret-draft\ndate: 2026-10-08\ndraft: true\n---\n\nClassified draft body.\n`,
      );
      await sleep(150);

      const index = await get("/");
      ok(
        !index.body.includes("Secret Draft"),
        "draft is absent from the index",
      );

      const direct = await get("/posts/secret-draft");
      eq(direct.status, 404, "direct draft URL returns 404");
      ok(
        !direct.body.includes("Classified draft body"),
        "draft body is not disclosed",
      );

      // A forged query parameter must not reveal a draft either.
      const forged = await get("/posts/secret-draft?drafts=1&preview=true");
      eq(
        forged.status,
        404,
        "forged draft query parameters do not reveal the draft",
      );

      const forgedIndex = await get("/?drafts=1");
      ok(
        !forgedIndex.body.includes("Secret Draft"),
        "forged index query does not reveal drafts",
      );

      await rm(path.join(content, "posts", "secret-draft.md"));
      await sleep(120);
    });

    // -----------------------------------------------------------------------
    await test("§9 reserved slugs cannot shadow framework routes", async () => {
      await writeFile(
        path.join(content, "pages", "admin.md"),
        `---\ntitle: Impostor\nslug: admin\ndraft: false\n---\n\nShould never render.\n`,
      );
      await sleep(150);

      const r = await get("/admin");
      ok(
        r.status !== 200 || !r.body.includes("Should never render"),
        "/admin is not shadowed by content",
      );

      await rm(path.join(content, "pages", "admin.md"));
      await sleep(120);
    });

    // -----------------------------------------------------------------------
    await test("§16/§17 custom CSS and JS are external same-origin resources", async () => {
      const css = await get("/custom.css");
      eq(css.status, 200, "GET /custom.css returns 200");
      ok(
        (css.headers.get("content-type") ?? "").startsWith("text/css"),
        `custom.css is text/css (got ${css.headers.get("content-type")})`,
      );
      ok(
        css.body.includes("rebeccapurple"),
        "custom.css serves the file from content/system/",
      );

      const js = await get("/custom.js");
      eq(js.status, 200, "GET /custom.js returns 200");
      ok(
        (js.headers.get("content-type") ?? "").startsWith("text/javascript"),
        `custom.js is text/javascript (got ${js.headers.get("content-type")})`,
      );
      ok(
        js.body.includes("__customJsLoaded"),
        "custom.js serves the file from content/system/",
      );

      const index = await get("/");
      ok(
        index.body.includes('href="/custom.css"'),
        "layout links custom.css externally",
      );
      ok(
        index.body.includes('src="/custom.js"'),
        "layout loads custom.js externally",
      );
      ok(
        !/<style[^>]*>\s*\.mark\s*\{/.test(index.body),
        "custom CSS is NOT inlined into a <style> block",
      );

      // Editing custom code must be visible without a rebuild.
      await writeFile(
        path.join(content, "system", "custom.css"),
        `.edited { color: teal; }\n`,
      );
      await sleep(120);
      const edited = await get("/custom.css");
      ok(
        edited.body.includes("teal"),
        "edited custom.css is served immediately",
      );

      // Raw HTML in a post body must not become live markup.
      await writeFile(
        path.join(content, "posts", "raw-html.md"),
        `---\ntitle: Raw HTML\nslug: raw-html\ndate: 2026-10-09\ndraft: false\n---\n\nBefore <script>alert(1)</script> after.\n\nInline: <img src=x onerror=alert(2)>\n`,
      );
      await sleep(150);
      const raw = await get("/posts/raw-html");
      eq(raw.status, 200, "the raw-HTML post still renders");
      ok(
        !/<script>alert\(1\)<\/script>/.test(raw.body),
        "raw <script> in a post is neutralised",
      );
      // The text `onerror=...` may legitimately survive as escaped text; what
      // matters is that it is never a live attribute on a real element.
      ok(
        !/<img[^>]*onerror/i.test(raw.body),
        "raw inline event handler is neutralised (no live attribute)",
      );
      ok(
        raw.body.includes("&lt;img"),
        "the raw <img> is rendered as escaped text",
      );

      await rm(path.join(content, "posts", "raw-html.md"));
      await sleep(120);
    });

    // -----------------------------------------------------------------------
    // ID-33 / ID-34 — managed custom assets, aggregated per request.
    //
    // The files here are created directly on disk rather than through the API, and
    // that is the point: this suite runs Astro with no backend at all (API_BASE points
    // at a closed port). The aggregator must answer without one, so writing files by
    // hand is a supported way to use this feature and not a test-only shortcut. If the
    // aggregator ever grows a dependency on Go, the first assertion here fails and
    // says why.
    await test("ID-33 managed CSS assets aggregate in filename order", async () => {
      const cssDir = path.join(content, "system", "css");
      await mkdir(cssDir, { recursive: true });

      // Written in an order that is deliberately not the load order.
      await writeFile(path.join(cssDir, "020-components.css"), ".twenty{}\n");
      await writeFile(path.join(cssDir, "001-base.css"), ".one{}\n");
      await writeFile(path.join(cssDir, "010-layout.css"), ".ten{}\n");
      await sleep(150);

      const css = await get("/custom.css");
      eq(css.status, 200, "GET /custom.css returns 200");
      ok(
        (css.headers.get("content-type") ?? "").startsWith("text/css"),
        `the aggregate is still text/css (got ${css.headers.get("content-type")})`,
      );

      const at = (name) => {
        const i = css.body.indexOf(`blogcms:css:${name}`);
        ok(i !== -1, `${name} is in the aggregate`);
        return i;
      };
      const legacy = css.body.indexOf(".edited { color: teal; }");
      ok(legacy !== -1, "the legacy custom.css is still in the aggregate");
      const one = at("001-base.css");
      const ten = at("010-layout.css");
      const twenty = at("020-components.css");
      ok(legacy < one, "the legacy file loads before every managed asset");
      ok(one < ten && ten < twenty, "001 -> 010 -> 020, in that order");

      // §105: a public response must not disclose the content root or its layout.
      ok(
        !css.body.includes(content) && !css.body.includes("content/system"),
        "the response names no filesystem path",
      );
      ok(
        !/^\s*<(!doctype|html|div|body)/im.test(css.body),
        "the aggregate is not HTML, whatever it contains",
      );

      // §112: adding a file is live on the next request. No build step exists.
      await writeFile(path.join(cssDir, "040-late.css"), ".forty{}\n");
      await sleep(150);
      ok(
        (await get("/custom.css")).body.includes(".forty{}"),
        "a new file is served immediately",
      );

      // §70: so is removing one.
      await rm(path.join(cssDir, "020-components.css"));
      await sleep(150);
      const removed = await get("/custom.css");
      ok(
        !removed.body.includes(".twenty{}"),
        "a deleted file stops being served immediately",
      );
      ok(
        removed.body.includes(".ten{}") && removed.body.includes(".forty{}"),
        "its neighbours keep being served",
      );

      // §38: two files with identical content are two assets. Collapsing them would
      // change how many times a rule applies, which is the author's decision and not
      // an optimisation the CMS is entitled to make silently.
      await writeFile(
        path.join(cssDir, "050-same.css"),
        ".same{color:#010101}\n",
      );
      await writeFile(
        path.join(cssDir, "051-same.css"),
        ".same{color:#010101}\n",
      );
      await sleep(150);
      const dupes = await get("/custom.css");
      eq(
        dupes.body.split(".same{color:#010101}").length - 1,
        2,
        "identical content in two files is served twice, not deduplicated",
      );

      await rm(cssDir, { recursive: true, force: true });
      await sleep(150);
    });

    // -----------------------------------------------------------------------
    await test("ID-34 a disabled asset is not served, and one bad file is skipped", async () => {
      const cssDir = path.join(content, "system", "css");
      const parkedDir = path.join(content, "system", "parked", "css");
      await mkdir(cssDir, { recursive: true });
      await mkdir(parkedDir, { recursive: true });

      await writeFile(path.join(cssDir, "001-base.css"), ".one{}\n");
      await writeFile(path.join(cssDir, "010-layout.css"), ".ten{}\n");
      // Enabled is a location: this file is written where the aggregator does not look.
      await writeFile(path.join(parkedDir, "030-parked.css"), ".parked{}\n");
      await sleep(150);
      const parked = await get("/custom.css");
      ok(
        !parked.body.includes(".parked{}") &&
          !parked.body.includes("030-parked.css"),
        "a parked asset is not served",
      );
      ok(parked.body.includes(".ten{}"), "and the enabled ones still are");

      // Every filename shape the brief lists, plus a size, a subdirectory and the
      // temp file a crashed atomic write leaves behind.
      await writeFile(path.join(cssDir, "evil.css"), ".evil{}\n");
      await writeFile(path.join(cssDir, "011-bad name.css"), ".spaced{}\n");
      await writeFile(path.join(cssDir, "012-UPPER.css"), ".upper{}\n");
      await writeFile(path.join(cssDir, "foo.js.css"), ".doubleext{}\n");
      await writeFile(
        path.join(cssDir, "040-huge.css"),
        "a".repeat(512 * 1024 + 1),
      );
      await writeFile(path.join(cssDir, ".tmp-crashed.md"), ".crash{}\n");
      await mkdir(path.join(cssDir, "nested"), { recursive: true });
      await sleep(150);

      const css = await get("/custom.css");
      eq(
        css.status,
        200,
        "/custom.css still returns 200 with unusable files present",
      );
      ok(css.body.includes(".one{}"), "the good file before them still loads");
      ok(css.body.includes(".ten{}"), "the good file after them still loads");
      for (const marker of [
        ".evil{}",
        ".spaced{}",
        ".upper{}",
        ".doubleext{}",
        ".crash{}",
      ]) {
        ok(!css.body.includes(marker), `${marker} is not served`);
      }
      ok(!css.body.includes("a".repeat(64)), "an oversized file is not served");
      ok(
        logText().includes("custom_asset_skipped"),
        "each skip is logged, not swallowed",
      );
      ok(
        !logText().includes(`"${content}"`),
        "the skip log does not print the content root",
      );

      await rm(cssDir, { recursive: true, force: true });
      await rm(path.join(content, "system", "parked"), {
        recursive: true,
        force: true,
      });
      await sleep(150);
    });

    // -----------------------------------------------------------------------
    await test("ID-34 custom JS assets concatenate without eating each other", async () => {
      const jsDir = path.join(content, "system", "js");
      await mkdir(jsDir, { recursive: true });

      // The first file deliberately ends in a line comment with no trailing newline.
      // Concatenating without a separator would swallow every file after it, and the
      // symptom would be missing code with no error anywhere — so the separator is
      // load-bearing, not decoration.
      await writeFile(
        path.join(jsDir, "001-line-comment.js"),
        "window.__a = 1; // trailing",
      );
      await writeFile(path.join(jsDir, "002-second.js"), "window.__b = 2;\n");
      await writeFile(path.join(jsDir, "100-last.js"), "window.__c = 3;\n");
      await sleep(150);

      const js = await get("/custom.js");
      eq(js.status, 200, "GET /custom.js returns 200");
      ok(
        (js.headers.get("content-type") ?? "").startsWith("text/javascript"),
        `the aggregate is still text/javascript (got ${js.headers.get("content-type")})`,
      );
      ok(
        js.body.includes("__customJsLoaded"),
        "the legacy custom.js is still in the aggregate",
      );
      const a = js.body.indexOf("__a = 1");
      const b = js.body.indexOf("__b = 2");
      const c = js.body.indexOf("__c = 3");
      ok(a !== -1 && b !== -1 && c !== -1, "every enabled asset is present");
      ok(a < b && b < c, "001 -> 002 -> 100, in that order");
      ok(
        !js.body.includes(content) && !js.body.includes("content/system"),
        "the response names no filesystem path",
      );

      await mkdir(path.join(content, "system", "parked", "js"), {
        recursive: true,
      });
      await writeFile(
        path.join(content, "system", "parked", "js", "050-off.js"),
        "window.__off = 1;\n",
      );
      await sleep(150);
      ok(
        !(await get("/custom.js")).body.includes("__off"),
        "a parked JS asset is not served",
      );

      await rm(jsDir, { recursive: true, force: true });
      await rm(path.join(content, "system", "parked"), {
        recursive: true,
        force: true,
      });
      await sleep(150);
      const back = await get("/custom.js");
      eq(
        back.status,
        200,
        "/custom.js returns 200 again once the assets are gone",
      );
      ok(
        !back.body.includes("blogcms:js:"),
        "the aggregate is back to the legacy file alone",
      );
    });

    // -----------------------------------------------------------------------
    await test("ID-34 the aggregate ETag follows the bytes it serves", async () => {
      const first = await get("/custom.css");
      const second = await get("/custom.css");
      eq(
        first.headers.get("etag"),
        second.headers.get("etag"),
        "two requests for unchanged content agree on the ETag",
      );
      ok(
        (first.headers.get("cache-control") ?? "").includes("no-cache"),
        "the aggregate is revalidated rather than stored forever",
      );

      const conditional = await fetch(`${BASE}/custom.css`, {
        headers: { "If-None-Match": first.headers.get("etag") ?? "" },
      });
      eq(conditional.status, 304, "a matching If-None-Match gets a 304");

      const cssDir = path.join(content, "system", "css");
      await mkdir(cssDir, { recursive: true });
      await writeFile(
        path.join(cssDir, "001-base.css"),
        ".one{color:#020202}\n",
      );
      await sleep(150);
      const changed = await get("/custom.css");
      ok(
        changed.headers.get("etag") !== first.headers.get("etag"),
        "editing one managed file changes the aggregate ETag",
      );
      ok(
        changed.body.includes("#020202"),
        "and its new text is what is served",
      );

      const stale = await fetch(`${BASE}/custom.css`, {
        headers: { "If-None-Match": first.headers.get("etag") ?? "" },
      });
      eq(
        stale.status,
        200,
        "the superseded ETag gets a fresh 200 rather than a 304",
      );

      await rm(cssDir, { recursive: true, force: true });
      await sleep(150);
    });

    // -----------------------------------------------------------------------
    // §34 — the Markdown style templates aggregate at request time, with
    // exactly the properties the custom-asset aggregator has: filename
    // order, location-as-enabled-state, skip-don't-crash on a bad file,
    // and an ETag that follows the bytes.
    await test("§34 markdown style templates aggregate at request time", async () => {
      const dir = path.join(content, "system", "markdown");
      await mkdir(dir, { recursive: true });
      await writeFile(
        path.join(dir, "001-base.css"),
        ".markdown-body p{color:#101010}\n",
      );
      await writeFile(
        path.join(dir, "020-code.css"),
        ".markdown-body pre{background:#f5f5f5}\n",
      );
      await sleep(150);

      const first = await get("/markdown.css");
      eq(first.status, 200, "GET /markdown.css returns 200");
      ok(
        (first.headers.get("content-type") ?? "").includes("text/css"),
        "the aggregate is served as CSS",
      );
      const firstOrder = [
        ...first.body.matchAll(/blogcms:markdown:([^*]+)/g),
      ].map((m) => m[1].trim());
      eq(
        firstOrder.join(","),
        "001-base.css,020-code.css",
        "templates are concatenated in filename order",
      );
      ok(
        first.body.includes("#101010") && first.body.includes("#f5f5f5"),
        "both template bodies are present",
      );

      // A new file appears with no rebuild.
      await writeFile(
        path.join(dir, "010-middle.css"),
        ".markdown-body h1{letter-spacing:-0.02em}\n",
      );
      await sleep(150);
      const withMiddle = await get("/markdown.css");
      const middleOrder = [
        ...withMiddle.body.matchAll(/blogcms:markdown:([^*]+)/g),
      ].map((m) => m[1].trim());
      eq(
        middleOrder.join(","),
        "001-base.css,010-middle.css,020-code.css",
        "a newly created template is served immediately, in order",
      );

      // An edit is live immediately, and the ETag follows the bytes.
      const etagBefore = withMiddle.headers.get("etag");
      await writeFile(
        path.join(dir, "020-code.css"),
        ".markdown-body pre{background:#eeeeee}\n",
      );
      await sleep(150);
      const edited = await get("/markdown.css");
      ok(
        edited.body.includes("#eeeeee") && !edited.body.includes("#f5f5f5"),
        "an edited template is served with no rebuild",
      );
      ok(
        edited.headers.get("etag") !== etagBefore,
        "editing a template changes the aggregate ETag",
      );

      // "Enabled" is a location: a parked file is not served.
      const parked = path.join(content, "system", "parked", "markdown");
      await mkdir(parked, { recursive: true });
      await rm(path.join(dir, "010-middle.css"));
      await writeFile(
        path.join(parked, "010-middle.css"),
        ".markdown-body h1{display:none}\n",
      );
      await sleep(150);
      const parkedState = await get("/markdown.css");
      ok(
        !parkedState.body.includes("display:none"),
        "a parked (disabled) template is excluded from the aggregate",
      );
      const parkedOrder = [
        ...parkedState.body.matchAll(/blogcms:markdown:([^*]+)/g),
      ].map((m) => m[1].trim());
      eq(
        parkedOrder.join(","),
        "001-base.css,020-code.css",
        "the aggregate order is unaffected by the parked file",
      );

      // One bad file is skipped, not fatal.
      await writeFile(
        path.join(dir, "not-a-template.css"),
        ".markdown-body{color:red}\n",
      );
      await writeFile(
        path.join(dir, "030-traversal.css"),
        ".markdown-body{color:blue}\n",
      );
      await sleep(150);
      const withBad = await get("/markdown.css");
      eq(
        withBad.status,
        200,
        "a file outside the grammar does not fail the endpoint",
      );
      ok(
        !withBad.body.includes("color:red"),
        "a file outside the filename grammar is skipped",
      );
      ok(
        withBad.body.includes("color:blue"),
        "a valid template next to a bad one is still served",
      );

      // Conditional requests work against the aggregate.
      const fresh = await fetch(`${BASE}/markdown.css`, {
        headers: { "If-None-Match": withBad.headers.get("etag") ?? "" },
      });
      eq(fresh.status, 304, "a matching If-None-Match gets a 304");

      // A delete is live immediately.
      await rm(path.join(dir, "030-traversal.css"));
      await rm(path.join(dir, "not-a-template.css"));
      await sleep(150);
      const afterDelete = await get("/markdown.css");
      ok(
        !afterDelete.body.includes("color:blue"),
        "a deleted template disappears with no rebuild",
      );

      await rm(path.join(content, "system", "parked"), {
        recursive: true,
        force: true,
      });
      await rm(dir, { recursive: true, force: true });
      await sleep(150);
    });

    // -----------------------------------------------------------------------
    // §34 — the semantic Markdown directives render as the stable class
    // contract the templates in content/system/markdown/ style.
    await test("§34 semantic Markdown directives render as stable classes", async () => {
      await writeFile(
        path.join(content, "posts", "directives.md"),
        [
          "---",
          "title: Directives",
          "slug: directives",
          "date: 2026-10-12",
          "draft: false",
          "---",
          "",
          ":::info",
          "An info note.",
          ":::",
          "",
          ":::warning",
          "A warning note.",
          ":::",
          "",
          ":::danger",
          "A danger note.",
          ":::",
          "",
          ":::card",
          "A card.",
          ":::",
          "",
          "Press :kbd[Ctrl] and :badge[New].",
          "",
          ":::figure",
          "![a pixel](data:image/svg+xml;utf8,%3Csvg xmlns='http://www.w3.org/2000/svg' width='1' height='1'/%3E)",
          "",
          "The caption.",
          ":::",
          "",
          "```go",
          "func main() {}",
          "```",
          "",
          "Inline `code` here.",
          "",
          "> A quote.",
          "",
          "| a | b |",
          "|---|---|",
          "| 1 | 2 |",
          "",
        ].join("\n"),
      );
      await sleep(150);
      const r = await get("/posts/directives");
      eq(r.status, 200, "a post of directives renders");
      ok(
        r.body.includes('class="markdown-body"'),
        "the body is wrapped in the template scope",
      );
      ok(
        r.body.includes("callout callout-info"),
        ":::info renders as a callout",
      );
      ok(
        r.body.includes("callout callout-warning"),
        ":::warning renders as a callout",
      );
      ok(
        r.body.includes("callout callout-danger"),
        ":::danger renders as a callout",
      );
      ok(r.body.includes('class="card"'), ":::card renders as a card");
      ok(
        r.body.includes('<kbd class="kbd">Ctrl</kbd>'),
        ":kbd[Ctrl] renders as a kbd",
      );
      ok(
        r.body.includes('<span class="badge">New</span>'),
        ":badge[New] renders as a badge",
      );
      ok(
        r.body.includes("<figure") && r.body.includes('class="figure"'),
        ":::figure renders as a real figure element",
      );
      ok(
        r.body.includes("<figcaption") && r.body.includes('class="caption"'),
        "the figure's last paragraph becomes a caption",
      );
      ok(
        r.body.includes('class="code-block"'),
        "a fenced code block carries the code-block class",
      );
      ok(
        !r.body.includes(":kbd[") && !r.body.includes(":badge["),
        "no directive syntax survives into the HTML",
      );
      await rm(path.join(content, "posts", "directives.md"));
      await sleep(120);
    });

    // -----------------------------------------------------------------------
    // The two size ceilings are duplicated in Go and TypeScript because neither can
    // read the other's constant. This is the cross-check, and it exists because the
    // alternative — two constants nobody compares — is how a save starts failing with
    // a 413 that no editor explains.
    await test("the Go and TypeScript size ceilings agree", async () => {
      const go = readFileSync(
        path.join(ROOT, "backend", "internal", "content", "content.go"),
        "utf-8",
      );
      const ts = readFileSync(
        path.join(ASTRO, "src", "lib", "system.ts"),
        "utf-8",
      );
      const goBytes = Number(go.match(/BodyMaxBytes\s+= (\d+) \* 1024/)?.[1]);
      const tsBytes = Number(
        ts.match(/CUSTOM_ASSET_MAX_BYTES = (\d+) \* 1024/)?.[1],
      );
      ok(goBytes > 0, "the Go ceiling is a byte count in KiB");
      eq(tsBytes, goBytes, "both runtimes cap a custom asset at the same size");
    });

    // -----------------------------------------------------------------------
    await test("pages, 404 and RSS", async () => {
      const about = await get("/about");
      eq(about.status, 200, "GET /about returns 200");
      ok(about.body.includes("About this blog."), "page body rendered");
      ok(about.body.includes(">About<"), "page appears in navigation");

      const missing = await get("/definitely-not-here");
      eq(missing.status, 404, "unknown slug returns 404");

      const rss = await get("/rss.xml");
      eq(rss.status, 200, "GET /rss.xml returns 200");
      ok(
        (rss.headers.get("content-type") ?? "").includes("xml"),
        "RSS has an XML content type",
      );
      ok(
        rss.body.includes('<rss version="2.0"'),
        "RSS is a valid RSS 2.0 document",
      );
      ok(
        rss.body.includes("Hello World EDITED"),
        "RSS lists the current post title",
      );

      // The hello post was edited earlier and its tags removed, so re-add one.
      await writeFile(
        path.join(content, "posts", "tagged.md"),
        `---\ntitle: Tagged Post\nslug: tagged\ndate: 2026-10-11\ntags:\n  - golang\ndraft: false\n---\n\nTagged body.\n`,
      );
      await sleep(150);
      const tagPage = await get("/tags/golang");
      eq(tagPage.status, 200, "a tag with posts returns 200");
      ok(tagPage.body.includes("Tagged Post"), "the tagged post is listed");

      const badTag = await get("/tags/NOT-A-TAG");
      eq(badTag.status, 404, "a malformed tag returns 404 rather than a query");

      await rm(path.join(content, "posts", "tagged.md"));
      await sleep(120);
    });

    // -----------------------------------------------------------------------
    await test("Markdown features and code fences", async () => {
      await writeFile(
        path.join(content, "posts", "features.md"),
        `---\ntitle: Features\nslug: features\ndate: 2026-10-10\ndraft: false\n---\n\n## Heading two\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n- [x] done\n- [ ] todo\n\n~~struck~~\n\n\`\`\`go\nfunc main() {}\n\`\`\`\n\n\`\`\`html\n<script>this stays escaped</script>\n\`\`\`\n`,
      );
      await sleep(150);
      const r = await get("/posts/features");
      eq(r.status, 200, "post with tables/lists/code renders");
      ok(r.body.includes("<table"), "GFM tables rendered");
      ok(r.body.includes('id="heading-two"'), "heading IDs generated");
      ok(r.body.includes('type="checkbox"'), "task lists rendered");
      ok(r.body.includes("del>struck"), "strikethrough rendered");
      ok(
        r.body.includes("&lt;") && !r.body.includes(">this stays escaped<"),
        "HTML inside a code fence is escaped rather than emitted verbatim",
      );
      ok(
        !r.body.includes("<script>this stays escaped"),
        "fenced HTML is never emitted as live markup",
      );
      // ID-21: the highlighter is off (`syntaxHighlight: false`), so a fence is a plain
      // <pre><code class="language-…"> for the theme to style. Prism's github-dark
      // theme emitted one inline `style` per token — a page that returned 200 and then
      // logged a dozen-and-a-half CSP violations under `style-src 'self'`, with every
      // colour the highlighter had computed already committed to the browser.
      // The `code-block` class on the <pre> is the template system's hook
      // (§34): it lets a template target the block without catching an
      // inline <code>.
      ok(
        /<pre class="code-block"><code class="language-go">/.test(r.body),
        "a code fence renders as a plain, theme-styleable block",
      );
      ok(!r.body.includes('style="'), "no inline style in the rendered post");
      await rm(path.join(content, "posts", "features.md"));
      await sleep(120);
    });
  } finally {
    await stopServer(server);
  }

  // -------------------------------------------------------------------------
  // §6 — a broken CONTENT_ROOT must fail loudly, not serve an empty blog.
  // -------------------------------------------------------------------------
  await test("§6 a broken CONTENT_ROOT fails loudly", async () => {
    const missingRoot = path.join(tmpdir(), "blogcms-does-not-exist-xyz");
    let child;
    let res;
    try {
      // A broken CONTENT_ROOT would also stop the Go backend from
      // starting, and this check is about Astro's own loud failure, so
      // no backend is started here.
      child = await startServer(missingRoot, { backend: false });
      res = await get("/");
    } catch (err) {
      // The server refusing to serve at all is also an acceptable loud failure.
      ok(
        String(err).includes("did not become ready"),
        "server refuses to serve with a missing CONTENT_ROOT",
      );
      child = undefined;
    }
    if (child) {
      ok(
        res.status >= 500,
        `GET / returns a server error, not an empty blog (got ${res.status})`,
      );
      await stopServer(child);
    }

    // Unset CONTENT_ROOT entirely must also be loud.
    const noEnvPort = await freePort();
    const noEnv = spawn(process.execPath, [ENTRY], {
      cwd: ASTRO,
      env: {
        PATH: process.env.PATH,
        HOST: "127.0.0.1",
        PORT: String(noEnvPort),
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    const out = [];
    noEnv.stdout.on("data", (d) => out.push(d.toString()));
    noEnv.stderr.on("data", (d) => out.push(d.toString()));
    // The root is resolved lazily at request time, so a request is needed to
    // trigger the failure.
    await sleep(600);
    try {
      await fetch(`http://127.0.0.1:${noEnvPort}/`);
    } catch {
      /* a refused connection is also a loud failure */
    }
    await sleep(600);
    ok(
      out.join("").includes("CONTENT_ROOT"),
      `an unset CONTENT_ROOT produces an explicit error message (got: ${out.join("").slice(0, 200) || "<no output>"})`,
    );
    noEnv.kill("SIGKILL");
  });

  await rm(content, { recursive: true, force: true });

  console.log(`\n${"=".repeat(60)}`);
  console.log(`INTEGRATION: ${passed} passed, ${failed} failed`);
  if (failures.length > 0) {
    console.error("\nFailures:");
    for (const f of failures) console.error(`  - ${f}`);
  }
  console.log("=".repeat(60));
  process.exit(failed === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
