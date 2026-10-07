/**
 * Shared full-stack test harness.
 *
 * ARCHITECTURE.md §34/§37: the production data flow is two processes, so testing it
 * honestly means running both. This module owns the parts that are not specific to
 * any one suite — building, starting Go and Astro against a temporary content root,
 * and the tiny assertion helpers — so that `fullstack-tests.mjs` and
 * `theme-tests.mjs` cannot drift in how they start a stack.
 *
 * If this file is wrong, every suite is wrong the same way, which is a much better
 * failure mode than two bootstraps that disagree.
 */
import { spawn } from "node:child_process";
import { mkdtemp, mkdir, rm } from "node:fs/promises";
import { existsSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const ROOT = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
);
export const ASTRO = path.join(ROOT, "astro");
export const BACKEND = path.join(ROOT, "backend");
export const ENTRY = path.join(ASTRO, "dist", "server", "entry.mjs");
export const GO_BINARY = path.join(BACKEND, "bin", "blogcms-server");

/** The address both processes bind to. Never 0.0.0.0: the backend is loopback-only. */
export const ORIGIN_HOST = "127.0.0.1";

/** A minimal reporter shared by every suite, so pass/fail output looks the same. */
export function createReporter(title) {
  const state = { passed: 0, failed: 0, failures: [], current: "" };

  const ok = (cond, message) => {
    if (cond) {
      state.passed++;
      console.log(`    PASS  ${message}`);
    } else {
      state.failed++;
      state.failures.push(`${state.current}: ${message}`);
      console.error(`    FAIL  ${message}`);
    }
  };

  const eq = (actual, expected, message) =>
    ok(
      actual === expected,
      `${message}${actual === expected ? "" : ` (expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)})`}`,
    );

  const test = async (name, fn) => {
    state.current = name;
    console.log(`\n  ${name}`);
    try {
      await fn();
    } catch (err) {
      state.failed++;
      state.failures.push(`${name}: threw ${err?.stack ?? err}`);
      console.error(`    FAIL  threw: ${err?.stack ?? err}`);
    }
  };

  const summary = () => {
    console.log("\n" + "=".repeat(60));
    console.log(
      `${title.toUpperCase()}: ${state.passed} passed, ${state.failed} failed`,
    );
    if (state.failed > 0) {
      console.log("\nFailures:");
      for (const failure of state.failures) console.log(`  - ${failure}`);
    }
    console.log("=".repeat(60));
    return state.failed === 0;
  };

  return { ok, eq, test, summary, state };
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export function freePort() {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once("error", reject);
    srv.listen(0, ORIGIN_HOST, () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

function run(cmd, args, cwd) {
  return new Promise((resolve, reject) => {
    const p = spawn(cmd, args, { cwd, stdio: "inherit" });
    p.on("exit", (code) =>
      code === 0
        ? resolve()
        : reject(new Error(`${cmd} ${args.join(" ")} failed`)),
    );
    p.on("error", reject);
  });
}

/**
 * Rebuild both halves.
 *
 * Always rebuild. A conditional build was how this suite twice reported PASS for a
 * reverted fix: `dist/` still existed, so the previous phase's output was tested.
 */
export async function ensureBuilds() {
  console.log("  building the Astro server...");
  await run("npx", ["astro", "build"], ASTRO);
  if (!existsSync(ENTRY)) {
    throw new Error(`astro build did not produce ${ENTRY}`);
  }
  console.log("  building the Go server...");
  await mkdir(path.join(BACKEND, "bin"), { recursive: true });
  await run("go", ["build", "-o", GO_BINARY, "./cmd/server"], BACKEND);
}

/** A throwaway content/media/data tree, so a suite never touches the real one. */
export async function makeRoots() {
  const base = await mkdtemp(path.join(tmpdir(), "blogcms-full-"));
  const roots = {
    base,
    content: path.join(base, "content"),
    media: path.join(base, "media"),
    data: path.join(base, "data"),
  };
  for (const d of [roots.content, roots.media, roots.data]) {
    await mkdir(path.join(d, "posts"), { recursive: true });
  }
  await mkdir(path.join(roots.content, "pages"), { recursive: true });
  await mkdir(path.join(roots.content, "system"), { recursive: true });
  return roots;
}

export async function waitFor(fn, { attempts = 120, interval = 150 } = {}) {
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

async function kill(child) {
  if (!child || child.exitCode !== null) return;
  const exited = new Promise((r) => child.once("exit", r));
  child.kill("SIGTERM");
  const timer = setTimeout(() => child.kill("SIGKILL"), 5000);
  await exited;
  clearTimeout(timer);
}

/**
 * Start Go on loopback and Astro in front of it.
 *
 * The two listen in the order the architecture demands: Go first, because Astro's
 * readiness probe is its first request, and an Astro that started first would
 * merely take longer to answer.
 */
export async function startStack(roots, astroPort, apiPort, overrides = {}) {
  const publicOrigin = `http://${ORIGIN_HOST}:${astroPort}`;

  const backend = spawn(GO_BINARY, [], {
    cwd: BACKEND,
    env: {
      ...process.env,
      PORT: String(apiPort),
      HOST: ORIGIN_HOST,
      CONTENT_ROOT: roots.content,
      MEDIA_ROOT: roots.media,
      DATA_ROOT: roots.data,
      PUBLIC_ORIGIN: publicOrigin,
      SECURE_COOKIES: "false",
      LOGIN_PER_MINUTE: "5",
      LOGIN_PER_HOUR: "50",
      COMMENT_PER_HOUR: "60",
      ...overrides,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const backendLogs = [];
  backend.stdout.on("data", (d) => backendLogs.push(d.toString()));
  backend.stderr.on("data", (d) => backendLogs.push(d.toString()));

  const backendUp = await waitFor(async () => {
    const r = await fetch(`http://${ORIGIN_HOST}:${apiPort}/api/v1/healthz`);
    return r.ok;
  });
  if (!backendUp) {
    backend.kill("SIGKILL");
    throw new Error(`backend did not start\n${backendLogs.join("")}`);
  }

  const astro = spawn(process.execPath, [ENTRY], {
    cwd: ASTRO,
    env: {
      ...process.env,
      CONTENT_ROOT: roots.content,
      MEDIA_ROOT: roots.media,
      API_BASE: `http://${ORIGIN_HOST}:${apiPort}`,
      PUBLIC_ORIGIN: publicOrigin,
      HOST: ORIGIN_HOST,
      PORT: String(astroPort),
      NODE_ENV: "production",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const astroLogs = [];
  astro.stdout.on("data", (d) => astroLogs.push(d.toString()));
  astro.stderr.on("data", (d) => astroLogs.push(d.toString()));

  const astroUp = await waitFor(async () => {
    const r = await fetch(`http://${ORIGIN_HOST}:${astroPort}/`);
    return r.status > 0;
  });
  if (!astroUp) {
    astro.kill("SIGKILL");
    backend.kill("SIGKILL");
    throw new Error(`astro did not start\n${astroLogs.join("")}`);
  }

  const base = `http://${ORIGIN_HOST}:${astroPort}`;
  return {
    base,
    api: `http://${ORIGIN_HOST}:${apiPort}`,
    backendLogs,
    astroLogs,
    async stop() {
      await kill(astro);
      await kill(backend);
    },
  };
}

/** Clean up a temporary tree. Never throws: a failure here hides the real result. */
export async function cleanup(roots) {
  if (roots?.base) await rm(roots.base, { recursive: true, force: true });
}

/** Extract the session cookie value from a Set-Cookie header list. */
export function sessionCookieFrom(res) {
  const raw = res.headers.getSetCookie?.() ?? [];
  for (const c of raw) {
    const [pair] = c.split(";");
    const idx = pair.indexOf("=");
    if (idx > 0 && pair.slice(0, idx).trim() === "blog_session") {
      return pair.slice(idx + 1).trim();
    }
  }
  return null;
}

/**
 * Create the single administrator and sign in.
 *
 * Returns the cookie and CSRF token. Every suite needs exactly one administrator,
 * and doing it the same way is what keeps the suites comparable.
 */
export async function bootstrapAdmin(
  api,
  base,
  username = "owner",
  password = "a-good-password",
) {
  const headers = { "Content-Type": "application/json", Origin: base };

  const setup = await fetch(`${api}/api/v1/auth/setup`, {
    method: "POST",
    headers,
    body: JSON.stringify({ username, password }),
  });
  if (setup.status !== 201) {
    throw new Error(`setup failed: ${setup.status} ${await setup.text()}`);
  }

  const login = await fetch(`${api}/api/v1/auth/login`, {
    method: "POST",
    headers,
    body: JSON.stringify({ username, password }),
  });
  if (login.status !== 200) {
    throw new Error(`login failed: ${login.status} ${await login.text()}`);
  }
  const cookie = sessionCookieFrom(login);
  const { csrfToken } = await login.json();
  if (!cookie || !csrfToken) {
    throw new Error("sign-in did not yield a cookie and a CSRF token");
  }
  return { cookie, csrfToken, username, password };
}
