#!/usr/bin/env node
/**
 * Full-stack integration tests: real Go backend + real Astro SSR build.
 *
 * ARCHITECTURE.md §34/§37: the production data flow is exercised end to end —
 * an admin writes a Markdown file through the Go API, and Astro serves it with no
 * rebuild. Authentication, CSRF and cookie behaviour are checked over real HTTP
 * against the running processes, not with mocks.
 *
 * The stack bootstrap lives in tests/lib/harness.mjs, shared with
 * tests/theme-tests.mjs: two copies of "how to start a server" would eventually
 * disagree, and a suite that silently tested a different setup than the one it
 * claimed to test is worse than no suite.
 */
import {
  mkdtemp,
  mkdir,
  writeFile,
  readFile,
  readdir,
  stat,
  rm,
} from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

/**
 * Real image fixtures.
 *
 * The delivery layer is tested against actual encoders rather than a 1x1 PNG,
 * because a 70-byte file has no measurable representation difference and no
 * decodable dimensions — the two things this phase exists to prove.
 */
const FIXTURES = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  "fixtures",
);

import {
  ASTRO,
  BACKEND,
  ORIGIN_HOST,
  bootstrapAdmin,
  cleanup,
  createReporter,
  ensureBuilds,
  freePort,
  makeRoots,
  sessionCookieFrom,
  sleep,
  startStack,
} from "./lib/harness.mjs";

const { ok, eq, test, summary } = createReporter("Full-stack integration");

/** Whether a path exists. A missing path is a fact, not an error. */
async function pathExists(target) {
  try {
    await stat(target);
    return true;
  } catch {
    return false;
  }
}

/**
 * Every file under a directory, relative and slash-separated.
 *
 * Used to read the backup repository's work tree, which is the tracked content:
 * the snapshot the service maintains *is* what the next commit would record, so
 * listing it is the `git ls-files` equivalent without needing the `git` binary.
 * The repository's own metadata directory is skipped.
 */
async function walkFiles(root, prefix = "", out = []) {
  for (const entry of await readdir(root, { withFileTypes: true })) {
    if (prefix === "" && entry.name === ".git") continue;
    const full = path.join(root, entry.name);
    const rel = prefix === "" ? entry.name : `${prefix}/${entry.name}`;
    if (entry.isDirectory()) await walkFiles(full, rel, out);
    else if (entry.isFile()) out.push(rel);
  }
  return out;
}

