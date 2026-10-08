/**
 * The media delivery route: the browser's only way to fetch an image.
 *
 * ARCHITECTURE.md §14: every CMS-managed image goes through here. The browser is
 * never told a filesystem path, so the storage layout is not public API and can
 * change — local filesystem to object storage is one resolver, not a theme edit.
 *
 * ARCHITECTURE.md §128/§129: this route *delegates*, it does not transform. It
 * forwards `Accept` and `If-None-Match` to the Go delivery layer, which owns path
 * validation, content negotiation, conversion, both caches and every header, and
 * streams the result back. A WebP encode happens in Go, on its own request
 * goroutine; nothing here decodes an image, and nothing here is in a page-rendering
 * path. Middleware was tempting and wrong: an image pipeline inside middleware would
 * make every unrelated request pay for it.
 *
 * ARCHITECTURE.md §18/§88: `Vary`, `Cache-Control`, `ETag` and `Content-Type` come
 * from Go and are passed through verbatim. Re-deriving any of them here would be a
 * second implementation that could disagree with the one that produced the bytes —
 * and `Vary: Accept` in particular is what stops a shared cache handing a WebP to a
 * client that cannot read one.
 */
import type { APIRoute } from 'astro';

export const prerender = false;

/** Response headers copied from the backend. Each is set in exactly one place. */
const PASS_THROUGH = [
  'content-type',
  'content-length',
  'cache-control',
  'content-disposition',
  'content-security-policy',
  'etag',
  'vary',
  'x-content-type-options',
] as const;

/** Request headers forwarded upstream. */
const FORWARD_REQUEST = ['accept', 'if-none-match', 'if-modified-since'] as const;

function backendBase(): string {
  const base = process.env.API_BASE?.trim();
  if (!base) {
    throw new Error('API_BASE is not set; the frontend cannot reach the Go backend.');
  }
  return base.replace(/\/+$/, '');
}

export const GET: APIRoute = ({ params, request }) => serveMedia(params['path'], request);

export const HEAD: APIRoute = ({ params, request }) => serveMedia(params['path'], request);

/**
 * Forwards to the backend and streams the answer back.
 *
 * The body is passed through as a stream rather than buffered, so a large image does
 * not sit in this process's heap. The 304 and HEAD branches have no body to stream.
 */
async function serveMedia(
  segments: string | string[] | undefined,
  request: Request,
): Promise<Response> {
  const relative = Array.isArray(segments) ? segments.join('/') : (segments ?? '');
  if (relative === '') {
    return new Response('Not found', { status: 404 });
  }

  const headers = new Headers();
  for (const name of FORWARD_REQUEST) {
    const value = request.headers.get(name);
    if (value !== null) headers.set(name, value);
  }

  const isHead = request.method === 'HEAD';

  let upstream: Response;
  try {
    upstream = await fetch(`${backendBase()}/media/${encodePath(relative)}`, {
      method: isHead ? 'HEAD' : 'GET',
      headers,
      redirect: 'manual',
    });
  } catch (err) {
    // ARCHITECTURE.md §61: a backend that cannot answer is a 503 for the image, not
    // a 500 for the page, and not a broken <img> with no explanation.
    return new Response(`Media delivery is unavailable: ${String(err)}`, {
      status: 503,
      headers: {
        'Content-Type': 'text/plain; charset=utf-8',
        'X-Content-Type-Options': 'nosniff',
      },
    });
  }

  const out = new Headers();
  for (const name of PASS_THROUGH) {
    const value = upstream.headers.get(name);
    if (value !== null) out.set(name, value);
  }

  return new Response(isHead || upstream.status === 304 ? null : upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: out,
  });
}

/**
 * Encodes each segment and leaves the separators intact.
 *
 * Encoding the whole path with `encodeURIComponent` would turn `/` into `%2F` and the
 * backend would receive one strange filename instead of a nested path.
 */
function encodePath(relative: string): string {
  return relative
    .split('/')
    .filter((segment) => segment !== '')
    .map(encodeURIComponent)
    .join('/');
}
