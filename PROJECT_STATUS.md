# Project Status

## Git Backup Phase 1 — Local Git Backup

**Status: implemented and verified.**

The CMS can now version its file-based content in a local Git
repository. The content filesystem remains the single source of
truth; Git is a versioning and backup layer over it, never the
other way around.

```
CMS content  →  Filesystem (source of truth)
                    ↓
              Local Git repository (history)
                    ↓
              commit → history → diff
```

### Implemented

The decision set is ARCHITECTURE.md §35 (ID-49 … ID-58); the rules that must
survive every change are AGENTS.md §35. This section is the status of this
phase, not the second copy of that architecture.

- **Local Git repository** for the site's file-based content,
  created on demand from the admin screen (`/admin/backup`).
- **Manual backup**: "Backup Now" produces one local commit of
  the current source content. A backup with no changes is
  refused with "nothing to commit" — empty commits are never
  fabricated.
- **History**: the commit list is read from the repository; the
  CMS stores no copy of it.
- **Diff**: pending changes between the source content and the
  last backup, plus a per-file unified diff, rendered as escaped
  plain text.
- **Backup inclusion policy**, centralized in
  `backend/internal/backup/policy.go`: Markdown posts and pages,
  the `content/system/` tree (Markdown CSS templates, custom CSS,
  custom JS) and original media enter the backup; editor leftovers,
  dotfiles and symlinks do not. The SQLite database, sessions,
  rate limits, the derived WebP cache and every other runtime
  state are excluded by construction — they live outside the
  content and media roots.
- **Repository location** is configuration, not code:
  `GIT_BACKUP_ROOT` (default `<DATA_ROOT>/git-backup`), validated
  at startup so the repository can never sit inside the content or
  media roots, nor contain the data root.
- **Repository-level lock** so concurrent backups cannot corrupt
  the index, **per-operation timeouts**, **no shell and no
  injection surface** — the service calls a Git *library*, so
  there is no command line, no argument vector and no
  interpolation to get wrong — and structured error codes
  (`backup_not_initialized`, `backup_nothing_to_commit`,
  `backup_file_too_large`, …): the Git library's own messages
  never reach the client.
- **Audit events** `git_backup.initialize` and `git_backup.commit`
  with the commit hash as the reference.
- **CSRF** protection on both mutating endpoints, via the existing
  `requireSession` wrapper.
- **Commit identity** is the fixed `CMS Backup <backup@local.invalid>`
  — the administrator's address is never written into a commit.
- **Default branch** is never hardcoded: an explicit
  `GIT_DEFAULT_BRANCH` wins, then the user's global Git
  `init.defaultBranch`, then the library default. The branch the
  screen shows is always read from the repository state.

### What enforces these properties

A rule nothing checks is a comment. Each of the claims above is
now a gate, and each gate has been verified to fail when the
property is broken:

| Property | Enforced by |
| --- | --- |
| Runtime state, the database, sessions and the WebP cache never enter a backup | `backup.TestRuntimeStateNeverEntersTheBackup` — the service is pointed at a realistic content/media/data tree and the committed work tree is compared against the policy, exactly |
| A backup never modifies, reformats or renames source content | `backup.TestBackupNeverModifiesSourceContent` — every operation is bracketed by a digest of both source roots, so a write anywhere in the sync path fails it |
| A backup is a local commit and never reaches a remote | `make arch` — no push/fetch/pull/remote API and no URL in `internal/backup` |
| No restore, checkout, revert or rollback | `make arch` — no worktree-mutating call in `internal/backup` |
| One site, one repository, one branch, many commits | `make arch` — no branch creation, and the initial branch is named only by `Initialize` |
| The inclusion policy lives in one file | `make arch` — no content directory is named anywhere else |
| The admin's address cannot become a commit | `make arch` — the package cannot reach the session or the admin row, and every signature field is a constant |
| Every backup route is behind `requireSession` (so CSRF cannot be bypassed) | `make arch` and the full-stack suite's anonymous / no-CSRF / foreign-Origin cases |
| Git is not the content authority | `make arch` — neither the Astro source nor `internal/content` can name the repository |
| The CMS keeps no copy of Git's history | `make arch` — no `git`/`commit`/`branch` table in the schema |
| The repository lives outside the content it versions | `config.backupRoot` at startup, plus `make arch` |
| No empty commit is fabricated | `backup.TestCommitRefusesEmptyCommit` and the full-stack 409 case |
| The closed loop: content → commit → history → diff | `tests/fullstack-tests.mjs` §57, against the real Go API and a real repository |
| The audit row records the decision and the hash, never the diff (ID-54) | `make arch` — every `d.Audit` call in `backup_handlers.go` names one of the two events and carries a commit hash or the branch |

