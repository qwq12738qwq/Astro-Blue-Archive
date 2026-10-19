package store

// GitHub account binding: the identity, the credential and the authorization
// transaction.
//
// The tables hold runtime identity and credential metadata — never blog content.
// A GitHub identity says *which* external account a CMS administrator proved they
// control; the credential says *what* this CMS may do with that account, and it is
// stored encrypted, so a stolen database yields neither a token nor a usable key.
//
// There is deliberately no `posts`-shaped table here and no title/body/description
// column, for the same reason the whole schema has none (ARCHITECTURE.md §2/§36).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrGitHubIdentityTaken is returned when a GitHub account is already bound to a
// different CMS administrator.
//
// The error is coarse on purpose: it names the conflict, never *whose* account it
// is, so a caller cannot use it to discover another administrator's identity.
var ErrGitHubIdentityTaken = errors.New("github identity already bound")

// GitHubIdentityRecord is one bound GitHub account.
//
// `GitHubUserID` is GitHub's immutable numeric id and is the identity's real key:
// a username is a label GitHub lets its owner change, and a binding keyed on it
// would silently detach the day someone renamed their account.
type GitHubIdentityRecord struct {
	ID           int64
	AdminID      int64
	GitHubUserID int64
	Login        string
	Name         string
	AvatarURL    string
	CreatedAt    string
	UpdatedAt    string
}

// GitHubCredentialRecord is one identity's OAuth credential, encrypted at rest.
//
// `AccessTokenCT` and `RefreshTokenCT` are ciphertext blobs produced by the
// credential encryption in internal/github — never a token. `RefreshTokenCT` is
// nullable because a scheme without refresh tokens (an OAuth App) simply leaves it
// empty rather than inventing a value.
type GitHubCredentialRecord struct {
	IdentityID     int64
	AccessTokenCT  string
	RefreshTokenCT sql.NullString
	TokenType      string
	Scope          string
	ExpiresAt      sql.NullString
	CreatedAt      string
	UpdatedAt      string
}

const githubIdentityColumns = `id, admin_id, github_user_id, login, name, avatar_url, created_at, updated_at`

