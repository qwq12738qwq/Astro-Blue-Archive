// Package store owns the SQLite database.
//
// ARCHITECTURE.md §2 / §36: this database must never contain article content.
// There is deliberately no posts, pages, content_cache or search_index table,
// and no title/body/description/tags/draft column anywhere. The only article
// reference permitted is comment.post_slug, which points at a filesystem slug.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, distroless-friendly images
)

// DB wraps the SQLite handle.
type DB struct {
	*sql.DB
}

// Open connects to SQLite, applies pragmas and migrates the schema.
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)",
		url.PathEscape(path),
	)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// A single connection keeps WAL contention trivial for a single-admin blog
	// (ARCHITECTURE.md §21, R9).
	//
	// Consequence to respect in every caller: never hold two open result sets
	// (or an open Rows plus another query) at the same time, because the second
	// one waits forever for the connection the first still holds.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	db := &DB{sqlDB}
	if err := db.migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// schemaVersion is bumped whenever migrations change.
const schemaVersion = 5

func (db *DB) migrate(ctx context.Context) error {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported %d", v, schemaVersion)
	}
	if v == schemaVersion {
		return nil
	}

	// Multi-statement DDL in one transaction. Article content is never stored.
	const ddlV1 = `
CREATE TABLE admin_user (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE session (
  id           TEXT PRIMARY KEY,
  token_hash   BLOB NOT NULL UNIQUE,
  csrf_secret  BLOB NOT NULL,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  ip_hash      BLOB,
  user_agent   TEXT NOT NULL
);
CREATE INDEX idx_session_expires ON session(expires_at);

CREATE TABLE login_attempt (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  username   TEXT NOT NULL,
  ip_hash    BLOB NOT NULL,
  ok         INTEGER NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX idx_login_lookup ON login_attempt(username, ip_hash, created_at);

CREATE TABLE comment (
  id           TEXT PRIMARY KEY,
  post_slug    TEXT NOT NULL,
  nickname     TEXT NOT NULL,
  content      TEXT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('pending','approved','spam','deleted')),
  created_at   TEXT NOT NULL,
  moderated_at TEXT,
  ip_hash      BLOB,
  user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_comment_post   ON comment(post_slug, status, created_at);
CREATE INDEX idx_comment_status ON comment(status, created_at);

CREATE TABLE media (
  id         TEXT PRIMARY KEY,
  filename   TEXT NOT NULL,
  path       TEXT NOT NULL UNIQUE,
  mime       TEXT NOT NULL,
  size       INTEGER NOT NULL,
  width      INTEGER,
  height     INTEGER,
  sha256     BLOB NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX idx_media_created ON media(created_at DESC);

CREATE TABLE setting (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE rate_limit (
  bucket       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  count        INTEGER NOT NULL,
  PRIMARY KEY (bucket, window_start)
) WITHOUT ROWID;

CREATE TABLE content_event (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL,
  ref        TEXT NOT NULL,
  actor      TEXT NOT NULL DEFAULT 'admin',
  created_at TEXT NOT NULL
);
CREATE INDEX idx_event_created ON content_event(created_at DESC);
`
	if v < 1 {
		if _, err := db.ExecContext(ctx, ddlV1); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	if v < 2 {
		if err := db.migrateV2(ctx); err != nil {
			return err
		}
	}
	if v < 3 {
		if err := db.migrateV3(ctx); err != nil {
			return err
		}
	}
	if v < 4 {
		if err := db.migrateV4(ctx); err != nil {
			return err
		}
	}
	if v < 5 {
		if err := db.migrateV5(ctx); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// migrateV2 adds the media usage index and the media metadata this phase needs.
//
// ARCHITECTURE.md §50: media_usage is a DERIVED index over `content/`. It holds a
// path and a slug, never a title, a body or any other content, so it cannot become
// a second source of truth for an article. Dropping the table costs a rescan.
func (db *DB) migrateV2(ctx context.Context) error {
	// `alt` is presentation metadata an admin may set for use when Markdown does
	// not supply its own alt text (ARCHITECTURE.md §134). It is empty by default,
	// which is exactly the "no opinion" state.
	const ddl = `
ALTER TABLE media ADD COLUMN alt TEXT NOT NULL DEFAULT '';

CREATE TABLE media_usage (
  media_id        TEXT NOT NULL,
  content_type    TEXT NOT NULL CHECK (content_type IN ('post','page')),
  content_slug    TEXT NOT NULL,
  reference_count INTEGER NOT NULL DEFAULT 1,
  last_seen_at    TEXT NOT NULL,
  PRIMARY KEY (media_id, content_type, content_slug)
);
CREATE INDEX idx_usage_media ON media_usage(media_id);
CREATE INDEX idx_usage_content ON media_usage(content_type, content_slug);
`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("apply schema v2: %w", err)
	}
	return nil
}

// migrateV3 adds the custom-asset metadata index.
//
// ARCHITECTURE.md ID-33: `content/system/css/` and `content/system/js/` are the
// source of truth for custom code, exactly as `custom.css` and `custom.js` already
// were. This table records *metadata* about those files and nothing else — no body,
// no content column, not even an empty one to fill in later.
//
// Every column here is derivable from the filesystem, which is the point: dropping
// the table costs one rescan of two directories, and a file dropped into `css/` by
// hand is a first-class asset rather than an untracked one.
func (db *DB) migrateV3(ctx context.Context) error {
	const ddl = `
CREATE TABLE custom_asset (
  filename   TEXT NOT NULL,
  type       TEXT NOT NULL CHECK (type IN ('css','js')),
  enabled    INTEGER NOT NULL DEFAULT 1,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  checksum   TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (type, filename)
);
CREATE INDEX idx_custom_asset_enabled ON custom_asset(type, enabled);
`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("apply schema v3: %w", err)
	}
	return nil
}

