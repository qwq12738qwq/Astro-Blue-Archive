/**
 * Access to content/system/* — the admin's custom CSS and JavaScript.
 *
 * ARCHITECTURE.md ID-33/ID-34: the files are the single source of truth, Go is the
 * only writer, and the two public URLs — `/custom.css` and `/custom.js` — are
 * *aggregates* assembled here at request time.
 *
 * ```
 * /custom.css   →  content/system/custom.css              (legacy, always first)
 *                  content/system/css/001-base.css        (managed, enabled)
 *                  content/system/css/010-layout.css      (managed, enabled)
 *                  …
 * /custom.js    →  content/system/custom.js               (legacy, always first)
 *                  content/system/js/001-base.js          (managed, enabled)
 *                  …
 * ```
 *
 * Four properties of that arrangement are load-bearing.
 *
 * - **It is runtime, not build-time.** Nothing is generated, nothing is written and
 *   no bundler is involved, so a file edited on disk is live on the very next
 *   request. `astro build` is not part of saving custom code.
 * - **"Enabled" is a location.** A managed file in `css/` is enabled; the same file
 *   in `parked/css/` is disabled. That is what lets this module answer with a plain
 *   `readdir`, needing no database and no second storage system — and it is why a
 *   disable cannot lag behind a save the way a cached index would.
 * - **The order is the filename.** `001-`, `010-`, `100-` is a three-digit prefix,
 *   so a lexical sort IS the intended order (ID-35). A filesystem's `readdir`
 *   order is never consulted: two identical requests on identical code must produce
 *   identical bytes.
 * - **One bad file is skipped, not fatal.** A file whose name is outside the
 *   grammar, or that cannot be read, is logged and left out; the rest of the site
 *   keeps working. This is the same rule the content loader applies per file
 *   (ARCHITECTURE.md §7), for the same reason: one typo in one asset must not take
 *   a blog offline.
 */
import { createHash } from 'node:crypto';
import { readFile, readdir, stat } from 'node:fs/promises';
import path from 'node:path';

import { contentRoot } from './content';

/** The two legacy files, and the only two this module will read by name. */
export const SYSTEM_FILES = {
  customCss: 'custom.css',
  customJs: 'custom.js',
} as const;

export type SystemFileName = (typeof SYSTEM_FILES)[keyof typeof SYSTEM_FILES];

/** The two custom-asset types. The set is closed: no HTML, no SVG, no JSON. */
export type CustomAssetType = 'css' | 'js';

const LEGACY_FILE: Record<CustomAssetType, SystemFileName> = {
  css: SYSTEM_FILES.customCss,
  js: SYSTEM_FILES.customJs,
};

/** The managed directory for each type, relative to `content/system`. */
const ASSET_DIR: Record<CustomAssetType, string> = { css: 'css', js: 'js' };

/**
 * The filename grammar, mirrored from `content.AssetFilenamePattern`.
 *
 * The prefix is the sort key and the stem is kebab-case, so a filename is safe in a
 * URL and in a log line. The grammar admits no `/`, no `\`, no `..` and no leading
 * dot, which is why nothing below needs to defend against traversal in its join: a
 * name that does not match here never becomes a path at all. `tests/integration-tests.mjs`
 * and `backend/internal/content/assets_test.go` cross-check the two copies.
 */
const ASSET_FILENAME = /^[0-9]{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.(css|js)$/;

/**
 * The per-asset size ceiling, mirroring `content.BodyMaxBytes`.
 *
 * 512 KiB of custom CSS or JavaScript is already far more than a personal blog
 * needs. The number is duplicated because Go and TypeScript cannot share a constant
 * and the alternative — asking Go — would make a public stylesheet depend on the API
 * being up. A test asserts the two agree.
 */
export const CUSTOM_ASSET_MAX_BYTES = 512 * 1024;

function resolveSystemPath(name: string): string {
  if (name !== SYSTEM_FILES.customCss && name !== SYSTEM_FILES.customJs) {
    throw new Error(`refusing to read unknown system file ${JSON.stringify(name)}`);
  }
  const root = contentRoot();
  const dir = path.join(root, 'system');
  const full = path.resolve(dir, name);

  // Defence in depth: even though `name` is a constant, re-verify containment.
  const rel = path.relative(dir, full);
  if (rel === '' || rel.startsWith('..') || path.isAbsolute(rel)) {
    throw new Error(`refusing to read ${full}: outside the system directory`);
  }
  return full;
}

export type SystemFile = {
  body: string;
  etag: string;
  modified: Date;
};

/**
 * Read a system file. A missing file yields empty content rather than an
 * error: a fresh install has no custom code yet, and that is not a fault.
 */
export async function readSystemFile(name: SystemFileName): Promise<SystemFile> {
  const full = resolveSystemPath(name);

  let st;
  try {
    st = await stat(full);
  } catch {
    return { body: '', etag: '"empty"', modified: new Date(0) };
  }
  if (!st.isFile()) {
    return { body: '', etag: '"empty"', modified: new Date(0) };
  }

  const body = await readFile(full, 'utf-8');
  const etag = `"${st.size.toString(16)}-${Math.trunc(st.mtimeMs).toString(16)}"`;
  return { body, etag, modified: st.mtime };
}

/** One managed file as the aggregator sees it. */
type AssetPart = {
  filename: string;
  body: string;
  modified: Date;
};

/**
 * Read the enabled managed files of one type, in the canonical order.
 *
 * Anything unusable is reported and skipped rather than thrown, which is the whole
 * point: `/custom.css` has to keep answering even when `css/` contains something
 * broken.
 */
async function readAssetParts(type: CustomAssetType): Promise<{
  parts: AssetPart[];
  skipped: { filename: string; reason: string }[];
}> {
  const dir = path.join(contentRoot(), 'system', ASSET_DIR[type]);

  let entries;
  try {
    entries = await readdir(dir, { withFileTypes: true });
  } catch {
    // No managed assets at all is the ordinary state of an install that only has
    // custom.css. Not a fault.
    return { parts: [], skipped: [] };
  }

  const named = entries
    // A dotfile is not an asset: that includes the `.tmp-*` file Go's atomic write
    // leaves behind if the process dies mid-write.
    .filter((entry) => !entry.name.startsWith('.'))
    .filter((entry) => !entry.isDirectory())
    .map((entry) => entry.name)
    // ID-35. Sorting by name is sorting by the three-digit prefix, then the stem.
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));

  const parts: AssetPart[] = [];
  const skipped: { filename: string; reason: string }[] = [];
  const skip = (filename: string, reason: string) => {
    reportSkipped(type, filename, reason);
    skipped.push({ filename, reason });
  };

  for (const filename of named) {
    const full = path.join(dir, filename);

    if (!ASSET_FILENAME.test(filename) || !filename.endsWith(`.${type}`)) {
      skip(filename, 'the filename is not allowed');
      continue;
    }

    let st;
    try {
      st = await stat(full);
    } catch {
      skip(filename, 'the file could not be read');
      continue;
    }
    if (!st.isFile()) {
      skip(filename, 'not a regular file');
      continue;
    }
    if (st.size > CUSTOM_ASSET_MAX_BYTES) {
      skip(filename, `larger than ${CUSTOM_ASSET_MAX_BYTES} bytes`);
      continue;
    }

    try {
      parts.push({ filename, body: await readFile(full, 'utf-8'), modified: st.mtime });
    } catch {
      skip(filename, 'the file could not be read');
    }
  }
  return { parts, skipped };
}

