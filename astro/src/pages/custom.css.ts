/**
 * ARCHITECTURE.md §16/ID-34: the site's custom stylesheet.
 *
 * This URL is the *entire* contract. It answers with `content/system/custom.css`
 * followed by every enabled file in `content/system/css/`, assembled per request.
 * A theme links it and knows nothing about which files are in it — the CMS decides
 * that (ID-38).
 *
 * Served externally rather than inlined so CSP can stay `style-src 'self'` with no
 * `unsafe-inline`, and with `no-cache` + an aggregate ETag so an edit is visible on
 * the next page view with no rebuild.
 */
import type { APIRoute } from 'astro';

import { isFresh, readCustomAssetBundle, serveSystemFile } from '../lib/system';

export const prerender = false;

export const GET: APIRoute = async ({ request }) => {
  const bundle = await readCustomAssetBundle('css');
  if (isFresh(request, bundle)) {
    return new Response(null, { status: 304, headers: { ETag: bundle.etag } });
  }
  return serveSystemFile(bundle, 'text/css');
};
