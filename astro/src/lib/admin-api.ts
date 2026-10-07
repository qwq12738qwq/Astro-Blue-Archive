/**
 * Typed API client for the Astro admin UI.
 *
 * ARCHITECTURE.md §11/§24: the admin UI is Astro and talks to Go over fetch().
 * The CSRF token comes from the server-rendered page and travels in a header.
 *
 * The session cookie has to be forwarded explicitly. `credentials:
 * 'same-origin'` is the browser's mechanism, and on the server there is no cookie
 * jar: a server-side fetch to the API carried no session at all, so every
 * admin page's data call came back 401 and the screens rendered their empty state
 * with an "authentication required" notice — a working-looking admin that showed
 * nothing (ARCHITECTURE.md ID-20). The middleware stashes the browser's Cookie
 * header on `locals.cookie`, and every server-rendered admin call passes it here.
 */

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly fields: Record<string, string>;

  constructor(status: number, code: string, message: string, fields: Record<string, string> = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.fields = fields;
  }
}

function apiBase(): string {
  const base = process.env.API_BASE?.trim();
  if (!base) throw new Error('API_BASE is not set.');
  return base.replace(/\/+$/, '');
}

/** The CSRF header name must match CSRFHeaderName in backend/internal/auth. */
export const CSRF_HEADER = 'X-CSRF-Token';

export type AdminFetchOptions = {
  method?: string;
  json?: unknown;
  form?: FormData;
  csrfToken?: string | null;
  /** The browser's Cookie header for this request, from `locals.cookie`. */
  cookie?: string | null;
};

/**
 * Perform an admin API call.
 *
 * `csrfToken` is required for every mutation. The backend rejects a mutation
 * without it (and without a matching Origin), so the UI must always pass one.
 */
export async function adminFetch<T>(path: string, opts: AdminFetchOptions = {}): Promise<T> {
  const method = opts.method ?? 'GET';
  const headers: Record<string, string> = { Accept: 'application/json' };

  let body: BodyInit | undefined;
  if (opts.json !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(opts.json);
  } else if (opts.form !== undefined) {
    body = opts.form;
  }

  if (opts.csrfToken) headers[CSRF_HEADER] = opts.csrfToken;
  // The session cookie is HttpOnly, so client-side JavaScript cannot read it —
  // which is why this call has to be made server-side at all.
  if (opts.cookie) headers['Cookie'] = opts.cookie;

  let res: Response;
  try {
    res = await fetch(`${apiBase()}${path}`, {
      method,
      headers,
      body,
    });
  } catch (err) {
    throw new ApiError(503, 'backend_unreachable', `Cannot reach the API: ${String(err)}`);
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let parsed: unknown;
  if (text.length > 0) {
    try {
      parsed = JSON.parse(text);
    } catch {
      throw new ApiError(502, 'invalid_backend_response', 'The API returned a non-JSON response.');
    }
  }

  if (!res.ok) {
    const env = parsed as {
      error?: { code?: string; message?: string; fields?: Record<string, string> };
    };
    throw new ApiError(
      res.status,
      env?.error?.code ?? 'unknown',
      env?.error?.message ?? `Request failed (${res.status})`,
      env?.error?.fields ?? {},
    );
  }

  return parsed as T;
}
