# Architecture Proposal — File-First Blog CMS

Status: **implemented.** Sections 5, 18, 25 and 28 are **superseded** — see
§12/ID-11. The deployment is a single Astro process in front of the Go backend on
loopback, with **no reverse proxy**. Sections 1, 3, 9, 16, 17 and 19 are
unchanged and describe the running system. Themability is settled by
§12/ID-13: the theme is a set of custom properties, the admin's `custom.css`
wins over it, and `<html data-theme>` is the only selection hook.

Toolchain actually verified on this machine:

| Tool | Version |
|---|---|
| Go | 1.25.3 |
| Node | 22.23.3 |
| Astro | **7.3.5** |
| `@astrojs/node` | 11.1.6 |
| SQLite (CLI) | 3.40.1 |
| Processes | Astro + Go, started as two commands (no launcher script, ID-25) |

---

## 0. Findings that change the design (verified by spike, not guessed)

I ran a throwaway spike in `/tmp/opencode/spike` (outside the repo) against real Astro 7.3.5 in
**production SSR mode** (`astro build` → `node dist/server/entry.mjs`) to settle the single riskiest
requirement — *"文章修改之后不应该要求手工执行 astro build 才能看到内容"* (§13).

### F1 — Build-time collections CANNOT satisfy §13. Live collections can.

`defineCollection` + `getCollection()` (the classic Content Collections API) writes content into a
**build-time data store** (`.astro/data-store.json` + digest checks). It is snapshotted by
`astro build`. That directly contradicts §13.

Astro 6+ / 7 added **live content collections**: `src/live.config.ts`, `defineLiveCollection()`,
`getLiveCollection()` / `getLiveEntry()`. A live loader exposes `loadCollection()` /
`loadEntry()` and is invoked **on every request**.

**Verified in production SSR, no rebuild between steps:**

```
STEP 1 (initial)
  GET /              -> <li><a href="/posts/hello-world">Hello World</a></li>
  GET /posts/hello-world -> <h1>Hello World</h1><h1 id="hello">Hello</h1><p>Original body text.</p>

STEP 2 (edit hello.md + create second.md — NO rebuild)
  GET /              -> <li>Hello World EDITED</li><li>Second Post</li>
  GET /posts/hello-world -> <h1>Hello World EDITED</h1><p>Body text changed by admin at runtime.</p>
  GET /posts/second-post  -> 200 "Brand new post created after the build."

STEP 3 (delete second.md — NO rebuild)
  GET /              -> <li>Hello World EDITED</li>
  GET /posts/second-post  -> 404
```

This is the mechanism the whole architecture rests on. **Decision required (D1).**

### F2 — We do not need to invent a Markdown parser

