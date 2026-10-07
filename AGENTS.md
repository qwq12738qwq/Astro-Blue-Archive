# AGENTS.md — permanent rules for coding agents

`ARCHITECTURE.md` is the architecture baseline. This file holds the rules that
must survive every change. If the two ever conflict, `ARCHITECTURE.md` wins and
this file must be updated.

---

## 1. The three laws

### Law 1 — Astro renders all HTML. Go returns JSON only.

The Go backend must never produce HTML, use `html/template`, concatenate markup,
set `text/html`, or contain UI markup. Any new UI goes in Astro.

Enforced mechanically:

```bash
make arch
```

which greps `backend/` (excluding `_test.go`, which legitimately asserts on the
absence of markup) for `text/html`, `html/template`, `<html`, `<body`, `<div`,
`<!DOCTYPE`, `<script`.

### Law 2 — `content/` is the only source of truth for articles and pages.

The database holds runtime state, identity, comments, media metadata, settings,
rate limits and audit events. It must never hold article content.

Enforced by `make arch` (schema inspection), by the fullstack suite (§2:
no article content may have reached the database) and by
`backend/internal/content`'s tests (the grammar that guards it).

### Law 3 — Publishing is a file write, never a build.

```
Admin (Astro) → Go JSON API → content/*.md → Astro live loader → SSR
```

Never `exec`, `npm`, `astro build` or `child_process` on the publish path. A
`make arch` check forbids `os/exec` in Go and `child_process` in `astro/src`.

---

## 2. Live collections, not build-time collections

`astro/src/live.config.ts` uses `defineLiveCollection()`. Pages read content with
`getLiveCollection()` / `getLiveEntry()`.

**Do not "simplify" this to `defineCollection()` + `getCollection()`.** Build-time
collections snapshot content during `astro build`, which breaks the requirement
that a Markdown edit appears with no rebuild. This was verified empirically; see
`ARCHITECTURE.md` §0/F1.

---

## 2b. Rendered output is a rule of its own

`make arch` proves "no inline styles" over `astro/src` and `astro/public`. That
is not enough, and ID-21 is why: the Markdown pipeline emitted `style="color:…"`
per code token, so a post with a code fence shipped 21 CSP violations that no
source-level check could see. Both test suites now assert that **served HTML**
carries no `style` attribute, and `lib/markdown.ts` passes
`syntaxHighlight: false` so the theme styles code blocks instead.

When a rule is about what a browser receives, assert it against what a browser
received.

---

## 3. The internal Markdown processor is quarantined

`@astrojs/markdown-satteri` is an **internal Astro package**, not part of Astro's
documented public content API.

> The Markdown processor is an internal implementation dependency pinned to a
> specific Astro version. On every Astro upgrade, re-verify
> `astro/src/lib/markdown.ts`.

Rules:

- Only `astro/src/lib/markdown.ts` may import it. A `make arch` check enforces this.
- Never widen its surface. Use `renderMarkdown()` / `parseFrontmatter()`.
- On any Astro upgrade run, in order:
  ```bash
  cd astro && npm install && npm run check && npm run build
  node tests/integration-tests.mjs
  ```
  All must pass. If the plugin API changed, the raw-HTML escaping plugin in
  `markdown.ts` is the most likely casualty — the integration tests assert that a
  `<script>` in a post body is neutralised, which is what catches a silent
  regression there.

---

## 4. Content rules

### Slug is the filename

`content/posts/hello-world.md` must declare `slug: hello-world`.

Go refuses to save a mismatch. The Astro loader skips a mismatch. Never relax
this: two authorities for one URL is a data-integrity bug.

### Slug grammar and limits

`^[a-z0-9]+(-[a-z0-9]+)*$`, ≤80 chars. Titles ≤200, descriptions ≤300, ≤20 tags,
tags `^[a-z0-9][a-z0-9-]{0,31}$`. Unknown frontmatter keys are **rejected**, not
dropped, so typos surface instead of silently losing data.

### Reserved slugs

`admin`, `posts`, `tags`, `login`, `logout` — a page with one of these slugs must
never shadow a framework route.

### One bad file must never break the site

Astro validates a live collection as one batch, so a single malformed file would
otherwise turn into an HTTP 500. `astro/src/loaders/fs.ts` parses, validates and
renders each file independently, logs a structured error, records the entry in
`getInvalidEntries()` for the admin UI, and skips it.

Do not replace this with a batch validate-then-throw approach. If you touch the
loader, `node tests/integration-tests.mjs` must still pass the §33 case
(`valid.md` + `valid2.md` + six broken files → index returns 200).

### Raw HTML in Markdown is escaped

A hast `raw` node is re-emitted verbatim by the processor, which would let a post
body smuggle an inline `<script>`. The plugin in `markdown.ts` converts those
nodes to `text` nodes so the serialiser escapes them once. Do not remove it, and
do not pre-escape the value as well (that double-escapes to `&amp;lt;`).

The only supported way to run admin JavaScript is `content/system/custom.js`,
served as a same-origin external script at `/custom.js`.

### No MDX

V1 is Markdown + YAML frontmatter only. No MDX parser, runtime compiler, component
execution or build workaround. Adding MDX is a new architecture decision and
requires amending `ARCHITECTURE.md` first.

---

## 5. `CONTENT_ROOT` and configuration

- `CONTENT_ROOT` from the environment is the only supported content root.
  `new URL('../content/', import.meta.url)` is **forbidden**: at runtime
  `import.meta.url` points inside `dist/server/chunks/`, which silently yields an
  empty site. A `make arch` check enforces this.
- Configuration errors are fatal, never degraded. A missing root, a root that is
  not a directory, or a missing `PUBLIC_ORIGIN` must stop the process.
- `/api/v1/readyz` checks SQLite plus all three roots and returns 503 on failure.
  `/api/v1/healthz` must **not** touch the database.

---

## 6. Trust boundaries

| Input                                          | Trust     | Rules                              |
| ---------------------------------------------- | --------- | ---------------------------------- |
| Post/page Markdown                             | admin     | never **writable** by anonymous users; served to them on purpose (§10/§16) |
| `custom.css`, `custom.js` (§33)                | admin     | optional — a fresh install ships neither; served to visitors when present |
| Comments, credentials, uploads, request params | untrusted | never becomes HTML, a path, or JS  |

- Comments are **plain text** (decision D4). No Markdown subset, no HTML, no
  `set:html`, no `innerHTML`. Render with Astro text interpolation only.
- Admin custom JS is trusted by design and can break the site, and it **runs for
  anonymous visitors**: `Base.astro` links `/custom.js` and `/custom.css`, and
  both endpoints are unauthenticated by design, because making the public site
  themable is the point of them. What anonymous users may never do is *write*
  them — `PUT /api/v1/admin/custom-code` requires a session.
  Do not "fix" this by adding auth to the two read endpoints.

---

## 7. Security rules that must not be relaxed

