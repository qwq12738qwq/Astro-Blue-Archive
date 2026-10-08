/**
 * Types for the comment API.
 *
 * ARCHITECTURE.md D4 / §14: `content` is PLAIN TEXT. It is never rendered as
 * HTML or Markdown anywhere in this project.
 */

export type CommentStatus = 'pending' | 'approved' | 'spam' | 'deleted';

export type PublicComment = {
  id: string;
  postSlug: string;
  nickname: string;
  /** Plain text. Render with Astro text interpolation only. */
  content: string;
  status: CommentStatus;
  createdAt: string;
};

export type PublicCommentList = {
  items: PublicComment[];
};

export type AdminCommentList = {
  items: PublicComment[];
  counts: Record<string, number>;
  pending: number;
};

export type CommentSubmitResult = {
  id: string;
  status: CommentStatus;
  postSlug?: string;
  nickname?: string;
  content?: string;
  createdAt?: string;
};