function reportSkipped(type: CustomAssetType, filename: string, reason: string): void {
  console.error(
    JSON.stringify({
      level: 'error',
      msg: 'custom_asset_skipped',
      type,
      file: filename,
      reason,
    }),
  );
}

/**
 * The response for `/custom.css` or `/custom.js`.
 *
 * `included` and `skipped` name files, never paths: §105 — a public response must
 * not tell a caller where CONTENT_ROOT is or how it is laid out. They exist for the
 * integration tests and for an operator reading a log, not for the browser.
 */
export type CustomAssetBundle = SystemFile & {
  included: string[];
  skipped: { filename: string; reason: string }[];
};

/**
 * Assemble one public custom-code response: the legacy file, then every enabled
 * managed file in filename order.
 *
 * The legacy file comes first and keeps its exact bytes when there are no managed
 * files, so an install that never opens the manager keeps serving a response that is
 * byte-identical to `custom.css`. Once a managed file exists, each part is separated
 * by a comment naming it, which does two jobs: a human reading devtools can see
 * which file a rule came from, and a `//` line comment at the end of one JavaScript
 * file cannot swallow the next one. That second problem is the reason the separator
 * is not merely cosmetic — concatenating without it produces an aggregate that
 * silently loses every file after the first one that does not end in a newline.
 */
export async function readCustomAssetBundle(type: CustomAssetType): Promise<CustomAssetBundle> {
  const legacy = await readSystemFile(LEGACY_FILE[type]);
  const { parts, skipped } = await readAssetParts(type);

  const chunks: string[] = [];
  const included: string[] = [];
  if (legacy.body !== '') {
    chunks.push(legacy.body);
    included.push(LEGACY_FILE[type]);
  }

  let modified = legacy.modified;
  for (const part of parts) {
    chunks.push(`\n/* blogcms:${type}:${part.filename} */\n${part.body}`);
    included.push(part.filename);
    if (part.modified > modified) modified = part.modified;
  }

  const body = chunks.join('');
  return {
    body,
    // Derived from the bytes, not from a stored version counter or a build stamp:
    // any edit to any included file changes it, and nothing else does. A client
    // holding the previous response therefore gets a 304 exactly when the answer is
    // unchanged, and a fresh 200 the moment it is not (ID-34).
    etag: `"sha256-${createHash('sha256').update(body).digest('hex').slice(0, 32)}"`,
    modified,
    included,
    skipped,
  };
}

/**
 * Serve a system file with revalidation.
 *
 * `no-cache` (rather than `no-store`) lets the browser revalidate cheaply: the
 * ETag changes whenever the admin edits a file, so an edit is visible on the next
 * request without a rebuild and without giving up conditional requests. `no-store`
 * would also be correct and would cost a full re-download of every asset on every
 * page view.
 */
export function serveSystemFile(file: SystemFile, contentType: string): Response {
  const headers = new Headers({
    'Content-Type': `${contentType}; charset=utf-8`,
    'Content-Length': String(Buffer.byteLength(file.body)),
    'Cache-Control': 'no-cache, must-revalidate',
    ETag: file.etag,
    'X-Content-Type-Options': 'nosniff',
  });

  return new Response(file.body, { status: 200, headers });
}

/** True when the client's ETag matches, so the body can be omitted. */
export function isFresh(request: Request, file: SystemFile): boolean {
  const inm = request.headers.get('if-none-match');
  if (!inm) return false;
  return inm
    .split(',')
    .map((v) => v.trim())
    .includes(file.etag);
}
