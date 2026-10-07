/**
 * Session helpers for Astro server-side code.
 *
 * Sessions live in the Go backend (ARCHITECTURE.md §12). Astro never creates or
 * inspects a session itself: it forwards the browser cookie and reads the JSON
 * answer. The session cookie is HttpOnly, so it is unreadable from client-side
 * JavaScript by design.
 */
import { SESSION_COOKIE, apiRequest } from './api';

export type SessionInfo = {
  authenticated: true;
  username: string;
  csrfToken: string;
  expiresAt: string;
};

export type SessionResponse =
  | { authenticated: false }
  | { authenticated: true; username: string; csrfToken: string; expiresAt: string };

/** Extract the session cookie value from an incoming request, if present. */
export function sessionCookie(request: Request): string | null {
  const header = request.headers.get('cookie');
  if (!header) return null;
  for (const part of header.split(';')) {
    const idx = part.indexOf('=');
    if (idx === -1) continue;
    if (part.slice(0, idx).trim() === SESSION_COOKIE) {
      return part.slice(idx + 1).trim();
    }
  }
  return null;
}

export function hasSessionCookie(request: Request): boolean {
  return sessionCookie(request) !== null;
}

/**
 * Resolve the current session, or null for anonymous visitors.
 *
 * A backend error is deliberately treated as "anonymous" for public pages: a
 * backend outage must not take the blog down, only the admin area.
 */
export async function resolveSession(request: Request): Promise<SessionInfo | null> {
  if (!hasSessionCookie(request)) return null;

  let res: SessionResponse;
  try {
    res = await apiRequest<SessionResponse>('/api/v1/auth/session', {
      cookie: request.headers.get('cookie'),
      signal: AbortSignal.timeout(5000),
    });
  } catch {
    return null;
  }

  if (!res?.authenticated) return null;
  return {
    authenticated: true,
    username: res.username,
    csrfToken: res.csrfToken,
    expiresAt: res.expiresAt,
  };
}
