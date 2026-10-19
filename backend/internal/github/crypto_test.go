package github

// Credential encryption.
//
// The claims worth proving are the ones a stolen database would test: that a sealed
// blob is not the token, that the same token sealed twice looks different, that a
// blob from another key does not open, and that a short key is refused rather than
// silently padded into something usable.

import (
	"encoding/base64"
	"strings"
	"testing"
)

const testKeyMaterial = "test-credential-encryption-key-0123456789abcdef"

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	keyring, err := NewKeyring([]byte(testKeyMaterial))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return keyring
}

func TestSealOpenRoundTrip(t *testing.T) {
	keyring := testKeyring(t)
	const token = "ghu_abc123SECRET"

	blob, err := keyring.Seal(token)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(blob, token) {
		t.Fatal("the sealed blob contains the plaintext token")
	}
	if !strings.HasPrefix(blob, blobPrefix) {
		t.Fatalf("blob %q has no version prefix", blob)
	}

	plaintext, err := keyring.Open(blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if plaintext != token {
		t.Fatalf("Open returned %q, want %q", plaintext, token)
	}
}

func TestSealIsNotDeterministic(t *testing.T) {
	keyring := testKeyring(t)
	const token = "ghu_same-token-twice"

	first, err := keyring.Seal(token)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := keyring.Seal(token)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if first == second {
		t.Fatal("two seals of the same token are identical; the salt is fixed")
	}
}

func TestOpenWithAnotherKeyFails(t *testing.T) {
	blob, err := testKeyring(t).Seal("ghu_somebody-elses-token")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other, err := NewKeyring([]byte("a-different-key-material-0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	if _, err := other.Open(blob); err == nil {
		t.Fatal("a blob opened with the wrong key must fail")
	}
}

func TestOpenRejectsATamperedBlob(t *testing.T) {
	keyring := testKeyring(t)
	blob, err := keyring.Seal("ghu_tamper-target")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(blob, blobPrefix))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw[len(raw)-1] ^= 0xff
	tampered := blobPrefix + base64.RawURLEncoding.EncodeToString(raw)
	if _, err := keyring.Open(tampered); err == nil {
		t.Fatal("a flipped bit in the ciphertext must fail authentication")
	}
}

func TestOpenRejectsAnUnversionedValue(t *testing.T) {
	keyring := testKeyring(t)
	if _, err := keyring.Open(base64.RawURLEncoding.EncodeToString([]byte("ghu_not-a-blob"))); err == nil {
		t.Fatal("a value with no blob prefix must be refused, not decoded as something")
	}
}

func TestSealRefusesAnEmptyCredential(t *testing.T) {
	keyring := testKeyring(t)
	if _, err := keyring.Seal(""); err == nil {
		t.Fatal("an empty token must not be sealed: it would produce a blob that opens to nothing")
	}
}

func TestNewKeyringRefusesShortMaterial(t *testing.T) {
	for _, material := range []string{"", "short", strings.Repeat(" ", 40)} {
		if _, err := NewKeyring([]byte(material)); err == nil {
			t.Fatalf("NewKeyring accepted %d bytes of key material", len(material))
		}
	}
}

func TestFingerprintIsStableAndNotReversible(t *testing.T) {
	const token = "ghu_fingerprint-me"
	first := Fingerprint(token)
	if second := Fingerprint(token); first != second {
		t.Fatalf("Fingerprint is not stable: %q vs %q", first, second)
	}
	if strings.Contains(first, token) {
		t.Fatal("the fingerprint contains the token")
	}
	if other := Fingerprint("ghu_a-different-token"); other == first {
		t.Fatal("two different tokens share a fingerprint")
	}
}