func scanGitHubIdentity(scan func(...any) error) (GitHubIdentityRecord, error) {
	var r GitHubIdentityRecord
	err := scan(&r.ID, &r.AdminID, &r.GitHubUserID, &r.Login, &r.Name, &r.AvatarURL, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

const githubCredentialColumns = `identity_id, access_token_ct, refresh_token_ct, token_type, scope, expires_at, created_at, updated_at`

func scanGitHubCredential(scan func(...any) error) (GitHubCredentialRecord, error) {
	var r GitHubCredentialRecord
	err := scan(&r.IdentityID, &r.AccessTokenCT, &r.RefreshTokenCT, &r.TokenType,
		&r.Scope, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// FindGitHubIdentityByUserID returns the binding for a GitHub account, or
// (nil, nil) when that account is not bound to anyone.
func (db *DB) FindGitHubIdentityByUserID(ctx context.Context, githubUserID int64) (*GitHubIdentityRecord, error) {
	record, err := scanGitHubIdentity(db.QueryRowContext(ctx,
		`SELECT `+githubIdentityColumns+` FROM github_identity WHERE github_user_id = ?`,
		githubUserID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// GetGitHubIdentity returns one binding, or ErrNotFound.
func (db *DB) GetGitHubIdentity(ctx context.Context, id int64) (GitHubIdentityRecord, error) {
	record, err := scanGitHubIdentity(db.QueryRowContext(ctx,
		`SELECT `+githubIdentityColumns+` FROM github_identity WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubIdentityRecord{}, ErrNotFound
	}
	return record, err
}

// ListGitHubIdentitiesByAdmin returns one administrator's bindings, newest first.
func (db *DB) ListGitHubIdentitiesByAdmin(ctx context.Context, adminID int64) ([]GitHubIdentityRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+githubIdentityColumns+` FROM github_identity WHERE admin_id = ? ORDER BY id DESC`,
		adminID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []GitHubIdentityRecord{}
	for rows.Next() {
		record, err := scanGitHubIdentity(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// CountGitHubIdentitiesByAdmin is the binding count the admin list shows, so the
// list can report it without fetching each binding.
func (db *DB) CountGitHubIdentitiesByAdmin(ctx context.Context, adminID int64) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM github_identity WHERE admin_id = ?`, adminID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// InsertGitHubIdentity records a new binding.
//
// The caller is expected to have checked FindGitHubIdentityByUserID first; the
// UNIQUE constraint on github_user_id is still what makes the check authoritative,
// and a violation is mapped onto ErrGitHubIdentityTaken rather than surfaced as a
// driver-specific string.
func (db *DB) InsertGitHubIdentity(ctx context.Context, r GitHubIdentityRecord) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO github_identity (admin_id, github_user_id, login, name, avatar_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.AdminID, r.GitHubUserID, r.Login, r.Name, r.AvatarURL, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "github_identity.github_user_id") {
			return 0, ErrGitHubIdentityTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateGitHubIdentity refreshes the display fields of an existing binding.
//
// Only display fields: the immutable id and the owning administrator never move,
// so a GitHub rename updates the label and cannot re-point the binding.
func (db *DB) UpdateGitHubIdentity(ctx context.Context, r GitHubIdentityRecord) error {
	res, err := db.ExecContext(ctx,
		`UPDATE github_identity SET login = ?, name = ?, avatar_url = ?, updated_at = ? WHERE id = ?`,
		r.Login, r.Name, r.AvatarURL, r.UpdatedAt, r.ID)
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

// DeleteGitHubIdentity removes a binding and its credential together.
//
// One transaction: a credential row without its identity is a token nothing can
// reach, which is worse than useless. The local Git backup repository is not
// touched — unbinding a GitHub account says nothing about the local history.
func (db *DB) DeleteGitHubIdentity(ctx context.Context, id int64) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM github_credential WHERE identity_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM github_identity WHERE id = ?`, id)
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
	})
}

// GetGitHubCredential returns one identity's encrypted credential, or ErrNotFound.
func (db *DB) GetGitHubCredential(ctx context.Context, identityID int64) (GitHubCredentialRecord, error) {
	record, err := scanGitHubCredential(db.QueryRowContext(ctx,
		`SELECT `+githubCredentialColumns+` FROM github_credential WHERE identity_id = ?`,
		identityID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubCredentialRecord{}, ErrNotFound
	}
	return record, err
}

// UpsertGitHubCredential stores (or replaces) the encrypted credential for one
// identity. One identity holds one live credential: a re-authorization replaces
// it rather than accumulating a second row beside it.
func (db *DB) UpsertGitHubCredential(ctx context.Context, r GitHubCredentialRecord) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(ctx,
		`INSERT INTO github_credential
		   (identity_id, access_token_ct, refresh_token_ct, token_type, scope, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (identity_id) DO UPDATE SET
		   access_token_ct  = excluded.access_token_ct,
		   refresh_token_ct = excluded.refresh_token_ct,
		   token_type       = excluded.token_type,
		   scope            = excluded.scope,
		   expires_at       = excluded.expires_at,
		   updated_at       = excluded.updated_at`,
		r.IdentityID, r.AccessTokenCT, r.RefreshTokenCT, r.TokenType, r.Scope, r.ExpiresAt, now, now)
	return err
}

// ---------------------------------------------------------------------------
// The OAuth authorization transaction
// ---------------------------------------------------------------------------

// OAuthStateRecord is one start→callback transaction.
//
// `CodeVerifier` is the PKCE verifier, which is a server-side secret: it is
// written here, read once by the token exchange, and never leaves the process in
// a response, a URL or a log line.
type OAuthStateRecord struct {
	State        string
	AdminID      int64
	CodeVerifier string
	CreatedAt    string
	ExpiresAt    string
	ConsumedAt   sql.NullString
}

// InsertOAuthState starts a transaction. The state is generated by the caller
// (crypto/rand) so this function cannot be the reason two transactions collide.
func (db *DB) InsertOAuthState(ctx context.Context, r OAuthStateRecord) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO oauth_state (state, admin_id, code_verifier, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		r.State, r.AdminID, r.CodeVerifier, r.CreatedAt, r.ExpiresAt)
	return err
}

// GetOAuthState returns one transaction, or ErrNotFound. Used for diagnostics:
// the consume path below is the authoritative one.
func (db *DB) GetOAuthState(ctx context.Context, state string) (OAuthStateRecord, error) {
	var r OAuthStateRecord
	err := db.QueryRowContext(ctx,
		`SELECT state, admin_id, code_verifier, created_at, expires_at, consumed_at
		   FROM oauth_state WHERE state = ?`, state).
		Scan(&r.State, &r.AdminID, &r.CodeVerifier, &r.CreatedAt, &r.ExpiresAt, &r.ConsumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthStateRecord{}, ErrNotFound
	}
	return r, err
}

// ConsumeOAuthState marks a transaction used and returns it, or ErrNotFound when
// it does not exist, has already been consumed, or has expired.
//
// The UPDATE and the read are one statement, so two callbacks presenting the same
// state cannot both win: the second sees zero updated rows and gets the same
// refusal the first would have given an unknown state. That is what makes the state
// single-use rather than merely intended to be.
func (db *DB) ConsumeOAuthState(ctx context.Context, state, now string) (OAuthStateRecord, error) {
	var r OAuthStateRecord
	err := db.QueryRowContext(ctx,
		`UPDATE oauth_state SET consumed_at = ?
		  WHERE state = ? AND consumed_at IS NULL AND expires_at > ?
		  RETURNING admin_id, code_verifier, created_at, expires_at, consumed_at`,
		now, state, now).
		Scan(&r.AdminID, &r.CodeVerifier, &r.CreatedAt, &r.ExpiresAt, &r.ConsumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthStateRecord{}, ErrNotFound
	}
	r.State = state
	return r, err
}

// PurgeOAuthStates removes dead transactions. Expired rows are unreachable, and a
// consumed row is never needed again, so both are safe to drop.
func (db *DB) PurgeOAuthStates(ctx context.Context, now string) (int64, error) {
	res, err := db.ExecContext(ctx,
		`DELETE FROM oauth_state WHERE expires_at < ? OR consumed_at IS NOT NULL`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
