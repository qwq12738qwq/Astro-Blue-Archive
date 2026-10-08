/**
 * Filesystem live loader.
 *
 * ARCHITECTURE.md §5: this loader is READ-ONLY. Go is the sole writer of
 * content/. The Astro container mounts content read-only.
 *
 * ARCHITECTURE.md §7 / D5: every file is parsed, validated and rendered
 * independently. A single malformed file is logged and skipped; it must never
 * fail the whole collection, because Astro validates a live collection as one
 * batch and would otherwise turn one typo into a site-wide HTTP 500.
 */
import { readdir, readFile, stat } from 'node:fs/promises';
import path from 'node:path';

import { parseFrontmatter, renderMarkdown } from '../lib/markdown';
import { contentRoot } from '../lib/content';
import { fieldErrors } from '../lib/schema';
import type { z } from 'astro/zod';
import type { LiveDataEntry } from 'astro';
import type { LiveLoader } from 'astro/loaders';

/** Hard cap on a single Markdown file, to bound memory and render cost. */
const MAX_FILE_BYTES = 512 * 1024;

/** Bound on the render cache. The cache is a performance detail, never a source of truth. */
const CACHE_LIMIT = 512;

export type CollectionFilter = {
  /** Only include drafts when explicitly requested. Defaults to false. */
  drafts?: boolean;
};

export type EntryFilter = CollectionFilter & {
  id: string;
};

export type InvalidEntry = {
  collection: string;
  file: string;
  reason: string;
  fields?: Record<string, string>;
  at: string;
};

/** Invalid entries are surfaced in the admin UI so they can be fixed. */
const invalidEntries: InvalidEntry[] = [];
const MAX_INVALID_REPORTED = 100;

export function getInvalidEntries(collection?: string): InvalidEntry[] {
  return collection === undefined
    ? [...invalidEntries]
    : invalidEntries.filter((e) => e.collection === collection);
}

function reportInvalid(entry: InvalidEntry): void {
  console.error(
    JSON.stringify({
      level: 'error',
      msg: 'content_entry_invalid',
      collection: entry.collection,
      file: entry.file,
      reason: entry.reason,
      fields: entry.fields,
    }),
  );
  invalidEntries.push(entry);
  if (invalidEntries.length > MAX_INVALID_REPORTED) invalidEntries.shift();
}

type CachedRender = {
  mtimeMs: number;
  size: number;
  id: string;
  data: Record<string, unknown>;
  html: string;
  headings: { depth: number; slug: string; text: string }[];
};

const renderCache = new Map<string, CachedRender>();

function cacheGet(key: string): CachedRender | undefined {
  const hit = renderCache.get(key);
  if (hit) {
    // Refresh recency.
    renderCache.delete(key);
    renderCache.set(key, hit);
  }
  return hit;
}

function cacheSet(key: string, value: CachedRender): void {
  renderCache.set(key, value);
  while (renderCache.size > CACHE_LIMIT) {
    const oldest = renderCache.keys().next();
    if (oldest.done) break;
    renderCache.delete(oldest.value);
  }
}

/** Drop cache entries for files that no longer exist. */
async function pruneCache(dir: string): Promise<void> {
  for (const key of [...renderCache.keys()]) {
    if (!key.startsWith(`${dir}${path.sep}`)) continue;
    try {
      await stat(key);
    } catch {
      renderCache.delete(key);
    }
  }
}

export type MarkdownLoaderOptions = {
  /** Collection name, used in error reports. */
  collection: string;
  /** Directory under CONTENT_ROOT, e.g. "posts". */
  subdir: string;
  /** Zod schema, also declared in live.config.ts. */
  schema: z.ZodType;
};

type LoadOutcome = { ok: true; entries: LiveDataEntry[] } | { ok: false; fatal: string };

/**
 * Read and validate every Markdown file in a content subdirectory.
 *
 * Returns `fatal` only for problems that affect the whole directory (a missing
 * content root). Per-file problems are reported and skipped.
 */