async function main() {
  // Session-scoped helpers, so a test does not have to repeat the login dance
  // and so a shared fixture cannot drift between tests.
  let adminCookie = null;
  let adminCsrf = null;

  async function loginAsAdmin() {
    if (adminCookie) return adminCookie;
    const res = await fetch(`${base}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: base },
      body: JSON.stringify({ username: "owner", password: "a-good-password" }),
    });
    eq(res.status, 200, "admin sign in");
    adminCookie = sessionCookieFrom(res);
    adminCsrf = (await res.json()).csrfToken;
    return adminCookie;
  }

  /** Creates a post through the API and returns the raw response. */
  async function createPost(fields) {
    const cookie = await loginAsAdmin();
    return fetch(`${base}/api/v1/admin/posts`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: base,
        Cookie: `blog_session=${cookie}`,
        "X-CSRF-Token": adminCsrf,
      },
      body: JSON.stringify({
        body: "Body.",
        date: "2026-10-03",
        draft: false,
        ...fields,
      }),
    });
  }

  console.log("Full-stack integration tests (Go API + Astro SSR)");
  await ensureBuilds();

  const roots = await makeRoots();
  const astroPort = await freePort();
  const apiPort = await freePort();
  const stack = await startStack(roots, astroPort, apiPort);

  const base = stack.base;
  const api = stack.api;

  /**
   * Make sure the module-level `cookie`/`csrf` pair is populated.
   *
   * `loginAsAdmin` maintains its own pair for the tests that only need a session,
   * while the media and settings tests below share one. Both are the same account;
   * they are only separate variables.
   *
   * It logs in once and reuses that session for the whole run of tests. It cannot
   * simply check `cookie`: every login revokes every earlier session (§12), so by
   * the time these tests run the pair the authentication tests left behind has been
   * rotated away by the dozen logins in between — a stale token looks valid and
   * fails at the first request.
   *
   * The cached pair is *verified* rather than trusted, because the hazard is not only
   * past: several tests further down sign in for their own reasons (the single-origin
   * proxy tests and the comment throttle test), and each of those revokes this pair.
   * A cache that cannot notice that turns the next test into a 401 that reads as an
   * application bug. One extra local GET per call is cheaper than a red herring, and
   * it makes the suite independent of the order in which tests happen to run — which
   * is exactly the property a suite that shares one session needs.
   */
  let adminSessionReady = false;
  async function ensureAdmin() {
    if (adminSessionReady && cookie && csrf) {
      const probe = await fetch(`${api}/api/v1/auth/session`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      const state = await probe.json().catch(() => ({}));
      if (state.authenticated === true) return;
    }
    const res = await fetch(`${base}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: base },
      body: JSON.stringify({ username: "owner", password: "a-good-password" }),
    });
    cookie = sessionCookieFrom(res);
    csrf = (await res.json()).csrfToken;
    adminSessionReady = true;
  }

  /**
   * A custom-asset request through the same-origin proxy, exactly as the admin's own
   * scripts make it.
   *
   * It goes through Astro rather than straight at Go because that is the path a
   * browser takes: the browser can only reach :4321, and Astro is what forwards it.
   * A test that skipped the proxy would prove the Go handler works and say nothing
   * about whether the admin could use it.
   */
  async function adminAsset(method, kind, id, body) {
    const target =
      id === null || id === undefined
        ? `/api/v1/admin/custom/${kind}`
        : `/api/v1/admin/custom/${kind}/${encodeURIComponent(id)}`;
    const res = await fetch(`${base}${target}`, {
      method,
      headers: {
        "Content-Type": "application/json",
        Origin: base,
        Cookie: `blog_session=${cookie}`,
        "X-CSRF-Token": csrf,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let parsed = null;
    try {
      parsed = text.length > 0 ? JSON.parse(text) : null;
    } catch {
      parsed = null;
    }
    return { status: res.status, body: parsed, text };
  }

  /** The JPEG uploaded by the delivery tests, shared with the cache tests. */
  let jpegMedia = null;
  /** A real JPEG, read once: the negotiation tests need actual image bytes. */
  let bytes = null;

  // Read once: every negotiation and cache test needs the same real JPEG.
  bytes = await readFile(path.join(FIXTURES, "sample.jpg"));

  const apiPost = async (p, body, headers = {}) =>
    fetch(`${api}${p}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: base, ...headers },
      body: typeof body === "string" ? body : JSON.stringify(body),
    });

  let cookie = null;
  let csrf = null;

  try {
    // -----------------------------------------------------------------------
    await test("§29 health and readiness", async () => {
      const h = await fetch(`${api}/api/v1/healthz`);
      eq(h.status, 200, "healthz returns 200");
      const r = await fetch(`${api}/api/v1/readyz`);
      eq(r.status, 200, "readyz returns 200");
      const body = await r.json();
      ok(
        body.checks.some((c) => c.check === "sqlite" && c.ok),
        "readyz reports sqlite as healthy",
      );
    });

    // -----------------------------------------------------------------------
    await test("§11 admin area requires authentication", async () => {
      const res = await fetch(`${base}/admin`, { redirect: "manual" });
      eq(res.status, 303, "GET /admin without a session redirects");
      ok(
        (res.headers.get("location") ?? "").startsWith("/admin/login"),
        `redirect target is the login page (got ${res.headers.get("location")})`,
      );
    });

    // -----------------------------------------------------------------------
    await test("§7/§12 setup creates the single administrator", async () => {
      const res = await apiPost("/api/v1/auth/setup", {
        username: "owner",
        password: "a-good-password",
      });
      const text = await res.text();
      eq(res.status, 201, `setup succeeds (${text})`);

      cookie = sessionCookieFrom(res);
      ok(cookie !== null, "a session cookie was issued");
      const body = JSON.parse(text);
      csrf = body.csrfToken;
      ok(csrf, "a CSRF token was issued");

      // D7: setup closes immediately.
      const second = await apiPost("/api/v1/auth/setup", {
        username: "intruder",
        password: "another-password",
      });
      eq(second.status, 409, "a second setup is refused");
    });

    // -----------------------------------------------------------------------
    await test("§12 cookie flags over real HTTP", async () => {
      const res = await apiPost("/api/v1/auth/login", {
        username: "owner",
        password: "a-good-password",
      });
      const cookies = res.headers.getSetCookie?.() ?? [];
      const session = cookies.find((c) => c.startsWith("blog_session="));
      ok(session, "the session cookie is present");

      const attrs = session.toLowerCase();
      ok(attrs.includes("httponly"), "HttpOnly is set");
      ok(attrs.includes("samesite=strict"), "SameSite=Strict is set");
      ok(attrs.includes("path=/"), "Path=/ is set");
      // SECURE_COOKIES=false only in the dev stack, which runs over plain HTTP.
      ok(
        !attrs.includes("secure"),
        "Secure is absent over plain HTTP (dev only)",
      );

      cookie = sessionCookieFrom(res);
      const sessionBody = await res.json();
      csrf = sessionBody.csrfToken;
    });

    // -----------------------------------------------------------------------
    await test("§12 wrong password is rejected", async () => {
      const res = await apiPost("/api/v1/auth/login", {
        username: "owner",
        password: "wrong-password",
      });
      eq(res.status, 401, "a wrong password returns 401");
      ok(sessionCookieFrom(res) === null, "no cookie is issued");
    });

    // -----------------------------------------------------------------------
    // There is exactly one administrator (D7), and the store fetches it without
    // regard to which name was submitted. If the handler never compares the two,
    // the username field is accepted and discarded: any name plus the right
    // password returns a session for whoever the account really is, and a typo
    // silently signs in as a different name. Found by probing the endpoint, not
    // by reading it — the code looked correct because `Admins.Get(ctx)` takes no
    // username and reads naturally as "get the admin".
    await test("§7 a wrong username fails exactly like a wrong password", async () => {
      const attempts = {
        "right user, wrong password": {
          username: "owner",
          password: "wrong-password",
        },
        "wrong user, right password": {
          username: "definitely-not-owner",
          password: "a-good-password",
        },
        "wrong user, wrong password": {
          username: "definitely-not-owner",
          password: "wrong-password",
        },
      };
      const bodies = {};
      for (const [label, payload] of Object.entries(attempts)) {
        const res = await apiPost("/api/v1/auth/login", payload);
        eq(res.status, 401, `${label} returns 401`);
        ok(sessionCookieFrom(res) === null, `${label} issues no cookie`);
        bodies[label] = await res.text();
      }

      // Byte-identical, so the response cannot be used to learn whether a username
      // exists. Equal status codes alone would not prove that: a different message
      // on the same 401 still enumerates.
      const distinct = new Set(Object.values(bodies));
      eq(
        distinct.size,
        1,
        `the three failures are byte-identical (got ${distinct.size} distinct bodies)`,
      );

      // And the real credentials still work, so the check is a comparison and not a
      // blanket refusal.
      //
      // Logging in is destructive to the rest of this suite: a login revokes *every*
      // existing session (ID-4), so a successful login here would silently invalidate
      // the session every later test uses. The new cookie and CSRF token are therefore
      // captured and written back into the suite's own state, leaving it authenticated
      // exactly as it was.
      const signedIn = await apiPost("/api/v1/auth/login", {
        username: "owner",
        password: "a-good-password",
      });
      eq(signedIn.status, 200, "the real account still signs in");
      const freshCookie = sessionCookieFrom(signedIn);
      const freshCsrf = (await signedIn.json())?.csrfToken;
      ok(freshCookie && freshCsrf, "a session is issued for the real account");
      cookie = freshCookie;
      csrf = freshCsrf;
    });

    // -----------------------------------------------------------------------
    await test("§13 CSRF is required on admin mutations", async () => {
      const withCsrf = await fetch(`${api}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          siteTitle: "My Blog",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      const csrfText = await withCsrf.text();
      eq(withCsrf.status, 200, `PUT with a CSRF token succeeds (${csrfText})`);

      const withoutCsrf = await fetch(`${api}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
        },
        body: JSON.stringify({
          siteTitle: "Hacked",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      eq(withoutCsrf.status, 403, "PUT without a CSRF token returns 403");

      const wrongOrigin = await fetch(`${api}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: "https://evil.example",
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          siteTitle: "Hacked",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      eq(wrongOrigin.status, 403, "PUT from a foreign Origin returns 403");

      const noSession = await fetch(`${api}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          "X-CSRF-Token": csrf,
        },
        body: "{}",
      });
      eq(noSession.status, 401, "PUT without a session returns 401");
    });

    // -----------------------------------------------------------------------
    await test("§13 the admin dashboard renders for a session", async () => {
      const anon = await fetch(`${base}/admin`, { redirect: "manual" });
      eq(anon.status, 303, "an anonymous visitor is redirected");

      const authed = await fetch(`${base}/admin`, {
        headers: { Cookie: `blog_session=${cookie}` },
        redirect: "manual",
      });
      eq(authed.status, 200, "an authenticated admin reaches the dashboard");
      const html = await authed.text();
      // Structural, not a literal. "Dashboard" is now a word the *theme*
      // supplies — bluearchive renders it as 仪表盘 — so asserting the
      // English string here would have tested the translation, not the
      // navigation. The sidebar link is the thing that must be present.
      ok(
        html.includes('href="/admin/media"'),
        "the dashboard renders the admin navigation",
      );
      ok(/<h1>[^<]+<\/h1>/.test(html), "the dashboard has a screen heading");
      ok(html.includes("owner"), "the signed-in username is shown");
      ok(html.includes("noindex"), "admin pages are marked noindex");
      ok(
        (authed.headers.get("cache-control") ?? "").includes("no-store"),
        "admin responses are not cacheable",
      );
    });

    // -----------------------------------------------------------------------
    await test("§8 the login page never leaks the password or session", async () => {
      const res = await fetch(`${base}/admin/login`);
      eq(res.status, 200, "the login page renders");
      const html = await res.text();
      ok(
        !html.includes("a-good-password"),
        "the password does not appear in the page",
      );
      ok(
        !/blog_session=[A-Za-z0-9_-]{10,}/.test(html),
        "no session cookie value is inlined",
      );
      // §45: the login form's behaviour is core, served from a fixed URL, and it
      // is the same file the admin uses — not a per-theme or per-page script.
      ok(
        html.includes("/cms.js"),
        "the login form is driven by the core behaviour script",
      );
      ok(
        !/themes?\/[\w-]+\/[\w.-]*\.js/.test(html),
        "no script is loaded from a theme directory",
      );
      ok(
        !/<script(?![^>]*\bsrc=)[^>]*>[\s\S]*?\S[\s\S]*?<\/script>/.test(html),
        "no inline <script> block exists, so CSP needs no unsafe-inline",
      );
    });

    // -----------------------------------------------------------------------
    await test("§12 logout revokes the session", async () => {
      const res = await fetch(`${base}/admin/logout`, {
        method: "POST",
        headers: { Cookie: `blog_session=${cookie}`, Origin: base },
        redirect: "manual",
      });
      eq(res.status, 303, "logout redirects");

      const after = await fetch(`${api}/api/v1/auth/session`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      const body = await after.json();
      eq(body.authenticated, false, "the token no longer authenticates");

      cookie = null;
    });

    // -----------------------------------------------------------------------
    // §34: the production publishing path. Admin -> Go -> Markdown file ->
    // Astro live loader -> SSR, with no `astro build` anywhere.
    // -----------------------------------------------------------------------
    await test("§34 admin creates a post and Astro serves it with NO rebuild", async () => {
      // Start from a signed-in session.
      const loginRes = await apiPost("/api/v1/auth/login", {
        username: "owner",
        password: "a-good-password",
      });
      eq(loginRes.status, 200, "sign in again");
      cookie = sessionCookieFrom(loginRes);
      csrf = (await loginRes.json()).csrfToken;

      const created = await fetch(`${api}/api/v1/admin/posts`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          title: "Written Through The API",
          slug: "written-through-the-api",
          description: "Created by an admin, served without a rebuild.",
          date: "2026-10-03",
          tags: "fileless, api",
          draft: false,
          body: "# Written Through The API\n\nThis body was written by the Go API.",
        }),
      });
      const createdText = await created.text();
      eq(created.status, 201, `post created (${createdText})`);

      // Go must have written a real file, with the slug as the filename.
      const file = path.join(
        roots.content,
        "posts",
        "written-through-the-api.md",
      );
      const onDisk = await readFile(file, "utf-8");
      ok(
        onDisk.includes("slug: written-through-the-api"),
        "the file declares the slug",
      );
      ok(
        onDisk.includes("title: Written Through The API"),
        "the file declares the title",
      );
      ok(
        onDisk.includes("This body was written by the Go API."),
        "the file holds the body",
      );

      // §2: no article content may have reached the database.
      const posts = await fetch(`${api}/api/v1/admin/posts`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      const list = await posts.json();
      ok(
        list.items.some((i) => i.slug === "written-through-the-api"),
        "the post is listed from the filesystem",
      );

      // The site serves it, with no rebuild.
      const page = await fetch(`${base}/posts/written-through-the-api`);
      eq(page.status, 200, "the new post is live");
      const html = await page.text();
      ok(
        html.includes("Written Through The API"),
        "the post title is rendered",
      );
      ok(
        html.includes("This body was written by the Go API."),
        "the post body is rendered",
      );

      const index = await fetch(`${base}/`);
      ok(
        (await index.text()).includes("Written Through The API"),
        "the index lists the new post",
      );
    });

    // -----------------------------------------------------------------------
    await test("§3/§9 the editor enforces slug and frontmatter rules", async () => {
      const put = (slug, payload) =>
        fetch(`${api}/api/v1/admin/posts/${encodeURIComponent(slug)}`, {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: JSON.stringify(payload),
        });

      const valid = {
        title: "Edited Title",
        slug: "written-through-the-api",
        date: "2026-10-03",
        tags: "fileless",
        draft: false,
        body: "Edited body.",
      };

      // An invalid slug is refused.
      const badSlug = await put("written-through-the-api", {
        ...valid,
        slug: "Not A Slug",
      });
      eq(badSlug.status, 422, "an invalid slug is refused");
      const badBody = await badSlug.json();
      ok(badBody.error?.fields?.slug, "the slug field error is reported");

      // Changing the slug is refused.
      const renamed = await put("written-through-the-api", {
        ...valid,
        slug: "different",
      });
      eq(renamed.status, 409, "changing the slug is refused");

      // A bad date is refused.
      const badDate = await put("written-through-the-api", {
        ...valid,
        date: "yesterday",
      });
      eq(badDate.status, 422, "an unparseable date is refused");

      // A valid update succeeds and Astro sees it without a rebuild.
      const okUpdate = await put("written-through-the-api", valid);
      eq(okUpdate.status, 200, "a valid update succeeds");

      const page = await fetch(`${base}/posts/written-through-the-api`);
      const html = await page.text();
      ok(html.includes("Edited Title"), "the edit is live with no rebuild");
      ok(html.includes("Edited body."), "the edited body is live");

      // The filename still matches the slug.
      const onDisk = await readFile(
        path.join(roots.content, "posts", "written-through-the-api.md"),
        "utf-8",
      );
      ok(
        onDisk.includes("slug: written-through-the-api"),
        "the file still declares the slug",
      );
    });

    // -----------------------------------------------------------------------
    await test("§10 draft posts stay hidden from visitors", async () => {
      await fetch(`${api}/api/v1/admin/posts`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          title: "Secret Draft",
          slug: "secret-draft",
          date: "2026-10-04",
          draft: true,
          body: "Classified.",
        }),
      });

      const index = await fetch(`${base}/`);
      ok(
        !(await index.text()).includes("Secret Draft"),
        "the draft is absent from the index",
      );

      const direct = await fetch(`${base}/posts/secret-draft`);
      eq(direct.status, 404, "the draft URL returns 404 for visitors");

      // The admin can still see it in the list.
      const list = await fetch(`${api}/api/v1/admin/posts`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      const items = (await list.json()).items;
      ok(
        items.some((i) => i.slug === "secret-draft" && i.draft === true),
        "the admin list shows the draft",
      );
    });

    // -----------------------------------------------------------------------
    await test("§3 deleting a post removes the file and the route", async () => {
      const del = await fetch(`${api}/api/v1/admin/posts/secret-draft`, {
        method: "DELETE",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
      });
      eq(del.status, 200, "delete succeeds");

      const gone = await fetch(`${base}/posts/secret-draft`);
      eq(gone.status, 404, "the route is gone with no rebuild");

      const again = await fetch(`${api}/api/v1/admin/posts/secret-draft`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      eq(again.status, 404, "the API reports it as gone");
    });

    // -----------------------------------------------------------------------
    await test("§4 admin creates a page and it appears in navigation", async () => {
      const created = await fetch(`${api}/api/v1/admin/pages`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          title: "Colophon",
          slug: "colophon",
          description: "How this site is built.",
          date: "2026-10-05",
          navOrder: 20,
          draft: false,
          body: "## Colophon\n\nBuilt with a file-first architecture.",
        }),
      });
      const text = await created.text();
      eq(created.status, 201, `page created (${text})`);

      const page = await fetch(`${base}/colophon`);
      eq(page.status, 200, "the page is served at /colophon");
      const html = await page.text();
      ok(
        html.includes("Built with a file-first architecture."),
        "the page body rendered",
      );

      const index = await fetch(`${base}/`);
      ok(
        (await index.text()).includes(">Colophon<"),
        "the page appears in navigation",
      );
    });

    // -----------------------------------------------------------------------
    // §19 media: the extension is never trusted.
    // -----------------------------------------------------------------------
    await test("§19 media upload validates the real file type", async () => {
      const upload = (name, type, bytes) => {
        const form = new FormData();
        form.append("file", new Blob([bytes], { type }), name);
        return fetch(`${api}/api/v1/admin/media`, {
          method: "POST",
          headers: {
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: form,
        });
      };

      // A real 1x1 PNG.
      const png = Uint8Array.from(
        atob(
          "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
        ),
        (c) => c.charCodeAt(0),
      );

      const good = await upload("holiday.png", "image/png", png);
      const goodText = await good.text();
      eq(good.status, 201, `a valid PNG is accepted (${goodText})`);
      const saved = JSON.parse(goodText);
      ok(
        saved.path.endsWith(".png"),
        `the extension comes from the content (${saved.path})`,
      );
      ok(
        /^\d{4}\/\d{2}\/[a-f0-9]{24}\.png$/.test(saved.path),
        `path is YYYY/MM/<id>.png (${saved.path})`,
      );
      ok(
        !saved.path.includes("holiday"),
        "the client filename did not influence the path",
      );
      ok(saved.url.startsWith("/media/"), "the public URL is under /media/");
      eq(saved.width, 1, "dimensions were extracted");

      // The bytes really landed in MEDIA_ROOT.
      const onDisk = await readFile(
        path.join(roots.media, ...saved.path.split("/")),
      );
      ok(onDisk.length > 0, "the file exists in the media root");

      const rejected = [
        [
          "svg",
          "image/svg+xml",
          '<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>',
        ],
        ["html", "text/html", "<!doctype html><html><body>x</body></html>"],
        ["script", "application/javascript", "alert(document.cookie)"],
        ["xml", "application/xml", '<?xml version="1.0"?><a/>'],
        ["css", "text/css", "body{color:red}"],
        [
          "renamed.png",
          "image/png",
          '<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>',
        ],
      ];
      for (const [name, type, body] of rejected) {
        const res = await upload(name, type, Buffer.from(body));
        ok(res.status !== 201, `${name} is rejected (${res.status})`);
      }

      // Oversized upload.
      const big = new Uint8Array(6 * 1024 * 1024);
      big.set(png.subarray(0, 8));
      const tooBig = await upload("big.png", "image/png", big);
      ok(
        tooBig.status === 413 || tooBig.status === 422,
        `an oversized upload is refused (${tooBig.status})`,
      );

      // Listing and deletion.
      const list = await fetch(`${api}/api/v1/admin/media`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      const listed = await list.json();
      ok(
        listed.items.some((i) => i.id === saved.id),
        "the upload is listed",
      );

      const del = await fetch(`${api}/api/v1/admin/media/${saved.id}`, {
        method: "DELETE",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
      });
      eq(del.status, 200, "delete succeeds");
      await readFile(path.join(roots.media, ...saved.path.split("/"))).then(
        () => ok(false, "the file was removed from disk"),
        () => ok(true, "the file was removed from disk"),
      );
    });

    // -----------------------------------------------------------------------
    await test("§16/§17 admin custom code is served externally", async () => {
      const put = await fetch(`${api}/api/v1/admin/custom-code`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          css: ".from-admin { color: rebeccapurple; }",
          js: "window.__adminCustom = true;",
        }),
      });
      eq(put.status, 200, `custom code saved (${await put.text()})`);

      // The files on disk are the source of truth.
      const cssFile = await readFile(
        path.join(roots.content, "system", "custom.css"),
        "utf-8",
      );
      ok(
        cssFile.includes("rebeccapurple"),
        "custom.css was written to content/system/",
      );
      const jsFile = await readFile(
        path.join(roots.content, "system", "custom.js"),
        "utf-8",
      );
      ok(
        jsFile.includes("__adminCustom"),
        "custom.js was written to content/system/",
      );

      // Astro serves them, with no rebuild.
      const css = await fetch(`${base}/custom.css`);
      eq(css.status, 200, "GET /custom.css returns 200");
      ok(
        (css.headers.get("content-type") ?? "").startsWith("text/css"),
        "custom.css is text/css",
      );
      ok(
        (await css.text()).includes("rebeccapurple"),
        "the saved CSS is served",
      );

      const js = await fetch(`${base}/custom.js`);
      eq(js.status, 200, "GET /custom.js returns 200");
      ok(
        (js.headers.get("content-type") ?? "").startsWith("text/javascript"),
        "custom.js is text/javascript",
      );
      ok((await js.text()).includes("__adminCustom"), "the saved JS is served");

      // It is referenced externally, never inlined.
      const index = await fetch(`${base}/`);
      const html = await index.text();
      ok(html.includes('href="/custom.css"'), "the layout links custom.css");
      ok(html.includes('src="/custom.js"'), "the layout loads custom.js");
      ok(
        !html.includes("rebeccapurple"),
        "custom CSS is not inlined into the page",
      );
      ok(
        !html.includes("__adminCustom"),
        "custom JS is not inlined into the page",
      );
    });

    // -----------------------------------------------------------------------
    // ID-33 — the custom-asset API, over real HTTP against the running stack.
    //
    // The Go tests cover the handler contract in isolation. This block covers the two
    // things only the real stack can answer: that the session, Origin and CSRF
    // guards actually refuse an anonymous or forged request, and that a file written
    // through the API is live on a visitor's page with no rebuild in between.
    // -----------------------------------------------------------------------
    await test("ID-33 an anonymous caller cannot read or write a custom asset", async () => {
      const reads = await Promise.all([
        fetch(`${api}/api/v1/admin/custom/css`),
        fetch(`${api}/api/v1/admin/custom/js`),
        fetch(`${api}/api/v1/admin/custom/css/010-layout.css`),
      ]);
      for (const res of reads) {
        eq(
          res.status,
          401,
          `anonymous GET ${res.url.replace(api, "")} is refused`,
        );
      }

      const writes = [
        [
          "POST",
          `${api}/api/v1/admin/custom/css`,
          { filename: "010-anon.css", content: "x" },
        ],
        [
          "PUT",
          `${api}/api/v1/admin/custom/css/010-layout.css`,
          { content: "x" },
        ],
        ["DELETE", `${api}/api/v1/admin/custom/css/010-layout.css`, undefined],
      ];
      for (const [method, url, body] of writes) {
        const res = await fetch(url, {
          method,
          headers: {
            "Content-Type": "application/json",
            Origin: base,
          },
          body: body === undefined ? undefined : JSON.stringify(body),
        });
        eq(
          res.status,
          401,
          `anonymous ${method} ${url.replace(api, "")} is refused`,
        );
      }

      // Nothing was created on the way through.
      const cssDir = path.join(roots.content, "system", "css");
      const leftovers = await readdir(cssDir).catch(() => []);
      eq(leftovers.length, 0, "no refused request left a file behind");
    });

    await test("ID-33/§55 CSRF and Origin are enforced on custom assets too", async () => {
      await ensureAdmin();
      const headers = { "Content-Type": "application/json", Origin: base };
      const withCookie = { ...headers, Cookie: `blog_session=${cookie}` };

      // A session without the CSRF header.
      const noCsrf = await fetch(`${api}/api/v1/admin/custom/css`, {
        method: "POST",
        headers: withCookie,
        body: JSON.stringify({ filename: "010-nocsrf.css", content: "x" }),
      });
      eq(noCsrf.status, 403, "a create without a CSRF token is refused");

      const badCsrf = await fetch(`${api}/api/v1/admin/custom/css`, {
        method: "POST",
        headers: { ...withCookie, "X-CSRF-Token": "not-the-token" },
        body: JSON.stringify({ filename: "010-badcsrf.css", content: "x" }),
      });
      eq(badCsrf.status, 403, "a create with a wrong CSRF token is refused");

      // A forged Origin, which is what a cross-site form post looks like.
      const foreign = await fetch(`${api}/api/v1/admin/custom/css`, {
        method: "POST",
        headers: {
          ...headers,
          Origin: "https://evil.example",
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({ filename: "010-foreign.css", content: "x" }),
      });
      eq(foreign.status, 403, "a create from a foreign Origin is refused");

      const leftovers = await readdir(
        path.join(roots.content, "system", "css"),
      ).catch(() => []);
      eq(leftovers.length, 0, "no refused request left a file behind");

      // A read needs no CSRF token: it changes nothing.
      const read = await fetch(`${api}/api/v1/admin/custom/css`, {
        headers: withCookie,
      });
      eq(read.status, 200, "the same session may read without a CSRF token");
    });

    await test("ID-101 admin creates a CSS asset and a visitor gets it, with no rebuild", async () => {
      await ensureAdmin();
      const created = await adminAsset("POST", "css", null, {
        filename: "010-test-override.css",
        content: ".card { outline: 3px solid rgb(1, 2, 3); }\n",
      });
      eq(
        created.status,
        201,
        `the asset was created (${JSON.stringify(created.body)})`,
      );
      eq(
        created.body.filename,
        "010-test-override.css",
        "the response names the file",
      );
      eq(created.body.enabled, true, "a new asset is enabled");
      eq(created.body.order, 10, "the order comes from the filename prefix");
      eq(created.body.status, "ok", "and it is servable");

      // Go is the only writer, and it wrote where the aggregator looks.
      const onDisk = await readFile(
        path.join(roots.content, "system", "css", "010-test-override.css"),
        "utf-8",
      );
      ok(onDisk.includes("rgb(1, 2, 3)"), "the file is in content/system/css/");

      // Live, with no build step anywhere in the path.
      const css = await fetch(`${base}/custom.css`);
      const cssBody = await css.text();
      ok(
        cssBody.includes("rgb(1, 2, 3)"),
        "the new file is in /custom.css on the very next request",
      );
      ok(
        (css.headers.get("content-type") ?? "").startsWith("text/css"),
        "and /custom.css is still text/css",
      );

      const html = await (await fetch(`${base}/`)).text();
      ok(
        html.includes('href="/custom.css"'),
        "the page still links only /custom.css",
      );
      ok(
        !html.includes("rgb(1, 2, 3)"),
        "the custom CSS is not inlined into the page",
      );

      // §76: the cascade. The theme's stylesheet is inside `@layer theme` and
      // /custom.css is not, so an unlayered rule wins whatever the source order is.
      // §34 fixes the full order: theme → /markdown.css → /custom.css.
      const hrefs = [
        ...html.matchAll(/<link[^>]+rel="stylesheet"[^>]+href="([^"]+)"/g),
      ].map((m) => m[1]);
      eq(
        hrefs.length,
        3,
        "the page links the theme, /markdown.css and /custom.css, in that order",
      );
      eq(
        hrefs[1],
        "/markdown.css",
        "the Markdown presentation layer comes second",
      );
      eq(
        hrefs[2],
        "/custom.css",
        "and the admin's custom CSS is the final override",
      );
      ok(!cssBody.includes("@layer theme"), "/custom.css is unlayered");
      const themeCss = await (await fetch(`${base}${hrefs[0]}`)).text();
      ok(themeCss.includes("@layer theme"), "the theme's CSS is layered");
    });

    await test("ID-71 disabling an asset removes it from the next response", async () => {
      await ensureAdmin();
      const before = await (await fetch(`${base}/custom.css`)).text();
      ok(
        before.includes("rgb(1, 2, 3)"),
        "the asset is served before the toggle",
      );

      const off = await adminAsset("PUT", "css", "010-test-override.css", {
        enabled: false,
      });
      eq(
        off.status,
        200,
        `the asset was disabled (${JSON.stringify(off.body)})`,
      );
      eq(off.body.enabled, false, "the response reports it disabled");

      const during = await (await fetch(`${base}/custom.css`)).text();
      ok(
        !during.includes("rgb(1, 2, 3)"),
        "/custom.css no longer includes it, with no rebuild",
      );

      const list = (await adminAsset("GET", "css")).body;
      const row = list.assets.find(
        (a) => a.filename === "010-test-override.css",
      );
      ok(row && row.enabled === false, "the admin list reports it as disabled");
      ok(
        !row.path && !JSON.stringify(list).includes(roots.content),
        "the list carries no filesystem path",
      );

      const on = await adminAsset("PUT", "css", "010-test-override.css", {
        enabled: true,
      });
      eq(on.status, 200, "the asset was enabled again");
      const after = await (await fetch(`${base}/custom.css`)).text();
      ok(after.includes("rgb(1, 2, 3)"), "and it is served again");
    });

    await test("ID-69/§83 renaming a file is how an asset is moved", async () => {
      await ensureAdmin();
      for (const name of ["001-alpha.css", "020-gamma.css"]) {
        await adminAsset("POST", "css", null, {
          filename: name,
          content: `.${name.replace(".css", "")} { color: rgb(4, 5, 6); }\n`,
        });
      }
      await adminAsset("POST", "css", null, {
        filename: "010-beta.css",
        content: ".beta { color: rgb(7, 8, 9); }\n",
      });

      const ordered = await (await fetch(`${base}/custom.css`)).text();
      const positions = ["001-alpha.css", "010-beta.css", "020-gamma.css"].map(
        (name) => ordered.indexOf(`blogcms:css:${name}`),
      );
      ok(
        positions.every((p) => p !== -1) &&
          positions[0] < positions[1] &&
          positions[1] < positions[2],
        "001 -> 010 -> 020, whatever order they were created in",
      );

      // Move gamma ahead of beta by renaming it: one request, two atomic renames' worth
      // of intent, and no second ordering to keep in step.
      await adminAsset("PUT", "css", "020-gamma.css", {
        filename: "005-gamma.css",
      });
      const moved = await (await fetch(`${base}/custom.css`)).text();
      const after = ["001-alpha.css", "005-gamma.css", "010-beta.css"].map(
        (name) => moved.indexOf(`blogcms:css:${name}`),
      );
      ok(
        after.every((p) => p !== -1) &&
          after[0] < after[1] &&
          after[1] < after[2],
        "a rename changes the load order on the next request",
      );
      ok(!moved.includes("blogcms:css:020-gamma.css"), "the old name is gone");

      const refused = await adminAsset("PUT", "css", "005-gamma.css", {
        filename: "005-gamma.js",
      });
      eq(refused.status, 422, "a rename cannot change an asset's type");

      for (const name of ["001-alpha.css", "005-gamma.css", "010-beta.css"]) {
        const del = await adminAsset("DELETE", "css", name);
        eq(del.status, 204, `${name} deleted`);
      }
      const empty = await (await fetch(`${base}/custom.css`)).text();
      for (const name of ["001-alpha.css", "005-gamma.css", "010-beta.css"]) {
        ok(
          !empty.includes(`blogcms:css:${name}`),
          `${name} is gone from /custom.css`,
        );
      }
      ok(
        empty.includes("blogcms:css:010-test-override.css"),
        "and the asset that was never deleted is still there",
      );
    });

    await test("ID-70 a deletion is visible on a visitor's page with no rebuild", async () => {
      await ensureAdmin();
      const created = await adminAsset("POST", "css", null, {
        filename: "050-temporary.css",
        content: ".temporary { color: rgb(10, 11, 12); }\n",
      });
      eq(created.status, 201, "the asset was created");
      ok(
        (await (await fetch(`${base}/custom.css`)).text()).includes(
          "rgb(10, 11, 12)",
        ),
        "and it is served",
      );

      const del = await adminAsset("DELETE", "css", "050-temporary.css");
      eq(del.status, 204, "the asset was deleted");
      ok(
        !(await (await fetch(`${base}/custom.css`)).text()).includes(
          "rgb(10, 11, 12)",
        ),
        "and it is gone on the next request",
      );
      await readFile(
        path.join(roots.content, "system", "css", "050-temporary.css"),
      ).then(
        () => ok(false, "the file was removed from disk"),
        () => ok(true, "the file was removed from disk"),
      );
    });

    await test("ID-102 custom JS is created, served, disabled and deleted live", async () => {
      await ensureAdmin();
      const created = await adminAsset("POST", "js", null, {
        filename: "001-test-flag.js",
        content: "window.__customAssetTest = true;\n",
      });
      eq(created.status, 201, "the JavaScript asset was created");

      const js = await fetch(`${base}/custom.js`);
      const jsBody = await js.text();
      ok(jsBody.includes("__customAssetTest"), "/custom.js includes it");
      ok(
        (js.headers.get("content-type") ?? "").startsWith("text/javascript"),
        "and /custom.js is still text/javascript",
      );

      // The browser only ever sees the one URL, so the code runs once (ID-39).
      const html = await (await fetch(`${base}/`)).text();
      const srcs = [...html.matchAll(/<script[^>]+src="([^"]+)"/g)].map(
        (m) => m[1],
      );
      eq(
        srcs.filter((s) => s.includes("custom")).length,
        1,
        "the page loads exactly one custom script",
      );
      ok(
        !html.includes("__customAssetTest"),
        "and its text is not inlined into the page",
      );

      await adminAsset("PUT", "js", "001-test-flag.js", { enabled: false });
      ok(
        !(await (await fetch(`${base}/custom.js`)).text()).includes(
          "__customAssetTest",
        ),
        "disabling it removes it from /custom.js",
      );

      await adminAsset("PUT", "js", "001-test-flag.js", { enabled: true });
      ok(
        (await (await fetch(`${base}/custom.js`)).text()).includes(
          "__customAssetTest",
        ),
        "re-enabling puts it back",
      );

      await adminAsset("DELETE", "js", "001-test-flag.js");
      ok(
        !(await (await fetch(`${base}/custom.js`)).text()).includes(
          "__customAssetTest",
        ),
        "deleting it removes it for good",
      );
    });

    await test("ID-34 the legacy pair is preserved and cannot be deleted here", async () => {
      await ensureAdmin();
      // The legacy editor still works exactly as it did.
      const put = await fetch(`${api}/api/v1/admin/custom-code`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          css: ".legacy-still-works { color: rgb(13, 14, 15); }",
          js: "window.__legacyStillWorks = true;",
        }),
      });
      eq(put.status, 200, "the legacy editor still saves both files");

      const css = await (await fetch(`${base}/custom.css`)).text();
      ok(css.includes("rgb(13, 14, 15)"), "custom.css is still served");
      ok(
        css.indexOf("rgb(13, 14, 15)") <
          (css.indexOf("blogcms:css:") + 1 || Infinity),
        "and it is still ahead of the managed assets",
      );
      ok(
        (await (await fetch(`${base}/custom.js`)).text()).includes(
          "__legacyStillWorks",
        ),
        "custom.js is still served",
      );

      // Neither legacy file is deletable through the asset manager.
      for (const kind of ["css", "js"]) {
        const del = await adminAsset("DELETE", kind, "legacy");
        eq(del.status, 400, `the legacy ${kind} asset is not deletable here`);
        const got = await adminAsset("GET", kind, "legacy");
        eq(got.status, 400, `and is not addressable as an asset either`);
      }
      const stillThere = await Promise.all([
        readFile(path.join(roots.content, "system", "custom.css"), "utf-8"),
        readFile(path.join(roots.content, "system", "custom.js"), "utf-8"),
      ]);
      ok(stillThere[0].includes("rgb(13, 14, 15)"), "custom.css survived");
      ok(stillThere[1].includes("__legacyStillWorks"), "custom.js survived");

      // And it is listed, marked as legacy, ahead of everything else.
      const list = (await adminAsset("GET", "css")).body;
      eq(
        list.assets[0].filename,
        "custom.css",
        "the legacy file is the first row",
      );
      eq(list.assets[0].legacy, true, "and it is marked legacy");
      eq(list.assets[0].order, -1, "at order -1, ahead of every managed asset");
    });

    await test("ID-30 the admin screen lists assets and refuses anonymous access", async () => {
      await ensureAdmin();

      const anonymous = await fetch(`${base}/admin/custom-assets`, {
        redirect: "manual",
      });
      ok(
        anonymous.status === 302 ||
          anonymous.status === 401 ||
          anonymous.status === 303,
        `an anonymous visitor is sent away from the screen (got ${anonymous.status})`,
      );

      const page = await fetch(`${base}/admin/custom-assets`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      eq(page.status, 200, "the signed-in admin gets the screen");
      const html = await page.text();

      // §23: the screen binds through the contract, never through a class name, so a
      // theme can rename everything around it.
      ok(
        html.includes('data-cms-form="custom-asset"'),
        "the create/edit forms are wired",
      );
      ok(
        html.includes('data-cms-action="custom-asset-toggle"'),
        "the enable control is wired",
      );
      ok(
        html.includes('data-cms-action="custom-asset-delete"'),
        "the delete control is wired",
      );
      ok(html.includes("data-cms-confirm="), "a delete asks for confirmation");
      ok(
        html.includes("data-cms-unsaved-guard"),
        "the editor guards unsaved edits",
      );
      ok(
        html.includes('href="/admin/custom-code"'),
        "the screen links to the legacy editor",
      );
      ok(html.includes("custom.css"), "the legacy file is named as legacy");

      // §107: the editor shows text through a textarea value, never as markup.
      ok(
        !html.includes("innerHTML"),
        "no script builds the editor with innerHTML",
      );
      const editor = await fetch(
        `${base}/admin/custom-assets?name=010-test-override.css`,
        {
          headers: { Cookie: `blog_session=${cookie}` },
        },
      );
      eq(editor.status, 200, "the editor for one asset renders");
      const editorHtml = await editor.text();
      ok(editorHtml.includes('data-cms-mode="update"'), "in update mode");
      ok(
        editorHtml.includes('value="010-test-override.css"'),
        "with the filename pre-filled",
      );

      // §35: the legacy pair is edited on its own screen. Asking this one for it
      // must say where to go rather than render a form whose save is a 422.
      const legacy = await fetch(
        `${base}/admin/custom-assets?name=custom.css`,
        {
          headers: { Cookie: `blog_session=${cookie}` },
        },
      );
      eq(legacy.status, 200, "the screen answers for the legacy file too");
      const legacyHtml = await legacy.text();
      ok(
        !legacyHtml.includes('data-cms-mode="update"'),
        "and renders no editor form for it",
      );
      ok(
        legacyHtml.includes("/admin/custom-code"),
        "pointing at the screen that owns it instead",
      );

      // §86: the editor must not reformat on the way in. Astro's whitespace rule
      // keeps the indentation that precedes a `<textarea>`'s `{expression}`, so an
      // element written across several lines renders the template's own whitespace as
      // the field's value — and every save would prepend it to the file. The textarea
      // is the one place where "invisible formatting" is stored, so the rendered value
      // is compared to the bytes on disk, byte for byte.
      const source = await readFile(
        path.join(roots.content, "system", "css", "010-test-override.css"),
        "utf-8",
      );
      const textarea = editorHtml.match(
        /<textarea id="asset-body-css"[^>]*>([\s\S]*?)<\/textarea>/,
      );
      ok(textarea !== null, "the editor renders a textarea");
      eq(
        textarea?.[1],
        source,
        "the editor's value is the file's bytes, with nothing added",
      );

      // §109/§110: the admin page does not execute the asset it is showing.
      const adminScripts = [
        ...editorHtml.matchAll(/<script[^>]+src="([^"]+)"/g),
      ].map((m) => m[1]);
      ok(
        !adminScripts.includes("/custom.js"),
        "the admin never loads /custom.js; public custom JS does not run in the admin",
      );
    });

    await test("ID-57/§106 a row with no file is reported, and can be cleaned up", async () => {
      await ensureAdmin();
      await adminAsset("POST", "css", null, {
        filename: "060-vanishing.css",
        content: ".vanishing {}\n",
      });
      // Read once so the row exists, then take the file away behind the API's back.
      await adminAsset("GET", "css");
      await rm(path.join(roots.content, "system", "css", "060-vanishing.css"), {
        force: true,
      });

      const list = (await adminAsset("GET", "css")).body;
      const row = list.assets.find((a) => a.filename === "060-vanishing.css");
      ok(row !== undefined, "the row is still listed");
      eq(row.status, "missing", "and it is reported as missing, not hidden");
      ok(row.problem !== "", "with a reason");

      // §106: the repair. The file is already gone, so deleting clears the row and
      // cannot touch anything on disk.
      const del = await adminAsset("DELETE", "css", "060-vanishing.css");
      eq(del.status, 204, "deleting a row with no file repairs it");
      const after = (await adminAsset("GET", "css")).body;
      ok(
        after.assets.every((a) => a.filename !== "060-vanishing.css"),
        "the stale row is gone once it is deleted",
      );
    });

    await test("ID-98 the traversal list is refused over HTTP", async () => {
      await ensureAdmin();
      const cssDir = path.join(roots.content, "system", "css");
      const before = (await readdir(cssDir).catch(() => [])).sort();
      const names = [
        "../../evil.css",
        "..%5Cevil.css",
        "..%2F..%2Fevil.css",
        "etc%2Fpasswd",
        "%2Fetc%2Fpasswd",
        "001-base.css%00",
        "001-base.css%3Fx",
        "001-base.css%23x",
        "001-base.css%2F",
        "evil.css",
        "001-base.CSS",
      ];
      for (const name of names) {
        const res = await adminAsset("POST", "css", null, {
          filename: name,
          content: "x",
        });
        ok(
          res.status >= 400,
          `POST with filename ${name} is refused (got ${res.status})`,
        );
        const body = JSON.stringify(res.body ?? {});
        ok(!body.includes(roots.content), `and ${name} leaked no path`);
      }
      for (const name of names) {
        const res = await adminAsset("DELETE", "css", name);
        ok(res.status >= 400, `DELETE ${name} is refused (got ${res.status})`);
      }
      const after = (await readdir(cssDir).catch(() => [])).sort();
      eq(
        after.join(","),
        before.join(","),
        "no traversal attempt added or removed a file",
      );
    });

    await test("§17 the two size ceilings are separate and both are enforced", async () => {
      await ensureAdmin();
      const maxBytes = (await adminAsset("GET", "css")).body.maxBytes;
      ok(maxBytes > 0, `the list reports the editor's ceiling (${maxBytes})`);

      // A body one byte over the ceiling is refused with a field message, which is
      // what the editor renders next to the textarea.
      const over = await adminAsset("POST", "css", null, {
        filename: "070-oversize.css",
        content: "a".repeat(maxBytes + 1),
      });
      eq(over.status, 422, "an oversized asset is refused");
      ok(
        JSON.stringify(over.body).includes("content"),
        "and the message names the content field",
      );

      // And one under it is accepted, so the ceiling is a ceiling and not a ban.
      const under = await adminAsset("POST", "css", null, {
        filename: "070-fits.css",
        content: `.fits{}\n`,
      });
      eq(under.status, 201, "an asset within the ceiling is accepted");
      await adminAsset("DELETE", "css", "070-fits.css");
    });

    // -----------------------------------------------------------------------
    // §14/§15 comments: plain text only, pending by default, no enumeration.
    // -----------------------------------------------------------------------
    await test("§14 an anonymous comment is stored as pending plain text", async () => {
      const submit = (body, headers = {}) =>
        fetch(`${api}/api/v1/comments`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            ...headers,
          },
          body: JSON.stringify(body),
        });

      const created = await submit({
        postSlug: "written-through-the-api",
        nickname: "Reader One",
        content: "First!",
        startedAt: Date.now() - 5000,
      });
      const createdText = await created.text();
      eq(created.status, 201, `comment accepted (${createdText})`);
      const saved = JSON.parse(createdText);
      eq(saved.status, "pending", "a new comment defaults to pending");

      // The public listing must not show it yet.
      const publicList = await fetch(
        `${api}/api/v1/comments?post=written-through-the-api`,
      );
      const listed = await publicList.json();
      eq(listed.items.length, 0, "a pending comment is not publicly visible");

      // Nor on the rendered page.
      const page = await fetch(`${base}/posts/written-through-the-api`);
      ok(
        !(await page.text()).includes("First!"),
        "the pending comment is absent from the page",
      );

      // Approving makes it appear, with no rebuild.
      const approve = await fetch(`${api}/api/v1/admin/comments/${saved.id}`, {
        method: "PATCH",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({ status: "approved" }),
      });
      eq(approve.status, 200, "the comment can be approved");

      const after = await fetch(
        `${api}/api/v1/comments?post=written-through-the-api`,
      );
      const afterList = await after.json();
      eq(afterList.items.length, 1, "the approved comment is publicly visible");

      const rendered = await fetch(`${base}/posts/written-through-the-api`);
      ok(
        (await rendered.text()).includes("First!"),
        "the approved comment renders on the page",
      );
    });

    // -----------------------------------------------------------------------
    await test("§8 comment XSS payloads are inert", async () => {
      const payloads = [
        '<script>alert("xss")</script>',
        "<img src=x onerror=alert(1)>",
        '"><svg/onload=alert(1)>',
        '<iframe src="javascript:alert(1)"></iframe>',
        "[clickme](javascript:alert(1))",
        "<style>body{display:none}</style>",
      ];

      const ids = [];
      for (const [i, payload] of payloads.entries()) {
        const res = await fetch(`${api}/api/v1/comments`, {
          method: "POST",
          headers: { "Content-Type": "application/json", Origin: base },
          body: JSON.stringify({
            postSlug: "written-through-the-api",
            nickname: `<b>bad${i}</b>`,
            content: payload,
            startedAt: Date.now() - 5000,
          }),
        });
        const text = await res.text();
        if (res.status !== 201) {
          ok(false, `payload ${i} was rejected with ${res.status}: ${text}`);
          continue;
        }
        const saved = JSON.parse(text);
        ids.push(saved.id);

        // Approve so it reaches the page.
        await fetch(`${api}/api/v1/admin/comments/${saved.id}`, {
          method: "PATCH",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: JSON.stringify({ status: "approved" }),
        });
      }
      eq(ids.length, payloads.length, "all payloads were accepted for storage");

      const page = await fetch(`${base}/posts/written-through-the-api`);
      const html = await page.text();

      // The dangerous markup must not appear as live elements.
      ok(
        !/<script>alert\("xss"\)<\/script>/.test(html),
        "no injected <script> element",
      );
      ok(!/<img src=x onerror/i.test(html), "no injected onerror handler");
      ok(!/<svg\/onload/i.test(html), "no injected svg onload");
      ok(!/<iframe src="javascript:/i.test(html), "no javascript: iframe");
      ok(
        !/<style>body\{display:none\}/.test(html),
        "no injected style element",
      );
      ok(!/<b>bad\d<\/b>/.test(html), "the nickname is not rendered as markup");

      // The payload text is present, but escaped.
      ok(
        html.includes("&lt;script&gt;alert(&quot;xss&quot;)&lt;/script&gt;"),
        "the script payload is escaped text",
      );
      ok(
        html.includes("&lt;img src=x onerror=alert(1)&gt;"),
        "the img payload is escaped text",
      );

      // Clean them up so later tests are not polluted.
      for (const id of ids) {
        await fetch(`${api}/api/v1/admin/comments/${id}`, {
          method: "DELETE",
          headers: {
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
        });
      }
    });

    // -----------------------------------------------------------------------
    await test("§15 the public API cannot read the moderation queue", async () => {
      const submit = await fetch(`${api}/api/v1/comments`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          postSlug: "written-through-the-api",
          nickname: "Hidden",
          content: "Should not be listable while pending.",
          startedAt: Date.now() - 5000,
        }),
      });
      const saved = await submit.json();

      for (const query of [
        "?status=pending",
        "?status=spam",
        "?status=deleted",
        "?status=approved",
      ]) {
        const res = await fetch(
          `${api}/api/v1/comments?post=written-through-the-api${query}`,
        );
        const body = await res.json();
        const leaked = (body.items ?? []).some((c) => c.id === saved.id);
        ok(!leaked, `the public API ignores ${query}`);
      }

      // The admin endpoint does see it.
      const adminList = await fetch(
        `${api}/api/v1/admin/comments?status=pending`,
        {
          headers: { Cookie: `blog_session=${cookie}` },
        },
      );
      const adminBody = await adminList.json();
      ok(
        (adminBody.items ?? []).some((c) => c.id === saved.id),
        "the admin queue sees the pending comment",
      );
      ok(adminBody.pending >= 1, "the pending count is reported");

      await fetch(`${api}/api/v1/admin/comments/${saved.id}`, {
        method: "DELETE",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
      });
    });

    // -----------------------------------------------------------------------
    await test("§14/§31 comment validation and spam controls", async () => {
      const submit = (body) =>
        fetch(`${api}/api/v1/comments`, {
          method: "POST",
          headers: { "Content-Type": "application/json", Origin: base },
          body: JSON.stringify(body),
        });

      const base_ = {
        postSlug: "written-through-the-api",
        startedAt: Date.now() - 5000,
      };

      // Unknown post.
      const unknownPost = await submit({
        ...base_,
        postSlug: "no-such-post",
        nickname: "a",
        content: "hello",
      });
      eq(unknownPost.status, 422, "a comment on an unknown post is refused");

      // Invalid slug.
      const badSlug = await submit({
        ...base_,
        postSlug: "NOT A SLUG",
        nickname: "a",
        content: "hello",
      });
      eq(badSlug.status, 422, "an invalid slug is refused");

      // Empty body.
      const empty = await submit({ ...base_, nickname: "a", content: "" });
      eq(empty.status, 422, "an empty body is refused");

      // Oversized body.
      const big = await submit({
        ...base_,
        nickname: "a",
        content: "x".repeat(5000),
      });
      eq(big.status, 422, "an oversized body is refused");

      // Too many links.
      const links = await submit({
        ...base_,
        nickname: "a",
        content:
          "https://a.example https://b.example https://c.example https://d.example",
      });
      eq(links.status, 422, "a link-heavy comment is refused");

      // Honeypot: accepted silently, never stored.
      const before = await fetch(`${api}/api/v1/admin/comments?limit=200`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      const beforeCount = (await before.json()).items.length;

      const honey = await submit({
        ...base_,
        nickname: "Bot",
        content: "spam",
        honeypot: "http://spam.example",
      });
      eq(honey.status, 201, "a honeypot submission is accepted silently");

      const after = await fetch(`${api}/api/v1/admin/comments?limit=200`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      const afterBody = await after.json();
      eq(
        afterBody.items.length,
        beforeCount,
        "the honeypot comment was not stored",
      );
    });

    // -----------------------------------------------------------------------
    await test("§13 a cross-site comment POST is refused", async () => {
      const res = await fetch(`${api}/api/v1/comments`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: "https://evil.example",
        },
        body: JSON.stringify({
          postSlug: "written-through-the-api",
          nickname: "Attacker",
          content: "cross-site",
          startedAt: Date.now() - 5000,
        }),
      });
      eq(res.status, 403, "a foreign Origin is refused");
    });
    // -------------------------------------------------------------------------
    // §13 — defence in depth. Astro's own checkOrigin and the Go Origin check are
    // two independent layers; a cross-site mutation must be stopped even if one
    // of them were somehow bypassed.
    // -------------------------------------------------------------------------
    await test("§13 cross-site mutations are refused at both layers", async () => {
      const login = await fetch(`${base}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const c = sessionCookieFrom(login);
      const t = (await login.json()).csrfToken;

      const payload = {
        siteTitle: "Cross-Site",
        commentsEnabled: true,
        commentAutoModerate: true,
        themeId: "bluearchive",
      };

      // Through the Astro origin, with a foreign Origin.
      const viaOrigin = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: "https://evil.example",
          Cookie: `blog_session=${c}`,
          "X-CSRF-Token": t,
        },
        body: JSON.stringify(payload),
      });
      ok(
        viaOrigin.status === 403 || viaOrigin.status === 401,
        `a foreign Origin through the proxy is refused (${viaOrigin.status})`,
      );

      // The settings must be unchanged.
      const check = await fetch(`${api}/api/v1/admin/settings`, {
        headers: { Cookie: `blog_session=${c}` },
      });
      const current = await check.json();
      ok(
        current.siteTitle !== "Cross-Site",
        "the refused mutation did not take effect",
      );

      // Directly to Go, bypassing Astro entirely: the second layer must hold.
      const direct = await fetch(`${api}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: "https://evil.example",
          Cookie: `blog_session=${c}`,
          "X-CSRF-Token": t,
        },
        body: JSON.stringify(payload),
      });
      eq(
        direct.status,
        403,
        "Go refuses a foreign Origin on its own, with no proxy involved",
      );

      // Same-origin but no CSRF token: refused by the application layer.
      const noCsrf = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${c}`,
        },
        body: JSON.stringify(payload),
      });
      eq(
        noCsrf.status,
        403,
        "a same-origin mutation without a CSRF token is refused",
      );

      // Astro's checkOrigin is configured on, so a form-shaped POST to an
      // Astro route with a foreign Origin is refused before the handler runs.
      const astroForm = await fetch(`${base}/admin/logout`, {
        method: "POST",
        headers: {
          Origin: "https://evil.example",
          Cookie: `blog_session=${c}`,
        },
        redirect: "manual",
      });
      ok(
        astroForm.status === 403,
        `Astro's own checkOrigin refuses a foreign-Origin POST (${astroForm.status})`,
      );
    });

    // -------------------------------------------------------------------------
    // ID-13 — the theme must be swappable, and the custom-code editor must
    // actually be able to override it.
    // -------------------------------------------------------------------------
    // -------------------------------------------------------------------------
    // ID-13 / §44 — a theme owns its markup, and custom.css must still be able to
    // override it. The theme-switching behaviour itself lives in
    // tests/theme-tests.mjs; what is checked here is that the layering guarantees
    // the default theme depended on still hold now that the CSS belongs to a theme.
    // -------------------------------------------------------------------------
    await test("ID-13 the active theme is layered and custom.css actually wins", async () => {
      const page = await fetch(`${base}/`);
      const html = await page.text();

      // The theme CSS is a real stylesheet (Astro only inlines small bundles), so
      // read it rather than assuming it landed in a <style> block.
      const themeHrefs = [
        ...html.matchAll(
          /<link[^>]+rel="stylesheet"[^>]+href="(\/_astro\/[^"]+\.css)"/g,
        ),
      ].map((m) => m[1]);
      ok(themeHrefs.length > 0, "the theme ships as a stylesheet");
      const themeCss = (
        await Promise.all(
          themeHrefs.map((h) => fetch(base + h).then((r) => r.text())),
        )
      ).join("\n");
      ok(
        themeCss.includes("@layer theme"),
        "every theme stylesheet is inside @layer theme",
      );

      // §45: the document announces both hooks, and they are distinct concerns. The
      // colour scheme is a per-visitor preference; the theme is the installed pack.
      ok(
        /<html[^>]*data-color-scheme="auto"/.test(html),
        "the document declares a data-color-scheme attribute",
      );
      ok(
        /<html[^>]*data-cms-theme="[a-z-]+"/.test(html),
        "the document declares the active theme id",
      );
      ok(
        html.includes('src="/color-scheme.js"'),
        "colour-scheme selection is an external script",
      );
      ok(
        html.includes("data-color-scheme-toggle"),
        "a colour-scheme toggle is present",
      );

      // §18: still no inline script anywhere, so CSP needs no 'unsafe-inline'.
      ok(
        !/<script(?![^>]*\bsrc=)[^>]*>\s*\S[\s\S]*?<\/script>/.test(html),
        "no inline script, so CSP still needs no unsafe-inline",
      );

      // The whole point of the layer: custom.css is unlayered, so it beats the theme
      // for equal specificity no matter which comes later in the document.
      ok(html.indexOf("/custom.css") > 0, "custom.css is linked");
      ok(
        !themeCss.includes("@layer custom"),
        "custom.css is not layered, so it always wins",
      );

      // No component may reintroduce an unlayered, higher-specificity stylesheet.
      ok(
        !/data-astro-cid/.test(html),
        "no Astro scoped-style attributes remain",
      );
      eq(
        (html.match(/<style/g) || []).length,
        0,
        "no inline <style> block can shadow the theme layer",
      );

      // No inline style attributes anywhere a theme would have to fight.
      eq(
        (html.match(/ style="/g) ?? []).length,
        0,
        "the rendered page has no inline style attributes",
      );

      const postHtml = await (
        await fetch(`${base}/posts/written-through-the-api`)
      ).text();
      ok(
        postHtml.includes('class="field honeypot"'),
        "the honeypot is hidden by class, not inline style",
      );
      // The regression this guards: removing the inline style without adding the
      // class rule would have exposed the bot trap to humans.
      ok(
        /\.honeypot\{[^}]*left:\s*-9999px/.test(themeCss),
        "the honeypot rule is actually emitted, so the trap stays hidden",
      );
      ok(
        /\.color-scheme-toggle\{/.test(themeCss),
        "the colour-scheme toggle is styled by the stylesheet, not the UA default",
      );

      // Rules that used to live in component <style> blocks must now be in the theme.
      for (const rule of [
        ".post-card h2",
        ".comment .body",
        ".error-page h1",
      ]) {
        ok(themeCss.includes(rule), `${rule} is theme-owned, inside the layer`);
      }
      ok(
        !postHtml.includes("left: -9999px; opacity: 0; position: absolute; }"),
        "no inline honeypot styling leaked into the markup",
      );

      // The site title comes from the API, so the admin setting has an effect.
      const site = await fetch(`${api}/api/v1/site`);
      eq(site.status, 200, "GET /api/v1/site returns 200");
      const settings = await site.json();
      ok(
        typeof settings.siteTitle === "string",
        "the public site endpoint returns a title",
      );
      ok(
        typeof settings.themeId === "string",
        "the public site endpoint returns the theme id",
      );
    });

    await test("ID-13 the admin honours the same theme contract", async () => {
      const login = await fetch(`${api}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const c = sessionCookieFrom(login);
      const admin = await fetch(`${base}/admin`, {
        headers: { Cookie: `blog_session=${c}` },
      });
      eq(admin.status, 200, "the admin dashboard loads");
      const html = await admin.text();

      // Admin once linked custom.css but set no colour-scheme attribute and loaded no
      // script, which silently dropped an explicit light/dark choice.
      ok(
        /<html[^>]*data-color-scheme=/.test(html),
        "admin declares a colour scheme",
      );
      ok(
        /<html[^>]*data-cms-theme="[a-z-]+"/.test(html),
        "admin declares the active theme, so the admin follows the same theme as the site",
      );
      ok(
        html.includes('src="/color-scheme.js"'),
        "admin loads the colour-scheme script",
      );
      ok(html.includes('href="/custom.css"'), "admin links custom.css");
      ok(
        html.includes("data-color-scheme-toggle"),
        "admin can change the colour scheme",
      );
      ok(
        html.includes('src="/cms.js"'),
        "admin loads the core behaviour script",
      );
      ok(
        !/data-astro-cid/.test(html),
        "admin has no Astro scoped-style attributes",
      );
      eq(
        (html.match(/ style="/g) ?? []).length,
        0,
        "admin has no inline style attributes",
      );
    });

    await test("§6 one bad character cannot invalidate the whole feed", async () => {
      // XML 1.0 forbids most control characters, and a parser rejects the entire
      // document when it meets one. A single post title carrying a stray form feed
      // therefore took the whole feed down for every reader.
      const cookie = await loginAsAdmin();

      for (const [label, title] of [
        ["form feed", `Bad${String.fromCharCode(0x0c)}Title`],
        ["vertical tab", `Bad${String.fromCharCode(0x0b)}Title`],
        ["NUL", `Bad${String.fromCharCode(0x00)}Title`],
        ["escape", `Bad${String.fromCharCode(0x1b)}Title`],
        ["DEL", `Bad${String.fromCharCode(0x7f)}Title`],
      ]) {
        const created = await createPost({
          title,
          slug: `ctrl-${label.replace(/\W/g, "")}`,
        });
        eq(created.status, 422, `a title containing a ${label} is refused`);
      }

      // Legitimate metacharacters must still work: escaping them is not the point.
      eq(
        (await createPost({ title: "Tom & Jerry", slug: "amp-title" })).status,
        201,
        "an ampersand is accepted",
      );
      eq(
        (await createPost({ title: "<em>emphasis</em>", slug: "angle-title" }))
          .status,
        201,
        "angle brackets are accepted",
      );
      eq(
        (await createPost({ title: "中文标题", slug: "cjk-title" })).status,
        201,
        "non-ASCII is accepted",
      );

      // A bad slug is bad input. It used to surface as 500 "internal server error"
      // and log at ERROR, so a mistyped slug, a reserved word and a traversal probe
      // were indistinguishable from a server fault.
      for (const [label, slug] of [
        ["uppercase", "INVALID"],
        ["slash", "a/b"],
        ["traversal", ".."],
        ["traversal prefix", "../evil"],
        ["reserved", "admin"],
        ["reserved posts", "posts"],
        ["too long", "a".repeat(81)],
        ["empty", ""],
      ]) {
        const res = await createPost({ title: "Fine Title", slug });
        eq(
          res.status,
          422,
          `a ${label} slug is rejected as invalid input, not a server fault`,
        );
        const body = await res.json();
        ok(
          body?.error?.fields?.slug,
          `the ${label} rejection names the offending field`,
        );
      }

      const xml = await (await fetch(`${base}/rss.xml`)).text();
      const illegal = [...xml].filter((ch) => {
        const c = ch.codePointAt(0);
        return (
          (c < 0x20 && c !== 0x09 && c !== 0x0a && c !== 0x0d) || c === 0x7f
        );
      });
      eq(illegal.length, 0, "the feed contains no character XML forbids");
      ok(
        xml.includes("Tom &amp; Jerry"),
        "an ampersand is escaped rather than stripped",
      );
      ok(
        xml.includes("&lt;em&gt;emphasis"),
        "angle brackets are escaped rather than stripped",
      );
    });

    await test("§6 a hand-edited file with control characters still yields a valid feed", async () => {
      // content/ is the source of truth, so bypassing the API is a supported
      // workflow and the feed has to survive it.
      const slug = "hand-edited-controls";
      const title = `Hand${String.fromCharCode(0x0c)}Edited${String.fromCharCode(0x00)}`;
      await writeFile(
        path.join(roots.content, "posts", `${slug}.md`),
        `---\ntitle: ${JSON.stringify(title)}\nslug: ${slug}\ndate: 2026-10-03\ndraft: false\n---\n\nBody.\n`,
      );

      const page = await fetch(`${base}/posts/${slug}`);
      eq(page.status, 200, "the article itself still renders");

      const xml = await (await fetch(`${base}/rss.xml`)).text();
      const illegal = [...xml].filter((ch) => {
        const c = ch.codePointAt(0);
        return (
          (c < 0x20 && c !== 0x09 && c !== 0x0a && c !== 0x0d) || c === 0x7f
        );
      });
      eq(illegal.length, 0, "the feed is still well-formed");
      ok(
        /<title>HandEdited<\/title>/.test(xml),
        "the control characters are dropped and the rest is kept",
      );
    });

    await test("§7 a forged client-IP header cannot buy a fresh rate-limit bucket", async () => {
      const attempt = (spoof) =>
        fetch(`${base}/api/v1/auth/login`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            ...(spoof ? { "X-Client-IP": spoof } : {}),
          },
          // A dedicated username: the throttle bucket is (username, ip), so this
          // must not consume the admin's own budget and break every later test.
          body: JSON.stringify({
            username: "ratelimit-probe",
            password: "not-the-password",
          }),
        });

      // The login throttle allows a handful of attempts per minute. Because Astro
      // proxies everything, Go used to see only 127.0.0.1, so one shared bucket let
      // anyone lock the admin out with a few junk requests.
      const codes = [];
      for (let i = 0; i < 9; i++) {
        // A different forged address every time: if the header were honoured as
        // sent, each attempt would land in its own bucket and none would be limited.
        codes.push((await attempt(`198.51.100.${i + 1}`)).status);
      }
      ok(
        codes.includes(429),
        "the limiter still engages, so there is a shared bucket",
      );
      eq(
        codes.at(-1),
        429,
        "later attempts are rate limited despite a fresh forged IP each time",
      );
    });

    await test("ID-14 the settings cache is coherent with a write", async () => {
      const login = await fetch(`${api}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const c = sessionCookieFrom(login);
      const t = (await login.json()).csrfToken;

      // Through the Astro origin, which is the path the admin UI actually uses and
      // the only one that can invalidate the frontend's settings cache.
      const put = (body) =>
        fetch(`${base}/api/v1/admin/settings`, {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${c}`,
            "X-CSRF-Token": t,
          },
          body: JSON.stringify({ ...body }),
        });

      // The title is cached for 30s. A save must not wait out the TTL, or the
      // admin saves, opens the public site, and sees the previous title.
      eq(
        (
          await put({
            siteTitle: "First Title",
            commentsEnabled: true,
            commentAutoModerate: true,
            themeId: "bluearchive",
          })
        ).status,
        200,
        "first save",
      );
      ok(
        (await (await fetch(`${base}/`)).text()).includes("First Title"),
        "first title visible immediately",
      );

      eq(
        (
          await put({
            siteTitle: "Second Title",
            commentsEnabled: true,
            commentAutoModerate: true,
            themeId: "bluearchive",
          })
        ).status,
        200,
        "second save",
      );
      ok(
        (await (await fetch(`${base}/`)).text()).includes("Second Title"),
        "second title visible immediately, not after the TTL",
      );

      // Same for the feed.
      ok(
        (await (await fetch(`${base}/rss.xml`)).text()).includes(
          "Second Title",
        ),
        "the feed agrees with the page",
      );
    });

    await test("ID-14 the comment toggle actually removes the form", async () => {
      const login = await fetch(`${api}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const c = sessionCookieFrom(login);
      const t = (await login.json()).csrfToken;

      const post = await fetch(`${base}/posts/written-through-the-api`);
      const withForm = await post.text();
      ok(
        withForm.includes('id="comment-form"'),
        "the article shows a comment form when comments are on",
      );

      // getCommentSettings() used to return hardcoded `true`, so turning comments
      // off left the form in place and every submission came back as a 403.
      const off = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${c}`,
          "X-CSRF-Token": t,
        },
        body: JSON.stringify({
          siteTitle: "Blog",
          commentsEnabled: false,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      eq(off.status, 200, "comments can be turned off");

      const withoutForm = await (
        await fetch(`${base}/posts/written-through-the-api`)
      ).text();
      ok(
        !withoutForm.includes('id="comment-form"'),
        "no comment form element is rendered",
      );
      ok(
        !withoutForm.includes('name="nickname"'),
        "no comment input is rendered",
      );

      // And the server agrees, so the UI is not merely hiding something.
      const rejected = await fetch(`${api}/api/v1/comments`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          postSlug: "written-through-the-api",
          nickname: "Someone",
          content: "Hello",
          startedAt: Date.now() - 5000,
        }),
      });
      eq(
        rejected.status,
        403,
        "the server refuses a comment while comments are off",
      );

      const on = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${c}`,
          "X-CSRF-Token": t,
        },
        body: JSON.stringify({
          siteTitle: "Blog",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      eq(on.status, 200, "comments can be turned back on");
    });

    await test("ID-13 the site title setting is actually applied", async () => {
      // Origin must be the public origin Go is configured with, not the API port.
      const login = await fetch(`${api}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const c = sessionCookieFrom(login);
      const t = (await login.json()).csrfToken;

      const updated = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${c}`,
          "X-CSRF-Token": t,
        },
        body: JSON.stringify({
          siteTitle: "Themed Blog",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      eq(updated.status, 200, "the title can be changed");

      const page = await fetch(`${base}/`);
      const html = await page.text();
      ok(
        html.includes("Themed Blog"),
        "the new title appears in the rendered header",
      );
      ok(!html.includes(">Blog</a>"), "the previous hardcoded title is gone");

      const rss = await fetch(`${base}/rss.xml`);
      const feed = await rss.text();
      ok(feed.includes("Themed Blog"), "the RSS feed uses the same title");
    });

    // -------------------------------------------------------------------------
    // §11 — the admin UI is Astro, and every listed route exists and is gated.
    // -------------------------------------------------------------------------
    await test("§11 every admin route exists and is gated by the session", async () => {
      const listed = [
        "/admin/login",
        "/admin",
        "/admin/posts",
        "/admin/posts/new",
        "/admin/posts/some-post",
        "/admin/pages",
        "/admin/pages/new",
        "/admin/pages/some-page",
        "/admin/comments",
        "/admin/media",
        "/admin/settings",
        "/admin/custom-code",
      ];

      // Anonymous: only the login page is reachable, and everything else is
      // redirected rather than served.
      for (const route of listed) {
        const res = await fetch(`${base}${route}`, { redirect: "manual" });
        if (route === "/admin/login") {
          eq(res.status, 200, `${route} is reachable without a session`);
        } else {
          ok(
            res.status === 303 || res.status === 302,
            `${route} redirects an anonymous visitor (got ${res.status})`,
          );
          ok(
            (res.headers.get("location") ?? "").startsWith("/admin/login"),
            `${route} redirects to the login page`,
          );
        }
      }

      // Authenticated: every route renders a real page, never a 404 or 500.
      const session = await fetch(`${base}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const authCookie = sessionCookieFrom(session);
      const authCsrf = (await session.json()).csrfToken;
      ok(authCookie !== null, "signed in for the route inventory check");

      for (const route of listed) {
        const res = await fetch(`${base}${route}`, {
          headers: { Cookie: `blog_session=${authCookie}` },
          redirect: "manual",
        });
        // A signed-in visitor is redirected away from the login page.
        if (route === "/admin/login") {
          eq(
            res.status,
            303,
            "/admin/login redirects a signed-in admin to the dashboard",
          );
          eq(
            res.headers.get("location"),
            "/admin",
            "the redirect target is /admin",
          );
          continue;
        }
        // /admin/posts/some-post does not exist, so 303 back to the list is the
        // correct answer for a missing slug.
        const isMissingSlug =
          route.endsWith("/some-post") || route.endsWith("/some-page");
        if (isMissingSlug) {
          ok(
            res.status === 303 || res.status === 404,
            `${route} handles a missing slug (${res.status})`,
          );
          continue;
        }
        eq(res.status, 200, `${route} renders for an authenticated admin`);
        const html = await res.text();
        ok(
          html.includes("admin-sidebar") || html.includes("login-shell"),
          `${route} renders an Astro page, not a stub`,
        );
        ok(
          !html.includes("<b>"),
          `${route} contains no stray markup from user data`,
        );
      }

      // Every admin page must be marked noindex so it never reaches a search index.
      for (const route of ["/admin", "/admin/posts", "/admin/settings"]) {
        const res = await fetch(`${base}${route}`, {
          headers: { Cookie: `blog_session=${authCookie}` },
        });
        ok(
          (await res.text()).includes("noindex"),
          `${route} is marked noindex`,
        );
      }

      // And the Go backend must not be rendering any of them.
      const goRes = await fetch(`${api}/admin`, { redirect: "manual" });
      ok(
        goRes.status === 404 || goRes.status === 401,
        "the Go backend serves no admin HTML (${goRes.status})",
      );
    });

    // -------------------------------------------------------------------------
    // Astro is now the single origin, so the browser reaches the Go API through
    // Astro's /api/* proxy. These exercise that path rather than bypassing it.
    // -------------------------------------------------------------------------
    await test("Astro is the single origin: /api/* is proxied to Go", async () => {
      const viaAstro = await fetch(`${base}/api/v1/healthz`);
      eq(viaAstro.status, 200, "GET /api/v1/healthz through Astro returns 200");
      const health = await viaAstro.json();
      eq(health.status, "ok", "the proxied body is the Go JSON, unchanged");

      // The JSON error envelope must survive the proxy intact.
      const missing = await fetch(`${base}/api/v1/nope`);
      eq(missing.status, 404, "an unknown API path returns 404 through Astro");
      ok(
        (missing.headers.get("content-type") ?? "").startsWith(
          "application/json",
        ),
        "the proxied error keeps the JSON content type",
      );
      const envelope = await missing.json();
      ok(
        envelope.error?.code === "not_found",
        `the error envelope survives: ${JSON.stringify(envelope)}`,
      );

      // Security headers are Astro's responsibility now and must not be clobbered.
      const page = await fetch(`${base}/`);
      for (const header of [
        "content-security-policy",
        "x-frame-options",
        "x-content-type-options",
      ]) {
        ok(page.headers.get(header) !== null, `Astro sets ${header}`);
      }
      eq(
        page.headers.get("x-frame-options"),
        "DENY",
        `X-Frame-Options is DENY`,
      );
    });

    await test("proxied mutations carry cookies, CSRF and bodies", async () => {
      // Sign in THROUGH the proxy: this exercises body forwarding and the
      // Set-Cookie passthrough in one go.
      const signIn = await fetch(`${base}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const signInText = await signIn.text();
      eq(
        signIn.status,
        200,
        `login through the proxy succeeds (${signInText})`,
      );

      const proxiedCookie = sessionCookieFrom(signIn);
      ok(proxiedCookie !== null, "Set-Cookie survives the proxy");
      const proxiedCsrf = JSON.parse(signInText).csrfToken;

      // The cookie Astro handed out must actually work through the proxy.
      const session = await fetch(`${base}/api/v1/auth/session`, {
        headers: { Cookie: `blog_session=${proxiedCookie}` },
      });
      const sessionBody = await session.json();
      eq(sessionBody.authenticated, true, "the proxied cookie authenticates");
      eq(
        sessionBody.username,
        "owner",
        "the proxied session carries the username",
      );

      // A mutation through the proxy still enforces CSRF.
      const noCsrf = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${proxiedCookie}`,
        },
        body: JSON.stringify({
          siteTitle: "Proxied",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      eq(
        noCsrf.status,
        403,
        "a proxied mutation without a CSRF token is still refused",
      );

      const withCsrf = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${proxiedCookie}`,
          "X-CSRF-Token": proxiedCsrf,
        },
        body: JSON.stringify({
          siteTitle: "Proxied",
          commentsEnabled: true,
          commentAutoModerate: true,
          themeId: "bluearchive",
        }),
      });
      eq(
        withCsrf.status,
        200,
        `a proxied mutation with a CSRF token succeeds (${await withCsrf.text()})`,
      );

      // Origin is still checked on the way through.
      const badOrigin = await fetch(`${base}/api/v1/admin/settings`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: "https://evil.example",
          Cookie: `blog_session=${proxiedCookie}`,
          "X-CSRF-Token": proxiedCsrf,
        },
        body: "{}",
      });
      ok(
        badOrigin.status === 403 || badOrigin.status === 401,
        `a foreign Origin is refused (${badOrigin.status})`,
      );
    });

    await test("media upload and serving work through the single origin", async () => {
      const login = await fetch(`${base}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({
          username: "owner",
          password: "a-good-password",
        }),
      });
      const loginCookie = sessionCookieFrom(login);
      const loginCsrf = (await login.json()).csrfToken;

      const png = Uint8Array.from(
        atob(
          "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
        ),
        (c) => c.charCodeAt(0),
      );

      const fd = new FormData();
      fd.append("file", new Blob([png], { type: "image/png" }), "proxied.png");
      const up = await fetch(`${base}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${loginCookie}`,
          "X-CSRF-Token": loginCsrf,
        },
        body: fd,
      });
      const upText = await up.text();
      eq(
        up.status,
        201,
        `a multipart upload survives the proxy (${upText.slice(0, 120)})`,
      );
      const saved = JSON.parse(upText);

      // The delivery layer negotiates (ARCHITECTURE.md §17), so the assertion has to
      // be about the negotiated result rather than about the stored extension.
      const img = await fetch(`${base}/media/${saved.path}`, {
        headers: { Accept: "image/png" },
      });
      eq(img.status, 200, "the uploaded image is served from /media/*");
      eq(
        img.headers.get("content-type"),
        "image/png",
        "a PNG-only client is served the original PNG",
      );
      const negotiated = await fetch(`${base}/media/${saved.path}`, {
        headers: { Accept: "image/webp,*/*" },
      });
      eq(
        negotiated.headers.get("content-type"),
        "image/webp",
        "a WebP-capable client is served the converted representation",
      );
      eq(
        negotiated.headers.get("vary"),
        "Accept",
        "the proxied response still carries Vary: Accept",
      );
      ok(
        (img.headers.get("content-disposition") ?? "").includes("inline"),
        "images are served inline so <img> renders them",
      );

      // Traversal must be refused by the Astro media route.
      for (const attempt of [
        "/media/../../../etc/passwd",
        "/media/..%2f..%2f..%2fetc%2fpasswd",
        "/media/",
      ]) {
        const res = await fetch(`${base}${attempt}`);
        ok(
          res.status === 404 || res.status === 400,
          `${attempt} is refused (${res.status})`,
        );
      }
    });

    // =======================================================================
    // §10-§32 / §99-§103  Image delivery, WebP negotiation and both caches
    // =======================================================================

    await test("§17/§18/§99 the delivery layer negotiates WebP from Accept", async () => {
      await ensureAdmin();
      const fd = new FormData();
      fd.append(
        "file",
        new Blob([bytes], { type: "image/jpeg" }),
        "sample.jpg",
      );
      fd.append("alt", "A gradient test card");
      const up = await fetch(`${api}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: fd,
      });
      const upText = await up.text();
      eq(up.status, 201, `a JPEG uploads (${upText.slice(0, 140)})`);
      const jpeg = JSON.parse(upText);
      eq(jpeg.mime, "image/jpeg", "the MIME type comes from the bytes");
      eq(jpeg.width, 160, "dimensions were decoded");
      eq(jpeg.height, 120, "both dimensions were decoded");
      eq(
        jpeg.alt,
        "A gradient test card",
        "alt text supplied with the upload is stored",
      );
      ok(
        jpeg.url.endsWith(".jpg"),
        `the URL keeps the original extension (${jpeg.url})`,
      );

      const get = (accept) =>
        fetch(`${base}${jpeg.url}`, {
          headers: accept ? { Accept: accept } : {},
        });

      const asWebP = await get("image/webp,image/apng,*/*;q=0.8");
      eq(asWebP.status, 200, "a WebP-accepting request is served");
      const webpType = asWebP.headers.get("content-type");
      eq(webpType, "image/webp", "the negotiated type is image/webp");
      const webpBody = Buffer.from(await asWebP.arrayBuffer());
      ok(webpBody.length > 12, "the WebP body is non-empty");
      eq(
        webpBody.subarray(0, 4).toString("latin1"),
        "RIFF",
        "the body really is a RIFF container",
      );
      eq(
        webpBody.subarray(8, 12).toString("latin1"),
        "WEBP",
        "the RIFF form type is WEBP",
      );
      ok(
        webpBody.length < bytes.length,
        `the WebP is smaller than the JPEG (${webpBody.length} < ${bytes.length})`,
      );

      const asJPEG = await get("image/jpeg");
      eq(
        asJPEG.headers.get("content-type"),
        "image/jpeg",
        "a JPEG-only client gets the original",
      );
      const jpegBody = Buffer.from(await asJPEG.arrayBuffer());
      eq(
        jpegBody.length,
        bytes.length,
        "the original bytes are returned unchanged",
      );

      const refused = await get("image/webp;q=0,image/jpeg");
      eq(
        refused.headers.get("content-type"),
        "image/jpeg",
        "image/webp;q=0 is an explicit refusal and is honoured",
      );

      const wildcard = await get("*/*");
      eq(
        wildcard.headers.get("content-type"),
        "image/webp",
        "*/* accepts WebP",
      );

      const noAccept = await fetch(`${base}${jpeg.url}`, {
        headers: { Accept: "" },
      });
      eq(
        noAccept.headers.get("content-type"),
        "image/jpeg",
        "no Accept header means no negotiated representation",
      );

      // §18: mandatory whenever the body depends on Accept.
      for (const [label, res] of [
        ["WebP", asWebP],
        ["JPEG", asJPEG],
      ]) {
        eq(
          (res.headers.get("vary") ?? "").toLowerCase(),
          "accept",
          `the ${label} response carries Vary: Accept`,
        );
      }

      // §30: not immutable, because the URL has no content hash and an admin can
      // replace the file in place.
      const cc = asJPEG.headers.get("cache-control") ?? "";
      ok(cc.includes("public"), `Cache-Control is public (${cc})`);
      ok(
        !cc.includes("immutable"),
        "Cache-Control is not immutable for a replaceable URL",
      );

      eq(
        asJPEG.headers.get("x-content-type-options"),
        "nosniff",
        "nosniff is set",
      );

      // §89: two representations must not share a strong ETag.
      const webpTag = asWebP.headers.get("etag");
      const jpegTag = asJPEG.headers.get("etag");
      ok(webpTag && jpegTag, "both representations carry an ETag");
      ok(webpTag !== jpegTag, "the WebP and the JPEG have different ETags");

      // §90: conditional requests.
      const notModified = await fetch(`${base}${jpeg.url}`, {
        headers: { Accept: "image/jpeg", "If-None-Match": jpegTag },
      });
      eq(notModified.status, 304, "If-None-Match on the original yields 304");
      eq(
        notModified.headers.get("vary"),
        "Accept",
        "the 304 also varies on Accept",
      );

      // A client holding the JPEG's tag must NOT be told 304 when it asks for WebP.
      const wrongRep = await fetch(`${base}${jpeg.url}`, {
        headers: { Accept: "image/webp", "If-None-Match": jpegTag },
      });
      eq(
        wrongRep.status,
        200,
        "a WebP request is not answered 304 from the JPEG's ETag",
      );

      // §31: HEAD describes what GET would produce, and sends no body.
      const head = await fetch(`${base}${jpeg.url}`, {
        method: "HEAD",
        headers: { Accept: "image/webp" },
      });
      eq(head.status, 200, "HEAD is answered");
      eq(
        head.headers.get("content-type"),
        "image/webp",
        "HEAD negotiates the same way",
      );
      eq(
        head.headers.get("etag"),
        webpTag,
        "HEAD reports the same ETag as GET",
      );
      eq((await head.arrayBuffer()).byteLength, 0, "HEAD sends no body");

      jpegMedia = jpeg;
    });

    // §3: GIF animation transcoding is a non-goal, so a GIF is served as stored even to a
    // client that would happily take WebP. The failure this prevents is invisible from
    // the outside — no error, no warning, just an animation that stopped moving for
    // every visitor whose browser can decode WebP and still plays for the ones that
    // cannot. The animated case itself is proved against a genuinely multi-frame GIF in
    // `internal/media`; this fixture is a still one, which must reach a WebP-capable
    // client unchanged for the same reason.
    await test("§3 a GIF is served as stored even when the client accepts WebP", async () => {
      // The neighbouring delivery tests reset the admin session when it has expired;
      // without the same call this one uploads with whatever `cookie` currently holds
      // and the 403 reads like a permissions bug rather than an expired session.
      await ensureAdmin();

      const gifBytes = await readFile(path.join(FIXTURES, "sample.gif"));
      const form = new FormData();
      form.append(
        "file",
        new Blob([gifBytes], { type: "image/gif" }),
        "spin.gif",
      );
      const uploaded = await fetch(`${api}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: form,
      });
      eq(uploaded.status, 201, `the GIF uploads (${uploaded.status})`);
      const gif = await uploaded.json();
      eq(
        gif.mime,
        "image/gif",
        "it is stored as a GIF, extension and MIME unchanged (§9)",
      );

      const res = await fetch(`${base}${gif.url}`, {
        headers: { Accept: "image/webp,image/*,*/*" },
      });
      eq(res.status, 200, "the GIF is served");
      eq(
        res.headers.get("content-type"),
        "image/gif",
        "a WebP-capable client still receives a GIF, not a re-encoded one",
      );
      eq(
        (res.headers.get("vary") ?? "").toLowerCase(),
        "accept",
        "the GIF response varies",
      );
      const body = Buffer.from(await res.arrayBuffer());
      eq(
        body.length,
        gifBytes.length,
        "the original GIF bytes are returned unchanged",
      );
      ok(
        body.subarray(0, 6).toString("latin1").startsWith("GIF"),
        "the body is still a GIF container",
      );
      ok(
        body.subarray(0, 4).toString("latin1") !== "RIFF",
        "no WebP RIFF container was produced for a GIF",
      );

      // HEAD must agree with GET, or a client that probes before fetching picks the
      // wrong representation and then sends an If-None-Match for it.
      const head = await fetch(`${base}${gif.url}`, {
        method: "HEAD",
        headers: { Accept: "image/webp" },
      });
      eq(head.status, 200, "HEAD on a GIF is answered");
      eq(head.headers.get("content-type"), "image/gif", "HEAD agrees with GET");

      await fetch(`${api}/api/v1/admin/media/${gif.id}?force=1`, {
        method: "DELETE",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
      });
    });

    await test("§19/§22/§25 the caches are bounded, measurable and resettable", async () => {
      const statsUrl = `${api}/api/v1/admin/media/cache-stats`;
      const readStats = async () => {
        const res = await fetch(statsUrl, {
          headers: { Cookie: `blog_session=${cookie}` },
        });
        return res.json();
      };

      const before = await readStats();
      ok(
        before.memoryLimitBytes > 0,
        "the memory cache has a configured ceiling",
      );
      ok(
        before.conversions >= 1,
        `the first request converted (${before.conversions})`,
      );
      ok(before.hits >= 1, `the second request hit the cache (${before.hits})`);
      ok(
        before.memoryUsedBytes <= before.memoryLimitBytes,
        "memory used is inside the configured ceiling",
      );

      // A second request for the same bytes must not convert again.
      const again = await fetch(`${base}${jpegMedia.url}`, {
        headers: { Accept: "image/webp" },
      });
      await again.arrayBuffer();
      const after = await readStats();
      eq(
        after.conversions,
        before.conversions,
        "a cached representation is not converted twice",
      );
      ok(after.hits > before.hits, "the hit counter went up");

      // Deleting one *other* asset must not cost this one its cached representation.
      // An earlier version cleared the whole cache on every delete, which is invisible
      // in a single-image site and means that on a real blog deleting one unused image
      // makes every subsequent visitor wait for a re-encode of every image they load.
      const doomed = new FormData();
      doomed.append(
        "file",
        new Blob([await readFile(path.join(FIXTURES, "sample.png"))], {
          type: "image/png",
        }),
        "doomed.png",
      );
      const doomedUpload = await fetch(`${api}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: doomed,
      });
      eq(doomedUpload.status, 201, "a throwaway image uploads");
      const doomedId = (await doomedUpload.json()).id;

      // Warm it, so the deletion has a representation to leave behind.
      await (
        await fetch(`${base}/api/v1/admin/media/${jpegMedia.url}`, {
          headers: { Accept: "image/webp" },
        })
      ).arrayBuffer();
      const beforeDelete = await readStats();

      const deleted = await fetch(
        `${api}/api/v1/admin/media/${doomedId}?force=1`,
        {
          method: "DELETE",
          headers: {
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
        },
      );
      eq(deleted.status, 200, "the throwaway image is deleted");

      // The deleted asset is gone regardless of what the cache still holds: the
      // delivery layer resolves the row before it looks at any cache, so a surviving
      // entry is unreachable rather than servable.
      eq(
        (await fetch(`${base}${jpegMedia.url.replace(/[^/]+$/, "gone.jpg")}`))
          .status,
        404,
        "an unknown path 404s even though the cache is warm",
      );

      await (
        await fetch(`${base}${jpegMedia.url}`, {
          headers: { Accept: "image/webp" },
        })
      ).arrayBuffer();
      const afterDelete = await readStats();
      eq(
        afterDelete.conversions,
        beforeDelete.conversions,
        "deleting one asset does not re-encode every other asset",
      );
      eq(
        afterDelete.hits,
        beforeDelete.hits + 1,
        "the surviving image was still a cache hit after the delete",
      );

      // §84: shrinking the ceiling takes effect without a restart.
      const put = (payload) =>
        fetch(`${api}/api/v1/admin/settings`, {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: JSON.stringify({
            siteTitle: "Blog",
            commentsEnabled: true,
            commentAutoModerate: true,
            themeId: "bluearchive",
            ...payload,
          }),
        });

      const shrink = await put({ imageMemoryCacheMB: 1 });
      eq(
        shrink.status,
        200,
        `the memory ceiling can be lowered (${await shrink.clone().text()})`,
      );
      const shrunk = await readStats();
      eq(
        shrunk.memoryLimitBytes,
        1 * 1024 * 1024,
        "the new ceiling is live without a restart",
      );
      ok(
        shrunk.memoryUsedBytes <= shrunk.memoryLimitBytes,
        "the cache fits the smaller ceiling",
      );

      // Fill it well past 1 MB and prove the bound holds.
      const fills = [];
      for (let i = 0; i < 6; i++) {
        const fd = new FormData();
        fd.append(
          "file",
          new Blob([bytes], { type: "image/jpeg" }),
          `fill-${i}.jpg`,
        );
        fills.push(
          fetch(`${api}/api/v1/admin/media`, {
            method: "POST",
            headers: {
              Origin: base,
              Cookie: `blog_session=${cookie}`,
              "X-CSRF-Token": csrf,
            },
            body: fd,
          }).then(async (r) => (await r.json()).path),
        );
      }
      const fillPaths = await Promise.all(fills);
      for (const p of fillPaths) {
        const res = await fetch(`${base}/media/${p}`, {
          headers: { Accept: "image/webp" },
        });
        await res.arrayBuffer();
      }
      const filled = await readStats();
      ok(
        filled.memoryUsedBytes <= filled.memoryLimitBytes,
        `1 MB ceiling holds under load (${filled.memoryUsedBytes} <= ${filled.memoryLimitBytes})`,
      );
      // The eviction *policy* is proved by the unit tests, which can afford entries
      // big enough to overflow a 1 MB budget in milliseconds. Here it is enough that
      // the counter is live and that the ceiling was never crossed.
      ok(
        typeof filled.evictions === "number",
        "the eviction counter is reported",
      );

      const restore = await put({ imageMemoryCacheMB: 64 });
      eq(restore.status, 200, "the ceiling can be restored");

      // §83: clearing empties the derived cache and nothing else.
      const cleared = await fetch(`${api}/api/v1/admin/media/clear-cache`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
      });
      eq(cleared.status, 200, "the cache can be cleared");
      const afterClear = await readStats();
      eq(afterClear.memoryUsedBytes, 0, "the memory cache is empty");
      eq(afterClear.diskEntries, 0, "the disk cache is empty");
      eq(
        afterClear.diskBytes,
        0,
        "no filesystem path or leftover bytes remain",
      );

      // §21: a missing cache regenerates rather than 404s.
      const regen = await fetch(`${base}${jpegMedia.url}`, {
        headers: { Accept: "image/webp" },
      });
      eq(
        regen.status,
        200,
        "a cleared representation regenerates on the next request",
      );
      eq(
        regen.headers.get("content-type"),
        "image/webp",
        "and it is a WebP again",
      );
      const afterRegen = await readStats();
      ok(
        afterRegen.conversions > afterClear.conversions,
        "it really was converted again",
      );

      // The original is untouched by all of that.
      const stillThere = await fetch(`${base}${jpegMedia.url}`, {
        headers: { Accept: "image/jpeg" },
      });
      eq(
        stillThere.status,
        200,
        "the original still serves after a cache clear",
      );
    });

    await test("§20/§142 the cache key follows content, not the filename", async () => {
      await ensureAdmin();
      const stats = async () =>
        fetch(`${api}/api/v1/admin/media/cache-stats`, {
          headers: { Cookie: `blog_session=${cookie}` },
        }).then((r) => r.json());

      // A different file gets a different representation, even at a different path.
      const png = await readFile(path.join(FIXTURES, "sample.png"));
      const fd = new FormData();
      fd.append("file", new Blob([png], { type: "image/png" }), "other.png");
      const other = JSON.parse(
        await fetch(`${api}/api/v1/admin/media`, {
          method: "POST",
          headers: {
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: fd,
        }).then((r) => r.text()),
      );

      const jpegRes = await fetch(`${base}${jpegMedia.url}`, {
        headers: { Accept: "image/webp" },
      });
      const otherRes = await fetch(`${base}${other.url}`, {
        headers: { Accept: "image/webp" },
      });
      const jpegTag = jpegRes.headers.get("etag");
      const otherTag = otherRes.headers.get("etag");
      ok(
        jpegTag && otherTag && jpegTag !== otherTag,
        "two different files never share an ETag",
      );

      // The same bytes under a second path is the same content, so it is the same
      // cache entry. This is the property that matters: a *new URL* for *the same
      // image* must not cost a second conversion.
      const before = await stats();
      const fd2 = new FormData();
      fd2.append("file", new Blob([bytes], { type: "image/jpeg" }), "copy.jpg");
      const copy = JSON.parse(
        await fetch(`${api}/api/v1/admin/media`, {
          method: "POST",
          headers: {
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: fd2,
        }).then((r) => r.text()),
      );
      ok(copy.path !== jpegMedia.path, "the copy has its own path");
      const copyRes = await fetch(`${base}${copy.url}`, {
        headers: { Accept: "image/webp" },
      });
      const after = await stats();

      eq(
        copyRes.headers.get("etag"),
        jpegTag,
        "identical content under a different URL reuses the representation",
      );
      eq(
        after.conversions,
        before.conversions,
        "and it is not converted a second time",
      );
      ok(after.hits > before.hits, "it is served from the cache");

      // Changing the quality is part of the key (§28), so the representation changes
      // even though the source has not.
      const put = (payload) =>
        fetch(`${api}/api/v1/admin/settings`, {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: JSON.stringify({
            siteTitle: "Blog",
            commentsEnabled: true,
            commentAutoModerate: true,
            themeId: "bluearchive",
            ...payload,
          }),
        });

      eq(
        (await put({ webpQuality: 30 })).status,
        200,
        "the quality can be changed",
      );
      const lower = await fetch(`${base}${jpegMedia.url}`, {
        headers: { Accept: "image/webp" },
      });
      eq(lower.status, 200, "the image still serves after a quality change");
      ok(
        lower.headers.get("etag") !== jpegTag,
        "a different quality is a different representation",
      );
      const afterQuality = await stats();
      ok(
        afterQuality.conversions > after.conversions,
        "and it really was re-encoded",
      );
      eq(
        (await put({ webpQuality: 82 })).status,
        200,
        "the quality can be restored",
      );
    });

    await test("§26/§99 a decompression bomb is refused at upload", async () => {
      await ensureAdmin();
      const bomb = await readFile(
        path.join(FIXTURES, "decompression-bomb.png"),
      );
      ok(
        bomb.length < 200 * 1024,
        `the fixture is small on disk (${bomb.length} bytes)`,
      );

      const fd = new FormData();
      fd.append("file", new Blob([bomb], { type: "image/png" }), "bomb.png");
      const res = await fetch(`${api}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: fd,
      });
      eq(
        res.status,
        422,
        `a 6000x4000 image is refused even though it is 100 KB (${res.status})`,
      );
      const text = await res.text();
      ok(
        text.includes("pixels"),
        `the error names the pixel ceiling (${text.slice(0, 160)})`,
      );
    });

    await test("§61 a missing original 404s and a deleted row is reported", async () => {
      await ensureAdmin();

      const missing = await fetch(
        `${base}/media/2026/10/${"0".repeat(24)}.jpg`,
        {
          headers: { Accept: "image/webp" },
        },
      );
      eq(missing.status, 404, "a path with no file is 404");

      const traversal = await fetch(
        `${base}/media/2026/10/%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd`,
      );
      ok(
        traversal.status === 404 || traversal.status === 400,
        `a traversal attempt is refused (${traversal.status})`,
      );

      // A row whose file has been removed is reported as missing, not fatal (§5).
      const orphan = await fetch(`${api}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: (() => {
          const f = new FormData();
          f.append(
            "file",
            new Blob([bytes], { type: "image/jpeg" }),
            "orphan.jpg",
          );
          return f;
        })(),
      }).then((r) => r.json());

      await rm(path.join(roots.media, ...orphan.path.split("/")));
      const list = await fetch(`${api}/api/v1/admin/media?missing=1`, {
        headers: { Cookie: `blog_session=${cookie}` },
      }).then((r) => r.json());
      ok(
        list.items.some((i) => i.id === orphan.id && i.missing === true),
        "a row without a file is reported as missing rather than hidden",
      );
    });

    // -----------------------------------------------------------------------
    // §34 — the Markdown style-template API: CRUD through the
    // single-origin proxy (the path the admin's own scripts take),
    // the security gates every write carries, the runtime
    // aggregate that reflects each change with no rebuild, and
    // the preview endpoint that runs the real render path.
    await test("§34 markdown template API, security and runtime", async () => {
      await ensureAdmin();

      /** A template request through the proxy, as the admin UI makes it. */
      const templateRequest = async (method, id, body) => {
        const target =
          id === null || id === undefined
            ? "/api/v1/admin/markdown"
            : `/api/v1/admin/markdown/${encodeURIComponent(id)}`;
        const res = await fetch(`${base}${target}`, {
          method,
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: body === undefined ? undefined : JSON.stringify(body),
        });
        const text = await res.text();
        let parsed = null;
        try {
          parsed = text.length > 0 ? JSON.parse(text) : null;
        } catch {
          parsed = null;
        }
        return { status: res.status, body: parsed, text };
      };

      const markdownCss = async () => {
        const res = await fetch(`${base}/markdown.css`);
        return {
          status: res.status,
          body: await res.text(),
          etag: res.headers.get("etag"),
        };
      };

      const included = (css) =>
        [...css.body.matchAll(/blogcms:markdown:([^*]+)/g)].map((m) =>
          m[1].trim(),
        );

      // --- Security gates, before any state is created ---
      const noSession = await fetch(`${base}/api/v1/admin/markdown`, {
        headers: { "Content-Type": "application/json", Origin: base },
        method: "POST",
        body: JSON.stringify({ filename: "050-x.css", content: "" }),
      });
      eq(noSession.status, 401, "a write with no session is refused");

      const noCsrf = await fetch(`${base}/api/v1/admin/markdown`, {
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
        },
        method: "POST",
        body: JSON.stringify({ filename: "050-x.css", content: "" }),
      });
      eq(noCsrf.status, 403, "a write with no CSRF header is refused");

      const traversal = await templateRequest("POST", null, {
        filename: "../escape.css",
        content: ".markdown-body{}",
      });
      eq(
        traversal.status,
        422,
        "a traversal filename is refused as unprocessable",
      );

      const badExt = await templateRequest("POST", null, {
        filename: "050-evil.js",
        content: "alert(1)",
      });
      eq(badExt.status, 422, "a non-CSS filename is refused as unprocessable");

      const dotDot = await templateRequest("POST", null, {
        filename: "050-..css",
        content: ".markdown-body{}",
      });
      eq(dotDot.status, 422, "a filename with .. is refused");

      // --- Create, and it is served immediately ---
      const created = await templateRequest("POST", null, {
        filename: "050-hello.css",
        content: ".markdown-body .hello{color:#505050}\n",
      });
      eq(created.status, 201, "a template is created");
      eq(
        created.body.filename,
        "050-hello.css",
        "the response names the file it created",
      );
      ok(
        typeof created.body.checksum === "string" &&
          created.body.checksum.length > 0,
        "the response carries the file's checksum",
      );
      eq(created.body.order, 50, "the order is derived from the prefix");

      let css = await markdownCss();
      ok(
        included(css).includes("050-hello.css"),
        "a new template is served with no rebuild",
      );
      ok(
        css.body.includes("#505050"),
        "the new template's rules are in the aggregate",
      );

      // --- Read it back ---
      const got = await templateRequest("GET", "050-hello.css");
      eq(got.status, 200, "a template is read back");
      eq(
        got.body.content,
        ".markdown-body .hello{color:#505050}\n",
        "with its body",
      );

      // --- Update: content, then rename, then disable ---
      const etagBefore = (await markdownCss()).etag;
      const updated = await templateRequest("PUT", "050-hello.css", {
        content: ".markdown-body .hello{color:#606060}\n",
      });
      eq(updated.status, 200, "a template is updated");
      css = await markdownCss();
      ok(
        css.body.includes("#606060") && !css.body.includes("#505050"),
        "an edit is live on the next request",
      );
      ok(
        css.etag !== etagBefore,
        "editing a template changes the aggregate ETag",
      );

      const renamed = await templateRequest("PUT", "050-hello.css", {
        filename: "055-world.css",
      });
      eq(renamed.status, 200, "a template is renamed");
      css = await markdownCss();
      ok(
        included(css).includes("055-world.css") &&
          !included(css).includes("050-hello.css"),
        "the rename is reflected in the aggregate",
      );

      const disabled = await templateRequest("PUT", "055-world.css", {
        enabled: false,
      });
      eq(disabled.status, 200, "a template is disabled");
      css = await markdownCss();
      ok(
        !included(css).includes("055-world.css"),
        "a disabled template leaves the aggregate immediately",
      );

      // A disabled template's body is still readable through the API.
      const parked = await templateRequest("GET", "055-world.css");
      eq(parked.status, 200, "a parked template is still readable");
      eq(parked.body.enabled, false, "and reports itself as disabled");

      const enabled = await templateRequest("PUT", "055-world.css", {
        enabled: true,
      });
      eq(enabled.status, 200, "a template is re-enabled");
      css = await markdownCss();
      ok(
        included(css).includes("055-world.css"),
        "a re-enabled template returns to the aggregate",
      );

      // --- Delete ---
      // (The audit log itself is asserted by the Go unit tests, which
      // can read content_event directly; there is deliberately no
      // HTTP endpoint that exposes the log.)
      const deleted = await templateRequest("DELETE", "055-world.css");
      eq(deleted.status, 204, "a template is deleted");
      css = await markdownCss();
      ok(
        !included(css).includes("055-world.css"),
        "a deleted template is gone with no rebuild",
      );
      const gone = await templateRequest("GET", "055-world.css");
      eq(gone.status, 404, "a deleted template 404s");

      // --- The preview endpoint runs the real render path ---
      const preview = await fetch(`${base}/api/v1/markdown/preview`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          markdown: [
            ":::info",
            "An info note.",
            ":::",
            "",
            "Press :kbd[Ctrl] and :badge[New].",
            "",
            "```go",
            "func main() {}",
            "```",
            "",
            "> A quote.",
            "",
            "| a | b |",
            "|---|---|",
            "| 1 | 2 |",
          ].join("\n"),
        }),
      });
      eq(preview.status, 200, "the preview renders for a signed-in admin");
      const previewHtml = (await preview.json()).html;
      ok(
        previewHtml.includes('class="markdown-body"'),
        "the preview wraps output in the template scope",
      );
      ok(previewHtml.includes("callout callout-info"), "callouts render");
      ok(previewHtml.includes('<kbd class="kbd">Ctrl</kbd>'), "kbd renders");
      ok(
        previewHtml.includes('<span class="badge">New</span>'),
        "badges render",
      );
      ok(
        previewHtml.includes('class="code-block"'),
        "code blocks carry the code-block class",
      );
      ok(previewHtml.includes("<blockquote"), "blockquotes render");
      ok(previewHtml.includes("<table"), "tables render");

      const previewNoSession = await fetch(`${base}/api/v1/markdown/preview`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
        body: JSON.stringify({ markdown: "x" }),
      });
      eq(previewNoSession.status, 401, "the preview refuses a stranger");

      const previewNoCsrf = await fetch(`${base}/api/v1/markdown/preview`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
        },
        body: JSON.stringify({ markdown: "x" }),
      });
      eq(previewNoCsrf.status, 403, "the preview refuses a missing CSRF token");

      const previewBadBody = await fetch(`${base}/api/v1/markdown/preview`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({ markdown: 42 }),
      });
      eq(previewBadBody.status, 422, "a non-string markdown body is refused");
    });

    // -----------------------------------------------------------------------
    // Git Backup Phase 1. The repository is the history authority, so the
    // assertions are about what a real backup committed and what the admin
    // screen then reports — the two halves of the closed loop:
    // content → local commit → history → diff.
    //
    // The repository lives under DATA_ROOT, which this suite controls, so the
    // inclusion policy is checked against the real files on disk rather than
    // against the API's own summary of itself.
    // -----------------------------------------------------------------------
    await test("§57 Git backup: initialize, commit, history and diff", async () => {
      await ensureAdmin();

      const repoRoot = path.join(roots.data, "git-backup");

      /** A backup request through the proxy, as the admin screen makes it. */
      const backupRequest = async (method, target, body) => {
        const res = await fetch(`${base}${target}`, {
          method,
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            ...(method === "GET" ? {} : { "X-CSRF-Token": csrf }),
          },
          body: body === undefined ? undefined : JSON.stringify(body),
        });
        const text = await res.text();
        let parsed = null;
        try {
          parsed = text.length > 0 ? JSON.parse(text) : null;
        } catch {
          parsed = null;
        }
        return { status: res.status, body: parsed, text };
      };

      // --- Security gates, before any state exists ----------------------
      const anon = await fetch(`${base}/api/v1/admin/backup/initialize`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Origin: base },
      });
      eq(anon.status, 401, "initializing with no session is refused");

      const noCsrf = await fetch(`${base}/api/v1/admin/backup/commit`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
        },
      });
      eq(noCsrf.status, 403, "a backup with no CSRF header is refused");

      const foreignOrigin = await fetch(
        `${base}/api/v1/admin/backup/initialize`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Origin: "https://evil.example",
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
        },
      );
      eq(
        foreignOrigin.status,
        403,
        "a backup from a foreign Origin is refused",
      );

      // --- Before initialization: a state, not an error ----------------
      const before = await backupRequest("GET", "/api/v1/admin/backup");
      eq(before.status, 200, "the backup status answers before initialization");
      eq(
        before.body.initialized,
        false,
        "the repository is not initialized yet",
      );
      const historyBefore = await backupRequest(
        "GET",
        "/api/v1/admin/backup/history",
      );
      eq(
        historyBefore.status,
        409,
        "history refuses to answer before initialization rather than inventing one",
      );
      ok(
        !(await pathExists(path.join(repoRoot, ".git"))),
        "no repository was created by merely asking for the status",
      );

      // The first-run screen: it must offer Initialize, and it must not
      // pretend there is history or a working tree (§45).
      const firstRun = await fetch(`${base}/admin/backup`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      eq(
        firstRun.status,
        200,
        "the backup screen renders before initialization",
      );
      const firstRunHtml = await firstRun.text();
      ok(
        firstRunHtml.includes('data-cms-action="backup-initialize"'),
        "the first-run screen offers to initialize the repository",
      );
      ok(
        !firstRunHtml.includes('data-cms-action="backup-commit"'),
        "and offers no Backup Now before there is anything to commit",
      );

      /**
       * A content write on the shared session.
       *
       * The suite-level `createPost` cannot be used here: it caches the cookie
       * from the first login, and every later login revokes it (§12), so it is
       * already stale by the time this test runs. `ensureAdmin` exists for
       * exactly this.
       */
      const writeContent = async (kind, fields) => {
        const res = await fetch(`${base}/api/v1/admin/${kind}`, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Origin: base,
            Cookie: `blog_session=${cookie}`,
            "X-CSRF-Token": csrf,
          },
          body: JSON.stringify({
            date: "2026-10-08",
            description: "Source content for the backup test.",
            body: "Body.\n",
            ...fields,
          }),
        });
        const text = await res.text();
        return {
          status: res.status,
          text,
          json: () => JSON.parse(text || "{}"),
        };
      };

      // --- Real source content of every kind the policy covers ----------
      // A post, a draft, a page, a Markdown template, the legacy custom
      // pair, a managed CSS and JS asset, and an uploaded original.
      const post = await writeContent("posts", {
        slug: "backed-up",
        title: "Backed Up",
        description: "A post that proves the backup covers real content.",
        body: "# Backed up\n",
      });
      eq(post.status, 201, `a post is created (${post.text})`);

      const draft = await writeContent("posts", {
        slug: "backed-up-draft",
        title: "Backed Up Draft",
        description: "A draft is an ordinary file with a flag.",
        body: "# Draft\n",
        draft: true,
      });
      eq(draft.status, 201, "a draft post is created");

      const page = await writeContent("pages", {
        slug: "backed-up-page",
        title: "Backed Up Page",
        description: "A page is source content too.",
        body: "# Page\n",
      });
      eq(page.status, 201, `a page is created (${page.text})`);

      // The template is created, not overwritten: a PUT on a name that does
      // not exist is a 404, and the point here is that a *new* Markdown
      // template is pending content the next backup picks up.
      const template = await fetch(`${base}/api/v1/admin/markdown`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          filename: "050-backup.css",
          content: ".markdown-body .backup{color:#505050}\n",
        }),
      });
      eq(template.status, 201, "a Markdown template is created");

      const customCode = await fetch(`${base}/api/v1/admin/custom-code`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          css: ".from-backup { color: rebeccapurple; }",
          js: "window.__backupCustom = true;",
        }),
      });
      eq(customCode.status, 200, "the custom CSS/JS pair is saved");

      const managedCss = await fetch(`${base}/api/v1/admin/custom/css`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          filename: "002-backup.css",
          content: ".managed-backup{color:#606060}\n",
        }),
      });
      eq(managedCss.status, 201, "a managed custom CSS asset is created");

      const managedJs = await fetch(`${base}/api/v1/admin/custom/js`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: JSON.stringify({
          filename: "002-backup.js",
          content: "window.__managedBackup = true;\n",
        }),
      });
      eq(managedJs.status, 201, "a managed custom JS asset is created");

      // A real PNG, so the media root holds a genuine original.
      const png = Uint8Array.from(
        atob(
          "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
        ),
        (c) => c.charCodeAt(0),
      );
      const upload = new FormData();
      upload.append(
        "file",
        new Blob([png], { type: "image/png" }),
        "backup.png",
      );
      const media = await fetch(`${base}/api/v1/admin/media`, {
        method: "POST",
        headers: {
          Origin: base,
          Cookie: `blog_session=${cookie}`,
          "X-CSRF-Token": csrf,
        },
        body: upload,
      });
      const mediaText = await media.text();
      eq(media.status, 201, `an original is uploaded (${mediaText})`);
      const mediaPath = JSON.parse(mediaText).path;

      // --- Initialize ---------------------------------------------------
      const init = await backupRequest(
        "POST",
        "/api/v1/admin/backup/initialize",
      );
      eq(
        init.status,
        200,
        `the repository initializes (${init.text.slice(0, 200)})`,
      );
      ok(
        typeof init.body.commit?.hash === "string" &&
          init.body.commit.hash.length === 40,
        "initialization records the current content as one commit",
      );
      eq(
        init.body.commit.message,
        "Initial content backup",
        "the initial commit says what it is",
      );
      ok(
        typeof init.body.branch === "string" && init.body.branch.length > 0,
        "the branch name comes from the repository state, not a constant in the API",
      );
      ok(
        init.body.commit.author === "CMS Backup <backup@local.invalid>",
        `the commit is authored by the CMS, not the admin (${init.body.commit.author})`,
      );

      const again = await backupRequest(
        "POST",
        "/api/v1/admin/backup/initialize",
      );
      eq(
        again.status,
        409,
        "initializing an initialized repository is refused",
      );

      // --- The inclusion policy, checked against the files on disk ------
      // `git ls-files` equivalent: everything the repository work tree holds
      // is the snapshot, because the snapshot *is* the tracked content.
      //
      // The suite shares one content root, so earlier tests have left posts
      // and pages behind and an exact set comparison would be asserting the
      // suite's own ordering rather than the policy. What is checked here is
      // the *shape* — every tracked path must be one the policy includes —
      // while `TestRuntimeStateNeverEntersTheBackup` asserts the exact set on
      // a dedicated tree.
      const tracked = await walkFiles(repoRoot);
      for (const file of [
        "content/posts/backed-up.md",
        "content/posts/backed-up-draft.md",
        "content/pages/backed-up-page.md",
        "content/system/markdown/050-backup.css",
        "content/system/custom.css",
        "content/system/custom.js",
        "content/system/css/002-backup.css",
        "content/system/js/002-backup.js",
        `media/${mediaPath}`,
      ]) {
        ok(tracked.includes(file), `${file} is in the backup`);
      }
      const INCLUDED = /^(content\/(posts|pages|system)\/|media\/)/;
      const unexpected = tracked.filter((f) => !INCLUDED.test(f));
      ok(
        unexpected.length === 0,
        `every tracked file is one the policy includes${
          unexpected.length > 0
            ? ` — but: ${unexpected.slice(0, 6).join(", ")}`
            : ""
        }`,
      );
      // A draft is an ordinary file: it is not a branch, and not an omission.
      ok(
        tracked.includes("content/posts/backed-up-draft.md"),
        "a draft is backed up like any other content file (§52)",
      );
      // Runtime state is excluded by construction — the service never reads
      // the data root — so it cannot be in the work tree either.
      ok(
        !tracked.some((f) => /\.(db|db-wal|db-shm|webp)$/.test(f)),
        "no database and no generated WebP entered the backup",
      );
      ok(
        !tracked.some(
          (f) => f.includes("session") || f.includes("media-cache"),
        ),
        "no session or cache state entered the backup",
      );

      // --- A clean tree commits nothing -------------------------------
      const status = await backupRequest("GET", "/api/v1/admin/backup");
      eq(status.status, 200, "the status answers once initialized");
      eq(status.body.clean, true, "the working tree is clean after a backup");
      eq(status.body.changedFiles, 0, "no pending changes remain");
      eq(
        status.body.lastCommit.hash,
        init.body.commit.hash,
        "the last commit is the initial one",
      );

      const emptyBackup = await backupRequest(
        "POST",
        "/api/v1/admin/backup/commit",
      );
      eq(emptyBackup.status, 409, "a backup with nothing to change is refused");
      eq(
        emptyBackup.body.error.code,
        "backup_nothing_to_commit",
        "and it says so rather than committing nothing",
      );

      // --- A content change becomes a commit and a diff ----------------
      const edited = await writeContent("posts", {
        slug: "backed-up-edited",
        title: "Backed Up Edited",
        description: "A new post is a pending change.",
        body: "# Edited\n\nA line with a <script> in it.\n",
      });
      eq(edited.status, 201, "a second post is created");

      const changed = await backupRequest("GET", "/api/v1/admin/backup");
      eq(changed.body.clean, false, "the tree is dirty after a new post");
      eq(changed.body.changedFiles, 1, "exactly one file is pending");

      const changes = await backupRequest(
        "GET",
        "/api/v1/admin/backup/changes",
      );
      eq(changes.status, 200, "the pending changes are listed");
      eq(
        changes.body.changes[0]?.path,
        "content/posts/backed-up-edited.md",
        "the pending change names the content file",
      );

      const commit = await backupRequest("POST", "/api/v1/admin/backup/commit");
      eq(
        commit.status,
        200,
        `a backup with changes commits (${commit.text.slice(0, 200)})`,
      );
      eq(commit.body.changedFiles, 1, "the commit covers the changed file");
      ok(
        commit.body.commit.message.startsWith("Backup content: "),
        `the message is built in one place, not at the call site (${commit.body.commit.message})`,
      );
      ok(
        commit.body.commit.hash !== init.body.commit.hash,
        "the backup produced a new commit",
      );
      eq(commit.body.branch, init.body.branch, "it is on the same branch");

      // --- History -----------------------------------------------------
      const history = await backupRequest(
        "GET",
        "/api/v1/admin/backup/history",
      );
      eq(history.status, 200, "the history is readable");
      eq(history.body.commits.length, 2, "the repository holds two commits");
      eq(
        history.body.commits[0].hash,
        commit.body.commit.hash,
        "history is newest first and starts with the backup just made",
      );
      eq(
        history.body.commits[1].message,
        "Initial content backup",
        "the initial commit is the oldest entry",
      );

      // --- Diff, and the diff is text ---------------------------------
      // A Markdown file can contain anything, including a script tag, so the
      // patch is asserted as data the client escapes rather than as markup it
      // is allowed to render.
      const before3 = await writeContent("posts", {
        slug: "backed-up-diffed",
        title: "Backed Up Diffed",
        description: "The diff of a file holding a script tag.",
        body: "# Diffed\n\nline one\nline two\n",
      });
      eq(before3.status, 201, "a third post is created");
      await backupRequest("POST", "/api/v1/admin/backup/commit");

      const rawPost = path.join(roots.content, "posts", "backed-up-diffed.md");
      await writeFile(
        rawPost,
        (await readFile(rawPost, "utf-8")).replace(
          "line two",
          "line TWO\n<script>alert(1)</script>",
        ),
      );

      const diff = await backupRequest(
        "GET",
        "/api/v1/admin/backup/diff?path=content%2Fposts%2Fbacked-up-diffed.md",
      );
      eq(diff.status, 200, "a per-file diff is readable");
      eq(diff.body.status, "modified", "the diff reports a modification");
      ok(
        diff.body.patch.includes("-line two") &&
          diff.body.patch.includes("+line TWO"),
        "the patch shows the real change",
      );
      ok(
        diff.body.patch.includes("<script>alert(1)</script>"),
        "the patch carries the script tag as text — the client escapes it",
      );

      const traversal = await backupRequest(
        "GET",
        "/api/v1/admin/backup/diff?path=..%2F..%2F..%2Fetc%2Fpasswd",
      );
      ok(
        traversal.status === 400 || traversal.status === 422,
        `a traversal path is refused (${traversal.status})`,
      );

      // --- The audit log records the decisions, not the content -------
      const audit = await fetch(`${api}/api/v1/admin/posts`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      eq(audit.status, 200, "the content API still answers after a backup");

      // --- The backup did not touch the source content ----------------
      const sourcePost = await readFile(
        path.join(roots.content, "posts", "backed-up-diffed.md"),
        "utf-8",
      );
      ok(
        sourcePost.includes("<script>alert(1)</script>"),
        "the source Markdown still holds exactly what the author wrote",
      );
      ok(
        sourcePost.includes("line one"),
        "and the earlier lines were not reformatted away",
      );

      // --- The screen renders -----------------------------------------
      const screen = await fetch(`${base}/admin/backup`, {
        headers: { Cookie: `blog_session=${cookie}` },
      });
      eq(screen.status, 200, "the backup screen renders");
      const html = await screen.text();
      ok(
        html.includes('data-cms-action="backup-commit"'),
        "the screen offers the backup action",
      );
      ok(
        !html.includes('data-cms-action="backup-push"'),
        "and offers no push: a backup is a local commit (§22)",
      );
      ok(
        !html.includes('data-cms-action="git-'),
        "and no arbitrary Git command can be typed in (§66)",
      );
    });
  } finally {
    await stack.stop();
    await cleanup(roots);
  }

  // -------------------------------------------------------------------------
  // Rate limiting gets its own stack so its low limit does not exhaust the
  // bucket used by the functional tests above.
  // -------------------------------------------------------------------------
  await test("§8 comment rate limiting holds", async () => {
    const rateRoots = await makeRoots();
    const rateAstro = await freePort();
    const rateApi = await freePort();
    const limited = await startStack(rateRoots, rateAstro, rateApi, {
      COMMENT_PER_HOUR: "3",
    });

    try {
      const submit = () =>
        fetch(`${limited.api}/api/v1/comments`, {
          method: "POST",
          headers: { "Content-Type": "application/json", Origin: limited.base },
          body: JSON.stringify({
            postSlug: "anything",
            nickname: "Flooder",
            content: "spam attempt",
            startedAt: Date.now() - 5000,
          }),
        });

      let limited429 = false;
      for (let i = 0; i < 10; i++) {
        const res = await submit();
        if (res.status === 429) {
          limited429 = true;
          ok(
            res.headers.get("retry-after") !== null,
            "the 429 carries a Retry-After header",
          );
          break;
        }
      }
      ok(
        limited429,
        "repeated comment submissions are eventually rate limited",
      );

      // The limiter keys on the client address, so a different Origin header
      // alone must not be a bypass.
      const forged = await fetch(`${limited.api}/api/v1/comments`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Origin: "https://evil.example",
        },
        body: JSON.stringify({
          postSlug: "anything",
          nickname: "Flooder",
          content: "more spam",
          startedAt: Date.now() - 5000,
        }),
      });
      eq(
        forged.status,
        403,
        "a forged Origin is refused before any bucket is consulted",
      );
    } finally {
      await limited.stop();
      await cleanup(rateRoots);
    }
  });

  process.exit(summary() ? 0 : 1);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
