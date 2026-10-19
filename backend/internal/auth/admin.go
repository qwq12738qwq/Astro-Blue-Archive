package auth

// Administrator accounts.
//
// ARCHITECTURE.md §7/D7 used to describe a *single* administrator; that was true
// of the schema (CHECK (id = 1)) and is no longer. A CMS administrator is local
// identity: a username, an argon2id hash, a role and a live/disabled state. It is
// not a GitHub account and not a Git credential — those are two further kinds of
// fact, in two further tables (store.GitHubIdentityRecord and
// store.GitHubCredentialRecord), and nothing here reaches them.
//
// Two roles, and deliberately only two:
//
//   - owner: the account the first-run setup created. Creates, disables and resets
//     other administrators, and binds or unbinds any administrator's GitHub
//     accounts.
//   - admin: does everything an administrator does — content, media, settings,
//     local backup — plus its own password and its own GitHub bindings.
//
// There is no permission table and no per-route policy language. A third role, a
// grouping, or a delegation chain would be a design for a problem this does not
// have, and the checks that exist are enforced in the API, not in the UI.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// RoleOwner may manage other administrators. RoleAdmin may not.
const (
	RoleOwner = "owner"
	RoleAdmin = "admin"
)

// StatusActive is a working account; StatusDisabled may not sign in and has no
// live session.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Admin is one administrator account.
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	Status       string
	CreatedAt    string
	UpdatedAt    string
	LastLoginAt  string
}

// AdminStore reads and writes administrator rows.
type AdminStore struct{ db dbLike }

// dbLike is the subset of *store.DB this file needs, which keeps the tests
// free of a full database fixture.
type dbLike interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// NewAdminStore builds an AdminStore.
func NewAdminStore(db dbLike) *AdminStore { return &AdminStore{db: db} }

// ErrNotFound is returned when no administrator exists with the given key.
var ErrNotFound = errors.New("admin not found")

// ErrDuplicateUsername is returned when a username is taken.
var ErrDuplicateUsername = errors.New("username already exists")

const adminColumns = `id, username, password_hash, role, status, created_at, updated_at, last_login_at`

func scanAdmin(scan func(...any) error) (Admin, error) {
	var a Admin
	var lastLogin sql.NullString
	err := scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.Status, &a.CreatedAt, &a.UpdatedAt, &lastLogin)
	a.LastLoginAt = lastLogin.String
	return a, err
}

// Get returns one administrator by id.
func (a *AdminStore) Get(ctx context.Context, id int64) (*Admin, error) {
	admin, err := scanAdmin(a.db.QueryRowContext(ctx,
		`SELECT `+adminColumns+` FROM admin_user WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &admin, nil
}

// GetByUsername returns one administrator by login name, or ErrNotFound.
//
// It reports the account without regard to status: the caller decides what a
// disabled account means, so that a login for a disabled account costs the same as
// a login for a wrong password (§7: no distinguishable responses).
func (a *AdminStore) GetByUsername(ctx context.Context, username string) (*Admin, error) {
	admin, err := scanAdmin(a.db.QueryRowContext(ctx,
		`SELECT `+adminColumns+` FROM admin_user WHERE username = ?`, username).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &admin, nil
}

// List returns every administrator, ordered by id: the first-run account is first.
func (a *AdminStore) List(ctx context.Context) ([]Admin, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT `+adminColumns+` FROM admin_user ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []Admin{}
	for rows.Next() {
		admin, err := scanAdmin(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, admin)
	}
	return out, rows.Err()
}

// Count returns the number of administrators.
func (a *AdminStore) Count(ctx context.Context) (int, error) {
	var n int
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM admin_user`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountActiveOwners counts the owners who can still sign in.
//
// This is the number the lockout guard reads: disabling the last active owner
// would leave nobody who can create, disable or reset another administrator.
func (a *AdminStore) CountActiveOwners(ctx context.Context) (int, error) {
	var n int
	if err := a.db.QueryRowContext(ctx,
		`SELECT count(*) FROM admin_user WHERE role = ? AND status = ?`,
		RoleOwner, StatusActive).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountActive counts the accounts that can still sign in, which is the other half
// of the lockout guard: an installation whose every account is disabled has no way
// back in either.
func (a *AdminStore) CountActive(ctx context.Context) (int, error) {
	var n int
	err := a.db.QueryRowContext(ctx,
		`SELECT count(*) FROM admin_user WHERE status = ?`, StatusActive).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Create inserts an administrator and returns it.
//
// A duplicate username is mapped onto ErrDuplicateUsername so the caller can answer
// a 409 without parsing a driver string.
func (a *AdminStore) Create(ctx context.Context, username, passwordHash, role string) (*Admin, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := a.db.ExecContext(ctx,
		`INSERT INTO admin_user (username, password_hash, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		username, passwordHash, role, StatusActive, now, now)
	if err != nil {
		if isUniqueUsername(err) {
			return nil, ErrDuplicateUsername
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return a.Get(ctx, id)
}

// UpdatePassword replaces the stored hash.
func (a *AdminStore) UpdatePassword(ctx context.Context, id int64, passwordHash string) error {
	res, err := a.db.ExecContext(ctx,
		`UPDATE admin_user SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, time.Now().UTC().Format(time.RFC3339), id)
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

// SetStatus enables or disables an administrator.
func (a *AdminStore) SetStatus(ctx context.Context, id int64, status string) error {
	res, err := a.db.ExecContext(ctx,
		`UPDATE admin_user SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339), id)
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

// RecordLogin stamps the last successful sign-in. A failure here is not worth
// failing the login for, so callers ignore the error.
func (a *AdminStore) RecordLogin(ctx context.Context, id int64) error {
	_, err := a.db.ExecContext(ctx,
		`UPDATE admin_user SET last_login_at = ?, updated_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// isUniqueUsername reports whether an error is the username unique constraint.
func isUniqueUsername(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "admin_user.username") &&
		strings.Contains(err.Error(), "UNIQUE")
}
