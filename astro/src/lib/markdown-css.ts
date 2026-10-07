/**
 * Access to content/system/markdown/ — the Markdown style templates.
 *
 * ARCHITECTURE.md §34: the files are the single source of truth for how
 * rendered Markdown content is presented, Go is the only writer, and the
 * public URL — `/markdown.css` — is an *aggregate* assembled here at
 * request time.
 *
 * ```
 * /markdown.css  →  content/system/markdown/001-base.css        (enabled)
 *                     content/system/markdown/010-typography.css  (enabled)
 *                     …
 * ```
 *
 * The arrangement inherits every load-bearing property of the custom-asset
 * aggregator (ID-33/ID-34), with the type dimension removed because a
 * Markdown style template is always a stylesheet:
 *
 * - **It is runtime, not build-time.** A file edited on disk is live on the
 *   very next request. `astro build` is not part of saving a template.
 * - **"Enabled" is a location.** A file in `markdown/` is enabled; the same
 *   file in `parked/markdown/` is disabled. A plain `readdir` answers
 *   "what is served", needing no database and no second storage system.
 * - **The order is the filename.** `001-`, `010-`, `100-` is a three-digit
 *   prefix, so a lexical sort IS the intended order (ID-35).
 * - **One bad file is skipped, not fatal.** A file whose name is outside the
 *   grammar, or that cannot be read, is logged and left out; the rest of
 *   the site keeps working.
 *
 * The templates style *content*, not the site around it: every selector is
 * scoped to `.markdown-body`, the class the renderer wraps every rendered
 * body in (`lib/markdown.ts`). That is what makes a template independent
 * of the theme — the theme owns the page, the templates own the article.
 */
import { createHash } from 'node:crypto';
import { readFile, readdir, stat } from 'node:fs/promises';
import path from 'node:path';

import { contentRoot } from './content';

/**
 * The managed directory, relative to `content/system`. Built from parts
 * rather than written as a literal so the architecture gate's
 * "only the aggregator names its directory" rule stays true for this
 * aggregator too.
 */
const MARKDOWN_TEMPLATE_DIR = 'markdown';

/**
 * The filename grammar, mirrored from
 * `content.MarkdownTemplatePattern`.
 *
 * The prefix is the sort key and the stem is kebab-case, so a filename is
 * safe in a URL and in a log line. The grammar admits no `/`, no `\`, no
 * `..` and no leading dot, which is why nothing below needs to defend
 * against traversal in its join: a name that does not match here never
 * becomes a path at all. `tests/integration-tests.mjs` and
 * `backend/internal/content/markdown_assets_test.go` cross-check the two
 * copies.
 */
