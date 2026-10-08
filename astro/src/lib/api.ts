/**
 * Typed client for the Go JSON API.
 *
 * ARCHITECTURE.md §24: the data flow is Astro -> fetch() -> Go -> JSON -> Astro.
 * Nothing here ever receives HTML: every non-2xx response is required to be the
 * JSON error envelope, and a non-JSON body is treated as a protocol violation.
 */

export const SESSION_COOKIE = 'blog_session';

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

export function apiBase(): string {
  const base = process.env.API_BASE?.trim();
  if (!base) {
    throw new Error('API_BASE is not set; the frontend cannot reach the Go backend.');
  }
  return base.replace(/\/+$/, '');
}

export type ApiOptions = {
  method?: string;
  /** JSON body. Mutually exclusive with `form`. */
  json?: unknown;
  /** Multipart body. */
  form?: FormData;
  /** Cookie header of the incoming browser request, forwarded verbatim. */
  cookie?: string | null;
  /** Extra headers, e.g. the CSRF token. */
  headers?: Record<string, string>;
  signal?: AbortSignal;
};

type ErrorEnvelope = {
  error?: { code?: string; message?: string; fields?: Record<string, string> };
};

export async function apiRequest<T>(path: string, opts: ApiOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json', ...opts.headers };

  let body: BodyInit | undefined;
  if (opts.json !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(opts.json);
  } else if (opts.form !== undefined) {
    body = opts.form;
  }

  // The Go API sets HttpOnly on the session cookie, so it can only be read
  // here by forwarding the browser's own Cookie header.
  if (opts.cookie) headers['Cookie'] = opts.cookie;

  let res: Response;
  try {
    res = await fetch(`${apiBase()}${path}`, {
      method: opts.method ?? 'GET',
      headers,
      body,
      signal: opts.signal,
      redirect: 'manual',
    });
  } catch (err) {
    throw new ApiError(503, 'backend_unreachable', `cannot reach the API: ${String(err)}`);
  }

  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let parsed: unknown = undefined;
  if (text.length > 0) {
    try {
      parsed = JSON.parse(text);
    } catch {
      throw new ApiError(502, 'invalid_backend_response', 'the API returned a non-JSON response');
    }
  }

  if (!res.ok) {
    const envelope = parsed as ErrorEnvelope | undefined;
    throw new ApiError(
      res.status,
      envelope?.error?.code ?? 'unknown',
      envelope?.error?.message ?? `request failed with status ${res.status}`,
      envelope?.error?.fields ?? {},
    );
  }

  return parsed as T;
}