The full-stack suite checks the repository's own files on disk,
so the inclusion policy is verified against what was actually
committed rather than against the API's summary of itself.

### Not implemented (later phases)

- Remote push (GitHub / GitLab / Gitea / generic remote)
- Git provider account binding, OAuth, multiple Git accounts
- Automatic / scheduled backup
- Restore, checkout, revert, rollback
- Multi-site, per-post or per-content-type branches
- A backup manifest (optional, generated metadata — deliberately
  absent: nothing in Phase 1 needs it)

A "backup" is a **local commit**. Push is a separate, future
operation, so a remote being down can never make local backup
fail.

### Key decisions

1. **go-git, not the `git` executable.** `make arch` forbids
   `os/exec` and `exec.Command` anywhere in Go production code
   (the publish path must never shell out, and the check is
   blanket), so the reference implementation is a library.
   `github.com/go-git/go-git/v5` v5.16.2 was chosen because it
   is the newest release whose `golang.org/x/crypto` requirement
   (v0.37.0) is satisfied by the project's pinned v0.43.0 —
   newer go-git releases force an x/crypto upgrade that requires
   Go ≥ 1.26, and this project targets Go 1.25 (ID-55).

2. **The repository lives outside the content root and syncs a
   snapshot.** CONTENT_ROOT and MEDIA_ROOT are two separate
   filesystem roots, and a Git work tree cannot span two
   disjoint directories. The service therefore maintains a
   snapshot work tree inside the repository directory: every
   backup hardlinks (or copies, across filesystems) each allowed
   file from the content and media roots into it, prunes what the
   sources no longer have, then stages and commits. The snapshot
   is derived state — syncing it never touches the source content,
   and because the content writer uses atomic renames, a hardlink
   captures the file exactly as it was at sync time.

3. **One site, one repository, one primary branch, many commits.**
   Branches are workspaces, not content categories: posts, pages,
   media, Markdown CSS, custom CSS and custom JS all share one
   branch and one commit timeline, because a backup must represent
   one consistent state of the whole site. Drafts (`draft: true` in
   frontmatter) are ordinary files and enter the backup like
   everything else.

4. **Reads sync the snapshot.** Status, changes and diff sync the
   snapshot before reading it, so what the screen shows is exactly
   what a backup would commit. The sync is idempotent and touches
   only derived state.

5. **Size ceilings, not silent skips.** A file above
   `GIT_BACKUP_MAX_FILE_BYTES` (default 128 MiB) or a source tree
   above `GIT_BACKUP_MAX_TOTAL_BYTES` (default 2 GiB) blocks the
   backup with an error naming the file and its size. Nothing is
   silently left out.

6. **go-git cannot be cancelled mid-operation** (its API takes no
   contexts). Every service method still takes a `context.Context`,
   checks it at each phase boundary (walk, sync, stage, commit),
   and the HTTP handlers wrap the request in a timeout. A local
   commit on a blog's content completes in milliseconds; the
   boundary checks are the safety net.

### Phase roadmap

| Phase | Scope |
| --- | --- |
| 1 (this) | Local Git repository, manual backup, history, diff |
| 2 | Remote repository configuration and push |
| 3 | Git provider account binding (GitHub / GitLab / Gitea) |
| 4 | OAuth / GitHub App / GitLab OAuth / Gitea OAuth |
| 5 | Automatic push on backup |
| 6 | Restore / rollback |

The future account model (`GitAccount`: provider, external user
id, display name, credential reference) and remote model
(`GitRemote`: provider, owner, name, url, account, enabled) are
recorded here only — no table exists for them yet. Remote
credentials will never be stored as plaintext settings or embedded
in remote URLs; provider authentication will follow each
provider's current official recommendation (GitHub App / OAuth
rather than long-lived PATs).

The GitHub identity binding that already exists (Phase A, an OAuth
App) is not the backup's provider authorization: it proves which
GitHub account an administrator controls, its credential carries
no repository capability, and the backup phases decide their own
provider model — they must not assume the binding's credential may
be reused for a push (ARCHITECTURE.md §38, ID-76).

A Git account used for **backup** and a Git account used for
**CMS login** are two different concepts and are not assumed to be
the same person. CMS sign-in keeps using `admin_user` and
`session`, and this phase adds nothing to it.

### Why one branch, and not one per project or content type

A branch is a *workspace*, not a category. `posts`, `drafts`,
`media`, `css` and `js` branches would make "restore the site as it
was on Tuesday evening" a five-way merge rather than one checkout,
and there is one site: posts, pages, media, Markdown CSS and custom
CSS/JS are parts of one consistent state, so they share one branch
and one commit timeline.

