/**
 * Content root resolution.
 *
 * ARCHITECTURE.md §6: `process.env.CONTENT_ROOT` is the only supported source.
 * `new URL('../content/', import.meta.url)` is forbidden because at runtime
 * `import.meta.url` points inside `dist/server/chunks/`, which silently yields
 * an empty collection.
 *
 * Resolution is lazy so that `astro build` works in an image stage where no
 * content volume is mounted, but the first request fails loudly if the
 * variable is missing or wrong. A misconfigured deployment must never present
 * as a healthy empty blog.
 */
import { statSync } from 'node:fs';
import path from 'node:path';

let cached: string | null = null;

export class ContentRootError extends Error {
  constructor(message: string) {
    super(`CONTENT_ROOT misconfigured: ${message}`);
    this.name = 'ContentRootError';
  }
}

export function contentRoot(): string {
  if (cached !== null) return cached;

  const raw = process.env.CONTENT_ROOT;
  if (!raw || raw.trim() === '') {
    throw new ContentRootError(
      'environment variable CONTENT_ROOT is not set. Mount the content volume and set CONTENT_ROOT.',
    );
  }

  const abs = path.resolve(raw.trim());

  let st: ReturnType<typeof statSync>;
  try {
    st = statSync(abs);
  } catch {
    throw new ContentRootError(`path ${abs} does not exist. Is the content volume mounted?`);
  }
  if (!st.isDirectory()) {
    throw new ContentRootError(`path ${abs} is not a directory.`);
  }

  cached = abs;
  return abs;
}

/** Absolute path of a subdirectory of the content root. */
export function contentPath(...segments: string[]): string {
  return path.join(contentRoot(), ...segments);
}

/** Test hook: forget the resolved root. */
export function resetContentRootCache(): void {
  cached = null;
}
