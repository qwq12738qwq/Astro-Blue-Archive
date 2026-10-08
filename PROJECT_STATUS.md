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