Drafts are not a branch either. `draft: true` is a flag in
frontmatter on an ordinary file, so the history reads

```
initial → published → draft revision → draft updated → published
```

which is the site's real timeline. A collaborator review workflow,
if one is ever needed, is what `feature/*` branches are for — and
that is Phase 6's question, not Phase 1's.

---

# CMS Multi-Administrators & GitHub Account Binding

**Status: implemented and verified.**

A CMS administrator is a CMS-local account, created in the CMS and
signed in with a username and password. A GitHub identity is a
different kind of fact — which external account an administrator
proved they control — and a GitHub credential is a third: what the
CMS may *do* with that account, stored encrypted. The three are
three tables, and none of them grants a permission in another.

```
admin_user             local identity: username, argon2id hash,
                       role (owner|admin), status (active|disabled)
     │
     ├── github_identity    external identity, keyed on GitHub's
     │   │                  immutable numeric user id (UNIQUE)
     │   └── github_credential   sealed credential for that identity
     │
     └── session             bound to an admin_id, so a disabled
                            account's rows authenticate nothing
```

## Administrator management

- The pre-existing single-administrator schema (which had
  `CHECK (id = 1)`) is migrated to a set of administrators. The
  row the setup form created becomes the first **owner**.
- **Two roles, deliberately only two.** An `owner` may create,
  disable and reset other administrators and manage any binding; an
  `admin` may change its own password and its own bindings. There is
  no permission table and no policy language. Every check is in the
  API — a hidden button is not a permission model.
- `admin_user` gains `role`, `status` and `last_login_at`;
  `session` gains `admin_id` and an index, which is what makes
  "disable an account" mean "its sessions stop resolving". A
  disabled account cannot sign in, and its existing sessions are
  deleted.
- **Lockout guards.** Disabling yourself is refused
  (`admin_status_self`); disabling the last active administrator or
  the last active owner is refused (`admin_last_active`,
  `admin_last_owner`). There is no delete: an administrator is never
  removed, so its audit history survives.
- **Password changes.** Changing your own password verifies the
  current one and revokes every *other* session of that account;
  resetting somebody else's (owner-only) needs no old password and
  revokes all of their sessions. Resetting your own password through
  the reset route is refused, because that would be a way around the
  current-password check.
- New administrators sign in with CMS credentials alone. Nothing in
  this phase requires a GitHub account.

## First-administrator initialization

The first administrator is **generated by the backend, never defaulted**
(ARCHITECTURE.md ID-71). A first run with no credentials in the request
creates an owner whose username is `owner-` plus six random characters and
whose password is 20 random characters from an unambiguous alphabet. The
plaintext password is returned **exactly once**, in the setup response, and
the login screen displays it as a save-this-now panel. It appears nowhere
else: not in the database, not in the server log, not in the audit row, not
in any later API response. Losing it is recoverable only with the local
`set-password` command.

A deployment that declares itself a development one (`APP_ENV=development`,
or `dev`) may additionally create its first account from credentials typed
on the spot — `owner` / `12345`, for instance. A production deployment
refuses that request with `setup_typed_not_allowed`, because accepting an
operator-chosen first password by default is the same defect as a default
password, reached from the other side. An unset or unrecognised `APP_ENV` is
production.

The account that either path creates is an **owner**, is signed in
immediately, and needs no GitHub account. Setup is refused with 409 once any
administrator exists, so the window closes the moment the first run lands.

## GitHub account binding

**The scheme is the GitHub OAuth App *user authorization* flow**: the
binding phase solves exactly one problem — attaching a GitHub identity
to a CMS administrator — and an OAuth App is the simplest registration
that does exactly that. It requests **no scope**: the public profile
`/user` returns (immutable numeric id, login, name, avatar URL) is all
the binding stores. Repository permissions, installations and
short-lived App tokens belong to a later, independently-decided phase;
binding an identity is not remote-backup capability, and this phase
does not pre-implement one (ARCHITECTURE.md ID-64, §38/ID-76).

```
Admin → POST /bind/start        server creates a one-shot state row
      → browser to GitHub        (state, admin_id, PKCE verifier, expiry)
      → GitHub redirects to /api/v1/admin/github/callback
      → server consumes the state, exchanges the code with the
        client secret (server-to-server), asks GitHub /user who the
        token belongs to, and binds that id to the administrator the
        state named
      → 303 to /admin/admins/<id>?result=ok   (id = the transaction's admin)
```

Security properties, each provable in the test suites:

- **The callback is unauthenticated by design** — the browser arrives
  from GitHub with no session cookie, because the cookie is
  `SameSite=Strict`. The state row is the authentication: it names
  the administrator, so one administrator's authorization cannot be
  bound to another account.
