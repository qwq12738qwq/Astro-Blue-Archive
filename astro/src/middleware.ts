/**
 * Astro middleware: the single origin.
 *
 * ARCHITECTURE.md (revised): Astro is the only listening process. It owns the
 * security headers and the routing that a reverse proxy used to own, so the
 * deployment is a single container with no sidecar.
 *
 * The Go backend binds to localhost only and is never exposed directly, which
 * removes a whole network surface rather than adding a proxy.
 */
import { defineMiddleware } from 'astro:middleware';

import { hasSessionCookie, resolveSession } from './lib/session';
import { resolveRequestTheme } from './theme-system/resolve';

const ADMIN_PREFIX = '/admin';
const LOGIN_PATH = '/admin/login';

/**
 * Content-Security-Policy.
 *
 * `script-src 'self'` with no 'unsafe-inline' holds because every script is an
 * external same-origin file: /color-scheme.js, /cms.js, /comments.js,
 * /custom.js. Custom admin JavaScript is therefore never inlined.
 *
 * ARCHITECTURE.md §45: that list is also why a theme cannot weaken this policy.
 * A theme may not add a script, inline one, or point one at a URL it chose; it
 * renders markup, and this policy decides what that markup is allowed to run.
 */
const CSP = [
  "default-src 'self'",
  // gitee.com is the host the bluearchive theme's banner
  // artwork is served from. Images cannot execute script,
  // so allowing one image origin keeps the policy's teeth
  // while letting the site's own banner load.
  "img-src 'self' data: https://gitee.com",
  "style-src 'self'",
  "script-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join('; ');

/**
 * Response hardening headers.
 *
 * HSTS is deliberately absent: this process is not the TLS
 * terminator. The deployment puts a CDN or reverse proxy in
 * front, and whatever terminates TLS owns the HSTS decision —
 * a header set here would either be ignored (HTTP) or
 * contradict the terminator's own policy.
 */
function applySecurityHeaders(response: Response): void {
  // CSP and friends are set here and nowhere else, so there is exactly one
  // policy in play.
  response.headers.set('Content-Security-Policy', CSP);
  response.headers.set('X-Content-Type-Options', 'nosniff');
  response.headers.set('X-Frame-Options', 'DENY');
  response.headers.set('Referrer-Policy', 'strict-origin-when-cross-origin');
  response.headers.set(
    'Permissions-Policy',
    'geolocation=(), camera=(), microphone=(), payment=(), interest-cohort=()',
  );
}

export const onRequest = defineMiddleware(async (context, next) => {
  const { url, request, locals, redirect } = context;

  locals.session = null;

  /**
   * ARCHITECTURE.md ID-20: the browser's Cookie header, kept for server-side calls.
   *
   * There is no cookie jar on the server, so an admin page's own API call carried
   * no session and every admin screen answered 401 to itself. `resolveSession`
   * already forwards this header; the admin pages now do the same, from here, so
   * the value has exactly one source per request.
   */
  locals.cookie = request.headers.get('cookie');

  /**
   * ARCHITECTURE.md §19: the one place a request is bound to a theme.
   *
   * It runs before the admin gate so that the login page — which is rendered
   * through the public theme — is themed too, and before `next()` so that every
   * downstream page and endpoint sees the same value. It reads the cached
   * settings helper, so it costs nothing per request beyond a map lookup.
   */
  locals.theme = await resolveRequestTheme();

  // Public pages with no session cookie cost nothing: no backend round trip.
  if (hasSessionCookie(request)) {
    locals.session = await resolveSession(request);
  }

  // ARCHITECTURE.md §28: the RSS feed is served by the Go backend,
  // which owns both halves of it — the settings that shape the
  // channel and the content the items come from. The public URL
  // stays stable by rewriting the request onto the JSON-API proxy,
  // which forwards the response verbatim: the feed is one more
  // route the single origin exposes, not a page Astro renders.
  if (url.pathname === '/rss.xml') {
    const response = await context.rewrite('/api/v1/rss');
    applySecurityHeaders(response);
    return response;
  }

  const isAdminArea = url.pathname === ADMIN_PREFIX || url.pathname.startsWith(`${ADMIN_PREFIX}/`);
  const isLogin = url.pathname === LOGIN_PATH;

  if (isAdminArea && !isLogin && locals.session === null) {
    // Preserve the intended destination so login can redirect back.
    const nextParam = encodeURIComponent(url.pathname + url.search);
    return redirect(`${LOGIN_PATH}?next=${nextParam}`, 303);
  }

  if (isLogin && locals.session !== null) {
    return redirect(ADMIN_PREFIX, 303);
  }

  const response = await next();

  if (isAdminArea) {
    // Admin responses must never be cached by a browser or an intermediary.
    response.headers.set('Cache-Control', 'no-store');
  }

  applySecurityHeaders(response);
  return response;
});
