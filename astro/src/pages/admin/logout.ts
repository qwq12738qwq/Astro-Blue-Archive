/**
 * Admin logout.
 *
 * Astro owns the route (ARCHITECTURE.md §11) and delegates the session
 * revocation to the Go API, which is the only holder of the session.
 */
import type { APIRoute } from 'astro';

import { SESSION_COOKIE } from '../../lib/api';

export const prerender = false;

export const POST: APIRoute = async ({ request, redirect }) => {
  const cookie = request.headers.get('cookie') ?? '';
  if (cookie.includes(`${SESSION_COOKIE}=`)) {
    // Best effort: even if this fails the user lands on the login page and the
    // stale cookie is rejected there.
    await fetch(`${(process.env.API_BASE ?? '').replace(/\/+$/, '')}/api/v1/auth/logout`, {
      method: 'POST',
      headers: { Cookie: cookie, Origin: new URL(request.url).origin },
    }).catch(() => undefined);
  }
  return redirect('/admin/login', 303);
};