- **One-shot state with PKCE S256.** Consuming the state is the same
  statement that reads it (`UPDATE … RETURNING`), so a replay cannot
  win; an expired or unknown state is the same refusal.
- **Identity comes from GitHub's API, never from the callback
  parameters.** A username or avatar in a URL is user-controlled; the
  numeric id returned by `/user` is not.
- **A GitHub account binds to exactly one CMS administrator.** The
  unique index on `github_user_id` is the mechanism, and a repeat
  authorization refreshes the same binding instead of adding a
  second.
- **The client secret and the authorization code never leave the
  server.** The browser sees the authorize URL and a redirect.
- **Redirect target is built from the configured callback origin and
  a fixed path**, never from a query parameter.

## Credential storage

- The encryption key is owned by the backend, not by the deployment
  (ARCHITECTURE.md ID-73). It is a fixed-name file,
  `<DATA_ROOT>/github-credential.key`, created once with `crypto/rand`
  on the first binding, mode `0600`, written through a temporary file
  and a rename so a crash cannot leave half a key. **No operator action
  and no variable**, because a second spelling would be a second key.
- `github_credential` holds the access token (and refresh token, if
  the scheme has one) as **AES-256-GCM ciphertext** produced by a key
  derived with **argon2id** from that file, with a per-record salt, so
  two bindings encrypted with the same key do not share a derived key.
- **The key is never regenerated.** A missing key with bindings present
  is a refusal: a new key would orphan every ciphertext in the
  database, and the symptom would be a binding that claims a credential
  it cannot use. The backend logs it, keeps serving the site, and
  refuses to *start* a binding until the operator unbinds and re-binds.
- **No plaintext fallback.** The API answers only `hasCredential`,
  `scope` and `expiresAt`. A database backup yields ciphertext; a
  browser response yields no credential at all.
- The PKCE verifier is server-side secret material and lives only in
  the state row, which is deleted by the janitor once dead.

## Configuration

New variables, all optional (see `.env.example`). These are **site-level
deployment** configuration — the identity of the OAuth application, not
something an administrator screen should ever hold:

| Variable | Meaning |
| --- | --- |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | the OAuth App's user-authorization credentials |
| `GITHUB_CALLBACK_URL` | the fixed callback address, registered with the OAuth App; **https in production** |
| `GITHUB_OAUTH_STATE_TTL` | transaction lifetime, 1m…1h (default 10m) |
| `GITHUB_WEB_BASE` / `GITHUB_API_BASE` | endpoint overrides; production uses the GitHub defaults, the tests point at a local server |
| `APP_ENV` | `production` (default) or `development`. Only a development deployment may create its first administrator from typed credentials |
| `PUBLIC_ORIGIN` | a deployment-pinned Origin allowlist. Optional; with it unset, the Origin check accepts the address the browser dialled |

**Site setting (not a variable):** `allowedOrigins` in Settings → 访问地址.
Browser addresses mutations are additionally accepted from, one per line.
Empty means same-origin only — which is the working default, not a missing
configuration (ARCHITECTURE.md ID-72).

A **partially** configured binding is a fatal startup error: an ID
without a secret, a secret without a callback, or a binding without an
encryption key all fail loudly rather than half-working in front of an
administrator. A deployment that sets none of them is unchanged:
password sign-in, sessions, CSRF, rate limits, media, comments and the
local Git backup never touch GitHub.

Because the callback is a fixed address, a deployment behind TLS
termination or a hostname must set `GITHUB_CALLBACK_URL` to the
*external* URL the browser will use — the same reason `PUBLIC_ORIGIN`
exists.

## Mapping the site out to one address, or to a hundred

**Nothing to configure.** With no `PUBLIC_ORIGIN` and an empty
`allowedOrigins` list, a mutation is accepted exactly when its `Origin`
is the address the browser dialled — which Astro forwards to Go in
`X-Forwarded-Host`. A site reachable on a public IP, behind NAT, through
a container port-forward or on a new domain therefore works with no
configuration at all, and a request whose `Origin` names an address
nobody dialled is refused:

| the browser is on | configuration needed |
| --- | --- |
| `http://<ip>:9900`, `http://localhost:9900`, `http://any-domain:9900` served directly by Astro | none |
| a TLS-terminating front end, so Go sees `http` behind an `https` browser | one entry: `https://blog.example.com` |
| a second domain pointing at the same site | one entry per domain |

`allowedOrigins` in Settings (the 访问地址 section of the settings screen)
is where those entries live, taking effect on the next request without a
restart. An **empty list is not an open list**: it means "only the address
you are browsing from", which is both the default and the safest thing a
deployment can be in. `PUBLIC_ORIGIN` remains the deployment-pinned
allowlist — read at startup, not widenable from the browser — and the two
compose. A wildcard is refused on the way in and dropped on the way out,
because "allow all origins" would make every website on the internet a
permitted origin for admin mutations (ARCHITECTURE.md ID-72, ID-24).

