// Command set-password changes the admin password directly in the database.
//
// # Why this exists at all
//
// The admin password is stored as an argon2id PHC string and is never recoverable —
// which is correct, and is also a trap. A single-admin blog has exactly one account,
// and if its password is forgotten then the deployment has no owner and no way to get
// one back: the only remaining move is to delete the database, and that takes the
// comments, the media metadata and every session with it. Deleting a row to escape a
// forgotten password is not an acceptable answer for software somebody runs on their
// own machine.
//
// So this is a maintenance command, not an endpoint. It is deliberately NOT an HTTP
// route: it has to work when the thing you forgot is the credential that gets you into
// the web UI, and exposing a "reset any password" handler behind a login would be a
// new attack surface for a problem a local binary solves completely.
//
// # What it does NOT do
//
// It does not implement its own hashing. It calls auth.HashPassword, so there is one
// argon2id implementation in this repository and the CLI cannot drift away from the
// parameters the login path verifies against.
//
// The password is read from a terminal with echo disabled, or from stdin when piped.
// It is never accepted as a command-line argument: argv is visible to every process on
// the machine and is kept in shell history, which is the usual way a password ends up
// in a log file.
//
// Existing sessions are revoked. Changing the password and leaving stolen session
// cookies valid would make this command worse than useless.
//
// ARCHITECTURE.md §16 is respected: an audit row records that a password changed, and
// never records the password.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"blogcms/internal/auth"
	"blogcms/internal/config"
	"blogcms/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "set-password:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		username = flag.String("user", "", "account to update (required)")
		dryRun   = flag.Bool("check", false, "verify the new password without writing it")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: set-password -user <name>

Reads the new password from the terminal without echoing it, or from stdin when
piped. Passing it as an argument is not supported: argv is world-readable and
shell history is forever.

`)
		flag.PrintDefaults()
	}
	flag.Parse()

	name := strings.TrimSpace(*username)
	if name == "" {
		flag.Usage()
		return errors.New("-user is required")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}
	if password == "" {
		return auth.ErrEmptyPassword
	}

	// Hash first. A password the KDF refuses must not reach the database layer, and
	// hashing before opening the connection means a bad password costs no lock.
	encoded, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	if *dryRun {
		// Proves the value the operator just typed actually hashes and verifies, which
		// is worth being able to check before committing to it.
		if err := auth.VerifyPassword(password, encoded); err != nil {
			return fmt.Errorf("the password did not verify against its own hash: %w", err)
		}
		fmt.Println("password accepted; nothing written (-check)")
		return nil
	}

	var id string
	err = db.QueryRowContext(ctx, `SELECT id FROM admin_user WHERE username = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no account named %q; create it through the setup form first", name)
	}
	if err != nil {
		return err
	}

	// One transaction: the new hash and the session revocation either both land or
	// neither does. Revoking first inside a transaction that then fails would log the
	// admin out for nothing; revoking after a successful write would leave a window.
	if err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE admin_user SET password_hash = ? WHERE id = ?`, encoded, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM session`)
		return err
	}); err != nil {
		return err
	}

	// store.Now(), not time.Now(): content_event.created_at is TEXT with no default,
	// and the one timestamp format in this repository belongs to the store.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO content_event (kind, ref, actor, created_at)
		 VALUES ('admin.password_changed', ?, ?, ?)`,
		name, name, store.Now()); err != nil {
		// The password is already changed at this point. Losing the audit row is not
		// worth reporting as a failure of the command, but it must not be silent.
		fmt.Fprintln(os.Stderr, "set-password: warning: could not write the audit row:", err)
	}

	fmt.Printf("password updated for %q; all sessions revoked\n", name)
	return nil
}

// readPassword takes a password from the terminal with echo off, or from stdin when
// stdin is a pipe.
//
// The echo-off path uses TCGETS/TCSETS through golang.org/x/sys directly rather than
// pulling in golang.org/x/term, which is a thin wrapper over exactly this and is not
// in the module cache. Adding a dependency for two ioctls would also be the kind of
// change that needs a second `go get` in an offline build.
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())

	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		// Not a terminal: stdin is a pipe or a file. Read the line and trust the
		// caller to have chosen a channel that does not log.
		return readLine(os.Stdin)
	}

	muted := *original
	muted.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &muted); err != nil {
		return "", fmt.Errorf("cannot disable terminal echo: %w", err)
	}
	// Restored before the function returns on every path, so a failed read cannot
	// leave the operator's shell with echo switched off.
	defer func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, original)
	}()

	fmt.Fprint(os.Stderr, "new password: ")
	value, err := readLine(os.Stdin)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}

	return value, nil
}

func readLine(r *os.File) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
