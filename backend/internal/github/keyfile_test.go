package github

// The credential encryption key: backend-owned, created once, never regenerated.
//
// The properties that matter are the ones an operator would otherwise discover
// by losing every binding at once: the file is private, a restart reuses it, and a
// missing key with bindings present is refused *without* writing a new file.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"blogcms/internal/store"
)

const testKeyPhrase = "created-once-never-again-0123456789ab"

// noCredentials is the answer for a binding-less deployment.
func noCredentials(context.Context) (bool, error) { return false, nil }

// withCredentials is the answer for a deployment that already has bindings.
func withCredentials(context.Context) (bool, error) { return true, nil }

func TestLoadOrCreateKeyCreatesItOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the file mode is a POSIX notion")
	}
	dir := t.TempDir()

	keyring, err := LoadOrCreateCredentialKey(context.Background(), dir, noCredentials)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	path := CredentialKeyPath(dir)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the key file was not created: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", st.Mode().Perm())
	}

	// A second load reads the same file, so a credential sealed by the first is
	// still openable after a restart — which is the whole point of a persistent
	// key rather than a generated one.
	blob, err := keyring.Seal(testKeyPhrase)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	again, err := LoadOrCreateCredentialKey(context.Background(), dir, noCredentials)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	plaintext, err := again.Open(blob)
	if err != nil {
		t.Fatalf("a restarted deployment cannot open its own credential: %v", err)
	}
	if plaintext != testKeyPhrase {
		t.Errorf("Open = %q", plaintext)
	}
}

func TestLoadOrCreateKeyRefusesToRegenerateOverBindings(t *testing.T) {
	dir := t.TempDir()

	// The state a lost key leaves: bindings exist, the file does not.
	_, err := LoadOrCreateCredentialKey(context.Background(), dir, withCredentials)
	if !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("a missing key with bindings = %v, want ErrKeyMissing", err)
	}
	if _, statErr := os.Stat(CredentialKeyPath(dir)); statErr == nil {
		t.Fatal("a key file was created even though it would orphan every binding")
	}

	// The refusal is not a side effect of the order of the checks: a corrupt file
	// is also refused without being replaced.
	if err := os.WriteFile(CredentialKeyPath(dir), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCredentialKey(context.Background(), dir, withCredentials); err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
	raw, err := os.ReadFile(CredentialKeyPath(dir))
	if err != nil || string(raw) != "short" {
		t.Errorf("the corrupt file was rewritten: %q", raw)
	}
}

func TestLoadOrCreateKeyReadsWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	material := "an-existing-key-file-with-enough-entropy-0123456789"
	if err := os.WriteFile(CredentialKeyPath(dir), []byte(material), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := LoadOrCreateCredentialKey(context.Background(), dir, withCredentials)
	if err != nil {
		t.Fatalf("an existing key is not used: %v", err)
	}
	blob, err := keyring.Seal(testKeyPhrase)
	if err != nil {
		t.Fatal(err)
	}
	// A keyring built directly from the same file must open it: the file *is* the
	// key, so a deployment that restored the file from a separate backup is whole.
	direct, err := NewKeyring([]byte(material))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := direct.Open(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if plaintext != testKeyPhrase {
		t.Errorf("Open = %q", plaintext)
	}
}

// A key file is created here as exactly CredentialKeyBytes of crypto/rand, and a
// random byte can be whitespace.
//
// It was: the loader trimmed the file before measuring it, so a perfectly good
// 32-byte key ending in a tab measured 31 and was refused as "unusable" — a
// fatal startup error, on roughly one deployment in twenty, about a file that was
// exactly what the backend had written. The bytes are the key; the length is the
// rule; nothing trims.
func TestLoadOrCreateKeyAcceptsBytesThatHappenToBeWhitespace(t *testing.T) {
	// A full-length random key whose last byte is a tab: 32 bytes, one of them
	// whitespace, which the trimmed measurement used to read as 31.
	tabEnded := append([]byte("0123456789abcdef0123456789abcde"), '\t')
	if len(tabEnded) != CredentialKeyBytes {
		t.Fatalf("test material is %d bytes, want %d", len(tabEnded), CredentialKeyBytes)
	}
	newlineStarted := append([]byte{'\n'}, []byte("fedcba9876543210fedcba987654321")...)
	if len(newlineStarted) != CredentialKeyBytes {
		t.Fatalf("test material is %d bytes, want %d", len(newlineStarted), CredentialKeyBytes)
	}

	for name, material := range map[string][]byte{
		"a key ending in a tab":  tabEnded,
		"a key starting with LF": newlineStarted,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(CredentialKeyPath(dir), material, 0o600); err != nil {
				t.Fatal(err)
			}
			keyring, err := LoadOrCreateCredentialKey(context.Background(), dir, withCredentials)
			if err != nil {
				t.Fatalf("a %d-byte key file was refused: %v", len(material), err)
			}
			// The exact bytes are the key: a keyring built from the trimmed
			// interpretation would not open what this one seals.
			other, err := NewKeyring(material)
			if err != nil {
				t.Fatalf("NewKeyring: %v", err)
			}
			blob, err := keyring.Seal(testKeyPhrase)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.Open(blob); err != nil {
				t.Fatalf("the key is not the file's exact bytes: %v", err)
			}
		})
	}
}

// A file that really is too short is still refused, and still never written over.
func TestLoadOrCreateKeyRefusesATooShortFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(CredentialKeyPath(dir), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCredentialKey(context.Background(), dir, withCredentials); !errors.Is(err, ErrKeyCorrupt) {
		t.Fatalf("err = %v, want ErrKeyCorrupt", err)
	}
}

func TestLoadOrCreateKeySurfacesAReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: readable as a name, not as bytes.
	if err := os.Mkdir(CredentialKeyPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCredentialKey(context.Background(), dir, noCredentials); err == nil {
		t.Fatal("an unreadable key file was ignored")
	}
}

func TestLoadOrCreateKeySurfacesAProviderError(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("the store is unavailable")
	_, err := LoadOrCreateCredentialKey(context.Background(), dir,
		func(context.Context) (bool, error) { return false, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("a store failure was swallowed: %v", err)
	}
}

func TestCredentialKeyPathIsInsideDataRoot(t *testing.T) {
	dir := t.TempDir()
	got := CredentialKeyPath(dir)
	if filepath.Dir(got) != filepath.Clean(dir) {
		t.Fatalf("key path = %q, want it inside the data root", got)
	}
	// The name is fixed, so two spellings cannot become two keys.
	if again := CredentialKeyPath(dir + "/"); again != got {
		t.Errorf("CredentialKeyPath is not stable: %q vs %q", got, again)
	}
}

// store is referenced only by the store-shaped tests in this package; keep the
// import honest by using it here, where the credential row is the thing being
// protected.
func TestCredentialRowsAreWhatTheKeyProtects(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "blog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// A sealed credential stored directly, as the binding flow does.
	keyring, err := NewKeyring([]byte("a-key-file-with-enough-entropy-0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := keyring.Seal("ghu_a-token-the-database-must-not-contain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO github_credential (identity_id, access_token_ct, token_type, created_at, updated_at)
	                      VALUES (1, ?, 'bearer', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, blob); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := db.QueryRow(`SELECT access_token_ct FROM github_credential WHERE identity_id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "ghu_a-token-the-database-must-not-contain" {
		t.Fatal("the token was stored in the clear")
	}
}