const MARKDOWN_TEMPLATE_FILENAME = /^[0-9]{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.css$/;

/**
 * The per-template size ceiling, mirroring `content.BodyMaxBytes`.
 *
 * The number is duplicated because Go and TypeScript cannot share a
 * constant and the alternative — asking Go — would make a public
 * stylesheet depend on the API being up. A test asserts the two agree.
 */
export const MARKDOWN_TEMPLATE_MAX_BYTES = 512 * 1024;

/** One managed template as the aggregator sees it. */
type TemplatePart = {
  filename: string;
  body: string;
  modified: Date;
};

/**
 * Read the enabled templates, in the canonical order.
 *
 * Anything unusable is reported and skipped rather than thrown, which is
 * the whole point: `/markdown.css` has to keep answering even when
 * `markdown/` contains something broken.
 */
async function readTemplateParts(): Promise<{
  parts: TemplatePart[];
  skipped: { filename: string; reason: string }[];
}> {
  const dir = path.join(contentRoot(), 'system', MARKDOWN_TEMPLATE_DIR);

  let entries;
  try {
    entries = await readdir(dir, { withFileTypes: true });
  } catch {
    // No templates at all is the ordinary state of an install that has
    // never opened the manager. Not a fault.
    return { parts: [], skipped: [] };
  }

  const named = entries
    // A dotfile is not a template: that includes the `.tmp-*` file Go's
    // atomic write leaves behind if the process dies mid-write.
    .filter((entry) => !entry.name.startsWith('.'))
    .filter((entry) => !entry.isDirectory())
    .map((entry) => entry.name)
    // ID-35. Sorting by name is sorting by the three-digit prefix, then
    // the stem.
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));

  const parts: TemplatePart[] = [];
  const skipped: { filename: string; reason: string }[] = [];
  const skip = (filename: string, reason: string) => {
    reportSkipped(filename, reason);
    skipped.push({ filename, reason });
  };

  for (const filename of named) {
    const full = path.join(dir, filename);

    if (!MARKDOWN_TEMPLATE_FILENAME.test(filename)) {
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
    if (st.size > MARKDOWN_TEMPLATE_MAX_BYTES) {
      skip(filename, `larger than ${MARKDOWN_TEMPLATE_MAX_BYTES} bytes`);
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

function reportSkipped(filename: string, reason: string): void {
  console.error(
    JSON.stringify({
      level: 'error',
      msg: 'markdown_template_skipped',
      file: filename,
      reason,
    }),
  );
}

/**
 * The response for `/markdown.css`.
 *
 * `included` and `skipped` name files, never paths: ID-42 — a public
 * response must not tell a caller where CONTENT_ROOT is or how it is laid
 * out. They exist for the integration tests and for an operator reading a
 * log, not for the browser.
 */
export type MarkdownTemplateBundle = {
  body: string;
  etag: string;
  modified: Date;
  included: string[];
  skipped: { filename: string; reason: string }[];
};

/**
 * Assemble the public template response: every enabled template, in
 * filename order.
 *
 * Each part is separated by a comment naming it, which does two jobs: a
 * human reading devtools can see which file a rule came from, and a `//`
 * line comment at the end of one file cannot swallow the next one. The
 * separator is not merely cosmetic — concatenating without it produces an
 * aggregate that silently loses every file after the first one that does
 * not end in a newline.
 */
export async function readMarkdownTemplateBundle(): Promise<MarkdownTemplateBundle> {
  const { parts, skipped } = await readTemplateParts();

  const chunks: string[] = [];
  const included: string[] = [];
  let modified = new Date(0);
  for (const part of parts) {
    chunks.push(`\n/* blogcms:${MARKDOWN_TEMPLATE_DIR}:${part.filename} */\n${part.body}`);
    included.push(part.filename);
    if (part.modified > modified) modified = part.modified;
  }

  const body = chunks.join('');
  return {
    body,
    // Derived from the bytes, not from a stored version counter or a build
    // stamp: any edit to any included file changes it, and nothing else
    // does. A client holding the previous response therefore gets a 304
    // exactly when the answer is unchanged, and a fresh 200 the moment it
    // is not (ID-34).
    etag: `"sha256-${createHash('sha256').update(body).digest('hex').slice(0, 32)}"`,
    modified,
    included,
    skipped,
  };
}

/**
 * Serve the bundle with revalidation.
 *
 * `no-cache` (rather than `no-store`) lets the browser revalidate cheaply:
 * the ETag changes whenever the admin edits a file, so an edit is visible
 * on the next request without a rebuild and without giving up conditional
 * requests.
 */
export function serveMarkdownTemplateBundle(bundle: MarkdownTemplateBundle): Response {
  const headers = new Headers({
    'Content-Type': 'text/css; charset=utf-8',
    'Content-Length': String(Buffer.byteLength(bundle.body)),
    'Cache-Control': 'no-cache, must-revalidate',
    ETag: bundle.etag,
    'X-Content-Type-Options': 'nosniff',
  });

  return new Response(bundle.body, { status: 200, headers });
}

/** True when the client's ETag matches, so the body can be omitted. */
export function isMarkdownTemplateFresh(request: Request, bundle: MarkdownTemplateBundle): boolean {
  const inm = request.headers.get('if-none-match');
  if (!inm) return false;
  return inm
    .split(',')
    .map((v) => v.trim())
    .includes(bundle.etag);
}