## 配置

| Route | Who |
| --- | --- |
| `GET /api/v1/admin/me` | any administrator || `GET /api/v1/admin/admins` | owner |
| `POST /api/v1/admin/admins` | owner |
| `GET /api/v1/admin/admins/{id}` | self or owner |
| `POST /api/v1/admin/admins/{id}/status` | owner |
| `POST /api/v1/admin/me/password` | any administrator |
| `POST /api/v1/admin/admins/{id}/password` | owner |
| `GET /api/v1/admin/github/config` | any administrator |
| `GET /api/v1/admin/github/identities?adminId=` | self or owner |
| `POST /api/v1/admin/github/bind/start` | self or owner |
| `GET /api/v1/admin/github/callback` | the OAuth transaction |
| `POST /api/v1/admin/github/identities/{id}/unbind` | self or owner |

The callback is the one route in the admin surface that is not behind
a session, and it is validated by its one-shot state.

## New pages and views

- `/admin/admins` — the administrator list, the create form and the
  change-my-password form; an ordinary administrator is pointed to
  their own detail page instead of the list.
- `/admin/admins/{id}` — the account, the reset-password form
  (owner), and the GitHub panel: whether OAuth is configured, the
  callback address, the bound accounts with their authorization
  state, the bind button, and the result of a just-finished
  authorization. Binding has no screen of its own — it belongs to an
  administrator, and it lives on that administrator's page.
- Theme slots `AdminsView` and `AdminDetailView` in `bluearchive`,
  with the sidebar entry 管理员.
- Six new `data-cms-*` actions/forms and twelve new notice keys, all
  translated in the theme's language.

## Audit events

`admin.created`, `admin.enabled`, `admin.disabled`,
`admin.password_changed`, `admin.password_reset`,
`github_binding.started`, `github_binding.created`,
`github_binding.failed`, `github_binding.deleted` — each naming the
acting administrator as its actor, and never a password, token, code,
verifier, secret or session token. The audit log survives a disable
and an unbind.

## Database migration

`user_version` 4 → 5 (`store.migrateV5`), in one transaction:

- `admin_user` rebuilt with an AUTOINCREMENT key, `role`, `status`,
  `last_login_at`; the existing row becomes an active `owner`.
- `session.admin_id INTEGER NOT NULL DEFAULT 1` + index.
- `github_identity` (with `github_user_id UNIQUE`),
  `github_credential` (with `identity_id UNIQUE`) and `oauth_state`.

## Security boundaries

- A GitHub identity grants no CMS permission. Binding one creates no
  account, promotes none, and opens no admin route.
- No email-based account merging, and no name-based association: the
  immutable numeric id is the only key.
- An unbind removes the local credential and the identity row, and
  nothing else — not the administrator, not the local Git backup
  repository, not a remote repository.
- With no GitHub configuration, or with GitHub unreachable, the local
  backup (initialize / commit / history / changes / diff) works
  exactly as before. This is asserted in the full-stack suite.

## Tests

- `internal/github`: the credential crypto (round trip, nonce and
  salt uniqueness, wrong-key and tamper refusal, short-key refusal,
  fingerprint) and the whole flow against a local stand-in for GitHub
  (state missing / unknown / expired / replayed, exchange failure,
  user lookup failure, identity taken by another administrator, repeat
  authorization of the same account, unbind, unconfigured).
- `internal/api`: the administrator lifecycle over the real router —
  create, duplicate username, validation, login, own-password change
  with a wrong current password, password reset, disable (session
  revoked, sign-in refused), re-enable, lockout guards, 401 without a
  session, 403 without CSRF or with a foreign Origin, 403 for an
  ordinary administrator, and the audit actor.
- `internal/api/github_handlers_test.go` + the full-stack suite: the
  OAuth flow driven through the browser-facing origin, against a mock
  GitHub server, including the redirect, the replay, the cross-account
  unbind refusal and the ciphertext-on-disk assertion.
- `internal/config`: the optional-configuration, full-configuration,
  half-configuration and endpoint-override cases, plus `APP_ENV` — where an
  unrecognised value must resolve to the *stricter* mode.
- `internal/store`: the upgrade itself. A v4-shaped database is built (one
  administrator, one live session), reopened, and the assertions are that the
  administrator survived as an active **owner**, that its existing session
  belongs to it (so an upgrade does not sign the operator out), that the three
  new tables exist with the unique index that makes the identity unshareable,
  and that a second Open does not rebuild anything.
