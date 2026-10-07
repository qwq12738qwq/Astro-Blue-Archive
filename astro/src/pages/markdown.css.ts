/**
 * ARCHITECTURE.md §34: the Markdown content stylesheet.
 *
 * This URL is the *entire* contract. It answers with every enabled
 * file in `content/system/markdown/`, assembled per request. The
 * public layouts link it after the theme stylesheet and before
 * `/custom.css`, so a template styles the article while the theme
 * still owns the page around it (ID-38).
 *
 * Served externally rather than inlined so CSP can stay
 * `style-src 'self'` with no `unsafe-inline`, and with `no-cache` +
 * an aggregate ETag so an edit is visible on the next page view
 * with no rebuild.
 */
import type { APIRoute } from 'astro';

import {
  isMarkdownTemplateFresh,
  readMarkdownTemplateBundle,
  serveMarkdownTemplateBundle,
} from '../lib/markdown-css';

export const prerender = false;

export const GET: APIRoute = async ({ request }) => {
  const bundle = await readMarkdownTemplateBundle();
  if (isMarkdownTemplateFresh(request, bundle)) {
    return new Response(null, { status: 304, headers: { ETag: bundle.etag } });
  }
  return serveMarkdownTemplateBundle(bundle);
};
