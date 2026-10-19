package store

import (
	"path/filepath"
	"testing"
)

// What the four-column shape of the previous schema was. The upgrade must turn
// this one row into an active *owner*, and it must do so without losing the
// sessions the account already had.
const (
	v4AdminDDL = `CREATE TABLE admin_user (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
)`
	v4SessionDDL = `CREATE TABLE session (
  id           TEXT PRIMARY KEY,
  token_hash   BLOB NOT NULL UNIQUE,
  csrf_secret  BLOB NOT NULL,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  ip_hash      BLOB,
  user_agent   TEXT
)`
)

// openV4 builds a database that looks like the schema this change upgrades
// from: one administrator, one live session, nothing else. It is built with the
// current code's other tables (they are unchanged by the upgrade) and then
// stripped back to the four-column `admin_user` and the session without an
// `admin_id`, which is exactly what an older binary wrote.
func openV4(t *testing.T) (path string, tokenHash string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "blog.db")

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Deterministic placeholder hash: the migration must not care what it is.
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i)
	}
	if _, err := db.Exec(
		`INSERT INTO session (id, token_hash, csrf_secret, created_at, expires_at, last_seen_at, ip_hash, user_agent)
		 VALUES ('s1', ?, X'00', '2026-01-01T00:00:00Z', '2027-01-01T00:00:00Z', '2026-01-01T00:00:00Z', NULL, 'agent')`,
		hash,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO admin_user (username, password_hash, role, status, created_at, updated_at)
		 VALUES ('owner', 'argon2id-placeholder-hash', 'owner', 'active',
		         '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert admin: %v", err)
	}

	statements := []string{
		`DROP TABLE github_identity`,
		`DROP TABLE github_credential`,
		`DROP TABLE oauth_state`,
		`DROP INDEX IF EXISTS idx_session_admin`,
		`ALTER TABLE admin_user RENAME TO admin_user_v5`,
		v4AdminDDL,
		`INSERT INTO admin_user (id, username, password_hash, created_at, updated_at)
		   SELECT id, username, password_hash, created_at, updated_at FROM admin_user_v5`,
		`DROP TABLE admin_user_v5`,
		`ALTER TABLE session RENAME TO session_v5`,
		v4SessionDDL,
		`INSERT INTO session (id, token_hash, csrf_secret, created_at, expires_at, last_seen_at, ip_hash, user_agent)
		   SELECT id, token_hash, csrf_secret, created_at, expires_at, last_seen_at, ip_hash, user_agent FROM session_v5`,
		`DROP TABLE session_v5`,
		`PRAGMA user_version = 4`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("build v4 schema (%q): %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path, string(hash)
}

func columns(t *testing.T, db *DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrationV5KeepsTheExistingAdministratorAsOwner(t *testing.T) {
	path, _ := openV4(t)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()

	var (
		username string
		role     string
		status   string
	)
	if err := db.QueryRow(
		`SELECT username, role, status FROM admin_user WHERE id = 1`).Scan(&username, &role, &status); err != nil {
		t.Fatalf("the administrator did not survive the upgrade: %v", err)
	}
	if username != "owner" {
		t.Errorf("username = %q", username)
	}
	// The one account an older deployment had becomes the first *owner*, which is
	// what lets it create, disable and reset the others.
	if role != "owner" {
		t.Errorf("role = %q, want owner", role)
	}
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
}

func TestMigrationV5GivesAnExistingSessionItsAdministrator(t *testing.T) {
	path, hash := openV4(t)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()

	if !columns(t, db, "session")["admin_id"] {
		t.Fatal("session has no admin_id after the upgrade")
	}
	// The pre-existing session must belong to the pre-existing administrator, or
	// the first thing after an upgrade is being signed out.
	var adminID int64
	if err := db.QueryRow(
		`SELECT admin_id FROM session WHERE token_hash = ?`, []byte(hash)).Scan(&adminID); err != nil {
		t.Fatalf("the session did not survive the upgrade: %v", err)
	}
	if adminID != 1 {
		t.Errorf("session admin_id = %d, want 1", adminID)
	}
}

func TestMigrationV5CreatesTheBindingTables(t *testing.T) {
	path, _ := openV4(t)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, table := range []string{"github_identity", "github_credential", "oauth_state"} {
		if len(columns(t, db, table)) == 0 {
			t.Errorf("%s was not created", table)
		}
	}
	// The two rules the model rests on: one GitHub account binds once, and the
	// credential is not a column of the identity.
	var unique int
	if err := db.QueryRow(
		`SELECT count(*) FROM pragma_index_list('github_identity') WHERE "unique" = 1`).Scan(&unique); err != nil {
		t.Fatal(err)
	}
	if unique == 0 {
		t.Error("github_identity has no unique index, so two administrators could bind one account")
	}
	for column := range columns(t, db, "github_identity") {
		if column == "access_token_ct" || column == "token" {
			t.Error("github_identity carries a credential column; identity and credential are separate tables")
		}
	}
}

func TestMigrationV5IsIdempotent(t *testing.T) {
	path, _ := openV4(t)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("first reopen: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS probe(x)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A second Open must be a no-op: the schema is already at this version, and
	// re-running the rebuild would drop the accounts created since.
	again, err := Open(path)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer func() { _ = again.Close() }()
	if _, err := again.Exec(
		`INSERT INTO admin_user (username, password_hash, role, status, created_at, updated_at)
		 VALUES ('editor', 'hash', 'admin', 'active', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert after a second open: %v", err)
	}
	var count int
	if err := again.QueryRow(`SELECT count(*) FROM admin_user`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("admins = %d, want 2: the second Open rebuilt the table", count)
	}
}
