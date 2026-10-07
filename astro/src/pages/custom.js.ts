/**
 * ARCHITECTURE.md §17/ID-34: the site's custom JavaScript.
 *
 * This URL is the *entire* contract. It answers with `content/system/custom.js`
 * followed by every enabled file in `content/system/js/`, assembled per request, and
 * it is the ONLY way admin JavaScript reaches a visitor's browser. No theme script
 * can replace it and none can be swapped for it: the themes' own scripts are separate
 * assets (ID-39).
 *
 * Anonymous visitors can never write any of it: the files are written solely through
 * `/api/v1/admin/custom/*`, every route of which requires a valid admin session, a
 * matching Origin and a CSRF token.
 */
import type { APIRoute } from 'astro';

import { isFresh, readCustomAssetBundle, serveSystemFile } from '../lib/system';

export const prerender = false;

export const GET: APIRoute = async ({ request }) => {
  const bundle = await readCustomAssetBundle('js');
  if (isFresh(request, bundle)) {
    return new Response(null, { status: 304, headers: { ETag: bundle.etag } });
  }
  return serveSystemFile(bundle, 'text/javascript');
};
