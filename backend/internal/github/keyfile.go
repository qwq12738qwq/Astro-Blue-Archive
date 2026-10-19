package github

// The credential encryption key: a file the backend owns.
//
// ARCHITECTURE.md ID-73. A deployment must not have to create a file, generate a
// secret, or know a path to make GitHub account binding work — and it must not be
// able to *lose* its bindings by accident either. So the key lives at a fixed path
// inside `DATA_ROOT`, is created once with `crypto/rand`, and is never
// regenerated when it is missing.
//
// The three rules, each because the opposite is worse than it sounds:
//
//   - **The key is not in the database.** A database backup, `VACUUM INTO`
//     included, therefore carries ciphertext the backup cannot open. `DATA_ROOT`
//     holds both, but the backup script takes the *database*, not the directory
//     (ARCHITECTURE.md §14): leaking a backup must not leak a credential.
//   - **Missing means missing.** If the file is gone and bindings exist, a new key
//     is generated *nothing*: existing ciphertext becomes permanently
//     unreadable, and every binding turns into a row that claims to have a
//     credential it cannot use. The backend refuses instead, says why, and the
//     operator unbinds and re-binds. The file is only created when there is
//     nothing that could be orphaned.
//   - **Creation is atomic and private.** A temporary file, `0600`, then a
//     rename: a crash mid-write cannot leave a half key that a later start would
//     happily use, and another process on the machine cannot read it.
//
// What the key protects is a GitHub *access token*: a bearer credential for
// somebody else's account. That is why the KDF (argon2id) and the cipher
// (AES-256-GCM) are the same ones the rest of this repository already uses, and
// why none of them is weakened to make the operator's life easier.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CredentialKeyFileName is the key file's name inside DATA_ROOT.
//
// The name is fixed rather than configurable: a second spelling would be a second
// file, and a deployment that had two would have two keys — one of which opens
// nothing. The path is discovered from DATA_ROOT, which the deployment already
// has to configure for the database.
const CredentialKeyFileName = "github-credential.key"

// CredentialKeyBytes is the entropy of the key file.
//
// Thirty-two bytes of crypto/rand is 256 bits, which is the AES-256 key size and
// far beyond any derivation worth doing on top of it.
const CredentialKeyBytes = 32

// filePerm is the key file's mode: owner-only, never group or world readable.
const filePerm os.FileMode = 0o600

// Errors the key lifecycle returns. They are coarse on purpose — a caller
// distinguishes "missing" from "corrupt" to decide what to *say*, and never to
// decide whether to continue.
var (
	// ErrKeyMissing means the key file is absent while credentials exist. The one
	// state that must never be answered by generating a new key.
	ErrKeyMissing = errors.New(
		"the credential encryption key is missing while GitHub bindings exist")
	// ErrKeyCorrupt means the key file exists but is unusable. Also never
	// regenerated over: the file was there, so something removed or truncated it.
	ErrKeyCorrupt = errors.New("the credential encryption key is unusable")
)

// CredentialKeyPath is where the key lives for a deployment.
func CredentialKeyPath(dataRoot string) string {
	return filepath.Join(dataRoot, CredentialKeyFileName)
}

// KeyProvider answers "do any encrypted credentials exist?".
//
// It is a function rather than a store reference so this package stays about keys
// and knows nothing about the tables below it.
type KeyProvider func(ctx context.Context) (bool, error)

// LoadOrCreateCredentialKey returns the deployment's keyring.
//
// It creates the key file when, and only when, there is nothing that a new key
// could orphan. See the file comment: the three outcomes are (1) an existing key
// is read and used, (2) no key and no bindings means one is created, and (3) no
// key with bindings is an error that leaves the file alone.
//
// A read failure is returned rather than swallowed: the caller has to be able to
// say "binding is unavailable, and this is why", and an empty keyring would
// instead be answered by `Seal` refusing — which is the right last line of
// defence but a poor diagnostic.
func LoadOrCreateCredentialKey(
	ctx context.Context,
	dataRoot string,
	hasCredentials KeyProvider,
) (*Keyring, error) {
	path := CredentialKeyPath(dataRoot)

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		// The length of the file as stored. Not of its trimmed contents: this
		// file is generated here as exactly CredentialKeyBytes random bytes, and
		// crypto/rand can legitimately put a whitespace byte at either end. The
		// earlier trimmed check therefore refused a perfectly good key roughly one
		// time in twenty, and the deployment could not start at all.
		if len(raw) < minKeyMaterialBytes {
			return nil, fmt.Errorf("%w: %s is %d bytes", ErrKeyCorrupt, path, len(raw))
		}
		keyring, err := NewKeyring(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return keyring, nil

	case !errors.Is(err, os.ErrNotExist):
		// Present but unreadable — a permissions problem, most likely, and the
		// answer is an error rather than a second attempt at a different path.
		return nil, fmt.Errorf("read %s: %w", path, err)

	default:
		// Absent. Nothing is written until the caller's deployment is one where a
		// new key cannot orphan anything.
		if hasCredentials != nil {
			exists, err := hasCredentials(ctx)
			if err != nil {
				return nil, fmt.Errorf("check for existing credentials: %w", err)
			}
			if exists {
				return nil, fmt.Errorf("%w: %s would have to be created, and every binding would become unreadable",
					ErrKeyMissing, path)
			}
		}
		return createCredentialKey(path)
	}
}

// createCredentialKey writes a fresh key atomically.
func createCredentialKey(path string) (*Keyring, error) {
	material := make([]byte, CredentialKeyBytes)
	if _, err := rand.Read(material); err != nil {
		return nil, fmt.Errorf("generate key material: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+CredentialKeyFileName+"-*")
	if err != nil {
		return nil, fmt.Errorf("create key file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("set key file mode: %w", err)
	}
	if _, err := tmp.Write(material); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("flush key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close key file: %w", err)
	}
	// The rename is the commit point: a crash before it leaves no file at all,
	// which the next start answers by creating one (there is nothing to orphan).
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, fmt.Errorf("install key file: %w", err)
	}

	keyring, err := NewKeyring(material)
	if err != nil {
		return nil, err
	}
	return keyring, nil
}
