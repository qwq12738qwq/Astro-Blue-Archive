package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Admin is the single administrator account (ARCHITECTURE.md §7, D7).
type Admin struct {
	Username     string
	PasswordHash string
}

// AdminStore reads and writes the admin row.
type AdminStore struct{ db dbLike }

// dbLike is the subset of *store.DB this file needs, which keeps the tests
// free of a full database fixture.
type dbLike interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// NewAdminStore builds an AdminStore.
func NewAdminStore(db dbLike) *AdminStore { return &AdminStore{db: db} }

// ErrNotFound is returned when no administrator exists yet.
var ErrNotFound = errors.New("admin not found")

// Get returns the single administrator.
func (a *AdminStore) Get(ctx context.Context) (*Admin, error) {
	var admin Admin
	err := a.db.QueryRowContext(ctx,
		`SELECT username, password_hash FROM admin_user WHERE id = 1`).
		Scan(&admin.Username, &admin.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &admin, nil
}

// Count returns the number of admin rows, which is always 0 or 1.
func (a *AdminStore) Count(ctx context.Context) (int, error) {
	var n int
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM admin_user`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Create inserts the administrator. The schema CHECK (id = 1) makes a second
// row impossible, so a duplicate means the account already exists.
func (a *AdminStore) Create(ctx context.Context, username, passwordHash string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO admin_user (id, username, password_hash, created_at, updated_at)
		 VALUES (1, ?, ?, ?, ?)`,
		username, passwordHash, now, now)
	return err
}

// UpdatePassword replaces the stored hash.
func (a *AdminStore) UpdatePassword(ctx context.Context, passwordHash string) error {
	res, err := a.db.ExecContext(ctx,
		`UPDATE admin_user SET password_hash = ?, updated_at = ? WHERE id = 1`,
		passwordHash, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