Inside the live loader I render Markdown with
`createSatteriMarkdownProcessor()` from `@astrojs/markdown-satteri` — **the same processor Astro
core uses** (GFM, Shiki/Prism highlighting, heading-ID generation), and parse frontmatter with
`parseFrontmatter()` from `@astrojs/internal-helpers/frontmatter` (also Astro's own).

Verified output: `<h1 id="hello">Hello</h1>` + `metadata.headings`. So §5 (use Astro's loader/validation,
don't hand-roll a parser) is satisfied. Risk noted in §10 (R5).

### F3 — Collection validation is all-or-nothing. One bad file = 500

Astro validates live entries against the collection `schema` **after the loader returns**, as a
batch. One malformed file makes `getLiveCollection()` return `{ error }` with `entries === undefined`:

```
STEP 4 (add bad.md with `title: 12345`, `date: not-a-date`)
  [ERROR] TypeError: Cannot read properties of undefined (reading 'map')   -> HTTP 500
```

One typo in one post takes down the whole blog index. **The loader must therefore validate each
entry individually and skip + report bad ones.** Not optional — this is the single biggest
availability risk in the design. Handled by design D5.

### F4 — Content root must come from a runtime env var

`new URL('../content/', import.meta.url)` resolves against `dist/server/chunks/` at runtime →
`dist/server/content/`, which does not exist. Silent empty collection (the failure I actually hit).
The live loader **must** read `process.env.CONTENT_ROOT` at request time.

### F5 — Live loaders get no request context

`loadCollection({ filter, collection })` receives no `Request`, no cookies. Draft preview therefore
cannot be decided inside the loader. Verified that the `filter` argument is forwarded **verbatim**
from `getLiveCollection(name, filter)`:

```
GET /           -> [loader] filter = undefined            (draft hidden)
GET /?drafts=1  -> [loader] filter = {"drafts":true}      (draft visible)
```

So the **Astro page** decides (from `Astro.locals`, set by middleware) and passes a filter. Handled
by design D6.

---

## 1. Repository architecture

Three processes, one origin, one authority per concern.

```
                    Browser
                       │  https://blog.example  (single origin, Caddy :443)
                       ▼
                 ┌───────────┐
                 │  (proxy)  │  SUPERSEDED: these duties now live in Astro (§12/ID-11)
                 └─────┬─────┘  everything else → astro
              ┌────────┴─────────┐
              ▼                  ▼
      ┌───────────────┐   ┌──────────────────┐
      │ Astro (SSR)   │   │ Go backend       │
      │ :4321         │──▶│ :8080  JSON API  │
      │               │   │                  │
      │ THE ONLY HTML │   │ NO HTML. EVER.   │
      │ RENDERER      │   │                  │
      └───────┬───────┘   └────────┬─────────┘
              │                    │
              ▼                    ▼
   ┌────────────────────┐  ┌──────────────────┐
   │ /content  (ro-ish) │  │ /data/blog.db    │
   │  posts/ pages/     │  └──────────────────┘
   │  system/           │  ┌──────────────────┐
   │  ★ SOURCE OF TRUTH │  │ /media           │
   └────────────────────┘  └──────────────────┘
```

**Single-writer rule:** only Go writes to `/content`. Astro only ever reads. This makes the
filesystem unambiguous as the source of truth and eliminates write races.

**Reading content — there are exactly two readers, and that is deliberate:**

| Reader | Role | May write? |
|---|---|---|
| Go `internal/content` | validates + writes on admin save | **yes, sole writer** |
| Astro `src/loaders/fs.ts` (live loader) | reads + renders at request time | no, read-only |

Both read the *same files*. Neither caches content in the DB. See R4 in §10.

### Trust boundary

- **Anonymous input**: comments, login credentials, media uploads, request params.
  Never becomes HTML, never becomes a path, never becomes JS.
- **Admin-trusted input**: post/page Markdown, and `custom.css` / `custom.js`
  when the admin has created them (ID-33).
  Arbitrary JS by design (that is the feature), so the admin can break their own site — accepted,
  documented, and *only* the admin can reach it.

---

## 2. Directory structure

```
/
├── astro/                          # frontend ONLY
│   ├── public/                     # the core's static assets and nothing else:
│   │   ├── cms.js                  # the fixed-URL core scripts (§17/§19)
│   │   ├── color-scheme.js         # the scheme toggle, applied before first paint
│   │   ├── comments.js             # A theme's own assets live in its own
│   │   │                           # directory, never here
│   │   ├── favicon.png             # the previous default mark, kept for
│   │   │                           # readers who pinned it
│   │   └── WordPress/…/KivoTos.png # the core default favicon (§35), served
│   │                               # at the path it occupies, never rewritten (§25)
│   ├── src/
│   │   ├── live.config.ts          # posts / pages / system live collections
│   │   ├── loaders/
│   │   │   └── fs.ts               # filesystem live loader (read-only, mtime-cached)
│   │   ├── lib/                    # api, markdown, session, seo, media, …
│   │   ├── middleware.ts           # session → Astro.locals, CSRF, the one theme resolver
│   │   ├── theme-system/           # the CMS theme FRAMEWORK — the engine a
│   │   │   ├── contract.ts         # theme plugs into: the Theme Contract, the
│   │   │   ├── ids.ts              # id allowlist, the compile-time registry,
│   │   │   ├── js-contract.ts      # the per-request resolver, the view models
│   │   │   ├── registry.ts         # and the core-script/attribute contract.
│   │   │   ├── resolve.ts          # Distinct from themes/ on purpose: one is
│   │   │   └── view.ts             # the machinery, the other are the machines.
│   │   ├── themes/                 # installed CMS THEMES — one directory per
│   │   │   └── <id>/               # theme, registered in theme-system/registry.ts
│   │   │       ├── theme.ts        # the manifest; the only file the registry imports
│   │   │       ├── assets/         # the theme's own static files: artwork,
│   │   │       │   ├── img/        # cursors, webfonts. Served at the stable
│   │   │       │   ├── cursors/    # URL /themes/<id>/… (astro.config.mjs),
│   │   │       │   └── fonts/      # because admin content cannot know a hash
│   │   │       ├── public/         # the public component tree:
│   │   │       │   ├── layouts/    #   layouts, components, views, styles
│   │   │       │   ├── components/
│   │   │       │   ├── views/
│   │   │       │   └── styles/
│   │   │       └── admin/          # the admin component tree, same four folders
│   │   └── pages/                  # routes: the Astro app shell itself
│   │       ├── index.astro
│   │       ├── posts/[slug].astro
│   │       ├── pages/[slug].astro
│   │       ├── tags/[tag].astro
│   │       ├── custom.css.ts       # streams /content/system/custom.css
│   │       ├── custom.js.ts        # streams /content/system/custom.js
│   │       ├── 404.astro
│   │       └── admin/
│   │           ├── login.astro
│   │           ├── index.astro
│   │           ├── posts/index.astro · new.astro · [slug].astro
│   │           ├── pages/index.astro · new.astro · [slug].astro
│   │           ├── comments.astro
│   │           ├── media.astro
│   │           ├── settings.astro
│   │           └── custom-code.astro
│   ├── astro.config.mjs            # output:'server', node adapter, the theme-assets plugin
│   ├── package.json
│   └── tsconfig.json
│
├── backend/                        # Go ONLY
│   ├── cmd/server/main.go
│   ├── internal/
│   │   ├── config/                 # env parsing, validation
│   │   ├── api/                    # router, middleware, handlers (JSON only)
│   │   ├── auth/                   # argon2id, sessions, CSRF
│   │   ├── store/                  # sqlite schema + queries (WAL)
│   │   ├── content/                # read/write posts|pages|system on disk
│   │   ├── comments/
│   │   ├── media/
│   │   ├── ratelimit/
│   │   ├── httpx/                  # JSON helpers, error envelope, limits
│   │   └── testutil/               # shared test scaffolding (§30): the
│   │                               # temp content tree, the config env
│   ├── go.mod
│   └── go.sum
│
├── content/                        # ★ SINGLE SOURCE OF TRUTH
│   ├── posts/*.md
│   ├── pages/*.md
│   └── system/
│       ├── custom.css            # the optional legacy pair (ID-33): a
│       ├── custom.js             # fresh install has neither; the first
│       ├── css/NNN-name.css      # save creates the file. Managed assets
│       ├── js/NNN-name.js        # (§33) and Markdown templates (§34)
│       └── markdown/NNN-name.css # live in their own directories; a file
│                                   # in parked/ beside them is disabled.
├── media/                          # uploads only
├── data/                           # blog.db + runtime state only
├── AGENTS.md
└── ARCHITECTURE.md
```

`backend/internal/api` will contain **zero** HTML, templates, or `text/html` writes. This is
mechanically checkable in CI (see §9).

---

## 3. Database schema proposal

Only what **cannot** be derived from the filesystem. Note what is deliberately *absent*: no `posts`
table, no `pages` table, no title/body/slug/tags/draft columns anywhere.

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

-- 1 admin. CHECK(id=1) enforces "only one admin" at the schema level.
CREATE TABLE admin_user (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,          -- argon2id PHC string, never reversible
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

-- Sessions. Only a SHA-256 of the token is stored; DB theft cannot yield a cookie.
CREATE TABLE session (
  id           TEXT PRIMARY KEY,        -- random 32B hex (identifier only, not the token)
  token_hash   BLOB NOT NULL UNIQUE,    -- SHA-256(raw token)
  csrf_secret  BLOB NOT NULL,           -- 32B, for the double-submit token
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  ip_hash      BLOB,                    -- hashed, not raw (privacy)
  user_agent   TEXT NOT NULL
);
CREATE INDEX idx_session_expires ON session(expires_at);

-- Brute-force throttle (§8).
CREATE TABLE login_attempt (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  username   TEXT NOT NULL,
  ip_hash    BLOB NOT NULL,
  ok         INTEGER NOT NULL,          -- 0 fail / 1 success
  created_at TEXT NOT NULL
);
CREATE INDEX idx_login_lookup ON login_attempt(username, ip_hash, created_at);

-- Comments. post_slug is a *reference* to a filesystem slug, NOT a copy of content.
CREATE TABLE comment (
  id           TEXT PRIMARY KEY,        -- random 16B hex
  post_slug    TEXT NOT NULL,
  nickname     TEXT NOT NULL,
  content      TEXT NOT NULL,           -- PLAIN TEXT, see §7
  status       TEXT NOT NULL CHECK (status IN ('pending','approved','spam','deleted')),
  created_at   TEXT NOT NULL,
  moderated_at TEXT,
  ip_hash      BLOB,
  user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_comment_post   ON comment(post_slug, status, created_at);
CREATE INDEX idx_comment_status ON comment(status, created_at);

-- Media metadata only. `path` is always server-generated, relative to MEDIA_ROOT.
CREATE TABLE media (
  id         TEXT PRIMARY KEY,          -- random 12B hex
  filename   TEXT NOT NULL,            -- sanitized basename, e.g. 2026-10-03-a1b2c3.png
  path       TEXT NOT NULL UNIQUE,      -- "2026/10/<id>.png" — never client-supplied
  mime       TEXT NOT NULL,
  size       INTEGER NOT NULL,
  width      INTEGER, height INTEGER,
  sha256     BLOB NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX idx_media_created ON media(created_at DESC);

-- Runtime system settings (NOT content: things like posts_per_page, comment_auto_moderate).
CREATE TABLE setting (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,            -- JSON-encoded
  updated_at TEXT NOT NULL
);

-- Fixed-window rate limit buckets (comments, login). DB-backed so limits survive restart.
CREATE TABLE rate_limit (
  bucket       TEXT NOT NULL,          -- e.g. "comment:ip:<hash>"
  window_start INTEGER NOT NULL,       -- unix seconds, floor to window
  count        INTEGER NOT NULL,
  PRIMARY KEY (bucket, window_start)
) WITHOUT ROWID;

-- Append-only audit trail (admin mutations + comment moderation). Also the cache-invalidation log.
CREATE TABLE content_event (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL,            -- post.create|post.update|post.delete|page.*|comment.moderate|media.*|custom_code.update
  ref        TEXT NOT NULL,
  actor      TEXT NOT NULL DEFAULT 'admin',
  created_at TEXT NOT NULL
);
CREATE INDEX idx_event_created ON content_event(created_at DESC);
```

**Explicit non-goals:** no `post`/`page` table, no `content_cache` table, no search index table.
`content_event` is an audit log, not a content store.

---

## 4. API endpoint proposal

Base path `/api/v1`. **JSON only.** Served under the same origin as Astro (Caddy routes
`/api/*` → Go), so cookies are first-party and no CORS is needed.

Error envelope (uniform, so the Astro client has one error path):

```json
{ "error": { "code": "validation_failed", "message": "human readable", "fields": {"slug":"..."} } }
```

### Public

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/healthz` | liveness; no DB |
| `GET` | `/api/v1/readyz` | readiness; pings DB + `CONTENT_ROOT` |
| `POST` | `/api/v1/auth/login` | `{username,password}` → sets cookie; rate limited; constant-time |
| `POST` | `/api/v1/auth/logout` | destroys session, clears cookie |
| `GET` | `/api/v1/auth/session` | `{authenticated, username, csrfToken, expiresAt}` |
| `GET` | `/api/v1/comments?post=<slug>&limit=&cursor=` | **approved only** |
| `POST` | `/api/v1/comments` | `{postSlug,nickname,content}` → `201 {id,status:"pending"}` |

### Admin (session cookie + CSRF header required)

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/admin/posts?q=&status=&limit=&cursor=` | list from **filesystem**; returns raw fm + raw body |
| `POST` | `/api/v1/admin/posts` | create; validates, writes `content/posts/<slug>.md` |
| `GET` | `/api/v1/admin/posts/<slug>` | raw frontmatter + raw body (never HTML) |
| `PUT` | `/api/v1/admin/posts/<slug>` | update; atomic temp-file + rename |
| `DELETE` | `/api/v1/admin/posts/<slug>` | delete file |
| `POST` | `/api/v1/admin/posts/validate` | dry-run frontmatter validation for the editor |
| `GET` | `/api/v1/admin/pages…` | same six verbs as posts, `/content/pages/` |
| `GET` | `/api/v1/admin/comments?status=&post=&cursor=` | all statuses |
| `PATCH` | `/api/v1/admin/comments/<id>` | `{status}` transitions |
| `DELETE` | `/api/v1/admin/comments/<id>` | soft delete (`status='deleted'`) |
| `GET` | `/api/v1/admin/media?cursor=` | metadata list |
| `POST` | `/api/v1/admin/media` | multipart; validates MIME/magic/size/ext; returns id |
| `DELETE` | `/api/v1/admin/media/<id>` | delete file + row |
| `GET` | `/api/v1/admin/custom-code` | `{css,js}` from `content/system/*` |
| `PUT` | `/api/v1/admin/custom-code` | writes `content/system/*` |
| `GET` | `/api/v1/admin/settings` | runtime settings |
| `PUT` | `/api/v1/admin/settings` | allowlisted keys only |

**Go returns raw Markdown and frontmatter to the admin editor — never rendered HTML.** The editor
is a plain `<textarea>`; the live preview renders in Astro.

---

## 5. Docker Compose architecture

> **SUPERSEDED — see §12/ID-11.** There is no compose file and no Caddy. Astro proxies /api/* and serves /media/* itself.

Three services, all in Docker, dev and prod.

```yaml
# docker-compose.yml (base — production-shaped)
services:
  caddy:
    image: caddy:2-alpine
    ports: ["80:80", "443:443"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data          # TLS certs persist
      - caddy_config:/config
    depends_on: [astro, backend]
    restart: unless-stopped

  astro:
    build: { context: ./astro }
    environment:
      CONTENT_ROOT: /srv/content      # F4 — runtime env, NOT import.meta.url
      API_BASE: http://backend:8080
      PUBLIC_ORIGIN: https://blog.example
    volumes: [content:/srv/content:ro]   # read-only: Astro never writes content
    expose: ["4321"]
    restart: unless-stopped

  backend:
    build: { context: ./backend }
    environment:
      CONTENT_ROOT: /srv/content
      MEDIA_ROOT: /srv/media
      DATA_ROOT: /srv/data
      SESSION_SECRET_FILE: /srv/data/session_secret
      PUBLIC_ORIGIN: https://blog.example
      SECURE_COOKIES: "true"
    volumes:
      - content:/srv/content            # read-write — Go is the sole content writer
      - media:/srv/media
      - data:/srv/data
    expose: ["8080"]
    healthcheck: { test: ["CMD","/app/healthcheck"], interval: 10s }
    restart: unless-stopped

volumes:
  content:
  media:
  data:
```

`docker-compose.override.yml` (dev, auto-merged): bind-mount sources for hot reload,
`SECURE_COOKIES=false`, `astro dev --host 0.0.0.0`, Caddy on `:8080` with plain HTTP.

**Persistence guarantee** — `content`, `media`, `data` are named volumes (prod) / bind mounts (dev).
`docker compose down` keeps them; only `down -v` destroys them. `content` is `:ro` in Astro, which
enforces the single-writer rule at the mount level, not just by convention.

**Caddyfile routing**

```
blog.example {
  encode zstd gzip
  header { ...security headers, CSP... }

  @api path /api/*
  handle @api { reverse_proxy backend:8080 }

  @media path /media/*
  handle @media {
    header Content-Disposition "attachment"   # neutralize uploaded SVG/HTML
    header Content-Security-Policy "default-src 'none'; sandbox"
    file_server { root /srv/media }
  }

  handle { reverse_proxy astro:4321 }
}
```

Only Caddy binds a host port. Astro and Go are reachable only on the internal Docker network.

**Images** — `astro`: multi-stage `node:22-alpine` (install → build → prune dev deps → run
`dist/server/entry.mjs`), non-root, `dumb-init`. `backend`: multi-stage `golang:1.25-alpine` →
`distroless/static` or `scratch`, non-root, CA certs + `/etc/passwd` for the uid.

---

## 6. Astro routing architecture

`output: 'server'`, adapter `@astrojs/node` (`mode: 'standalone'`). Nothing prerendered.

### Content collections (`src/live.config.ts`)

| Collection | Source | Schema |
|---|---|---|
| `posts` | `$CONTENT_ROOT/posts/*.md` | `title, slug, description?, date, updated?, tags[], draft, cover?` |
| `pages` | `$CONTENT_ROOT/pages/*.md` | `title, slug, description?, draft, navOrder?` |
| `system` | `$CONTENT_ROOT/system/{custom.css,custom.js}` | one entry, raw text; absent until the admin saves |

**Public pages never see drafts.** The page passes a filter derived from `Astro.locals`
(D6/F5), so the public URL physically cannot render a draft even if a filter were forged —
the loader also enforces `draft=false` for unfiltered calls.

### Two ways content reaches a page

1. **Posts/pages** — read straight from the filesystem by the live loader. Go is *not* in the
   request path for reading content.
2. **Comments / session / admin data** — Astro `fetch()` → Go JSON API.

### Custom CSS / JS without weakening CSP

Files are the source of truth (§9 of the spec), but they are served by **Astro endpoints**, not
inlined:

- `GET /custom.css` → `text/css`, `Cache-Control: no-cache`
- `GET /custom.js`  → `text/javascript`, `Cache-Control: no-cache`

```astro
<link rel="stylesheet" href="/custom.css" />
<script src="/custom.js" defer></script>
```

This satisfies §9 (filesystem authoritative, injected by Astro, never by Go) **and** lets CSP stay
strict — `script-src 'self'` with **no `'unsafe-inline'`**. Inlining into `<style>`/`<script>` tags
would have forced `unsafe-inline` and handed anonymous users a CSP bypass primitive.

### Admin pages are Astro, always

`/admin/*` are Astro pages. They gate server-side in the Astro frontmatter: fetch
`/api/v1/auth/session` forwarding the cookie, `redirect('/admin/login')` on 401. Mutations are
plain `fetch()` from `<script>` (vanilla JS — §2 forbids React/Vue/Svelte by default), sending
`credentials: 'same-origin'` and `X-CSRF-Token`.

---

## 7. Content format

### `content/posts/<slug>.md`

```markdown
---
title: Example Article
slug: example
description: A short summary.
date: 2026-10-03
updated: 2026-10-04
tags:
  - astro
  - golang
cover: 2026/10/cover.png
draft: false
---

# Example

Article body.
```

### `content/pages/<slug>.md`

```markdown
---
title: About
slug: about
description: About this site
navOrder: 10
draft: false
---

Page body.
```

**Slug policy (prevents a second source of truth):** `slug` must equal the filename stem.
Go refuses to save otherwise; the loader skips otherwise. The URL therefore has exactly one
determinant — no `slug` field that can disagree with the filename.

Schema rules enforced on write (Go) *and* defensively on read (Astro loader):
`title` 1–200 chars; `slug` `^[a-z0-9]+(-[a-z0-9]+)*$`, ≤80; `description` ≤300;
`tags` ≤20 items, each `^[a-z0-9][a-z0-9-]{0,31}$` (lowercased, no `/`); `date` required ISO date;
`draft` bool; unknown keys rejected (catches typos instead of silently dropping them).

### Comments content — plain text

`comment.content` is stored and rendered as **plain text**. Astro escapes it. No Markdown subset
for anonymous input in v1.

This is a deliberate choice, not a shortcut. A restricted-Markdown subset would mean shipping a
sanitizer tuned to be exactly as strict as the renderer — one rehype/remark version bump is an XSS
in an anonymous-input path. Plain text removes the class of bug entirely. *Escape hatch, if wanted
later:* allow `**bold**`/`*italic*`/autolinked `http(s)` only, rendered with raw HTML disabled and
link `rel="nofollow ugc noopener"` — a Phase 6 hardening item, not Phase 5.

---

## 8. Security threat model

| # | Threat | Control | Where |
|---|---|---|---|
| 1 | **CSRF** | `SameSite=Strict` session cookie **+** per-session `csrf_secret` double-submit header on every mutating request. `Origin` checked against `PUBLIC_ORIGIN`. Astro's own `security.checkOrigin` stays **on in dev too**. | `auth/`, `httpx/` |
| 2 | **XSS (anonymous)** | Comments are plain text and escaped. No `set:html` on user data. No `innerHTML` on API responses. CSP without `unsafe-inline`. | Astro pages, Caddy |
| 3 | **XSS (admin custom.js)** | **Accepted by design** — trusted admin code. Reachable only by admin; documented in `AGENTS.md`. Cannot be triggered by anonymous input. | `/custom.js` |
| 4 | **SQL injection** | All queries are parameterised; **no string-concatenated SQL anywhere**. | `store/` |
| 5 | **Path traversal** | Client slugs/filenames are never concatenated into paths. Pattern-validated (`^[a-z0-9-]+$`), then `filepath.Clean` + prefix check against the root. Upload name is server-generated (`<yyyy>/<mm>/<id>.<ext>`). | `content/`, `media/` |
| 6 | **Arbitrary file upload** | Allowlisted MIME **and** magic-byte sniffing (extension is not trusted), 5 MB cap, generated extension, never `.html/.svg/.js/.xml`. | `media/` |
| 7 | **Malicious SVG / HTML** | Uploaded `.svg` blocked outright. Caddy serves `/media/*` with `Content-Disposition: attachment` + `sandbox` CSP, so even a bypass cannot execute in our origin. | upload + Caddy |
| 8 | **Session fixation** | New token + new `csrf_secret` on every login; rotate on privilege change; session never accepted pre-login. | `auth/` |
| 9 | **Token theft via DB dump** | Only `SHA-256(token)` is stored. | `session` |
| 10 | **Brute force** | DB-backed per-username+IP throttle; exponential lockout; **constant-time** password compare; generic error message (no user enumeration); timing-equalised dummy hash on unknown user. | `auth/` |
| 11 | **Password storage** | argon2id (m=64 MiB, t=3, p=2) PHC string. No bespoke crypto. **See D3.** | `auth/` |
| 12 | **Cookie theft** | `HttpOnly` + `Secure` (when `SECURE_COOKIES` is set) + `SameSite=Strict` + `Path=/` + short max-age + idle expiry. | `auth/` |
| 13 | **Comment abuse / spam** | Per-IP and per-post fixed-window limits; honeypot field; min submit time; link cap; length caps; `pending` by default; moderation queue. | `ratelimit/`, `comments/` |
| 14 | **Malformed Markdown** | Per-entry validation, skip + report (F3/D5). Loader never throws on one bad file. Body byte cap. | `loaders/fs.ts` |
| 15 | **Request flooding** | `http.MaxBytesReader` on every body; 10 s header/body timeouts; `MaxHeaderBytes`; `403` on oversize. | `httpx/` |
| 16 | **SSRF** | Backend makes **no** outbound HTTP. Astro only ever calls the fixed internal `API_BASE`. No user-supplied URL is ever fetched. No `image.domains`/`remotePatterns`. | by absence |
| 17 | **Header injection** | Go sets no user-derived response headers; Caddy strips/overwrites. | Caddy |
| 18 | **Clickjacking** | `X-Frame-Options: DENY` + `frame-ancestors 'none'`. | Caddy |
| 19 | **MIME sniff** | `X-Content-Type-Options: nosniff` on all responses. | Caddy |
| 20 | **Admin enumeration** | Login responses identical for bad user / bad password. | `auth/` |

### Security headers

```
Content-Security-Policy: default-src 'self'; img-src 'self' data:; style-src 'self';
  script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self';
  frame-ancestors 'none'
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: strict-origin-when-cross-origin
Permissions-Policy: geolocation=(), camera=(), microphone=(), interest-cohort=()
```

HSTS is deliberately not emitted: this process is not the TLS
terminator. Whatever fronts the deployment — a CDN, a reverse
proxy — owns the TLS decision, including HSTS.

### TLS-dependent controls

| Control | Default | Who decides |
|---|---|---|
| `Secure` cookie | **off** | the deployer sets `SECURE_COOKIES` for a TLS deployment; on plain HTTP a `Secure` cookie is a lockout, not a hardening |
| TLS itself | plain HTTP | outside this application entirely |
| Login throttle | 5/min → 20/hour | never disabled below 1 |
| `security.checkOrigin` | **on** | no reason to weaken |

No control is disabled by default that would hide a real
vulnerability. The failure mode of a missing env var is
"production-secure", never "production-open".

---

## 9. Implementation phases

Each phase ends with the verification gate in the last column before the next begins.

| Phase | Deliverables | Gate |
|---|---|---|
| **1** Docker, Astro SSR, Go skeleton, Caddy, SQLite, live content loader, public blog (index, post, page, 404, RSS), Dockerfile ×2, compose, volumes | `go vet` · `go test ./...` · `astro check` · `tsc --noEmit` · compose build · edit-a-file-and-see-it |
| **2** Admin login, argon2id, server-side sessions, CSRF, admin layout, dashboard | brute-force + CSRF + fixation tests pass · cookie flags asserted in tests |
| **3** Post CRUD (write to files), Markdown editor, draft/published, frontmatter validation, per-entry resilience | **grep gate: no post/page content columns in schema** · rename/delete reflected with no rebuild · bad file degrades gracefully not 500 |
| **4** Pages, media upload/serve, custom CSS/JS via Astro endpoints | upload rejects renamed `.svg`/`.html` · traversal tests pass · CSP has no `unsafe-inline` |
| **5** Comments, moderation, rate limiting, sanitisation | XSS payloads stored inert · rate limit holds · spam→approved flow |
| **6** Security hardening pass, backup script, structured logging, health/readiness, prod images | full gate below |

**Standing gate (every phase):** `gofmt -l` · `go vet` · `go test -race ./...` ·
`npm run lint` · `npm run check` (`astro check`) · `docker compose build` ·
**boundary test: `grep -rn "text/html\|html/template\|<html" backend/` returns nothing.**

---

## 10. Architectural risks

| ID | Risk | Severity | Mitigation |
|---|---|---|---|
| **R1** | **MDX is not supported with live collections.** `LiveDataEntry.rendered` is `{html: string}` — no component compilation, no `rendered.metadata`. So `.mdx` with Astro components cannot work under §13. **This conflicts with the stated stack (§2 "Markdown / MDX").** | **High — spec conflict** | Recommend **Markdown-first**: `.md` fully supported, `.mdx` explicitly out of scope for v1. Alternatives, both worse: (a) MDX via build-time collection → reintroduces the `astro build` requirement §13 forbids; (b) compile MDX in the live loader → unsupported, fragile. **Needs your decision (D2).** |
| **R2** | One malformed `.md` breaks the whole collection (F3). | **High** | Per-entry validation in the loader: parse → validate → on failure, log + skip + record in an in-memory `invalidEntries` map surfaced in admin. Never throw. Phase-1 requirement, not deferred. |
| **R3** | Per-request re-read + re-render of all Markdown. O(n) per request, Shiki highlighting is the expensive part. | Medium | mtime+size keyed memo cache in the loader (`Map<path,{mtime,size,html}>`), invalidated automatically because mtime changes. LRU-capped. Add only if measured — not speculative. |
| **R4** | **Two readers** of the same files (Go writes/validates, Astro reads/renders). Divergent validation ⇒ a file Go considers valid fails the Astro schema. | Medium | One schema definition is impossible across Go and Zod, so: Go = the only *writer* and the authority on write; Astro loader = read-only and defensive (skips invalid). Canonical field list documented in `AGENTS.md`; a Go test asserts every documented field exists in `live.config.ts`. |
| **R5** | `@astrojs/markdown-satteri` is an Astro-internal package, not part of the documented public content API. An Astro major could break it. | Medium | Pin Astro exactly; confine the entire usage to one file `astro/src/lib/markdown.ts`; CI fails loudly on upgrade rather than silently rendering wrong. Documented as an explicit dependency in `AGENTS.md`. |
| **R6** | Content root misconfiguration (F4) fails **silently** — empty site, no error. | Medium | Loader asserts `$CONTENT_ROOT` exists and is a directory; `/readyz` fails if not; container `HEALTHCHECK` covers it. Fails loud, not empty. |
| **R7** | Live loaders get no request context (F5) — drafts could leak if a filter is forged. | Medium | Public pages pass no filter and the loader hard-defaults to `draft: false`; draft visibility is decided by `Astro.locals`, and the *slug* lookup path re-checks `draft` before rendering. Tested. |
| **R8** | Two origins' worth of state (Astro + Go) means **two** round trips for authenticated admin pages, and cookie scope must cover both. | Low | Single origin via Caddy, cookie `Path=/`, so the browser sends it to both. Admin frontmatter does one session fetch; middleware caches it per request. |
| **R9** | SQLite single-writer contention (Go writes, Go reads; Astro never touches the DB). | Low | WAL + `busy_timeout`, one writer process, short transactions. Not a scaling story — it's a single-admin blog. |
| **R10** | Media growth is unbounded; no image pipeline. | Low | Size cap + per-file type allowlist. `sharp`/image optimisation deliberately **not** added (native dep, no current need). Recorded as a future item. |
| **R11** | Bind-mount semantics differ on Docker Desktop (Linux) vs macOS/Windows. | Low | Prod uses named volumes; bind mounts only in the dev override. `AGENTS.md` documents it. |
| **R12** | Docker/Caddy are not installed on this host, so `docker compose build` **cannot be verified locally**. | Medium | Compose/Caddyfile are written and reviewed, but the Phase-1 "Docker build" gate needs a Docker host. Flagging so the gate isn't silently skipped. |

---

## 11. Decisions needed before Phase 1

| ID | Decision | Recommendation |
|---|---|---|
| **D1** | Use Astro **live collections** (`src/live.config.ts` + `defineLiveCollection`) instead of build-time `getCollection()` | **Yes.** Build-time collections snapshot content at `astro build` and violate §13. Verified working (F1). |
| **D2** | **MDX is not possible** with live collections (R1). Drop `.mdx` from v1 scope, or accept a rebuild requirement? | **Drop MDX; Markdown-first.** Preserves §13, which is a hard architectural constraint. |
| **D3** | Password hashing: `argon2id` via `golang.org/x/crypto` (OWASP first choice, 2 small deps) vs stdlib `crypto/pbkdf2` (Go 1.24+, zero deps) | **argon2id.** §11 says "现代密码哈希算法" and "不要自行发明"; argon2id is the modern recommendation. Both are already verified working in this environment. |
| **D4** | Comments stored/rendered as **plain text** (§7) rather than a safe Markdown subset | **Plain text.** Removes the sanitizer-bypass class of bug from an anonymous-input path. Extendable in Phase 6. |
| **D5** | Per-entry validation + skip/report in the live loader (R2/F3) | **Required, in Phase 1.** Not deferred — otherwise one typo 500s the blog. |
| **D6** | Custom CSS/JS served as `/custom.css` + `/custom.js` from Astro endpoints instead of inlined `<style>`/`<script>` | **Yes.** Files stay authoritative; CSP keeps `script-src 'self'` with no `unsafe-inline`. |
| **D7** | Single-admin enforced by `CHECK (id = 1)` in schema | **Yes.** Cheapest possible enforcement of §7. |

---

## 12. Implementation Decisions

Recorded per §40. Each of these was forced by something discovered while
building, and each was resolved without changing a core invariant.

### ID-1 — Pages are served at `/<slug>`, not `/pages/<slug>`

§6 originally listed `src/pages/pages/[slug].astro`, which would publish pages at
`/pages/about`. Pages are served at `/<slug>` so they read like an ordinary
personal blog.

Consequence: a page slug could shadow a framework route, so reserved slugs
(`admin`, `posts`, `tags`, `login`, `logout`) are **rejected on write** by Go and
**skipped on read** by the loader. Tested.

### ID-2 — No `system` live collection; custom code is read directly

§6 listed a `system` collection for `content/system/custom.css` and `custom.js`.
A collection adds nothing: there is no frontmatter to validate and no listing to
build, and routing content through the collection cache would risk serving stale
custom code. Astro reads the two files directly in `src/lib/system.ts` and serves
them from `/custom.css` and `/custom.js`.

### ID-3 — Pages carry a `date` they do not use

The Go writer serialises one shared `Frontmatter` struct, so a page file gets a
`date` field. Rather than special-case the writer, `pageSchema` accepts an
optional `date` and ignores it. Ordering for pages uses `navOrder`.

### ID-4 — Login revokes **all** sessions, not just the caller's

§12 says "rotate on login". Revoking only the caller's own session left earlier
tokens valid, which weakens the rotation guarantee. Login now revokes every
session first. The trade-off is that signing in from a second device signs the
first one out — the right default for a single-administrator blog.

### ID-5 — `getLiveEntry` not-found is distinguished by error name

Astro reports a missing live entry as an error rather than `undefined`, so
`if (error) throw` turned every 404 into a 500. `src/lib/queries.ts` treats
`LiveEntryNotFoundError` as "absent". The check is on `error.name`, which is
exactly what Astro's own `LiveEntryNotFoundError.is()` compares, so no second
internal import is needed (§8).

### ID-6 — Raw HTML in Markdown is escaped, not merely discouraged

The processor keeps raw HTML as hast `raw` nodes and re-emits them verbatim, which
lets a post body smuggle an inline `<script>` past §17's "external script only"
rule. A hast plugin converts those nodes to `text` nodes. The value is passed
through **unchanged** so the serialiser escapes it exactly once; escaping it in
the plugin as well would double-escape to `&amp;lt;`.

Consequence: post bodies are prose and structure only. Custom CSS and JS are the
supported way to add behaviour.

### ID-7 — A method mismatch returns 404, not 405

`net/http`'s ServeMux returns `text/plain` 405, which would violate §1. Method
patterns fall through to a JSON catch-all that returns 404. This also avoids
revealing which admin endpoints exist and which methods they accept.

### ID-8 — Comments are gated by settings, read per request

`commentsEnabled` and `commentAutoModerate` are read from the `setting` table on
every submission rather than cached at startup, so an admin toggle takes effect
without a restart.

### ID-9 — Rate-limit counters use `INSERT … ON CONFLICT … RETURNING`

Reading the counter in a second statement let a burst of concurrent submissions
all observe the final total and reject requests that were legitimately within the
limit. The increment and the read are now one atomic statement.

### ID-10 — The database backup uses `VACUUM INTO`

§28 requires a consistent copy of a running WAL database. `VACUUM INTO` is
SQLite's online-backup primitive and produces a self-contained file. The fallback
(checkpoint, then copy) **fails loudly** rather than silently producing a backup
that is missing committed rows. Tested by committing a row that exists only in
the WAL and asserting it survives the round trip.

### ID-11 — Astro is the single origin; there is no reverse proxy

**This supersedes §5 and §18 of the original proposal, both of which assumed a
reverse proxy (Caddy) in front of the application.**

The deployment is two processes on one host:

```
browser → Astro (:4321)  ──proxy──▶  Go backend (127.0.0.1:8080)
             │
             └──serves──▶ /media/* from MEDIA_ROOT
```

Astro is the only listening process and now owns everything the proxy used to:

| Concern | Previously (Caddy) | Now |
|---|---|---|
| `/api/*` → Go | `reverse_proxy` | `src/pages/api/[...path].ts` |
| `/media/*` | `file_server` | `src/pages/media/[...path].ts` |
| CSP, nosniff, frame options, referrer, permissions | Caddy headers | `src/middleware.ts` |
| TLS, HSTS | Caddy + ACME | outside this application |

Consequences, all verified by tests:

- **One fewer component** and one fewer network hop.
- **Go binds to loopback only.** It is not reachable from outside the host, which
  removes an exposed port rather than adding a proxy in front of it.
- **One cookie scope**, because there is one origin.
- **CSP still has a single authority** (`src/middleware.ts`), so §18's intent
  holds even though the owner changed. `make arch` enforces it: a CSP header
  anywhere else in `astro/src` fails the check.

The only thing lost is serving media without passing through Node. For a personal
blog that cost is negligible; the files are streamed, ETagged and cached
immutably.

**Request bodies through the proxy are buffered, not streamed.** Node's `fetch`
requires `duplex: 'half'` to stream a request body, and omitting it fails every
POST and upload. Go already caps bodies (`MAX_JSON_BODY`, `MAX_UPLOAD_BYTES`), and
the proxy re-checks against its own ceiling before forwarding.

### ID-12 — The shipped example content obeys its own rules

`content/posts/hello.md` and `a-draft.md` originally declared slugs that
disagreed with their filenames, so a fresh install skipped them and showed an
empty blog. They were renamed to `hello-world.md` and `a-draft-post.md`.

Worth recording because it is exactly the failure mode §9's loader-isolation rule
makes *silent*: nothing errors, the file is simply not published.

### ID-13 — The theme is swappable, and custom.css actually wins

This was found by asking the awkward question: *if I wanted to replace the theme,
what would break?* Three things did.

**The custom-code editor was inert for equal-specificity rules.** Astro inlines the
bundled stylesheets, and the inlined `<style>` landed **after** the
`<link href="/custom.css">`. Both selectors had one class, so the theme won and the
admin's CSS silently did nothing — while §16 advertises exactly that feature.

Fix: both theme stylesheets are wrapped in `@layer theme`. Unlayered CSS beats
layered CSS regardless of source order, so `custom.css` always wins and its
position in `<head>` stops mattering.

**Eleven inline `style="…"` attributes** were spread across components. A theme
cannot override those without `!important`, and `!important` in a theme is the
thing that makes themes unmaintainable. All of them are now classes.

**There was no theme hook at all** — only `prefers-color-scheme`, so nothing could
select a theme and nothing could react to a change. `<html data-theme="auto">` is
now the interface, applied before first paint by `public/theme.js` (external, so
CSP still needs no `unsafe-inline`) and cycled by a keyboard-accessible toggle.

The theme contract, for anyone writing a replacement:

> **All theme CSS lives in `src/styles/*.css`, inside `@layer theme`.** There is
> no inline `style="…"` attribute and no component `<style>` block anywhere in
> `astro/src`. Define custom properties on `:root`; use a class plus a rule in a
> stylesheet for anything structural.

| Token | Purpose |
|---|---|
| `--bg` `--fg` `--muted` `--border` | surfaces and text |
| `--accent` `--accent-fg` | interactive colour |
| `--code-bg` `--danger` `--ok` `--warn` | code and status |
| `--radius` `--measure` | shape and line length |
| `--font-sans` `--font-mono` | typography |

**Three component `<style>` blocks were the same defect through a different
door.** Astro compiles a scoped block into an *unlayered* stylesheet whose
selectors carry a `[data-astro-cid-…]` attribute. Those rules therefore had
*higher* specificity than anything in `custom.css` and sat entirely outside the
theme layer: an admin override of `.post-item h2` or `.comment .body` would
have lost. All component CSS now lives in `src/styles/*.css`, which also removed
every `data-astro-cid` attribute from the output and pushed the theme past
Astro's inline threshold, so it ships as a real `.css` file.

Colours were already only ever used through these tokens — zero literals in
components — so the hard part of a theme existed. What was missing was a way to
*override* it, and that is now fixed.

### ID-15 — Rate limiting must identify the real client

Single-origin deployment has a consequence that is easy to miss. Go deliberately
never read `X-Forwarded-For` — trusting a client-controlled header for rate
limiting would let anyone choose their own bucket. But Astro proxies *every*
request, so `RemoteAddr` is always `127.0.0.1`. Correct-looking code produced a
single global bucket: measured, the sixth login attempt was refused and every
client shared the count. Five junk requests from anyone locked the admin out of
their own blog, repeatable every minute. One spammer could also silence every
commenter.

The fix keeps both properties that mattered. Astro reads `clientAddress` from the
TCP socket — the real peer, not a header the caller wrote — and forwards it as
`X-Client-IP`. The proxy drops any client-supplied copy of that header before
setting its own, so a caller still cannot choose its bucket. Go honours the header
only when the request arrived over loopback, which is the only way anything can
reach it at all. After the change, two client IPs produce two buckets; eight
requests carrying eight different forged addresses still produce one.

`make arch` enforces all three halves, including that the header name matches on
both sides, because a typo there disables the feature silently.

### ID-14 — The public site must not depend on the backend to render

Wiring `siteTitle` to the API (ID-13) introduced the public site's only runtime
dependency on Go, and the naive version of it was an amplification bug. Every
render fetched `/api/v1/site` with a 3s timeout. Measured against a backend that
accepted the connection and never answered — Go alive, SQLite wedged behind
`SetMaxOpenConns(1)`:

| | before | after |
|---|---|---|
| one anonymous request | 3007 ms | 607 ms |
| 20 concurrent requests | 3080 ms | 627 ms |
| 50 concurrent requests -> backend connections | 50 | 1 |

Content lives in files precisely so a database problem cannot take the site down.
A per-request fetch with a long timeout gave that property away for a page title:
N anonymous requests became N concurrent backend calls, aimed at a backend that
was already the bottleneck. A connection *refused* is instant, so the 3s stall only
appears when the backend is slow rather than absent, which is the harder failure
to notice.

`astro/src/lib/site.ts` fixes it four ways: a 30s TTL, coalescing of concurrent
callers into a single in-flight request, a 5s circuit breaker so a struggling
backend is not hammered, and a 600 ms timeout. The last known good value survives
an outage, so a title changed just before a failure is not silently reverted to
the default. `make arch` verifies all five properties, and that the consumer goes
through the helper — `Base.astro` and the feed had duplicated the fetch,
which is how the two drifted apart in the first place.

Also fixed while auditing: the **`siteTitle` setting was dead**. It was stored in
SQLite and editable in the admin UI, but `Base.astro` and the feed both
hardcoded `'Blog'`, so changing it had no visible effect. A minimal public
`GET /api/v1/site` exposes the non-sensitive presentation settings, and both
consumers read it with a fallback so a backend outage cannot take a page down.

`make arch` now enforces all of this, and each rule was verified by introducing
the violation and watching it fail:

- every stylesheet under `astro/src` is inside `@layer theme`
- no `.astro` file has an inline `style="…"`, and none has a `<style>` block
- no class used in markup is undefined by every stylesheet
- no script under `astro/src` **or `astro/public`** writes an inline style
- the rendered page carries no `style` attribute and no `data-astro-cid`
- every file in `layouts/` sets `data-theme`, loads `theme.js` and links
  `/custom.css` (admin once did only the third)
- `data-theme` exists, the theme script is external, and no component contains a
  colour literal
- both the header and the feed read the site title from the API

### ID-16 — One Theme Contract covers the public site and the admin

The theme system is not a scaffold and it is not half-built. A theme is a
directory containing a manifest, its own static assets and **two** component
trees:

```
astro/src/themes/<id>/
  theme.ts                the manifest, and the only file the registry imports
  assets/ img/ cursors/ fonts/   the theme's own static files, served at
                                  /themes/<id>/… — never astro/public/
  public/  layouts/ components/ views/ styles/
  admin/   layouts/ components/ views/ styles/
```

`astro/src/theme-system/contract.ts` is the entire interface. A theme receives view
models — `PostView`, `PageView`, `ListingView`, `CommentsView`,
`AdminShellView`, `PostEditorView`, and so on — and returns markup. It never
receives a database row, a loader entry, a session, an API client, a file path or
a CSRF token of its own making, and it never performs a request.

Three properties make the contract enforceable rather than aspirational, and each
is checked by `make arch`:

1. **Themes are compile-time registered code.** `theme-system/registry.ts` is a literal
   map and the only module permitted to import a theme. There is no dynamic
   `import()`, no glob, no directory scan and no HTTP import anywhere near it: a
   theme is not a path, and no value that crosses the network boundary is ever
   concatenated into a module specifier. The id that selects one comes from the
   `themeId` setting, which the backend validates against a fixed allowlist.
   `theme-system/ids.ts`, the registry keys and the backend's `knownThemeIDs` must be the
   same set or the build fails.
2. **One resolver, in the middleware.** `theme-system/resolve.ts` runs once per request in
   `src/middleware.ts` and assigns `Astro.locals.theme`. No page resolves its own
   theme; a second authority would eventually disagree with the first, and a page
   that resolved independently would be pinned to whatever it read.
3. **A theme may not own data.** Every file under `themes/` is walked for API
   calls, session handling, filesystem access, database access and theme
   resolution, and for shipped script files.

The theme must satisfy the whole slot list — 9 public and 13 admin — and `make
arch` compares each manifest against the exported `PUBLIC_SLOTS` / `ADMIN_SLOTS`
constants. A theme that registered cleanly but was missing a slot would otherwise
resolve to `undefined` at request time, and Astro renders a page with an
`undefined` component rather than refusing to start.

A stored `themeId` that no longer names an installed theme degrades to
`DEFAULT_THEME_ID` rather than throwing. The backend already refuses to *store* an
unknown id, so this is the path for a value that was stored before a theme was
uninstalled, a restored backup or a hand-edited row; a bad setting must not turn
every page into a 500. `make test-theme` §31.3 plants exactly that row with
`sqlite3` and asserts the site still renders.

One theme is registered: **`bluearchive`**, this project's own blog theme and
the default. The registry is deliberately static (ID-18), so registering a
second theme is a one-line, compile-time change — and it is the only way the
"the theme is swappable" claim becomes observable again, because the CSS
bundle boundary in ID-18 is a property of *two* stylesheets that must not
meet. `make test-theme` asserts that boundary only when a second theme is
registered, so the suite stays green with one theme and proves the property
the moment another theme arrives.

### ID-17 — JavaScript is core, and a theme may not replace it

A theme owns markup and CSS. The behaviour layer is not the theme's to touch.
`astro/public/*.js` ships at fixed URLs — `/color-scheme.js`, `/cms.js`,
`/comments.js`, `/custom.js` — identical for every theme, and
`astro/src/theme-system/js-contract.ts` is that list.

The asymmetry is deliberate and it is what makes the theme swappable without
endangering the CMS:

| | Owner | Changeable by swapping a theme? |
|---|---|---|
| markup, layout, CSS | the theme | yes |
| API calls, CSRF, sessions, redirects | the core | no |
| the script URLs themselves | the core | no |

Binding is through `data-cms-*` attributes, never CSS classes. A theme may rename,
restyle, move or wrap any control; as long as the contract attributes and the form
field names survive, the behaviour does too. Three `make arch` rules keep that
honest, and each is verified by introducing the violation:

- every attribute the contract declares must appear in the core scripts, so
  renaming one without updating `public/*.js` fails the build;
- no core script may select an element by class or id, so a theme cannot break
  behaviour by renaming a class;
- no core script may branch on which theme is active, so the behaviour layer
  never learns the theme's name.

A theme cannot drop a core script either, since the layout loads the fixed set —
otherwise a theme could remove the script its own markup needs, or add unreviewed
behaviour.

`content/system/custom.js` is unaffected by all of this: it remains
admin-authored, is still served as an external same-origin script, and is still not
a theme. The theme system introduced no `unsafe-inline` and no dynamic inline
script injection.

### ID-18 — A theme's stylesheet must be emitted as its own asset

This is the bug that hid inside ID-16, and it is worth recording because it looked
like a working feature.

Every layout originally did `import '../styles/public.css'`. That is the ordinary
way to import CSS in Astro, and the build succeeded. But the registry statically
imports **both** themes, so both stylesheets were reachable from the server entry
and Vite merged them into a single emitted file. Both themes' `:root` blocks were
then in one document; the later one won on source order; and the browser received
**byte-identical CSS** whichever theme was active. The `data-cms-theme` attribute
changed and the page did not — the default theme was not rendering as itself, and
`minimal`'s typography silently won on every theme.

The fix is `?url`, which makes each stylesheet its own emitted asset:

```astro
---
import themeCss from '../styles/public.css?url';
---
<link rel="stylesheet" href={themeCss} />
```

A page then links exactly one theme stylesheet, and the themes' `:root` blocks can
never meet. `make arch` fails on any plain `.css` import inside a theme, and
`make test-theme` asserts that neither theme's token values appear in the other's
page. Do not remove the `?url` as a no-op: it is the only thing that makes a theme
swap visible.

ID-13's layering rules apply to theme CSS unchanged — no inline `style="…"` and no
`<style>` block in any `.astro` file, in any theme, because a scoped block compiles
to an unlayered `[data-astro-cid-…]`-qualified stylesheet outside the theme layer.
Every theme declares all 14 design tokens in `:root` and in both dark variants, and
`make arch` compares the three token sets per theme.

### ID-19 — `bluearchive`: the project's own blog theme

The first *designed* theme, and the only one that is meant to be looked at. Its
visual reference is [`Alittfre/vitepress-theme-bluearchive`](https://github.com/Alittfre/vitepress-theme-bluearchive):
ice blue, white, very rounded shapes, a soft blue glow under every card, a ring in
the corner of the hero.

**Reference only, never a dependency.** Nothing from that repository is imported,
vendored or required at runtime, and no VitePress concept survives here: not its
config, its theme API, its Vue components, its routing or its content system.
There is no `vitepress` entry in `package.json` and there never will be. What was
taken is a *look* — a card with a coloured left edge, a pill-shaped tag, a compact
meta row — and the information architecture of a personal blog home page. The
implementation is plain Astro components and CSS.

| | |
|---|---|
| Light | an ice-blue page, white cards, one blue dark enough to pass contrast on white for body text and buttons |
| Accent | used for links, buttons and the active nav item only; the bright cyan is decorative (rings, the card's left bar) |
| Measure | `--measure: 65rem`, one column, no sidebar |
| Mobile | one column below 768px; the navigation collapses behind a disclosure button |
| Motion | a hover lift and a card shadow, both removed under `prefers-reduced-motion` |

Three decisions worth recording, because each was a way to get this wrong:

**The hero's copy is theme copy; the site name is a setting.** The `<h1>` and the
`og:title` come from the `siteTitle` setting. The sentence under it belongs to the
theme, so it lives in `HomeView.astro` rather than in a new `siteDescription` row in
SQLite. Adding a stored setting to hold one line of marketing copy would be a
second source of truth for something a theme owns.

**No pagination, no TOC, no image scaling.** V1 does not have them and a theme
cannot add them: a control with no route behind it is worse than no control, and
the post listing renders every post.

**The theme ships one script, and it opens a menu.** ID-17 says the behaviour
layer belongs to the core; that is about *acting*, not about *disclosing*. A
mobile navigation is presentation, so `SiteHeader.astro` contains a `<script>` that
opens and closes its own panel and nothing else — no request, no cookie, no token,
no inline style, no remote code, and no reference to a `data-cms-*` attribute. Five
`make arch` rules enforce each of those, because "it is only a menu" is exactly the
claim that grows a `fetch` six months later. A theme ships no standalone script
*file*, and the core script list is still fixed.

That script exposed a build-level trap worth naming. Astro inlines any hoisted
script chunk under `build.assetsInlineLimit` (4 KB by default) and emits it as
`<script type="module">…</script>`; with `script-src 'self'` and no
`'unsafe-inline'` the browser refuses exactly that. The result is a page that looks
correct, returns 200, passes every test, and whose only JavaScript does nothing —
with one CSP violation in a console nobody reads. `astro.config.mjs` therefore sets
`vite.build.assetsInlineLimit: 0`, `make arch` asserts it, and `make test-theme`
§31.9 asserts that the rendered HTML contains no inline `<script>` at all.

### ID-20 — Two bugs the theme tests found, both in the core

Neither has anything to do with themes, which is exactly why they were worth
writing the tests for.

**Admin pages were calling the API as anonymous visitors.** `lib/admin-api.ts` used
`credentials: 'same-origin'` and called it a day. That is the browser's mechanism,
and there is no cookie jar on the server: Node's `fetch` does not store or replay
cookies, so every server-rendered admin call arrived at Go with no session and came
back 401. The admin *shell* rendered — the middleware had already resolved the
session from the browser's cookie, so every screen returned 200 and looked like a
working CMS — and the screens themselves showed an empty state with an
"authentication required" notice. Nothing in the test suite asserted that the
lists contained anything, so this was invisible.

Fix: the middleware reads the incoming `Cookie` header once into `locals.cookie`,
and every server-side admin call passes it, exactly as `resolveSession` already
did. `adminFetch` no longer claims a cookie mechanism it does not have.

**A post with no tags made the admin post list 404.** `Normalize` leaves `Tags`
nil when a file declares none, a nil slice marshals to `null`, and the admin list
maps every summary into a view model that called `.map` on that field. One post
written without a tag took the whole screen down — as a 404, which is the least
diagnosable version of that failure. Fixed on both sides, because either alone
leaves the trap armed: Go emits `[]` for a repeated field
(`TestListMarshlesTagsAsAnArrayEvenWhenThereAreNone` asserts it in the JSON, since
that is where the bug lived), and `tagRefs` tolerates a nullish list. A view model
is a boundary, and a boundary that can be crashed by a field's absence is not one.

### ID-21 — Syntax highlighting may not use inline styles

The Markdown processor highlights fenced code by default, with Prism and the
`github-dark` theme, and it expresses highlighting as inline style attributes:

```html
<pre class="astro-code github-dark" style="background-color:#24292e;color:#e1e4e8; overflow-x: auto;">
  <span class="line"><span style="color:#F97583">:=</span></span>
```

Found by loading a page in a browser, not by a test. Three of this project's own
rules reject that output:

- `style-src 'self'` has no `'unsafe-inline'` (§10), so **every one of those
  attributes is refused by the browser**. A single code fence produced 21 CSP
  violations and rendered as uncoloured text.
- No markup rule allows an inline `style="…"`, because neither a theme nor
  `custom.css` can override one (ID-13).
- The palette is `github-dark`, so even with `'unsafe-inline'` it would paint a
  `#24292e` block onto a white page and ignore the colour scheme entirely.

`lib/markdown.ts` therefore passes `syntaxHighlight: false` and each theme styles
code blocks itself (`.prose pre`). Highlighting that cannot be themed, cannot
survive the CSP, and is wrong in one of the two colour schemes is not a feature.

The lesson is about *where* the rule lived. The architecture already said "no
inline styles", and `make arch` proved it — over `astro/src` and `astro/public`.
Nothing proved it about **rendered output**, because the styles were not written by
this repository: they came out of the Markdown pipeline at request time. A rule
enforced only over source cannot see a violation the framework introduces. Both
suites now assert on served HTML: a post with a code fence must contain no `style`
attribute at all (`make test-integration` and `make test-theme` §31.1).

### ID-25 — No launcher script

A start script is one more artefact that can disagree with the architecture, and this
project had two disagreements at once: it defaulted `PUBLIC_ORIGIN` to a loopback
address regardless of the port the site was actually served on, and it built one Go
binary while executing another — so a backend change silently did nothing until you
remembered the binary was three days old. The deployment is two processes:

```
PORT=9901 go run ./backend/cmd/server                 # JSON API, loopback only
PORT=9900 HOST=0.0.0.0 node astro/dist/server/entry.mjs
```

There is no script to fall out of date, and nothing in it decides a security setting.
A container or a supervisor wraps those two commands from outside.

### ID-24 — The Origin check compares the authority the request came from

`request origin is not allowed`, on every address the site was served on, with the
owner locked out. Three fixes were attempted before the real one, and each was wrong
in a way that is worth recording:

| Attempt | Why it could not work |
|---|---|
| Pin `PUBLIC_ORIGIN` to the LAN address | The browser's address is a property of the network path, not of the server. Behind NAT, a container port-forward or a reverse proxy it is not an address the server has. |
| Derive the allowlist from the machine's interfaces | Same objection, discovered late: `hostname -I` on the container reported `10.89.5.2` while the browser was at `10.20.10.150`, so no interface-derived set could ever contain the address in use. |
| Compare `Origin` against the `Host` header | Go is behind the single origin. Astro proxies every `/api/*` call over loopback, so `Host` was `127.0.0.1:9901`: the check compared the visitor's origin against **Go's own address**, no browser was ever same-origin, and only an explicitly listed address could post anything. |

The log line that settled it, from one refused request:

```
origin_not_allowed  origin=http://10.20.10.150:9900  host=127.0.0.1:9901
```

Two lines in one message: the visitor's origin, and the address the request actually
arrived on. They can never be equal in a proxied deployment, which is why *no*
browser-based login worked and why adding an address to a list could not fix it —
the defect was not which addresses were allowed.

**The fix is two lines of plumbing and one comparison.**

- Astro states the authority the browser dialled, read off the socket, in
  `X-Forwarded-Host`. It already does exactly this for the client IP, under
  `X-Client-IP`, and it drops any caller-supplied copy of either header before
  setting its own.
- Go compares the `Origin` header against that authority, **honouring the header
  only when the peer is loopback** — the identical condition under which it already
  honours `X-Client-IP`. From outside the process nothing can reach Go at all.

So the rule is now one sentence: *a mutation is accepted when the browser says it is
on the address the request arrived on, or when the origin is explicitly configured.*
There is no list to maintain, no interface to enumerate, and no address to add. A LAN
IP, a public IP, `localhost`, a container port-forward and a domain behind TLS
termination all work without configuration; a hostname in front of a proxy is one
`PUBLIC_ORIGIN` entry.

**What this rule is, precisely.** An attacker's page cannot make a browser send an
`Origin` equal to this server's address — a browser sets `Origin` to the attacker's
own origin, and the authority stays ours, so the two differ and the request is
refused. The way to manufacture agreement is DNS rebinding: point a name at the
server so that the name *is* the origin the browser is on. This rule accepts that
case, and the exposure is: a rebound name can reach the two *unauthenticated*
mutation endpoints — `POST /auth/login` and the anonymous comment form. Every
authenticated mutation additionally requires the per-session `csrf_secret` header,
which a rebound name cannot read because it carries no session cookie for that host,
and `SameSite=Strict` means it is not sent. Narrowing that further — refusing a
Host-derived match unless the authority is an IP literal, at the cost of requiring one
`PUBLIC_ORIGIN` entry per hostname — is a policy decision, and ARCHITECTURE.md §0
leaves it to the operator.
### ID-26 — WebP is a delivery representation, never a storage replacement

A browser that accepts WebP must receive WebP. The problem is that "serve WebP" has an
obvious implementation and every obvious one is wrong.

**The wrong implementations, and what each one breaks.**

- *Write a `.webp` next to the `.jpg`.* `MEDIA_ROOT` then holds files nobody uploaded.
  A restore from backup quietly promotes a cache entry into the source of truth, and a
  directory listing shows derived bytes as if they were assets. The originals are also
  no longer the only copy, so "delete the upload" has to know about two files.
- *Rewrite the extension in Markdown on upload.* This CMS is file-first: `content/*.md`
  is the source of truth and an author edits it by hand. A tool that rewrites an
  author's file to save a few hundred kilobytes has made the CMS a second source of
  truth for content, which is the one thing the architecture forbids.
- *Serve a `.webp` URL and rewrite the HTML.* Now the URL depends on the reader's
  `Accept` header, which means a shared cache, an RSS reader, a `curl` and a crawler
  all have to agree about a URL before it can be written down.
- *Sniff the `User-Agent`.* The moment a browser is missing from a table it gets the
  wrong format, and `Accept` exists precisely to answer this question.

**The decision.** `MEDIA_ROOT` holds originals and nothing else. Every URL names the
**original**. The delivery layer decides which bytes to send for that URL, from the
`Accept` header, and returns:

```
GET /media/2026/10/abc.jpg   Accept: image/webp,image/*;q=0.8
  200  Content-Type: image/webp
       Vary: Accept
       ETag: "webp-q82-1f3c…"
       Cache-Control: public, max-age=604800, stale-while-revalidate=86400

GET /media/2026/10/abc.jpg   Accept: image/jpeg
  200  Content-Type: image/jpeg
       Vary: Accept
       ETag: "original-9ab2…"
```

Three properties of that response are load-bearing.

- **`Vary: Accept` is mandatory, not hygiene.** The body depends on the request, so a
  shared cache that stored the WebP and served it to a later request that could not
  read WebP would be a correctness bug. One header, and it is checked by `make arch`
  because the failure is invisible in development where every request comes from one
  browser.
- **The ETag names the *representation*.** `photo.jpg` has two bodies, so a strong ETag
  shared between them would let a client holding the JPEG be answered `304` when it
  asked for the WebP. The tag is `representation + identity`.
- **Not `immutable`.** The URL carries no content hash, because it is the
  Markdown-visible original path. An admin *can* replace `photo.jpg` in place, and a
  browser holding a year-long immutable entry would never notice.

**Where the work happens.** In Go, not in Astro. Go owns the storage, the recorded
checksum and the caches, and it already speaks HTTP. `astro/src/pages/media/[...path].ts`
forwards `Accept` and `If-None-Match` and streams the answer back, passing `Vary`,
`ETag` and `Cache-Control` through untouched. Re-deriving any of them in the frontend
would be a second implementation that could disagree with the one that produced the
bytes — and `Vary` is exactly the header an implementation forgets.

**The cache is derived, keyed by content.** A representation is stored under
`sha256(original checksum, "webp", quality)`. Replacing the file changes the checksum,
so the old entry is never read again and nothing has to notice that the file changed;
re-uploading identical bytes reuses the entry, so a new URL for the same image costs no
conversion. The quality is in the key, so changing `webp_quality` invalidates every
representation at once — the originals are never re-encoded. A cleared cache is a
miss, never a `404`: the next request regenerates from the original.

**The memory cache is bounded.** An unbounded map of decoded images is a memory leak
with a long fuse: every distinct request that missed the disk cache adds an entry and
nothing removes one, so ordinary traffic OOMs the process. It is an LRU with a byte
ceiling, an admin setting (`image_memory_cache_mb`, default 64), live counters for the
diagnostics screen, and a ceiling that is re-applied on save rather than on restart —
§84.

**A conversion failure serves the original.** A file can pass every upload check and
still fail to decode. When that happens the delivery layer logs the reason, counts it,
and serves the stored bytes with their own content type. A cache problem must not take
a page's pictures down.

**Not implemented, deliberately:** AVIF, HEIC, resizing, multi-size generation, a CDN
or object storage. Those are a different design with different trade-offs, and §3 of the
phase brief rules them out.

### ID-27 — The media usage index is derived, and it is never a source of truth

The media library has to answer "which posts use this image?", and the tempting answer
is a `used_by_post` field an admin maintains. That answer is wrong in a specific way:
it makes the reference list the thing that is trusted, and the trusted thing is the
thing that rots.

The decision is that **the references come from the content**. `media_usage` is a
derived index over `content/*.md`, rebuilt by rescanning it, and losing the whole table
costs one rebuild and nothing else. It stores a path, a slug, a kind and a count —
never a title, a body or any other content, which is what `make arch` checks.

Three consequences worth naming:

- **Nobody maintains it.** There is no field to fill in and no button to remember to
  press. `![alt](/media/x.jpg)` in a post, a `cover:` in frontmatter and a raw
  `<img src>` are all references, because an author writes all three.
- **It cannot be quietly stale.** The index is refreshed when the newest Markdown file
  is newer than the newest row — one stat pass and one indexed `MAX`, cheap enough to
  run on every library screen load. A derived table that silently disagrees with its
  source is worse than no table: the screen says an image is unused while a post
  visibly uses it, and the delete guard would then agree with the screen.
- **It gates deletion.** A referenced asset is refused by default (§54), because
  deleting it leaves a broken image inside a file the CMS does not own. `?force=1`
  overrides it. The site icon is a reference too, and one that is not visible in the
  index because it lives in settings rather than in content.

### ID-28 — The site icon is a media asset, and every site setting is one resolver

The site title was already a setting. Subtitle, description, icon, WebP quality,
memory cache size and the four RSS settings join it, and the rule is that **all of them
are site metadata, never theme metadata**: a theme reads them and defines none of them,
so swapping the theme cannot rename the site, change its icon or take the feed away.

**The icon is a media id, not a path.** It is validated on upload like any other image,
served through the same delivery layer, and named by `site_icon_media_id`. A path would
be invalidated by a storage move and would need its own sanitiser; a media id is
already covered by everything the library does. The version token on the URL comes from
the asset's own checksum, so a replaced icon busts a browser's favicon cache without
the storage path ever changing. With no icon configured, a core default is used — a
theme is not allowed to name one, because the site's identity belongs to the
whole site, not to any single theme.

**One resolver computes the head.** `astro/src/lib/seo.ts` decides the document title,
the description, the canonical URL, the favicon, the Open Graph tags and the RSS
discovery decision; a layout renders them. `Post | Blog` written in two layouts is two
chances to disagree, and the second is the browser tab of a page whose title is wrong.
The admin uses the same resolver, so its favicon and its tab title cannot drift from
the public site's.

**The hero text is a setting too.** The subtitle and the description are the same
values the head uses, so the page a visitor reads and the metadata a crawler reads
cannot disagree — and a site called something else cannot keep a sentence about someone
else's blog under its own name.

**Settings writes are partial updates.** Every field is a pointer, so an omitted field
means "leave it alone". This is not a convenience: a client written before
`webpQuality` existed would otherwise be told `webpQuality must be 1-100` because of a
field it never knew about, and the only way to satisfy the API would be to send a value
the caller did not intend. Worse, a boolean sent as absent would silently switch a
feature *off* — `commentsEnabled` defaulting to false is a plausible way to lose a
setting nobody was editing.

### ID-29 — The RSS feed is a core endpoint, generated per request by the backend

`/rss.xml` is served by the Go backend (`GET /api/v1/rss`). It is
not a build artifact, not a file, and not a theme's concern. Astro's
middleware rewrites the public URL onto the JSON-API proxy so the
address readers subscribe to never moves; that rewrite is the only
Astro-side code on the path.

The feed is **served but not shown**: the public pages carry no
discovery link in the head, no entry in the navigation and no
subscribe button in the hero. A reader who knows the address can
subscribe, and that is the whole audience — the pages do not
advertise the feed, so no reader is taught to ignore an
`alternate` the site itself never mentions.

Items come from `content/`, which this backend already owns as its
sole writer, so there is no posts table to read and nothing to go
stale: editing a Markdown file changes the feed on the next
request, with no rebuild. Drafts are excluded. Comments never
appear, because a feed is a post feed.

Whether the feed exists is a **setting**, and a disabled feed is a `404` rather
than an empty channel — otherwise the switch is invisible. The pages advertise
nothing either way: there is no discovery link to omit, because the pages never
linked the feed in the first place.

The channel title falls back to the site title and the description to the site
description and then the subtitle, so renaming the site renames the feed. Every link
and every image URL is **absolute**: a reader resolving `/media/x.jpg` against its own
origin finds nothing, and the failure is silent — the picture is simply missing from
every reader.

### ID-30 — Raw media fallback requires a process-independent file server

The delivery layer is preferred and the originals remain on disk without it, which is
what makes a fallback possible in principle. It is not implemented here, and pretending
otherwise would be the worst outcome.

The deployment is **Astro plus Go, two processes, one origin** (ID-11). If Astro is
down, nothing in this application is serving — including a fallback route inside it. A
fallback that lives in a theme (`if the CMS failed, use /media-direct/`) would couple
the presentation layer to infrastructure, and it would not survive the failure it exists
for.

So the contract is: **the deployment layer may expose `MEDIA_ROOT` directly**, with the
same path prefix and the same `Vary: Accept`-less original bytes, and browsers would
receive original JPEG/PNG. That requires a process-independent file server — a CDN, a
bucket, or a static mapping at the edge — which ARCHITECTURE.md ID-11 deliberately
leaves to the deployer.

This deployment has no such server, so the raw-media fallback is **not verified**
here. The original bytes *are* on disk and *are* restorable

### ID-31 — A format that cannot be represented faithfully is served unchanged

`image.Decode` returns the *first frame* of a GIF. An encoder that accepts a decoded
image therefore cannot tell a still GIF from an animated one, and it will flatten an
animation without any way to report that it did.

ARCHITECTURE.md §3 lists GIF animation transcoding as a non-goal. The delivery layer
honours that by declining the conversion rather than by refusing the upload: a GIF is
still a legitimate asset, it is simply stored and served as stored. A browser that
accepts WebP receives `image/gif`.

`Pipeline.WebP` returns `ErrNotWebP` for both GIF and WebP sources, and the delivery
layer treats that sentinel as "serve the original" rather than as a failure. Keeping
those two cases on one sentinel is deliberate — the caller needs one behaviour, and a
boolean on the record would invite a third meaning later.

A WebP source is declined for a different reason: re-encoding a lossy format to itself
loses quality and doubles the cache.

### ID-33 — Language belongs to the theme, so the core asks for the words

§19 makes `/cms.js` and `/comments.js` the same file for every theme and lets them bind only
through `data-cms-*`. Extending that to *language* would be a mistake in both directions:
hard-coding English in a shared script puts an English word in the middle of a Chinese
moderation queue, and hard-coding Chinese makes every theme Chinese whether it wants to
be or not.

A notice is presentation, so the theme supplies it. Each theme declares two bags:

- `notices` — the strings the shared scripts raise, keyed by `NOTICE_KEYS`
- `adminTitles` — admin screen names, keyed by *the English literal the core page passes
  to `adminSeo()`*

The layout serialises `notices` into one hidden `data-cms-notices` carrier, and the scripts
look a key up and **fall back to the key itself**. That fallback is the point: a
half-finished translation shows `commentHeld` on screen rather than an empty status line,
because an empty line reads as "nothing happened" and a visible token does not.

Two bags rather than one because they are different concerns produced by different code:
the toasts are raised by shared scripts, the screen names live in core pages. One bag per
concern is also what lets the sidebar and the browser tab share `adminTitles` — they are
the same words, and a translated admin cannot end up with a Chinese tab above an English
sidebar.

A `data-cms-notices` attribute rather than a `<script type="application/json">` island
because CSP is `script-src 'self'` with no `unsafe-inline`, so an inline JSON island would
need its own exception. An empty hidden div needs none.

### ID-34 — An API error message is in the API's language, and stays there

The core's own *fallback* strings are themed. A message the backend produced is not.

`httpx` and the API handlers write English, in a Go process that serves every theme at
once. There is no theme to key a translation on, so a Chinese admin still sees the
server's wording when the server supplied one — for example `this image is used by 2
references`, which is the most useful string in that response and the one a translator
would most want to keep.

Theme-mapping the API's `message` field would need the backend to emit a code the theme
can look up, and the codes that matter here are not distinguishable (`conflict` covers
both the in-use and the is-the-site-icon case). Inventing codes now to serve one
translation is the wrong trade.

So it is recorded rather than papered over: **everything the CMS owns is themed; anything
that arrives verbatim from the API is in the API's language.**

### ID-32 — The image cache is never invalidated by a delete

The image cache is content-addressed (`sha256(checksum, "webp", quality)`), so a
representation is reachable only by content, never by path. Deleting a media row
therefore makes its entry unreachable rather than wrong, and the delivery layer
resolves the database row *before* it consults the cache, so the asset is a 404 either
way.

Clearing the whole cache on delete was correct but expensive: the cache is shared by
every image, so removing one unused asset re-encoded the entire site for the next
visitor. Making the invalidation *targeted* would require a checksum→key index — a
persistent table that looks authoritative and is not (§27). So delete does not touch the
cache, and an admin who wants the disk reclaimed uses the explicit action in §83.

### ID-33 — Custom CSS and JS as many files, with the file still the only truth

`content/system/custom.css` and `custom.js` were always the single source of truth for
custom code (ID-2). They still are, and they are still editable at `/admin/custom-code`.
What this adds is that an admin can now
have *more than one* file of each, without the CMS growing into a bundler. (The
bluearchive theme's own identity code — the preloader, the click fireworks, the
article lightbox, the banner typewriter — originally lived in the shipped
`custom.css`/`custom.js` and has since moved into the theme itself, §44; the
legacy pair is now the admin's optional layer, and a fresh install ships neither.)

```
content/system/custom.css                  legacy, always first
content/system/custom.js                   legacy, always first
content/system/css/001-base.css            managed
content/system/css/010-layout.css          managed
content/system/parked/css/010-layout.css   managed, disabled
```

Four decisions, and each of them is load-bearing.

**The filename is the order.** `^[0-9]{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.(css|js)$` — a
three-digit prefix, then a kebab-case stem, then the type's only legal extension. The
prefix *is* the sort key, so a lexical sort is the intended order and there is no second
ordering to keep in step with the first (ID-35). A directory listing's `readdir` order is
never consulted: two identical requests on identical code must produce identical bytes.

The grammar is also the entire traversal defence. It admits no `/`, no `\`, no `.` outside
the extension, no `..`, no leading dot and no whitespace, so a name that matches cannot
escape its directory whatever else is wrong with it. Containment is still re-checked after
validation, because a grammar is a filter and defence in depth is cheap. And because the
extension is tied to the collection, a rename cannot turn a CSS asset into a JavaScript
one: `010-layout.js` is not a legal name for the `css` kind.

**`custom_asset` holds metadata only.** Seven columns — `filename`, `type`, `enabled`,
`size_bytes`, `checksum`, `created_at`, `updated_at` — and no body, not even an empty one
to fill in later. Every one of them is derivable from the filesystem, which is the point:
dropping the table costs one rescan of two directories, and a file dropped into `css/` by
hand is a first-class asset rather than an untracked one. The screen reconciles on every
read rather than at startup, because two directory listings and a handful of upserts on a
page only one signed-in admin ever loads is cheaper than a stale index that is the thing
that is wrong.

**Go writes, validates, orders and audits; Astro aggregates.** As everywhere else in this
service, the split follows who owns what. Go owns the write path, the grammar, the atomic
rename, the audit events and the metadata. Astro owns `/custom.css` and `/custom.js`, which
it assembles per request out of the legacy file plus every enabled managed file.

### ID-34 — `/custom.css` and `/custom.js` aggregate at request time

The two URLs are the *entire* contract. A theme links `/custom.css` and loads
`/custom.js`; it never learns a filename, never reads a directory and cannot gain or lose
an asset by being switched (ID-38). Nothing is generated, nothing is written, no bundler
is involved and no build step exists on the path — `astro build` is not part of saving
custom code, so a file edited on disk is live on the very next request.

**`Cache-Control: no-cache` and an ETag that is a digest of the bytes.** The legacy file
keeps its exact bytes when there are no managed files, so an install that never opens the
manager keeps serving a response byte-identical to `custom.css`. Once a managed file
exists, each part is separated by a comment naming it — which does two jobs: a human
reading devtools can see which file a rule came from, and a `//` line comment at the end of
one JavaScript file cannot swallow the next one. That second problem is why the separator
is not decoration: concatenating without it produces an aggregate that silently loses
every file after the first one that does not end in a newline. It was found by writing a
test for it, and it has no error and no symptom other than code that is not there.

**One unusable file is skipped, not fatal.** A name outside the grammar, a file that cannot
be read, one larger than the ceiling, a subdirectory, the `.tmp-*` file a crashed atomic
write leaves behind: each is logged as structured JSON and left out, and the rest of the
site keeps working. This is the same rule the content loader applies per file (§7), for
the same reason — one typo in one asset must not take a blog offline.

**A disabled asset is not an error state.** §57 asks the admin to be able to see a file
whose metadata exists and whose file does not; the answer there is a `missing` row and a
delete that *repairs* rather than 404s, because a screen that offers a control and then
refuses it is worse than one that never offered it.

### ID-35 — One order, derived from the filename

There is exactly one ordering rule: **filename ascending**, in both runtimes. The
aggregator sorts its directory listing; the admin list sorts the same way; the API
reports `order` derived from the numeric prefix so a screen can render it without parsing
the name. There is deliberately no `order` column in `custom_asset`, because two orderings
eventually disagree and the disagreement is invisible until a stylesheet stops winning a
cascade for no reason anyone can find.

A "move up" / "move down" affordance is therefore a rename, and a rename is one atomic
`os.Rename`. The alternative — a numeric column that has to be rewritten across every row
that sits between two positions — buys nothing that renaming does not already give, and
loses the property that the filename alone determines the order.

### ID-36 — One flat directory, and only two types

`css/*.css` and `js/*.js`. No subdirectories, no `@import` graph, no package manager, no
bundler, no minifier. A subdirectory cannot be reached through the API and is *reported*
rather than followed, so an admin who creates one finds out immediately instead of
discovering later that half their files never load.

The type set is closed to `css` and `js`. A manager that also accepted HTML, SVG, JSON or a
shell script would be a general file uploader wearing a customisation feature's name, and
every one of those types is a fresh XSS or RCE decision this phase does not need to make.
It is enforced three times: by the grammar, by a `CHECK` constraint on the column, and by
the fact that routes are registered per type rather than dispatched on a string.

No minification either. The source stays readable, debugging stays simple, and HTTP
caching already does the work. If performance ever needs an optimisation layer, that is a
separate decision with its own cache and its own invalidation story.

### ID-37 — `file://` is refused on the way in; every other URL is the CSP's job

A stylesheet is not executed, but `url(file:///etc/passwd)` is a filesystem reference the
author almost certainly did not mean to make, and the honest place to refuse it is when it
is saved — where the editor can say why — rather than as a silently missing image. Every
other resource URL, local or remote, is left to the `img-src` and `font-src` the CSP
already carries: adding a second, coarser filter in Go would be theatre that rejects valid
CSS while missing what the browser actually enforces.

JavaScript gets **no** content inspection at all. The server never parses, compiles or runs
it; a "smart" check would only ever reject valid code. Custom JS is admin-authored code
running in the admin's own browser, by design (§7), and the controls that matter are the
ones that are not about content: a session, an Origin check, a CSRF token, an audit event
and a size ceiling.

The CMS never widens CSP to accommodate a custom asset. If an admin writes `fetch`, that
is the admin's code and the admin's responsibility; the policy stays
`script-src 'self'; style-src 'self'` with no `unsafe-inline` and no `unsafe-eval`.

### ID-38 — A theme knows two URLs and nothing else

`/custom.css` and `/custom.js` are the contract. `make arch` and `make test-theme` both
walk every file in every theme and fail on a managed filename, on `content/system/css`, on
the aggregator's internal marker, or on an invented `/custom/css/…` URL.

A theme that named an asset would couple itself to one admin's directory layout, and the
day a second admin used a different set of filenames it would be silently wrong rather
than visibly broken. The core cannot enumerate the files either: the aggregator owns the
list, and everything else links the two stable URLs.

The final document shape is unchanged:

```
HTML
├── Theme CSS            (the theme's own asset)
├── /custom.css          → legacy custom.css, then the enabled managed CSS, in order
└── /custom.js           → legacy custom.js, then the enabled managed JS, in order
```

### ID-39 — Custom JS is an addition, never a replacement

`/custom.js` is still the only way admin JavaScript reaches a visitor, and it is still
loaded at a fixed URL that no theme chooses. A theme's own script is a separate, hashed,
build artifact (§45), so the two cannot shadow each other: the theme's behaviour is
presentation, and custom JS is the admin's extra code on top of it. Neither is aware of the
other, and a theme cannot drop or replace the core script set.

Public custom JavaScript does **not** run in the admin. That is the half of the custom-code
contract that has to stay narrow: an admin screen should keep working whatever a visitor
gets, and `/admin/custom-assets` in particular must not have the file it is editing execute
itself under the cursor. The admin's own stylesheet *is* linked, because §11a makes that
part of the layout contract rather than a feature — an admin override of the theme is
useful, and it is a style, not code.

Markdown, for the record: a post body cannot run any of it. The processor converts raw HTML
nodes to text so a post can smuggle neither a `<script>` nor an inline handler (§8), and
that is unchanged.

## 34. The Markdown presentation layer

`content/system/markdown/NNN-name.css` files are the presentation layer for
rendered Markdown content: the styles a post's HTML carries. They are the
custom-asset manager's arrangement (§33) with one dimension removed — every
template is CSS, so there is no type and the grammar admits only `.css`:

```
content/system/markdown/001-base.css          enabled
content/system/markdown/020-code.css          enabled
content/system/parked/markdown/020-code.css   disabled
```

The division of labour is the same as every other content system here:

- **The files are the truth.** Go writes, validates, orders, audits and
  holds metadata. The database stores metadata only — there is no body
  column, not even an empty one — and losing the table costs one rescan
  of one directory.
- **Astro aggregates at request time.** `/markdown.css` reads the
  directory on every request, so an edit is live with no rebuild, no
  publish step and no cache to invalidate.
- **The scope is `.markdown-body`.** The renderer wraps every rendered
  body in `<div class="markdown-body">`, and every template scopes its
  selectors to it. A template styles *content*, never the page around
  it, so a template keeps working through a theme swap and cannot leak
  into the admin.

The cascade is fixed and documented: **theme stylesheet → `/markdown.css` →
`/custom.css`**. The theme owns the page, the templates own the article,
and the admin's custom CSS is the final override. A theme never names a
template file — it links the one stable URL, exactly as it links
`/custom.css`.

A post body expresses structure with Markdown, never with raw HTML (which
the processor neutralises, §8/ID-6), so the components a template can
style are *directives* — micromark's grammar, parsed by the same
processor every post already runs through:

```
:::info  … :::     → <div class="callout callout-info">
:::warning … :::   → <div class="callout callout-warning">
:::danger  … :::   → <div class="callout callout-danger">
:::card    … :::   → <div class="card">
:::figure  … :::   → <figure class="figure">, the last paragraph
                     becoming <figcaption class="caption">
:kbd[Ctrl]         → <kbd class="kbd">
:badge[新]         → <span class="badge">
```

Each class is a stable contract between the content, the templates and
every theme: the classes name *structure*, never a theme's own classes.
A fenced code block carries `code-block` on its `<pre>`, so a template
can target the block (`.markdown-body pre.code-block`) without catching
an inline `<code>` (`.markdown-body :not(pre) > code`). An unknown
directive name renders as a plain element with no class, because the
processor's own default — dropping it — would let a typo silently eat
the paragraph inside it.

The decisions below are the ones the implementation relies on. The
properties inherited from the custom-asset system (§33, ID-34, ID-35,
ID-37, ID-38) are not repeated here.

### ID-40 — Markdown style templates are files, finally

The CSS lives in `content/system/markdown/`, not in the database and not
in a build artifact. A template is created, edited, renamed, parked and
deleted as a file, with an atomic write, and the filesystem is the only
place its body ever exists. This is the same rule as custom code (§33)
applied to the presentation layer: content and presentation are both
files, and SQLite is runtime state only.

### ID-41 — The metadata index is derived, and losing it costs one rescan

`markdown_asset` records `filename, enabled, size_bytes, checksum,
created_at, updated_at` — every column derivable from the filesystem
except `created_at`, which is the one value a file's mtime cannot
supply. The index is refreshed on every admin list read: the tree is
read first and wins on every field, and a row with no file is reported
as `missing` rather than deleted. The index can therefore never be the
thing that is stale, and a restore or a hand-dropped file is absorbed
by the next screen load.

### ID-42 — Responses name files, never paths

A public or admin response must not tell a caller where `CONTENT_ROOT`
is or how it is laid out. The API's identity for a template is its
filename, the aggregate's `included`/`skipped` lists name files, and the
audit log records `system/markdown/<filename>` — a path relative to the
content root, which is the one path form that appears in a log for
content files anywhere in this service.

### ID-43 — A delete removes the file first, then the row

A delete needs the file gone, not just the row. The file is removed
first: if it survives and the row does not, the aggregator keeps
serving CSS the admin believes they removed. With neither a file nor a
row there is nothing to delete, and that is a 404.

### ID-44 — Deleting a template whose file is already gone is the repair case

A row whose file has been removed by hand is the one state the screen
promises the admin it can clear. Answering 404 there would leave a row
they cannot remove, so the API answers the delete by dropping the row —
which cannot touch a file, because there is none.

### ID-45 — An anomaly is reported, never quietly destroyed

A file whose name is outside the grammar, a file the database has never
seen, a subdirectory, a duplicate name: these are reported to the admin
with a reason, not deleted and not repaired. Startup and reads record an
anomaly; they do not quietly destroy it, because the admin may have
created it on purpose and the filesystem is the authority.

### ID-46 — Orphaned metadata is the admin's decision to clear

A metadata row with no file is the only evidence that something was
here. It is listed as `missing` so the admin can decide — delete the
row through the same screen — rather than being silently dropped by a
rescan.

### ID-47 — Rendered Markdown is wrapped in `.markdown-body` by the renderer

The wrapper is emitted by `renderMarkdown()` itself, so a post page, a
page and the admin preview cannot disagree about the scope: whichever
view renders a body gets the same hook. A theme or a template that
styles `.markdown-body` selectors is therefore styling *content*, and
nothing else on the page — which is what makes the template layer
independent of the theme (ID-38) and unable to reach the admin UI.

### ID-48 — The preview endpoint is the real render path, gated like a mutation

`/api/v1/markdown/preview` runs the same `renderMarkdown()` a post page
runs, so the admin's sandbox shows what a reader sees. It carries the
two gates every mutating admin surface carries — a live session resolved
against Go (the only session holder) and the CSRF double-submit header —
because a preview is a rendering service, and a foreign site must not be
able to use it as one. Its output is the already-escaped HTML the
renderer produces, so a preview cannot smuggle markup past the CSP.
