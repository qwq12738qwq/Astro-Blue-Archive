/**
 * Content schemas.
 *
 * ARCHITECTURE.md §9: this is the single definition used by both the live
 * loader (per-file validation) and `live.config.ts` (Astro-level validation).
 * ARCHITECTURE.md R4: the Go backend has an independent but equivalent
 * validator; a Go test asserts the two field lists stay in sync.
 */
import { z } from 'astro/zod';

/** ARCHITECTURE.md §9: strict slug grammar. */
export const SLUG_PATTERN = /^[a-z0-9]+(-[a-z0-9]+)*$/;
export const SLUG_MAX = 80;

export const TAG_PATTERN = /^[a-z0-9][a-z0-9-]{0,31}$/;

export const MAX_TITLE = 200;
export const MAX_DESCRIPTION = 300;
export const MAX_TAGS = 20;
export const MAX_BODY_BYTES = 512 * 1024;

const slug = z
  .string()
  .min(1)
  .max(SLUG_MAX)
  .regex(SLUG_PATTERN, 'must match ^[a-z0-9]+(-[a-z0-9]+)*$');

const tag = z
  .string()
  .max(32)
  .regex(TAG_PATTERN, 'must match ^[a-z0-9][a-z0-9-]{0,31}$')
  .transform((t) => t.toLowerCase());

/**
 * Unknown keys are rejected rather than dropped: a typo like `descripton`
 * should surface as an error instead of silently losing data.
 */
export const postSchema = z.strictObject({
  title: z.string().min(1).max(MAX_TITLE),
  slug,
  description: z.string().max(MAX_DESCRIPTION).optional(),
  date: z.coerce.date(),
  updated: z.coerce.date().optional(),
  tags: z.array(tag).max(MAX_TAGS).default([]),
  cover: z.string().max(300).optional(),
  draft: z.boolean().default(false),
});

/**
 * Pages share the post frontmatter model.
 *
 * `date` is carried by the shared writer but is not used for ordering (pages
 * order by `navOrder`), so it is accepted and ignored here rather than being
 * special-cased out of the Go side.
 */
export const pageSchema = z.strictObject({
  title: z.string().min(1).max(MAX_TITLE),
  slug,
  description: z.string().max(MAX_DESCRIPTION).optional(),
  date: z.coerce.date().optional(),
  tags: z.array(tag).max(MAX_TAGS).default([]),
  cover: z.string().max(300).optional(),
  draft: z.boolean().default(false),
  navOrder: z.number().int().optional(),
});

export type PostData = z.infer<typeof postSchema>;
export type PageData = z.infer<typeof pageSchema>;

/** Flatten a Zod error into `field -> message` for display in the admin UI. */
export function fieldErrors(error: z.ZodError): Record<string, string> {
  const out: Record<string, string> = {};
  for (const issue of error.issues) {
    const key = issue.path.length > 0 ? issue.path.join('.') : '(frontmatter)';
    if (!(key in out)) out[key] = issue.message;
  }
  return out;
}