async function readAll(opts: MarkdownLoaderOptions): Promise<LoadOutcome> {
  let dir: string;
  try {
    dir = path.join(contentRoot(), opts.subdir);
  } catch (err) {
    return { ok: false, fatal: err instanceof Error ? err.message : String(err) };
  }

  let names: string[];
  try {
    names = await readdir(dir);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    return {
      ok: false,
      fatal: `cannot read ${opts.subdir}/ directory (${code ?? 'error'}): ${err instanceof Error ? err.message : String(err)}`,
    };
  }

  await pruneCache(dir);

  const files = names.filter((n) => n.endsWith('.md')).sort();
  const entries: LiveDataEntry[] = [];

  for (const name of files) {
    const filePath = path.join(dir, name);
    const relPath = `${opts.subdir}/${name}`;

    let st;
    try {
      st = await stat(filePath);
      if (!st.isFile()) continue;
    } catch {
      continue;
    }

    if (st.size > MAX_FILE_BYTES) {
      reportInvalid({
        collection: opts.collection,
        file: relPath,
        reason: `file is ${st.size} bytes, limit is ${MAX_FILE_BYTES}`,
        at: new Date().toISOString(),
      });
      continue;
    }

    const cached = cacheGet(filePath);
    if (cached && cached.mtimeMs === st.mtimeMs && cached.size === st.size) {
      entries.push(toEntry(cached));
      continue;
    }

    let source: string;
    try {
      source = await readFile(filePath, 'utf-8');
    } catch (err) {
      reportInvalid({
        collection: opts.collection,
        file: relPath,
        reason: `unreadable: ${err instanceof Error ? err.message : String(err)}`,
        at: new Date().toISOString(),
      });
      continue;
    }

    let parsed: ReturnType<typeof parseFrontmatter>;
    try {
      parsed = parseFrontmatter(source);
    } catch (err) {
      reportInvalid({
        collection: opts.collection,
        file: relPath,
        reason: `malformed YAML frontmatter: ${err instanceof Error ? err.message : String(err)}`,
        at: new Date().toISOString(),
      });
      continue;
    }

    // ARCHITECTURE.md §9: the filename stem is the URL, and frontmatter.slug
    // must agree with it. Two authorities for one URL is a data-integrity bug,
    // so a mismatch is a hard skip rather than a silent preference.
    const stem = name.slice(0, -3);
    const fmSlug = typeof parsed.data['slug'] === 'string' ? parsed.data['slug'] : undefined;
    if (fmSlug !== stem) {
      reportInvalid({
        collection: opts.collection,
        file: relPath,
        reason: `frontmatter.slug ${JSON.stringify(fmSlug ?? null)} does not match filename stem ${JSON.stringify(stem)}`,
        fields: { slug: 'must equal the filename without the .md extension' },
        at: new Date().toISOString(),
      });
      continue;
    }

    const result = opts.schema.safeParse(parsed.data);
    if (!result.success) {
      reportInvalid({
        collection: opts.collection,
        file: relPath,
        reason: 'frontmatter failed schema validation',
        fields: fieldErrors(result.error),
        at: new Date().toISOString(),
      });
      continue;
    }

    let rendered: Awaited<ReturnType<typeof renderMarkdown>>;
    try {
      rendered = await renderMarkdown(parsed.body);
    } catch (err) {
      reportInvalid({
        collection: opts.collection,
        file: relPath,
        reason: `markdown render failed: ${err instanceof Error ? err.message : String(err)}`,
        at: new Date().toISOString(),
      });
      continue;
    }

    const record: CachedRender = {
      mtimeMs: st.mtimeMs,
      size: st.size,
      id: stem,
      data: result.data as Record<string, unknown>,
      html: rendered.html,
      headings: rendered.headings,
    };
    cacheSet(filePath, record);
    entries.push(toEntry(record));
  }

  return { ok: true, entries };
}

function toEntry(record: CachedRender): LiveDataEntry {
  return {
    id: record.id,
    data: record.data,
    rendered: { html: record.html },
    cacheHint: { lastModified: new Date(record.mtimeMs) },
  };
}

/** ARCHITECTURE.md §10: drafts are hidden unless explicitly requested. */
function visibleDrafts(filter: CollectionFilter | undefined): boolean {
  return filter?.drafts === true;
}

export function createMarkdownLoader(
  opts: MarkdownLoaderOptions,
): LiveLoader<Record<string, any>, EntryFilter, CollectionFilter> {
  return {
    name: `blogcms-fs-${opts.collection}`,

    async loadCollection({ filter }) {
      const outcome = await readAll(opts);
      if (!outcome.ok) {
        console.error(
          JSON.stringify({ level: 'fatal', msg: 'content_root_error', detail: outcome.fatal }),
        );
        return { error: new Error(outcome.fatal) };
      }
      const drafts = visibleDrafts(filter);
      return {
        entries: drafts ? outcome.entries : outcome.entries.filter((e) => e.data['draft'] !== true),
      };
    },

    async loadEntry({ filter }) {
      const outcome = await readAll(opts);
      if (!outcome.ok) {
        console.error(
          JSON.stringify({ level: 'fatal', msg: 'content_root_error', detail: outcome.fatal }),
        );
        return { error: new Error(outcome.fatal) };
      }
      const id = filter?.id;
      const found = outcome.entries.find((e) => e.id === id);
      if (!found) return undefined;
      // Second enforcement point: even a single-entry lookup hides drafts
      // unless drafts were explicitly allowed (ARCHITECTURE.md §10).
      if (found.data['draft'] === true && !visibleDrafts(filter)) return undefined;
      return found;
    },
  };
}

/** Test hook: clear caches and the invalid-entry report. */
export function resetLoaderState(): void {
  renderCache.clear();
  invalidEntries.length = 0;
}