- **CSRF**: `SameSite=Strict` + a per-session `csrf_secret` double-submit header on
  every mutating request. Astro's `security.checkOrigin` stays `true` in every
  environment as defence in depth.
- **Passwords**: Argon2id only. Never plaintext, reversible encryption, a bare
  hash of the password, or a bespoke KDF.
- **Sessions**: only `SHA-256(token)` is stored. HttpOnly, `SameSite=Strict`,
  `Path=/`, expiry plus idle timeout, rotated on login, revoked on logout.
- **Timing**: unknown usernames must still perform a dummy password verification,
  and login responses must be identical for a bad user and a bad password.
- **Path handling**: never `filepath.Join(root, userInput)`. Validate → clean →
  resolve → confirm containment under the root → only then access.
- **Uploads**: MIME allowlist **and** magic-byte sniffing; the extension is never
  trusted; the server generates the path (`YYYY/MM/<id>.<ext>`); the client never
  supplies the final path. SVG/HTML/JS/XML/CSS are rejected.
- **SQL**: parameterised queries only; no string-concatenated SQL anywhere.
- **Rate limits** are never fully disabled. Config values below 1 are rejected at
  startup rather than silently honoured.
- **A rate-limit key must contain the real client IP.** Astro proxies everything,
  so `RemoteAddr` alone is always `127.0.0.1` and one global bucket lets five
  junk logins lock the admin out. Astro forwards the TCP peer as `X-Client-IP`,
  strips any client-supplied copy first, and Go trusts it only from loopback.
  Never read `X-Forwarded-For` for this purpose. `make arch` enforces all three.
- **CSP** is owned by `astro/src/middleware.ts` alone. No other module may emit a
  competing CSP header. `script-src 'self'` with no `unsafe-inline`.
- **Dev may relax only TLS-dependent controls** (`SECURE_COOKIES`, plain
  HTTP). It must not disable CSRF, Origin checks, authentication, rate
  limiting, input validation, path validation or XSS protection.

---

## 8. Deployment: Astro is the single origin

There is **no reverse proxy**. `Dockerfile`, `Caddyfile` and
`docker-compose.yml` are deliberately absent from this repository.

```
browser -> Astro (:4321)  --proxy-->  Go backend (127.0.0.1:8080)
             |
             +--serves--> /media/* from MEDIA_ROOT
```

Rules:

- Astro is the only listening process. It owns the security headers
  (`src/middleware.ts`) and the routing a proxy used to provide.
- **The Go backend binds to loopback only.** Do not expose it.
- `src/middleware.ts` is the single authority for CSP. A CSP header anywhere else
  in `astro/src` fails `make arch`.
- `make arch` also fails if a `Caddyfile` or `docker-compose.yml` reappears: two
  origins means two cookie scopes and two chances to disagree about headers.
- Astro never writes content. `make arch` checks for filesystem write primitives
  in `astro/src`.
- Two commands, no launcher script: `go run ./backend/cmd/server` for the API and
  `node astro/dist/server/entry.mjs` for the site (ARCHITECTURE.md ID-25).

**`astro build` replaces `astro/dist/`, so it invalidates a running Astro process.**
This has now broken a live instance twice, and it is worth stating because nothing
about it is visible from the code:

- `astro build` empties `astro/dist/client/_astro` and rewrites
  `astro/dist/server/chunks`. A server started **before** the build keeps its old
  module graph in memory and goes on emitting HTML that names hashed assets which no
  longer exist. Those requests come back as the SSR error page with a `text/html`
  content type, and the browser then refuses them as stylesheets
  (`MIME type ('text/html') is not a supported stylesheet MIME type`). The page
  renders with **no theme CSS at all** — unstyled HTML in one column — and the
  theme's own JavaScript never loads, so every client-side behaviour silently stops
  too.
- The order is therefore fixed: **build, then restart.** Never build while an old
  process is still serving, and never judge a page through a process you have not
  restarted since the last build.
- Symptom-to-cause: `GET /_astro/public.<hash>.css → 404 or 500` plus
  `Refused to apply style … text/html` means the server is older than the last
  build. A *missing* asset answers 404; a 500 means the server's own chunks were
  replaced as well, which is the same fault one stage worse.

## 9. Untrusted input is never markup

ARCHITECTURE.md D4: comment bodies are plain text and must be rendered with Astro
text interpolation. `make arch` fails on `set:html`, `innerHTML`,
`outerHTML`, `insertAdjacentHTML` and `dangerouslySet` anywhere in
`astro/src` and `astro/public`.

Admin scripts build DOM with `textContent`, never `innerHTML`. An API
response echoes untrusted values back, so inserting it as HTML would reintroduce
the bug even though the server escaped it.

## 10. CSP belongs to Astro alone

`src/middleware.ts` sets the Content-Security-Policy and every other security
header. No other module in `astro/src` may emit a CSP header, and no file may
contain `unsafe-inline`. `make arch` enforces both.

This is why custom CSS and JS are served as external resources at `/custom.css`
and `/custom.js` rather than inlined, and why every admin script lives in
`astro/public/` instead of an inline `<script>`. With no reverse proxy, an inline
script would have nowhere to hide.

The one exception is the inert-download branch of `src/pages/media/[...path].ts`,
which sets a narrower sandbox policy on a subresource response rather than a
document. `make arch` allowlists that file explicitly.

## 11. Before declaring any phase complete

```bash
make verify         # the whole gate
make verify-race    # everything, plus the Go race detector
```

Before every phase:

```bash
gofmt -l .
go vet ./...
go test ./...                # make test-go
go test -race ./...          # make verify-race

npm run check
npx tsc --noEmit
npm run lint          # prettier + make arch

node tests/integration-tests.mjs
node tests/fullstack-tests.mjs
node tests/theme-tests.mjs
node tests/color-scheme-tests.mjs
```

**`make lint` is not optional.** It was silently failing while
`public/admin.js` contained TypeScript that would have thrown a `SyntaxError`
in the browser. `make arch` now also parses every file in `astro/public/`.

There is no separate container gate: the application is two local processes and
every claim about it is verified by running it.

Do not skip a failing test to make a phase look finished.

### 11a. The theme must stay swappable

`ARCHITECTURE.md` ID-13. A theme defines custom properties on `:root` and
nothing else. Three invariants, all enforced by `make arch`:

- `src/styles/base.css` and `src/styles/admin.css` live inside
  `@layer theme`. `content/system/custom.css` must **not** be layered —
  unlayered CSS beats layered CSS regardless of source order, which is the only
  reason the custom-code editor works while Astro inlines the theme after it.
  Do not "simplify" this by moving the `/custom.css` link, and do not wrap
  `custom.css` in a layer.

  **`!important` is the exception, and it is measured, not remembered.** The layer
  order is *reversed* for important declarations, so an unlayered `!important` is the
  **weakest** important origin: it loses to a layered one, and no amount of specificity
  in `custom.css` recovers it. Probed on the live page, two declarations differing only
  in their layer:

  | `custom.css` writes it | veil duration | lid `animation-name` |
  |---|---|---|
  | unlayered | 0.35 s (theme wins) | `none` (theme wins) |
  | inside `@layer theme` | 0.7 s | `cms-preloader-lid-shut-top` |

  **The theme therefore uses no `!important` at all**, and that is now enforced rather
  than hoped for. It used to need six of them to keep its reduced-motion rules, and the
  only way to change any of them was to add more — which is the maintenance debt this
  rule exists to prevent. Two changes removed all six:

  - **Motion is a token.** `--motion-fast` (150 ms), `--motion` (200 ms) and
    `--motion-preloader` (700 ms) are declared in every variant block with the rest of
    the palette, so "stop animating the page" is two assignments in the reduced-motion
    block rather than a fight. `--motion-preloader` is deliberately *not* remapped
    there: this site treats the preloader as the feature, so both halves of the loop
    play at their own pace whatever the OS asks for.
  - **The catch-all is `:where(*)`**, which has zero specificity. It still reaches
    every element and pseudo-element — a browser default, a bare `<div>` transition, a
    future rule without a class — while losing to anything the theme states with a
    selector. That is what the `!important` was there to do, done with the cascade
    instead of against it.

  What is left is one idiom, repeated where a stylesheet has a `[hidden]` element: the
  HTML `hidden` attribute has to beat any `display` the theme sets, and no specificity
  does that without it. `make arch` allows exactly that shape and nothing else, so a
  seventh `!important` is a failure rather than a decision someone has to remember.
- **No inline `style="…"` in any `.astro` file.** A theme cannot override one
  without `!important`, and `!important` in a theme is what makes themes
  unmaintainable. Every presentational decision belongs in a stylesheet.
- **No `<style>` block in any `.astro` file.** This is the same defect reached
  another way: Astro compiles a scoped block into an *unlayered* stylesheet whose
  selectors carry a `[data-astro-cid-…]` attribute, so it outranks anything in
  `custom.css` and sits outside the theme layer. All component CSS lives in
  `src/styles/base.css` or `src/styles/admin.css`. If you need a rule scoped to
  one component, add a class and put the rule in the stylesheet — do not reopen a
  `<style>` block.
- No colour literal (`#hex`, `rgb()`, `hsl()`) in a component. Colours go
  through the tokens in §11b, otherwise a theme can only restyle half the page.

- **No script may write an inline style** (`el.style.x = …`,
  `setAttribute('style', …)`). A theme cannot override a value the browser
  already computed from a style attribute, whoever wrote it. `make arch` scans
  `astro/src` **and** `astro/public` — `public/*.js` is served verbatim to the
  browser and is exactly as capable of bypassing the layer.

The theme hook is `<html data-theme="auto|light|dark">`, applied before first
paint by `public/theme.js`. It must stay an **external** script: CSP is
`script-src 'self'` with no `unsafe-inline`, so an inline theme script has
nowhere to hide (§10).

**Every layout must honour the whole contract** — the `data-theme` attribute, the
`theme.js` script, and the `/custom.css` link. `Admin.astro` once linked only
`custom.css`, which meant an explicit light/dark choice was dropped the moment
you opened the admin. `make arch` walks `layouts/` rather than trusting that
whoever adds a layout remembered all three.

### 11b. Design tokens

`--bg --fg --muted --border --accent --accent-fg --code-bg --danger --ok
--warn --radius --measure --font-sans --font-mono`.

**Every variant must declare every token.** `make arch` compares the token sets of
`:root`, `:root:not([data-theme='light'])` and `:root[data-theme='dark']` and
fails on any mismatch, so a token added to one theme and forgotten in another is a
build failure rather than a surprise. Adding a token means defining it in all
three places; adding a *colour* value is separate, and may legitimately differ
per theme.

## 12. Environment notes

- Go 1.25 is the target (`backend/go.mod` says `go 1.25.0`). `modernc.org/sqlite`
  is pinned to `v1.53.0` and `golang.org/x/crypto` to `v0.43.0` because newer
  releases require Go ≥1.26.
- `modernc.org/sqlite` is pure Go, so `CGO_ENABLED=0` works and the binary is
  fully static.
- The network here resolves `goproxy.io` but not `proxy.golang.org`. If
  `go get` times out, set `GOPROXY=https://goproxy.io,direct`.
- `npm audit` reports 3 high-severity advisories from the transitive
  `http-cache-semantics`. No patched version exists upstream; `npm audit fix`
  proposes downgrading Astro to v2, which would break live collections.
- **Do not pre-emptively override the Go cache.** `GOCACHE`, `GOMODCACHE`, `GOPATH`
  and `GOTMPDIR` are already set in this environment (`/go-cache`, `/go-mod`,
  `/go-tmp`) and work. Exporting `GOMODCACHE=$HOME/.cache/go/mod GOCACHE=$HOME/.cache/go/build`
  unconditionally builds a *second* cache beside the real one — it does not redirect
  anything, it duplicates roughly a gigabyte and leaves the first behind. That mistake
  was made here and cost 1.6 GB before it was noticed.
- **If `go build` genuinely fails with `no space left on device`, check first, then use
  Go's own tooling:**
  ```bash
  df -h /                       # is it actually full?
  go clean -cache               # drop the build cache — this is the fix, usually
  go clean -modcache            # only if the module cache is the problem
  ```
  Re-point the cache at `$HOME` **only** if the existing location is unwritable or
  genuinely full, and say why in the commit or the session notes. An earlier version of
  this file prescribed the redirect unconditionally; that was wrong and has been
  corrected rather than deleted, because the symptom it describes is real.

---

## 13. Rate limiting is never disabled

`ratelimit.New` clamps a limit below 1 to 1 rather than treating it as unlimited,
and `config.Load` rejects such values at startup. `Allow` uses a single
`INSERT … ON CONFLICT … RETURNING` so the count each caller sees is its own;
reading the counter in a second statement lets a concurrent burst reject requests
that were legitimately within the limit.

## 14. Backups must be consistent

`scripts/backup.sh` backs up `content/`, `media/` **and** the database — all three
or it is not a backup. The database copy uses `VACUUM INTO` (SQLite's online
backup primitive). Never "simplify" this to `cp blog.db`: the database runs in WAL
mode, so a file copy can miss committed transactions that still live in the
`-wal` sidecar. The fallback path fails loudly instead of silently producing an
incomplete archive.

## 15. Verification gates

```bash
make verify          # every gate (see §11)
```

Do not claim something works that was not run.

---

## 16. Two audit-log rules

`content_event` is an append-only audit log and nothing else (§22). Event names are
singular: `post.created`, `post.updated`, `post.deleted`, `page.*`,
`custom_css.updated`, `custom_js.updated`. The content directories are plural, so
the noun is mapped explicitly in `api.AuditContent` rather than derived from the
directory name.

It records only `kind`, `ref`, `actor`, `created_at`. Never a title, body or
frontmatter — `backend/internal/api/audit_test.go` asserts this, and it is
the only place that can: a browser never reads the audit table.

---

## 17. SQLite has one connection

