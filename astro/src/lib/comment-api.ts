/**
 * Comment API client used by server-rendered pages.
 *
 * ARCHITECTURE.md §24: Astro fetches, Go answers with JSON.
 */
import { apiRequest } from './api';
import { getSiteSettings } from './site';
import type { AdminCommentList, PublicCommentList } from './comments';

export async function getComments(slug: string): Promise<PublicCommentList> {
  return apiRequest<PublicCommentList>(`/api/v1/comments?post=${encodeURIComponent(slug)}`, {
    signal: AbortSignal.timeout(5000),
  });
}

export type CommentSettings = {
  commentsEnabled: boolean;
  commentAutoModerate: boolean;
};

/**
 * Read the public-facing comment settings.
 *
 * This used to return hardcoded `{ commentsEnabled: true }` on the grounds that
 * no public settings endpoint existed, so an admin who turned comments off still
 * got a comment form on every article: visitors filled it in and were answered
 * with a 403 from the server that does enforce the setting. It now reads the
 * cached site settings, which means the form appears only when comments are
 * actually accepted.
 */
export async function getCommentSettings(): Promise<CommentSettings> {
  const { commentsEnabled, commentAutoModerate } = await getSiteSettings();
  return { commentsEnabled, commentAutoModerate };
}

export type AdminListOptions = {
  status?: string;
  post?: string;
  limit?: number;
  offset?: number;
  /**
   * The browser's Cookie header for this request (`locals.cookie`).
   *
   * ARCHITECTURE.md ID-20: the moderation queue is an admin endpoint, and a
   * server-side fetch has no cookie jar, so without this the page answered itself
   * with 401 and rendered an empty queue.
   */
  cookie?: string | null;
};

export async function getAdminComments(opts: AdminListOptions = {}): Promise<AdminCommentList> {
  const params = new URLSearchParams();
  if (opts.status) params.set('status', opts.status);
  if (opts.post) params.set('post', opts.post);
  if (opts.limit !== undefined) params.set('limit', String(opts.limit));
  if (opts.offset !== undefined) params.set('offset', String(opts.offset));
  const qs = params.toString();

  const res = await apiRequest<AdminCommentList>(`/api/v1/admin/comments${qs ? `?${qs}` : ''}`, {
    cookie: opts.cookie ?? null,
    signal: AbortSignal.timeout(5000),
  });
  return { ...res, items: res.items ?? [], counts: res.counts ?? {}, pending: res.pending ?? 0 };
}
