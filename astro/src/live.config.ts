/**
 * Live content collections.
 *
 * ARCHITECTURE.md D1: these are LIVE collections, not build-time collections.
 * A build-time collection (`defineCollection` + `getCollection`) snapshots
 * content during `astro build`, which would violate ARCHITECTURE.md §13
 * ("no manual astro build to see an article change").
 *
 * ARCHITECTURE.md D5: the loader validates each file independently and skips
 * bad ones. The `schema` below is a second, Astro-level gate — defence in
 * depth, not the primary isolation mechanism.
 *
 * ARCHITECTURE.md D2: V1 is Markdown + YAML frontmatter only. There is no MDX
 * loader and no MDX runtime compiler.
 */
import { defineLiveCollection } from 'astro:content';

import { createMarkdownLoader } from './loaders/fs';
import { postSchema, pageSchema } from './lib/schema';

const posts = defineLiveCollection({
  loader: createMarkdownLoader({
    collection: 'posts',
    subdir: 'posts',
    schema: postSchema,
  }),
  schema: postSchema,
});

const pages = defineLiveCollection({
  loader: createMarkdownLoader({
    collection: 'pages',
    subdir: 'pages',
    schema: pageSchema,
  }),
  schema: pageSchema,
});

export const collections = { posts, pages };