- `scripts/arch-check.mjs` gained six checks: every administrator
  route behind a session, the callback the single validated exception,
  no seal without a keyring, unique identity with a separate
  credential, no credential in a response, and the key injected rather
  than literal. Three more cover the first run: credentials come from
  `crypto/rand`, the typed form is gated on the development deployment,
  and the generated password is not in the log line. The log check was
  verified by planting the leak it exists to catch, because a check that
  has never failed is not evidence.

Verification: `gofmt`, `go build ./...`, `go vet ./...`,
`go test ./...`, `go test -race ./...`, `npm run check`,
`npx tsc --noEmit`, prettier, `make arch` (202/202),
`tests/integration-tests.mjs` (155), `tests/fullstack-tests.mjs`
(735), `tests/theme-tests.mjs` (275),
`tests/color-scheme-tests.mjs` (55), `make verify`,
`make verify-race`.

## Not implemented

- Remote push/pull, automatic backup schedules, remote repository
  creation, remote restore, and multi-site management. The data model
  and service interface leave room for a remote phase to pick a bound
  identity and its credential; nothing here implements one.
- OAuth token refresh and revocation calls. The schema records
  `expires_at` and the refresh token, and the API reports expiry; the
  exchange itself is a later phase, because no code path needs a fresh
  token until a remote operation exists.
- Per-administrator two-factor authentication, password policy beyond
  the minimum length, and role changes after creation.

---

# Fix: the GitHub binding button went to the wrong GitHub

**Status: fixed and verified.**

## The fault, reproduced

Pressing **绑定新的 GitHub 账号** sent the browser to

```
http://127.0.0.1:9911/login/oauth/authorize?client_id=…&code_challenge=…&code_challenge_method=S256&redirect_uri=…&state=…
```

a local stand-in answering 404. Recorded from a real browser against a real
stack: the click POSTs `/api/v1/admin/github/bind/start`, receives that URL, and
navigates to it. The construction was never wrong — `AuthorizeURL` builds
`webBase + "/login/oauth/authorize"` and the default base is
`https://github.com`.

The root cause was the configuration: `GITHUB_WEB_BASE` and `GITHUB_API_BASE`
were read from the environment **unconditionally**. Those two variables are
precisely the ones that decide which GitHub an installation authorizes against,
and they were the only part of the configuration with no boundary: a residual
test value (`http://127.0.0.1:9911`) was as authoritative as a deliberate one.
Nothing in the application starts a mock, nothing logged where the flow was
going, and the only symptom was a button that left for a host nobody had chosen.

## The fix

`GITHUB_ALLOW_ENDPOINT_OVERRIDES` (ARCHITECTURE.md §37, ID-74). Unset — the
default — the deployment is pinned to `https://github.com` and
`https://api.github.com` on every path, including the unconfigured one, so the
bases cannot be empty on one path and a stand-in on another. Either override set
*without* the opt-in is a **fatal startup error** naming both variables and both
remedies, because the two failure directions are symmetric: silently dropping a
value an operator meant to honour would be the same surprise in the other
direction. With the opt-in set, the overrides are honoured exactly as before,
validation included — which is what a GitHub Enterprise Server host and a test
harness both legitimately need. `APP_ENV=development` is not the gate and cannot
be: a development deployment is still a deployment.

Also fixed on the way, found by the same reproduction pass (ID-75): the
credential key file was measured after `strings.TrimSpace`, so a machine-written
32-byte key that happened to begin or end in a whitespace byte measured 31 and
was refused as unusable — a fatal startup error on roughly one deployment in
twenty, about a file the backend had generated correctly. The file's bytes are
the key material now and its length is the whole test; whitespace-only material
is still refused, because that is `echo > key` rather than a key.

## What a deployment can now rely on

- A normal deployment's bind button leaves for
  `https://github.com/login/oauth/authorize`, with the configured
  `GITHUB_CALLBACK_URL` as `redirect_uri` byte for byte, a fresh 32-byte
  `state` and PKCE S256.
- A residual `GITHUB_WEB_BASE=http://127.0.0.1:9911` cannot become the
  provider: the process refuses to start, naming the override and the gate.
- The startup log records `authorize_endpoint`, `api_endpoint` and
  `endpoint_overrides_allowed`, so "where does this button go?" is answerable
  from the log.
- Mock GitHub remains available, in one explicit line, with its name on it.

## Files changed

