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
const schemaVersion = 4

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
