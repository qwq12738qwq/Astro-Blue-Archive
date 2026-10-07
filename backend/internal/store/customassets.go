package store

// The custom-asset metadata index.
//
// ARCHITECTURE.md ID-33: this is an index over `content/system/css/` and
// `content/system/js/`, not a store. The files are the truth; every field below can
// be recomputed by walking those two directories, and `ReconcileCustomAssets` does
// exactly that on every read. Losing this table costs one rescan and no user data.
//
// There is deliberately no `content` column. Storing the body here would turn the
// filesystem into a cache of the database, which is the inversion the whole
// architecture exists to prevent.

import (
	"context"
	"database/sql"
)

// CustomAssetRecord is one managed custom-asset file's metadata.
//
// `CreatedAt` and `UpdatedAt` are the only two fields the filesystem cannot supply:
// a file's mtime says when it was last written, not when an admin first created it.
type CustomAssetRecord struct {
	Type      string
	Filename  string
	Enabled   bool
	SizeBytes int64
	Checksum  string
	CreatedAt string
	UpdatedAt string
}

// customAssetColumns is the fixed projection, so List and Get cannot drift apart.
const customAssetColumns = `type, filename, enabled, size_bytes, checksum, created_at, updated_at`

func scanCustomAsset(scan func(...any) error) (CustomAssetRecord, error) {
	var r CustomAssetRecord
	var enabled int
	err := scan(&r.Type, &r.Filename, &enabled, &r.SizeBytes, &r.Checksum, &r.CreatedAt, &r.UpdatedAt)
	r.Enabled = enabled != 0
	return r, err
}

// ListCustomAssets returns every recorded asset of one type, in the canonical order.
//
// ARCHITECTURE.md ID-35: filename ascending. The three-digit prefix is the order and
// the filename carries it, so this is the same total order the public aggregator
// uses — one contract, two readers.
func (db *DB) ListCustomAssets(ctx context.Context, assetType string) ([]CustomAssetRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+customAssetColumns+` FROM custom_asset WHERE type = ? ORDER BY filename ASC`,
		assetType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// §17: one connection, so the rows must be fully drained before this function
	// returns and the caller may run another query.
	out := []CustomAssetRecord{}
	for rows.Next() {
		record, err := scanCustomAsset(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// GetCustomAsset returns one record, or ErrNotFound.
func (db *DB) GetCustomAsset(ctx context.Context, assetType, filename string) (CustomAssetRecord, error) {
	record, err := scanCustomAsset(db.QueryRowContext(ctx,
		`SELECT `+customAssetColumns+` FROM custom_asset WHERE type = ? AND filename = ?`,
		assetType, filename).Scan)
	if err == sql.ErrNoRows {
		return CustomAssetRecord{}, ErrNotFound
	}
	return record, err
}

// UpsertCustomAsset records what the filesystem says about one asset.
//
// `created_at` is inserted once and never updated: it is the only value the tree
// cannot reproduce, and rewriting it on every save would make it mean "last touched"
// instead of "created".
func (db *DB) UpsertCustomAsset(ctx context.Context, r CustomAssetRecord) error {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO custom_asset (type, filename, enabled, size_bytes, checksum, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (type, filename) DO UPDATE SET
		   enabled    = excluded.enabled,
		   size_bytes = excluded.size_bytes,
		   checksum   = excluded.checksum,
		   updated_at = excluded.updated_at`,
		r.Type, r.Filename, enabled, r.SizeBytes, r.Checksum, r.CreatedAt, r.UpdatedAt)
	return err
}

// RenameCustomAsset moves a record's identity, keeping its created_at.
//
// The file was already renamed on disk; this only follows it. A rename that has no
// row yet is not an error — an asset created by hand has no row either.
func (db *DB) RenameCustomAsset(ctx context.Context, assetType, from, to string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE custom_asset SET filename = ?, updated_at = ? WHERE type = ? AND filename = ?`,
		to, Now(), assetType, from)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		// No row to move. Return the not-found sentinel so the caller can insert one.
		return ErrNotFound
	}
	return nil
}

// DeleteCustomAsset removes a record. A missing row is not an error: the file is
// gone, which is the state the caller asked for.
func (db *DB) DeleteCustomAsset(ctx context.Context, assetType, filename string) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM custom_asset WHERE type = ? AND filename = ?`, assetType, filename)
	return err
}