// migrateV4 adds the Markdown style template metadata index.
//
// ARCHITECTURE.md §34: `content/system/markdown/` is the source of
// truth for the Markdown presentation layer, exactly as
// `content/system/css/` is for custom code. This table records
// *metadata* about those files and nothing else — no body, no
// content column, not even an empty one to fill in later. Every
// column is derivable from the filesystem; dropping the table costs
// one rescan of one directory.
func (db *DB) migrateV4(ctx context.Context) error {
	const ddl = `
CREATE TABLE markdown_asset (
  filename   TEXT NOT NULL PRIMARY KEY,
  enabled    INTEGER NOT NULL DEFAULT 1,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  checksum   TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX idx_markdown_asset_enabled ON markdown_asset(enabled);
`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("apply schema v4: %w", err)
	}
	return nil
}

// migrateV5 turns the single administrator into a set of administrators, and
// adds the GitHub account-binding tables.
//
// The three additions are three different kinds of fact, and they are three
// tables because of that:
//
//   - `admin_user` stops being a one-row table. A CMS administrator is local
//     identity: a username, an argon2id hash, a role and an enabled/disabled
//     state. The CHECK (id = 1) that made a second row impossible is replaced
//     by an AUTOINCREMENT key, and the pre-existing row — there is at most one,
//     created by the setup form — becomes the first *owner*, which is the only
//     account that may create, disable or reset other administrators.
//   - `github_identity` is an external identity: GitHub's immutable numeric
//     user id, which is why the column is UNIQUE. A username is display
//     information and can change on GitHub's side without invalidating the
//     binding; the id cannot.
//   - `github_credential` holds the OAuth credential, encrypted, keyed to the
//     identity. Identity and credential are separate rows because they answer
//     different questions: "which GitHub account is this" and "what may we do
//     with it". One identity never holds two live credentials.
//
// `oauth_state` is the one-shot authorization transaction. It is deliberately
// in the database rather than in memory: the browser leaves for github.com and
// comes back, a restart must not invalidate a legitimate transaction halfway
// through, and a table gives the state a row that can be expired and consumed
// exactly once.
//
// The `session` table gains an `admin_id`, which is what makes "disable an
// administrator" mean "their sessions stop working": the lookup joins on it and
// refuses a disabled account.
func (db *DB) migrateV5(ctx context.Context) error {
	// The table is rebuilt rather than altered: SQLite cannot drop a CHECK
	// constraint in place, and the column set changes anyway. This runs inside
	// one transaction, so a failure part-way leaves the previous schema
	// (user_version is only written at the end of migrate).
	//
	// database/sql does not promise multi-statement support, so the rebuild is
	// split into the discrete steps the driver certainly accepts.
	steps := []string{
		`ALTER TABLE admin_user RENAME TO admin_user_old`,
		`CREATE TABLE admin_user (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('owner','admin')),
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  last_login_at TEXT
);`,
		`INSERT INTO admin_user (id, username, password_hash, role, status, created_at, updated_at)
  SELECT id, username, password_hash, 'owner', 'active', created_at, updated_at FROM admin_user_old`,
		`DROP TABLE admin_user_old`,
		`ALTER TABLE session ADD COLUMN admin_id INTEGER NOT NULL DEFAULT 1`,
		`CREATE INDEX idx_session_admin ON session(admin_id)`,
		`CREATE TABLE github_identity (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  admin_id       INTEGER NOT NULL,
  github_user_id INTEGER NOT NULL UNIQUE,
  login          TEXT NOT NULL,
  name           TEXT NOT NULL DEFAULT '',
  avatar_url     TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);`,
		`CREATE INDEX idx_github_identity_admin ON github_identity(admin_id)`,
		`CREATE TABLE github_credential (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  identity_id      INTEGER NOT NULL UNIQUE,
  access_token_ct  TEXT NOT NULL,
  refresh_token_ct TEXT,
  token_type       TEXT NOT NULL DEFAULT '',
  scope            TEXT NOT NULL DEFAULT '',
  expires_at       TEXT,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);`,
		`CREATE INDEX idx_github_credential_identity ON github_credential(identity_id)`,
		`CREATE TABLE oauth_state (
  state         TEXT PRIMARY KEY,
  admin_id      INTEGER NOT NULL,
  code_verifier TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  consumed_at   TEXT
);`,
		`CREATE INDEX idx_oauth_state_expires ON oauth_state(expires_at)`,
	}
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		for _, step := range steps {
			if _, err := tx.ExecContext(ctx, step); err != nil {
				return fmt.Errorf("apply schema v5 (%q): %w", step, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// SettingAllowedOrigins is the site setting holding the operator's origin
// allowlist: the browser addresses a deployment may post mutations from, in
// addition to the one the browser is actually on.
//
// It is a *setting* rather than only configuration because the address a
// deployment is reachable at is a fact about the deployment, not a secret and not
// code (ARCHITECTURE.md §27). It lives here rather than in the auth package so
// that the writer (the settings API) and the reader (the CSRF check) share one
// name for one row.
const SettingAllowedOrigins = "allowedOrigins"

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// WithTx runs fn inside a transaction, rolling back on error or panic.
func (db *DB) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Now returns the canonical timestamp format used by every table.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }
