/**
 * Reverse proxy for the JSON API: /api/* -> Go backend.
 *
 * ARCHITECTURE.md (revised): Astro is the single origin, so it forwards API
 * calls itself instead of delegating routing to a reverse proxy. The Go backend
 * listens on localhost only and is never reachable from outside the process.
 *
 * The session cookie is HttpOnly, so it is forwarded verbatim from the incoming
 * request rather than read.
 */
import { invalidateSiteSettings } from '../../lib/site';
import type { APIRoute } from 'astro';

export const prerender = false;

/** Must match httpx.ClientIPHeader in the Go backend. */
const CLIENT_IP_HEADER = 'X-Client-IP';

/**
 * The authority the browser actually dialled, e.g. `host:9900`.
 *
 * ARCHITECTURE.md ID-24: Astro is the single origin and proxies every /api/* call,
 * so Go never sees the browser's `Host` — it sees its own loopback address. That
 * made Go's CSRF Origin check compare the visitor's origin against `127.0.0.1:9901`,
 * so *no* browser could ever satisfy "same-origin" and only an explicitly listed
 * address worked. Astro is the process that knows the real address, so Astro states
 * it and Go compares against it, trusting the header only over loopback — exactly
 * the rule `X-Client-IP` already follows.
 */
const FORWARDED_HOST_HEADER = 'X-Forwarded-Host';

/** The settings write, and the custom-code write, both change what pages render. */
const SETTINGS_WRITE = /\/api\/v1\/(admin\/settings|admin\/custom-code)$/;

/** Hop-by-hop headers that must not be forwarded. */
const HOP_BY_HOP = new Set([
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
  'host',
  'content-length',
]);

function backendBase(): string {
  const base = process.env.API_BASE?.trim();
  if (!base) {
    throw new Error('API_BASE is not set; the frontend cannot reach the Go backend.');
  }
  return base.replace(/\/+$/, '');
}

/**
 * Buffer the request body.
 *
 * The body is buffered rather than streamed: Node's fetch requires
 * `duplex: 'half'` to stream a request body, and omitting it fails every POST
 * and upload. Go caps bodies at MAX_JSON_BODY / MAX_UPLOAD_BYTES, so the buffer
 * is bounded by the same limits and re-checked here.
 */
const MAX_PROXY_BODY = 16 * 1024 * 1024;

async function forwardBody(request: Request): Promise<Buffer | undefined> {
  if (request.method === 'GET' || request.method === 'HEAD') return undefined;
  if (!request.body) return undefined;

  const raw = Buffer.from(await request.arrayBuffer());
  if (raw.byteLength > MAX_PROXY_BODY) {
    throw new Error(`request body exceeds ${MAX_PROXY_BODY} bytes`);
  }
  return raw;
}

export const ALL: APIRoute = async ({ request, url, clientAddress }) => {
  // Preserve the original path and query so the API sees a stable URL.
  const target = `${backendBase()}${url.pathname}${url.search}`;
  const writesSettings = SETTINGS_WRITE.test(url.pathname);

  const headers = new Headers();
  for (const [name, value] of request.headers) {
    const lower = name.toLowerCase();
    if (HOP_BY_HOP.has(lower)) continue;
    // A client must never be able to name its own address or its own origin: Go uses
    // these two headers for rate limiting and for the CSRF Origin check, and trusts
    // them only because they arrive over loopback. Anything the caller sent under
    // either name is dropped here and replaced below.
    if (lower === CLIENT_IP_HEADER) continue;
    if (lower === FORWARDED_HOST_HEADER) continue;
    headers.set(name, value);
  }
  // clientAddress is the TCP peer, read by Astro from the socket, so it is the
  // real address of the caller rather than anything the caller wrote.
  headers.set(CLIENT_IP_HEADER, clientAddress ?? '');

  // The authority the browser dialled. `request.headers` is the browser's own Host,
  // read by Astro from the socket, so this is a fact about the request rather than a
  // claim by the caller — the caller cannot choose which Host it reaches us on.
  headers.set(FORWARDED_HOST_HEADER, url.host);

  let upstream: Response;
  try {
    const body = await forwardBody(request);
    upstream = await fetch(target, {
      method: request.method,
      headers,
      // Buffer is a valid BodyInit at runtime; the DOM type does not model it.
      body: body as unknown as BodyInit,
      // The API never redirects; following one here would hide a misroute.
      redirect: 'manual',
    });
  } catch (err) {
    // The frontend must fail visibly rather than pretending the API is healthy.
    const status = /exceeds/.test(String(err)) ? 413 : 503;
    const code = status === 413 ? 'payload_too_large' : 'backend_unreachable';
    return new Response(
      JSON.stringify({ error: { code, message: `cannot forward to the API: ${String(err)}` } }),
      {
        status,
        headers: { 'Content-Type': 'application/json; charset=utf-8' },
      },
    );
  }

  const outHeaders = new Headers();
  for (const [name, value] of upstream.headers) {
    if (!HOP_BY_HOP.has(name.toLowerCase())) outHeaders.set(name, value);
  }

  // Set-Cookie may appear more than once; Headers collapses it otherwise.
  const setCookies = upstream.headers.getSetCookie?.() ?? [];
  outHeaders.delete('set-cookie');
  for (const cookie of setCookies) outHeaders.append('set-cookie', cookie);

  // Public pages render from a cached copy of the site settings. Invalidate it
  // once the write has been accepted, so the next page shows the new value
  // instead of the previous one for the remainder of the cache TTL.
  if (writesSettings && upstream.ok) invalidateSiteSettings();

  return new Response(upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: outHeaders,
  });
};