`store.Open` sets `SetMaxOpenConns(1)`, which is what makes WAL contention
trivial for a single-admin blog (§21). The consequence is that **no caller may hold
two open result sets at once**: the second query waits forever for the connection
the first still holds. Read all columns from one `Rows`, or close it first.

---

## 18. The Theme Contract is the architecture

`astro/src/theme-system/contract.ts` is the whole interface between the core and a theme,
and it covers **both** the public site and the admin. This is settled: the theme
system is not a scaffold and must not be "simplified" later.

- **A theme is compile-time registered code.** `theme-system/registry.ts` is a literal
  map and is the only module permitted to import a theme. Never build a module
  specifier from a value — no ``import(`/themes/${id}/...`)``, no directory scan,
  no glob, no HTTP import. The backend validates `themeId` against a fixed
  allowlist, and `make arch` fails unless `theme-system/ids.ts`, the registry keys and the
  backend's `knownThemeIDs` are the same set.
- **One resolver, in the middleware.** `theme-system/resolve.ts` runs in
  `src/middleware.ts` and assigns `Astro.locals.theme`. No page resolves its own
  theme; a second authority would eventually disagree with the first.
- **A theme receives view models, never capabilities.** No database row, loader
  entry, session, API client, CSRF token of its own making, or writable path. It
  renders markup and emits the `data-cms-*` contract; the core performs the
  request. `make arch` walks every file in every theme and fails on any API, session,
  filesystem, database or theme-resolution logic.
- **A theme ships no script file and no content.** Behaviour is core (§19); a
  theme may carry the one script its own markup needs (§19a). A Markdown file
  inside `themes/` would be a second source of truth, and so would a
  `posts.json`.
- **Adding a theme** means one directory, one `theme-system/ids.ts` entry, one registry
  key and one backend id. That is deliberately not dynamic.

### The one bug that hid here

Every layout originally did `import '../styles/public.css'`. Because the registry
statically imports *both* themes, Vite merged both stylesheets into one emitted
asset: both `:root` blocks landed in one document, the later one won on source
order, and the browser got byte-identical CSS whichever theme was active. The
theme attribute changed; the page did not. The *default* theme was not rendering
as itself.

Layouts must therefore import with `?url` and link their own asset:

```astro
---
import themeCss from '../styles/public.css?url';
---
<link rel="stylesheet" href={themeCss} />
```

`make arch` fails on any plain `.css` import inside a theme, and `make test-theme`
asserts that neither theme's tokens reach the other's page when two themes are
registered. Do not "simplify" the `?url` away — it looks like a no-op and it
un-swaps the theme.

The installed theme is `bluearchive` (the default: this project's own blog
theme, ID-19). The registry is static, so a second theme is a one-line
compile-time change — and registering one is the only way the swap boundary
becomes observable again.

---

## 19. JavaScript is core and is not replaceable

`astro/public/*.js` ships at fixed URLs — `/color-scheme.js`, `/cms.js`,
`/comments.js`, `/custom.js` — identical for every theme. `theme-system/js-contract.ts`
is the list, and both the layouts and the scripts are checked against it.

- Core scripts bind through `data-cms-*` attributes only. They may **not** select
  by CSS class or id, and may **not** branch on which theme is active; both are
  `make arch` failures. A theme may rename, restyle, move or wrap any control.
- A theme chooses no core scripts and cannot remove one. The layout loads the fixed
  set, so a theme cannot drop the script its own markup needs.
- `vite.build.assetsInlineLimit` is **0** in `astro.config.mjs`. Astro otherwise
  inlines a small hoisted script into the document as
  `<script type="module">`, which `script-src 'self'` refuses: a page that looks
  fine, returns 200, and whose JavaScript does nothing. `make arch` asserts the
  setting and `make test-theme` asserts the rendered HTML has no inline script.
- `/custom.js` remains an admin-authored layer, still served as an external
  same-origin script, and is still not a theme. The theme's own behaviour
  lives in the theme (§20); a fresh install ships no `custom.js` at all. The
  theme system introduced no `unsafe-inline` and no dynamic inline script
  injection.
- Admin scripts build DOM with `textContent`, including the comment `<template>`
  clone, so the theme keeps ownership of the markup without the core ever
  constructing it.

---

## 20. What a theme's own script may do

The core owns *acting*. A theme owns *presentation*, and opening its own menu is
presentation — so `bluearchive`'s `SiteHeader.astro` carries one `<script>` that
discloses the navigation on a phone and does nothing else.

`make arch` walks every theme script body and fails on any of:

| Forbidden | Why |
|---|---|
| `fetch(`, `XMLHttpRequest`, `sendBeacon` | a request is acting; the core owns the API |
| `/api/v1/`, `document.cookie`, `Authorization`, `X-CSRF` | credentials |
| `eval(`, `new Function(` | dynamic code |
| `innerHTML`, `outerHTML`, `insertAdjacentHTML` | markup injection (D4) |
| `.style.x =`, `setAttribute('style', …)` | an inline style no theme or `custom.css` can override |
| a remote `http(s)://` URL | §27: no third-party resources without an architecture change |
| any `data-cms-*` attribute | those belong to the core scripts |
| a literal theme id | behaviour must not branch on which theme is active |
| `is:inline` | CSP is `script-src 'self'`; an inline script does not run |

A theme also ships no standalone script file at all — the script lives in the
`.astro` file that owns the markup, and Astro emits it as its own hashed asset.

---

## 21. Theme CSS and custom CSS are different things

| | Theme CSS | Custom CSS |
|---|---|---|
| Owner | the deployer, in the repository | the admin, in `content/system/` |
| Layer | `@layer theme` | unlayered, so it always wins |
| Served from | `/_astro/<theme>.css` | `/custom.css` |

Both of the §11a rules still apply to theme CSS: no inline `style="…"` and no
`<style>` block in any `.astro` file, in any theme. A theme that reopened a
`<style>` block would compile to an unlayered, `[data-astro-cid-…]`-qualified
stylesheet sitting outside the theme layer, which is the ID-13 bug again.

Every theme must declare **all 14** design tokens in `:root` and in **both** dark
variants (`:root:not([data-theme='light'])` and `:root[data-theme='dark']`).
`make arch` compares the three token sets per theme, so a token added to one theme
and forgotten in another is a build failure rather than a surprise.

---

## 22. The admin is a theme, but its data is not

`bluearchive` styles the admin from the same tokens, radius, buttons and focus
ring as the public site, which is what makes a theme switch restyle both halves.
It does not share the public *layout*: a sidebar, a dense table and a form-first
editor are what make an admin usable.

Two rules came out of ID-20 and must not regress:

- **Server-side admin API calls must forward the cookie explicitly.** There is no
  cookie jar on the server; `credentials: 'same-origin'` does nothing in SSR. The
  middleware reads it once into `locals.cookie` and every admin page passes it to
  `adminFetch` / `getAdminComments`. Without it every admin screen renders 200
  with an empty list and an "authentication required" notice.
