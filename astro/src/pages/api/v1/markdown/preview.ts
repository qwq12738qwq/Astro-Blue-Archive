/**
 * The Markdown preview endpoint.
 *
 * ARCHITECTURE.md §34: the /admin/posts/markdown editor previews with
 * the *real* render path — the same `renderMarkdown()` a post page
 * runs — so what the admin sees is what a reader sees. This endpoint
 * is that render path, exposed as JSON.
 *
 * It is an admin-only tool, so it carries the same two gates every
 * mutating admin surface carries: a live session (resolved against
 * the Go backend, which is the only session holder) and the CSRF
 * double-submit header. The response is the rendered HTML, which
 * `renderMarkdown` has already escaped (raw HTML in the source is
 * neutralised by the processor, not passed through), so a preview
 * cannot smuggle markup past the Content-Security-Policy.
 */
import type { APIRoute } from 'astro';

import { renderMarkdown } from '../../../../lib/markdown';
import { resolveSession } from '../../../../lib/session';

export const prerender = false;

/** The CSRF header Go's requireSession checks (auth.CSRFHeaderName). */
const CSRF_HEADER = 'X-CSRF-Token';

/** A preview body is a Markdown document, not an upload: 1 MiB. */
const PREVIEW_MAX_BYTES = 1024 * 1024;

export const POST: APIRoute = async ({ request }) => {
  const session = await resolveSession(request);
  if (!session) {
    return Response.json(
      { error: { code: 'unauthorized', message: 'authentication required' } },
      { status: 401 },
    );
  }

  // The double-submit check, mirroring Go's requireSession: a valid
  // cookie from a foreign site must not be able to use this endpoint
  // as a rendering service.
  const presented = request.headers.get(CSRF_HEADER);
  if (!presented || presented !== session.csrfToken) {
    return Response.json(
      { error: { code: 'forbidden', message: 'missing or invalid CSRF token' } },
      { status: 403 },
    );
  }

  let raw: string;
  try {
    raw = await request.text();
  } catch {
    return Response.json(
      { error: { code: 'bad_request', message: 'unreadable body' } },
      { status: 400 },
    );
  }
  if (Buffer.byteLength(raw) > PREVIEW_MAX_BYTES) {
    return Response.json(
      { error: { code: 'payload_too_large', message: `body exceeds ${PREVIEW_MAX_BYTES} bytes` } },
      { status: 413 },
    );
  }

  let parsed: { markdown?: unknown };
  try {
    parsed = JSON.parse(raw);
  } catch {
    return Response.json(
      { error: { code: 'bad_request', message: 'body must be JSON' } },
      { status: 400 },
    );
  }
  const markdown = parsed.markdown;
  if (typeof markdown !== 'string') {
    return Response.json(
      { error: { code: 'validation_failed', message: 'markdown must be a string' } },
      { status: 422 },
    );
  }

  const rendered = await renderMarkdown(markdown);
  return Response.json({ html: rendered.html });
};
