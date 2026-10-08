package store

// The Markdown style template metadata index.
//
// ARCHITECTURE.md §34: this is an index over
// `content/system/markdown/`, not a store. The files are the
// truth; every field below can be recomputed by walking the
// directory, and the API handlers do exactly that on every read.
// Losing this table costs one rescan and no user data.
//
// There is deliberately no `content` column. Storing the CSS
// here would turn the filesystem into a cache of the database,
// which is the inversion the whole architecture exists to
// prevent.

import (
	"context"
	"database/sql"
)

// MarkdownTemplateRecord is one Markdown style template file's
// metadata.
//
// `CreatedAt` and `UpdatedAt` are the only two fields the
// filesystem cannot supply: a file's mtime says when it was last
// written, not when an admin first created it.
type MarkdownTemplateRecord struct {
	Filename  string
	Enabled   bool
	SizeBytes int64
	Checksum  string
	CreatedAt string
	UpdatedAt string
}

// markdownTemplateColumns is the fixed projection, so List and
// Get cannot drift apart.
const markdownTemplateColumns = `filename, enabled, size_bytes, checksum, created_at, updated_at`

func scanMarkdownTemplate(scan func(...any) error) (MarkdownTemplateRecord, error) {
	var r MarkdownTemplateRecord
	var enabled int
	err := scan(&r.Filename, &enabled, &r.SizeBytes, &r.Checksum, &r.CreatedAt, &r.UpdatedAt)
	r.Enabled = enabled != 0
	return r, err
}

// ListMarkdownTemplates returns every recorded template, in the
// canonical order.
//
// ARCHITECTURE.md ID-35: filename ascending. The three-digit
// prefix is the order and the filename carries it, so this is
// the same total order the public aggregator uses — one
// contract, two readers.
func (db *DB) ListMarkdownTemplates(ctx context.Context) ([]MarkdownTemplateRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+markdownTemplateColumns+` FROM markdown_asset ORDER BY filename ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// §17: one connection, so the rows must be fully drained
	// before this function returns and the caller may run
	// another query.
	out := []MarkdownTemplateRecord{}
	for rows.Next() {
		record, err := scanMarkdownTemplate(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// GetMarkdownTemplate returns one record, or ErrNotFound.
func (db *DB) GetMarkdownTemplate(ctx context.Context, filename string) (MarkdownTemplateRecord, error) {
	record, err := scanMarkdownTemplate(db.QueryRowContext(ctx,
		`SELECT `+markdownTemplateColumns+` FROM markdown_asset WHERE filename = ?`,
		filename).Scan)
	if err == sql.ErrNoRows {
		return MarkdownTemplateRecord{}, ErrNotFound
	}
	return record, err
}

// UpsertMarkdownTemplate records what the filesystem says about
// one template.
//
// `created_at` is inserted once and never updated: it is the
// only value the tree cannot reproduce, and rewriting it on
// every save would make it mean "last touched" instead of
// "created".
func (db *DB) UpsertMarkdownTemplate(ctx context.Context, r MarkdownTemplateRecord) error {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO markdown_asset (filename, enabled, size_bytes, checksum, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (filename) DO UPDATE SET
		   enabled    = excluded.enabled,
		   size_bytes = excluded.size_bytes,
		   checksum   = excluded.checksum,
		   updated_at = excluded.updated_at`,
		r.Filename, enabled, r.SizeBytes, r.Checksum, r.CreatedAt, r.UpdatedAt)
	return err
}

// RenameMarkdownTemplate moves a record's identity, keeping its
// created_at.
//
// The file was already renamed on disk; this only follows it. A
// rename that has no row yet is not an error — a template
// created by hand has no row either.
func (db *DB) RenameMarkdownTemplate(ctx context.Context, from, to string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE markdown_asset SET filename = ?, updated_at = ? WHERE filename = ?`,
		to, Now(), from)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// No row to move. Return the not-found sentinel so the
		// caller can insert one.
		return ErrNotFound
	}
	return nil
}

// DeleteMarkdownTemplate removes a record. A missing row is not
// an error: the file is gone, which is the state the caller
// asked for.
func (db *DB) DeleteMarkdownTemplate(ctx context.Context, filename string) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM markdown_asset WHERE filename = ?`, filename)
	return err
}