- **A theme's admin stylesheet declares its own tokens.** The admin layout links
  only the admin stylesheet (ID-18), so `admin/styles/admin.css` repeats all 14
  rather than inheriting them from `public.css`. `make arch` checks this per
  theme.

---

## 23. Astro is the single origin, so Go never sees the browser's address

ARCHITECTURE.md ID-11 made Astro the only listening process, and Go a loopback
backend behind `/api/*`. Two consequences that have each broken something:

- **Rate limiting** read `127.0.0.1` for every client, so everyone shared one
  bucket. Fixed by forwarding the real peer in `X-Client-IP`.
- **The CSRF `Origin` check** compared the visitor's origin against Go's own
  `Host`, so no browser was ever same-origin and every mutation from every address
  was refused with "request origin is not allowed". Fixed the same way:
  `X-Forwarded-Host`, honoured only from a loopback peer.

Rules:

- **A forwarded header is trusted only from loopback**, and a caller-supplied copy is
  dropped before the real one is set. Same rule for both headers; no exceptions.
- **Never compare a browser-facing value against something the proxy replaced.**
  If Go and Astro disagree about which address a request came from, every comparison
  built on it fails closed, and the symptom is a site that renders perfectly and
  accepts no input.
- **Never enumerate the server's interfaces to learn an address a browser might
  use.** It is not the same address behind NAT, and `hostname -I` inside a
  container reports the container, not the host the browser dialled.
---

## 24. WebP is a delivery representation, never a storage replacement

`MEDIA_ROOT` holds originals and nothing else. Every URL — in Markdown, in frontmatter,
in an `<img>`, in the admin, in RSS — names the **original**, and its extension never
changes.

```
GET /media/2026/10/abc.jpg   Accept: image/webp   → 200  image/webp   Vary: Accept
GET /media/2026/10/abc.jpg   Accept: image/jpeg   → 200  image/jpeg   Vary: Accept
```

The rules that keep it that way:

- **No `.webp` in a URL, ever.** WebP is chosen per request from `Accept`. Writing a
  `.webp` next to the original pollutes the source of truth; rewriting the extension in
  Markdown makes the CMS a second writer of content; serving a `.webp` URL makes the
  URL depend on the reader.
- **The conversion lives in Go, not in Astro.** Go owns the storage, the recorded
  checksum and the caches. `astro/src/pages/media/[...path].ts` forwards `Accept` and
  `If-None-Match` and streams the answer back, passing `Vary`, `ETag` and
  `Cache-Control` through untouched. Do not re-derive a header here: a second
  implementation is one more place for `Vary` to be forgotten.
- **`Vary: Accept` is mandatory** on every negotiated image response. It is checked by
  `make arch` because the failure is invisible in development, where every request
  comes from one browser.
- **The ETag names the representation**, not just the file. `photo.jpg` has two
  bodies, and a shared strong ETag would let a client holding the JPEG be answered
  `304` when it asked for the WebP.
- **Never `immutable`.** The URL carries no content hash, so an admin can replace the
  file in place and a browser holding a year would never notice.
- **Negotiate from `Accept`, never from `User-Agent`.** No `Chrome = WebP` table. `q=0`
  is an explicit refusal and is honoured, and an absent `Accept` means no preference, so
  it gets the original.
- **The cache is derived and content-addressed** — `sha256(checksum, "webp", quality)`.
  Replacing the file changes the key and the stale entry is never read; re-uploading
  identical bytes reuses it; changing the quality invalidates everything at once. A
  missing entry is a miss and regenerates, never a 404.
- **A conversion failure serves the original**, logs the reason and counts it. It never
  500s an image and never serves a truncated body.
- **A GIF is served as stored, and so is a WebP.** `image.Decode` on a GIF returns its
  *first frame*, so converting one hands every WebP-capable visitor a still image that
  used to move — no error, no warning, no failed request, and nothing for a test to
  assert on. GIF animation transcoding is a non-goal (§3), and the cheapest way to
  honour a non-goal is to decline the conversion rather than ship an encoder that
  quietly does the wrong thing. Both cases return `ErrNotWebP`, which the delivery layer
  reads as "serve the original" and *not* as a conversion failure — the distinction
  matters, because the second increments a counter an admin reads when judging the
  cache. `ErrNotWebP`'s message is deliberately not "already WebP" any more.
- **Deleting one media asset does not clear the image cache.** It used to, which made
  deleting a single unused image re-encode every other image on the site for the next
  visitor. It is also unnecessary: the cache is keyed by checksum, so the entry is not
  reachable by path, and the delivery layer resolves the database row *before* it looks
  at the cache — so a deleted asset is a 404 whether or not an entry survives. Making it
  targeted would need a checksum→key index, which is persistent state that looks
  authoritative and is not (§27). Explicit cache clearing stays an admin action (§83).
- **Decoded pixels are bounded separately from bytes.** A 100 KB crafted PNG can decode
  to 24 megapixels; `IMAGE_MAX_PIXELS` (default 20 MP) is checked from the header before
  anything is decoded.
- **Single-flight every conversion.** One cache key, one encode, however many requests
  arrive at once.

## 25. A media URL is resolved, never assembled

Nothing writes `"/media/" + filename`. `astro/src/lib/media.ts` is the only place a
media URL is built, and the backend's `media.PublicURL` is the Go side of it.

A theme that hardcodes the prefix has coupled itself to the storage layout, which is
the thing that makes a later move to object storage a one-file change instead of a
change to every theme. `make arch` fails on a media literal in `astro/src/themes/`.

`mediaSrc()` accepts both shapes a cover legitimately takes — a bare storage path and
a `/media/…` URL — because both exist in the wild and normalising an author's Markdown on
save is worse than tolerating two spellings of one reference. The Go validator accepts
both for the same reason, and reduces them through the same normaliser.

## 26. The media usage index is derived, and it is never a source of truth

`media_usage` is a rescan of `content/*.md`. It stores a path, a slug, a kind and a
count — never a title, a body or any other content.

- **Nobody maintains it.** No field to fill in, no button to remember to press.
- **It is rebuilt when content/ is newer than the newest row**, on the library screen
  load. A derived table that silently disagrees with its source is worse than no table:
  the screen would say an image is unused while a post visibly uses it, and the delete
  guard would agree with the screen.
- **Losing it costs one rebuild.** Do not add a column that makes it authoritative.
- **A referenced asset is not deletable** without `?force=1`. The site icon counts as a
  reference even though it lives in settings rather than in content.

## 27. Every site setting is CMS data, and one resolver computes the head

`siteTitle`, `siteSubtitle`, `siteDescription`, `siteIconMediaId`, `webpQuality`,
`imageMemoryCacheMB` and the four RSS settings are all `setting` rows. No theme defines
any of them.

- **The icon is a media id, not a path.** It is validated on upload, served through the
  same delivery layer, and cannot be deleted while in use. Its URL carries a checksum
  token so a replaced icon busts the browser's favicon cache. With none configured, a
  **core** default is used — a theme may not name one.
- **`lib/seo.ts` decides the head.** Title format, description fallback chain, canonical,
  favicon, Open Graph and the RSS discovery decision are computed once in the core and
  rendered by the layout. The admin uses the same resolver, so it cannot drift.
- **The hero's words are settings too.** The bluearchive home page reads `subtitle` and
  `description`; it does not carry Blue Archive copy about someone else's blog.
- **Settings writes are partial updates.** Every field is a pointer, so an omitted field
  means "leave it alone". An explicit `false` is applied; an omission is not an
  instruction to destroy a setting.
- **A change is live immediately.** The memory cache is resized on save, the feed
  reflects the new settings on the next request, and the favicon changes with a new
  token. Do not make any of them require a restart or a rebuild.

## 28. The RSS feed belongs to the Go backend

`/rss.xml` is served by the Go backend (`GET /api/v1/rss`), which
owns both halves of the feed: the settings that shape the channel
and the content the items come from. Astro's middleware rewrites
the public URL onto the JSON-API proxy; it produces no XML of its
own. The feed is served but not shown: the public pages carry no
discovery link, no navigation entry and no subscribe button — a
reader who knows the address can subscribe, and that is the whole
audience.

- **Items come from `content/`.** No posts table exists, so there is
  nothing to read the feed from and nothing to go stale. Editing a
  Markdown file changes the feed on the next request.
- **A disabled feed is a 404**, not an empty channel.
- **Every link and image is absolute.** A reader resolving
  `/media/x.jpg` against its own origin finds nothing, and the
  failure is silent.
- **Covers use `media:content`, not `<enclosure>`.** An RSS
  enclosure is supposed to carry a byte length and the core does
  not stat the file; `length="0"` is a lie a reader could act on,
  and an absent length is what real publishers send.
- **Bounded to 1–100 items.** A feed with a million items is a
  denial of service aimed at every reader.
- **Comments, drafts and admin settings never appear.**

## 29. Raw media fallback needs a process-independent file server

The delivery layer is preferred; the originals remain on disk without it. A fallback
route inside the application does not exist and must not be added: it would live in a
theme, and it would not survive the failure it exists for.

A deployment *may* expose `MEDIA_ROOT` directly for that purpose. This one does not,
so no raw-media fallback ships here: a fallback that cannot run is worse than none.

## 30. What the Go unit tests are for

`make test-go` and `make verify-race` exist because some properties of this phase are
unobservable through `fetch` without absurd cost:

- a bounded LRU evicting under a 1 MB ceiling, and dropping the *least recently used*
  entry rather than an arbitrary one;
- single-flight collapsing twenty concurrent conversions into one;
- a cache key that changes with content and with quality, and does not change with a
  filename;
- `Accept` parsing, including `q=0` as a refusal and an absent header as no preference;
- a pixel ceiling refusing a 100 KB file that decodes to 24 megapixels;
- a conversion failure falling back to the original instead of 500ing the image.

The same logic keeps the rest of the Go surface lean: a handler's *contract*
— the shape of its responses, the status for each refusal — is what
`tests/fullstack-tests.mjs` already proves end to end, so asserting it
again in Go is a second copy of the same claim, not a stronger one. The Go
tests that survive are the ones the HTTP suites cannot reach: the filename
grammar that refuses traversal before the filesystem is touched
(`internal/content`), the loopback bind (`internal/config`), the media
pipeline's bounds (`internal/media`), the delivery layer's refusals and its
fallback (`internal/api`), and the audit log's two promises
(`internal/api/audit_test.go`).

Where a helper is shared across packages it lives in `backend/internal/testutil`
(the temporary content tree and the config test environment). A helper that
reaches an unexported symbol stays with its package's tests: moving it out
would mean exporting production code only for the sake of a test.

The HTTP suites prove what a browser sees; these prove the bounds. Neither replaces the
other, and a phase is not done when only one passes.
---

## 31. Language belongs to the theme, not to the core scripts

`bluearchive` renders its UI in Chinese. That is deliberate: the words belong
to the theme (§18), and a future second theme may well choose another
language — which is the same property a second colour scheme would prove.
Do not un-translate `bluearchive` into English prose.

The rule this exists to protect: **`astro/public/*.js` contains no user-facing prose.**

- `/cms.js` and `/comments.js` are one file per URL, identical for every theme (§19). A
  literal English string in either of them appears in every theme, and a literal Chinese
  one does too. Neither is allowed.
- Every notice is a `t('key')` lookup. The words come from the theme's `notices` bag,
  serialised by the layout into a hidden `data-cms-notices` carrier.
- **A missing key falls back to the key itself, not to an empty string.** A blank status
  line reads as "nothing happened"; a visible `commentHeld` reads as an unfinished
  translation. Do not "improve" this with a default English string — that is the hardcoded
  prose this rule exists to remove.
- Admin screen names come from `adminTitles`, keyed by the English literal each core page
  passes to `adminSeo()`. The sidebar uses the same bag, because a screen name is the same
  word in both places.
- Adding a screen means adding the key to every theme. Each theme's bag is
  the reference for what a theme must supply.
- Tests assert the **bound action or the structural marker**, never the visible
  label. `data-cms-action="clear-image-cache"` holds for every theme;
  `"Clear image cache"` holds for one. An assertion on the English label passed
  for the wrong reason until a translation broke it — that is the whole lesson.
- Technical nouns stay in their original form inside Chinese text: WebP, RSS, Markdown,
  Open Graph, slug, Content Security Policy, Astro. Translating them produces text an
  admin has to translate back.

**What stays English on purpose:** an error `message` produced by the Go backend. One Go
process serves every theme, so there is no theme to key a translation on, and the codes
that matter are not distinguishable (ARCHITECTURE.md ID-34). The CMS's own fallbacks are
themed; a message that arrived from the API is shown as it arrived.

---

## 32. Three values, two screens, two buttons

`<html data-color-scheme>` is `auto | light | dark`. A screen is light or dark. So the
light/dark control **cannot be a three-value cycle** — and it was one, and that is the
whole bug:

```
auto → light → dark → auto
```

A reader arrives on `auto`, which renders whatever the OS renders. On a reader whose OS
was light the **first click produced `light`** — a state indistinguishable from the one
they were already looking at. The click did nothing visible, so they clicked again, and
the control looked broken: one dead click out of three, and only on the light side. In
dark mode it worked, which is why it read as "sometimes broken" rather than "broken".

The rule:

- **The toggle flips the scheme that is *rendered*, not the one that is *stored*.**
  `auto` resolves through `prefers-color-scheme` before anything is compared with it, so
  every click changes what is on screen in both directions.
- **`auto` has its own control** — `[data-color-scheme-auto]`, next to
  `[data-color-scheme-toggle]`. It is the one value a light/dark switch cannot express, so
  it needs a button of its own rather than a third stop on a cycle that would have to
  contain a dead click somewhere. `make arch` fails if a theme renders only one of the two.
- **The auto button's active state is styled from the document attribute**, not from
  `aria-pressed`, so it is right on the first paint and right without JavaScript.
  `aria-pressed` is still set, because a screen reader needs it.
- **The words are the theme's** (§31). The toggle is named for what the click will do —
  `colorScheme.toLight` / `colorScheme.toDark` — which is the only wording that cannot be
  wrong, since from `auto` the next click depends on the OS. The auto control keeps one
  stable name and carries its state in `aria-pressed`. A key the theme did not supply
  leaves the server-rendered label alone rather than overwriting it with a key name.
- **This file runs in the head, before its own buttons exist.** Its first `apply()`
  therefore sets the attribute and nothing else; the labels are synced on
  `DOMContentLoaded`. Do not "simplify" that by reading the carrier eagerly — an empty bag
  is never cached, or a cache primed before the parse pins every label to its key for the
  life of the page.

`make test-color-scheme` loads the real served `astro/public/color-scheme.js` into a stub
DOM and drives it: every stored value × every OS preference, asserting that a click
always changes the rendered scheme. It fails on the old code with exactly the reported
symptom. Do not "simplify" it into a source-text grep — `fetch` cannot observe a click,
which is why the suite exists (§2b).

---

## 33. The opening animation is a loop, so it needs a closing half

`bluearchive` opens every page with the 什亭之匣 preloader. It used to open and never
close, which is not a loop: leaving a page was a cut — the outgoing page vanished and the
incoming one was already shut at its first paint — and a reader who navigated a few times
saw the same opening animation start over with nothing ever closing.

| Half | Trigger | Owner |
|---|---|---|
| open (`.is-done`) | the arriving document's `load` event | `/custom.js`, the admin's layer |
| close (`.is-closing`) | a click on a link the page owns | `Preloader.astro`, the theme |

**A closing animation on a page being left has to hold the navigation up.** `pagehide`
cannot do it: the document stops painting microseconds later, so nothing is seen. That is
the one thing this theme script does that "open its own menu" does not, and the reason is
in the comment in `Preloader.astro`. It still performs the navigation the browser was going
to perform anyway — it only decides *when*.

The rules, all four of which exist because breaking one is invisible in a page render:

- **Only clicks the page owns may be held up.** Not a modified click, a named target, a
  download, another origin, or a bare fragment. A curtain over a page that was never going
  to be replaced is a page with no way back.
- **`location.assign` is reached from `animationend` *and* from a timer.** A backgrounded
  tab may never run the animation; without the timer the navigation would hang on a page
  that is otherwise fine.
- **A navigation that never commits must not leave the curtain shut.** `pagehide` cancels
  the undo timer; a request that is blocked never fires it, and the page reopens.
- **Back/forward is not an arrival.** The bfcache restores the document *with*
  `.is-closing` on it and never re-runs the script, so `pageshow` with `persisted` has to
  reopen. Adding `.is-done` is the whole fix, and it is not optional: the overlay's resting
  state is *shut*.

**Reduced motion keeps the closing effect and drops the slide**, and the first version of
this got it backwards. Zeroing the whole animation was "no curtain at all", which measured
as 2 frames and no covering at all — while the *dismissal* above keeps its 1s under the same
preference, so a reader in that mode got the opening and never the closing. A reader
reported exactly that. Reduced motion is about movement, not opacity: a full-viewport slide
is what can make someone ill and a cross-fade is not. So `.is-closing` keeps a duration of
its own (0.35s) and the panes get `animation: none`, which leaves them at their resting
position — *seated* — so what shows is the artwork fading up over the page. The navigation
still waits for it, which is the half of the rule above that is not about taste at all.
Measured, not assumed: 383ms and a fully covered page, against 2 frames and none before.

**This site then overrode that**, and the override is the one deliberate `@layer theme`
block in `custom.css` (§11a): the owner wants *both* halves every time — the lid coming
down and the backdrop fading up over it — so the admin restores the lid's `animation`
under reduced motion, at the same 0.7s so the script's `animationend` still lands after
the lid has seated. The theme's rules stay correct for a theme that has no admin override,
and the owner decides for their own site; deleting that block puts reduced motion back in
charge and needs no rebuild, because `/custom.css` is assembled per request and sent
`no-cache`.

**The two halves are one token, and that is the whole point.** The opening was 1s and the
closing 0.7s, and a reader who watched a 700 ms lid fall into a 1000 ms lid lift was told
by the timing alone that the page had been cut and pasted rather than carried. Both now
read `--motion-preloader`, so they cannot drift apart — two numbers can, a token cannot.
`make arch` fails if either half stops using the token, or if the script's `CLOSE_MS`
stops matching it.

`make arch` checks the three things that silently break this: the id the script reads is
the id the markup renders, the script's `CLOSE_MS` equals the CSS duration, and
`.is-closing` is declared after `.is-done`. Changing one duration without the other fails
the gate instead of producing a cut.

**`location.assign` also has to be reachable when the animation never runs at all** — an
`animationend` alone is a page that never navigates. The timer is derived from `CLOSE_MS`
rather than written beside it, so the two cannot drift into a cut.

**A closing animation has to be legible, and that is not the same as being fast.** The
first version was 0.45s of `cubic-bezier(0.4, 0, 0.2, 1)` and a reader reported the lid as
"falling too fast to feel falling". Three separate defects, each found by sampling computed
styles frame by frame rather than by watching it:

| | Measured | Why it read wrong |
|---|---|---|
| curve | 6% of the travel in the first 100 ms, 70% by 220 ms | an ease-in-out's first quarter is nearly motionless — that is a control that has not responded — and then it crosses at ~2750 px/s |
| duration | 0.45 s | half the time, twice the peak speed |
| fade | veil opacity tracked the movement exactly | a parent's opacity multiplies everything inside it, so at 161 ms the whole lid was 19% opaque; and the panes faded `0 → 1` alongside their own transform, so the artwork was half faded everywhere it went |

So: the curve is `cubic-bezier(0.3, 0.25, 0.7, 0.9)` at **0.7s** — chosen by *solving* the
timing function against a target profile, not by taste — the panes animate `transform` and
nothing else, and the veil reaches full opacity at 30% of the animation so the backdrop is
solid while the lid is still travelling. **A lid you cannot see is not a fall.** If a
future change makes this feel fast again, measure the timeline before changing the number:
`getComputedStyle(pane).transform` sampled per frame tells you exactly what the eye was
given.

`make arch` checks the three things that silently break this: the id the script reads is
the id the markup renders, the script's `CLOSE_MS` equals the CSS duration, and
`.is-closing` is declared after `.is-done`. Changing one duration without the other fails
the gate instead of producing a cut.

---

## 33. A custom asset is a file, and `/custom.css` is the only way to reach it

`content/system/custom.css` and `custom.js` are the admin's own layer. A
fresh install ships neither — the theme's identity lives in the theme
(§44) — and the first save at `/admin/custom-code` creates the file,
which is then edited there like any other. They stay authoritative, may
not be deleted through the asset manager, and `make arch` fails if they
leave either runtime's allowlist. A missing file is not a fault: the
endpoint answers 200 with an empty body, so the `/custom.css` and
`/custom.js` links every layout carries stay valid before any custom
code exists.