| File | Change |
| --- | --- |
| `backend/internal/config/config.go` | the override gate, defaults on every path, endpoint logging |
| `backend/internal/config/github_config_test.go` | gate tests, and a config→client test for the real authorize URL |
| `backend/internal/github/crypto.go`, `keyfile.go` | the key file is bytes, not a trimmed string |
| `backend/internal/github/keyfile_test.go` | whitespace-byte and too-short key cases |
| `astro/src/themes/bluearchive/admin/views/GitHubView.astro` | the unconfigured screen no longer names `GITHUB_TOKEN_KEY_FILE`, which no longer exists |
| `scripts/arch-check.mjs` | fails if the override read leaves the opt-in branch |
| `tests/fullstack-tests.mjs` | the mock stack declares the opt-in; two new cases (§36) |
| `.env.example` | documents the gate and what the overrides are for |
| `ARCHITECTURE.md` §37, `AGENTS.md` §36 | the decision set, as rules |

## Verification

Real runs, not only unit tests. Against a stack configured as a normal
deployment (credentials set, no overrides), the click was recorded in a real
browser navigating to `https://github.com/login/oauth/authorize?…` and landing on
GitHub's own sign-in page, with `redirect_uri` exactly the configured callback.
The real OAuth flow was **not** completed: this deployment has no registered
OAuth App, so no authorization was performed and none is claimed. With
`GITHUB_ALLOW_ENDPOINT_OVERRIDES=1` and a local stand-in, the same click
completed the whole flow in the browser — authorize, callback, `?result=ok`,
and a binding table naming the account and its immutable id. A cancelled
authorization and a forged/replayed state each landed on the result page with
their own message. CMS sign-in, the multi-administrator screen, an existing
binding and the local Git backup (initialize → commit) were all exercised after
the change.

Gates: `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...`
(`make verify-race`), `npm run check`, `npx tsc --noEmit`, prettier,
`make arch` (203/203), `tests/integration-tests.mjs` (155),
`tests/fullstack-tests.mjs` (751), `tests/theme-tests.mjs` (275),
`tests/color-scheme-tests.mjs` (55), `make verify`.

## Re-verification with a running stack (2026-10-16)

The whole boundary was re-run from scratch against a live deployment, with no
source changes in between — this is the record of what the tree above was
observed to do.

- **The fault, reproduced as originally reported.** A backend started with
  `GITHUB_WEB_BASE=GITHUB_API_BASE=http://127.0.0.1:9911` and no opt-in
  refuses to start, naming both variables and `GITHUB_ALLOW_ENDPOINT_OVERRIDES`
  as a test configuration. Nothing serves.
- **A normal deployment.** Startup logged
  `authorize_endpoint=https://github.com/login/oauth/authorize`,
  `api_endpoint=https://api.github.com`, `endpoint_overrides_allowed=false`.
  The click on 绑定新的 GitHub 账号 was recorded in a real browser: the POST
  to `/api/v1/admin/github/bind/start` returned the `authorizeUrl`, and the
  browser navigated to

  ```
  https://github.com/login/oauth/authorize?client_id=…&code_challenge=…&code_challenge_method=S256&redirect_uri=http%3A%2F%2F127.0.0.1%3A9910%2Fapi%2Fv1%2Fadmin%2Fgithub%2Fcallback&state=…
  ```

  with `redirect_uri` byte for byte the configured `GITHUB_CALLBACK_URL`, a
  64-hex `state`, and PKCE S256. The navigation landed on GitHub's own
  sign-in page. The real OAuth flow was **not** completed: this deployment
  has no registered OAuth App, so no authorization was performed and none
  is claimed.
- **The opt-in path.** With `GITHUB_ALLOW_ENDPOINT_OVERRIDES=1` and a local
  stand-in, the browser completed the whole flow: authorize → callback →
  `/admin/github?result=ok`, and the binding table named `MOCK-OCTO` with
  immutable GitHub user id 8899. The stored credential is sealed ciphertext
  (`v1.…`); no plaintext token exists in the database. Cancelling the
  authorization landed on `?result=cancelled` (「授权已在 GitHub 取消…」),
  and a replayed or forged state on `?result=error&reason=github_error`
  (「绑定没有完成…」).
- **The cleanup used the real route.** The test-mode binding was removed
  through `POST /api/v1/admin/github/identities/{id}/unbind`: the identity
  and its credential rows are gone, `GET …/identities` returns
  `{"bindings":[]}`, and the audit log records `github_binding.deleted` with
  the immutable GitHub user id as the reference.

Gates re-run on this tree: `make verify` exit 0 — architecture checks 203/203,
integration 155, full-stack 751, theme contract 275, colour-scheme 55 — and
`make test-go-race` exit 0 (`go test -race ./...`).

## Phase boundary amendment (2026-10-16)

The binding phase now explicitly uses a **GitHub OAuth App** and stops at
identity (ARCHITECTURE.md ID-64, §38/ID-76). The earlier text had made a
future remote backup a precondition of linking an account — the scheme was
a GitHub App *because a later push would want one*, so an operator had to
understand App registration, permissions and installation before the first
identity could be bound. The boundary is now enforced in three places:

- **The decision.** ID-64 rewritten: an OAuth App is the simplest
  registration that proves an account, and **no `scope` is requested** —
  the public profile `/user` returns is all the binding stores, and
  `user:email` is added only if a phase actually needs email. §38/ID-76
  added: Phase B (remote-backup design) and Phase C (a GitHub App for
  repository operations) are independent; Phase C may not inherit Phase A's
  authorization model; a bound identity is never a claim about a
  repository. `AGENTS.md` §36 carries the same rule.
- **The data model.** No installation id, repository, remote or push state
  exists anywhere; `make arch` now compares the column sets of
  `github_identity` and `github_credential` against exact lists, so a
  Phase C field cannot appear without the architecture being amended
  first.
- **The words.** The admin screen says binding grants no repository
  permission and is not remote-backup capability; the callback hint and
  the unconfigured notice name the OAuth App; `.env.example`, the Go
  comments and the unit-test message describe the same scheme.

**No behavior changed.** The flow already requested no scopes and the
authorize/token/user calls are protocol-identical for both registrations.
Against the rebuilt stack, `POST /api/v1/admin/github/bind/start` was
re-observed returning

```
https://github.com/login/oauth/authorize?client_id=…&code_challenge=…&code_challenge_method=S256&redirect_uri=…&state=…
```

with no `scope` parameter, and `/admin/github` rendered the new copy.

Gates after the amendment: `make verify` exit 0 — architecture checks
**205/205** (two new column-set checks), integration 155, full-stack 751,
theme contract 275, colour-scheme 55.

## Binding moved into the administrator page (2026-10-16)

GitHub binding is no longer a first-level admin screen. It was one page with
its own sidebar entry (`/admin/github`, theme slot `GitHubView`); it is now a
panel on the administrator's detail page (`/admin/admins/{id}`), because a
binding belongs to an administrator rather than to the site. What changed:

- **The callback lands on the administrator's page.** ID-69: the `303` target
  is `<configured origin>/admin/admins/<id>`, where the id is read from the
  one-shot state row and rendered as an integer — server-owned, never a
  request parameter. A callback naming a transaction this server never issued
  has no id to show, so it lands on `/admin/admins` instead of inventing one.
- **The panel carries all three states.** The detail screen fetches the same
  config and bindings (ID-20 cookie forwarding intact), keeps the binding
  table, and adds the bind button, the callback address an OAuth App must
  register, the unconfigured notice naming the environment variables, and the
  machine-coded `?result=` message the callback redirects with.
- **The owner can bind for another administrator, and now the UI can too.**
  `bind/start` always accepted an `adminId` for an owner; the button now
  carries `data-cms-id` and `/cms.js` sends it. The API still validates
  self-or-owner, so the attribute is the target, not the authority.
- **The standalone page is gone.** `pages/admin/github.astro`,
  `GitHubView.astro`, the `GitHubView` theme slot, the `GitHub` admin-title
  key and the sidebar entry were removed; `/admin/github` answers 404. An
  ordinary administrator is pointed from the Admins screen to their own detail
  page, which is the only place a binding is managed.
- **`make arch` now checks it.** §36 6c: the result base is `/admin/admins`,
  the id comes from `GetOAuthState` and is formatted with `strconv.FormatInt`,
  and neither the standalone page nor the sidebar entry exists.

Verified against the rebuilt stack (build first, then restart, §8):

- A signed-in browser opened `/admin/admins/1`: the sidebar lists 11 entries
  with no GitHub item; the page renders 账户, 修改我的密码 and GitHub 账号绑定
  with `data-cms-action="github-bind" data-cms-id="1"`, the registered
  callback hint and the identity-only copy.
- The click POSTed `/api/v1/admin/github/bind/start` and navigated to
  `https://github.com/login?client_id=…&return_to=/login/oauth/authorize?…`
  with PKCE S256 and no `scope`. The real authorization was **not** completed:
  the deployment still holds the placeholder client id, so no binding was
  created and none is claimed.
- A callback with no state `303`s to
  `/admin/admins?result=error&reason=github_error` — an unknown transaction
  gets the administrators screen, not somebody's detail page.
- `/admin/github` returns 404, and no page links it.

Gates on this tree: `make verify` exit 0 — architecture checks **206/206**
(§36 6c added), integration 155, full-stack 745, theme contract 275,
colour-scheme 55 — and `make test-go-race` exit 0. The full-stack count moved
751 → 745 because the removed route left the two route inventories (−7
assertions) and the panel check gained one (+1); the difference is accounted
for, not lost.