```
/custom.css  →  content/system/custom.css          legacy, always first
               content/system/css/001-base.css      managed, enabled
               content/system/css/010-layout.css    managed, enabled
/custom.js   →  content/system/custom.js           legacy, always first
               content/system/js/001-base.js        managed, enabled
```

Five rules, each of which exists because breaking it is invisible in a page render.

- **The filename is the order, and it is the only order.** `NNN-name.css` with a
  three-digit prefix, a kebab-case stem and the type's only legal extension. Sorting by
  filename *is* the load order. There is no `order` column in `custom_asset`, because two
  orderings eventually disagree and nobody can tell which one won. Moving an asset is a
  rename, and a rename is one atomic `os.Rename`.
- **"Enabled" is a location, not a flag.** A file in `css/` is enabled; the same file in
  `parked/css/` is disabled. That is what lets Astro aggregate the directory with a
  `readdir` — no database, no build, no second storage system — and it is why a save is
  live on the very next request with nothing to invalidate. The database records the flag
  as metadata, reconciled from the tree on every read. The tree is the truth.
- **The database holds metadata only.** `filename`, `type`, `enabled`, `size_bytes`,
  `checksum`, `created_at`, `updated_at`. No body, not even an empty `content` column to
  fill in later. `make arch` compares the column set against that exact list rather than
  a forbidden list, because a table like this is where a `content` column appears "just to
  avoid reading the file twice".
- **One bad file is skipped, not fatal.** A name outside the grammar, an unreadable file,
  one over the ceiling, a subdirectory, the `.tmp-*` file a crashed atomic write leaves
  behind: logged as structured JSON and left out. The aggregate and the rest of the site
  keep working.
- **Parts are separated by a comment naming the file.** Not decoration. A `//` line
  comment at the end of one JavaScript file, with no trailing newline, would otherwise
  swallow every file after it — an aggregate that silently loses code, with no error
  anywhere.

Things that are deliberately absent: subdirectories, `@import` graphs, package managers,
bundlers, minifiers, folders, a type other than `css` or `js`, and any per-file public
URL. `/custom.css` and `/custom.js` are the entire contract; a theme links them and never
learns a filename. `make arch` and `make test-theme` both fail a theme that names an asset
or reaches into `content/system/css`.

Custom JS runs on the public site and **not** in the admin — an admin screen must not have
the file it is editing execute itself. Custom CSS is linked in both, because §11a makes the
`/custom.css` link part of every layout's contract rather than a feature. Neither may
cause CSP to be widened: it stays `style-src 'self'` and `script-src 'self'` with no
`unsafe-inline` and no `unsafe-eval`, whatever an asset contains.

Go is the only writer. `make arch` asserts that every call to `CreateCustomAsset`,
`WriteCustomAsset`, `DeleteCustomAsset`, `SetCustomAssetEnabled` and `RenameCustomAsset`
lives in `internal/api`, that every route is behind `requireSession`, and that
`astro/src/lib/system.ts` — the one file that has to hold this whole feature — writes
nothing at all.

The editor's `<textarea>` carries the file's text and nothing else. Astro keeps the
whitespace that precedes a `<textarea>`'s `{expression}`, so a textarea written across
several lines renders the template's own indentation as the field's value, and every save
would prepend it to the file. That is the one place where "invisible formatting" is
stored, so the attributes live in one object, the element stays on a single line, and
`tests/fullstack-tests.mjs` compares the rendered value to the bytes on disk.

---

## 34. The Markdown presentation layer

`content/system/markdown/NNN-name.css` files are the presentation layer for
rendered Markdown content. The arrangement is the custom-asset manager's
(§33) with one dimension removed — every template is CSS, so the grammar
admits only `.css`:

```
/markdown.css  →  content/system/markdown/001-base.css   enabled
                      content/system/markdown/020-code.css   enabled
                      content/system/parked/markdown/020-code.css  disabled
```

Rules that must not be relaxed:

- **The cascade is theme → `/markdown.css` → `/custom.css`.** Every public
  layout links `/markdown.css` after the theme stylesheet and before
  `/custom.css` (the constant is `MARKDOWN_STYLESHEET` in
  `theme-system/js-contract.ts`). A theme never names a template file.
- **The scope is `.markdown-body`.** `renderMarkdown()` wraps every rendered
  body in `<div class="markdown-body">`, and every template scopes its
  selectors to it. A template styles content, never the page around it —
  that is what makes templates theme-independent and unable to reach the
  admin UI. The admin links `/markdown.css` only inside its preview
  sandbox, never globally.
- **The class contract is stable.** `callout callout-info|callout-warning|
  callout-danger`, `card`, `figure` + `caption`, `code-block` (on the
  `<pre>`), `kbd`, `badge`. They name structure, never a theme's classes.
  The directive grammar is micromark's: `:::name … :::` containers,
  `:kbd[Ctrl]` text directives. A colon between the name and the bracket
  (`:kbd:[Ctrl]`) is not the grammar and renders as literal text.
- **The filename grammar is `^[0-9]{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.css$`.**
  Same shape as the custom-asset grammar, closed to `.css`. A matching name
  cannot escape its directory; containment is re-checked regardless.
- **The order is the filename, and only the filename.** A three-digit prefix
  makes a lexical sort the intended order. There is no `order` column.
- **"Enabled" is a location.** A file in `markdown/` is enabled; the same
  file in `parked/markdown/` is disabled. The move is an atomic rename and
  never rewrites the body.
- **The database holds metadata only.** `markdown_asset` records `filename,
  enabled, size_bytes, checksum, created_at, updated_at` — no body column.
  The index is refreshed on every admin list read; the tree wins on every
  field, and a row with no file is reported as `missing`, never deleted.
- **One bad file is skipped, not fatal.** `/markdown.css` keeps answering
  200 with the templates that are usable; the skipped file is logged as
  structured JSON.
- **Go is the only writer.** Every `CreateMarkdownTemplate`,
  `WriteMarkdownTemplate`, `RenameMarkdownTemplate`,
  `SetMarkdownTemplateEnabled` and `DeleteMarkdownTemplate` call lives in
  `internal/api`, behind `requireSession`, with an audit event
  (`markdown_template.created|updated|enabled|disabled|deleted`) per
  decision. `astro/src/lib/markdown-css.ts` — the aggregator — writes
  nothing at all.
- **The preview endpoint is the real render path.** `/api/v1/markdown/preview`
  runs the same `renderMarkdown()` a post page runs, gated by a live session
  and the CSRF double-submit header, so the admin's sandbox shows what a
  reader sees.

The admin screen is `/admin/markdown`: template list, per-file editor,
create form, and a sandbox editor whose preview runs the production
renderer. Its sample document is core-owned reference material, not theme
content. See ARCHITECTURE.md §34 for the full decision set (ID-40 … ID-48).
