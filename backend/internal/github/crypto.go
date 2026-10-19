// Package github binds a GitHub identity to a CMS administrator.
//
// Three kinds of fact live here and they are deliberately not one thing:
//
//   - a CMS administrator (auth.Admin) is local identity, created in the CMS;
//   - a GitHub identity (store.GitHubIdentityRecord) says which external account an
//     administrator proved they control, keyed on GitHub's immutable numeric id;
//   - a GitHub credential (store.GitHubCredentialRecord) says what this CMS may do
//     with that account, and is stored encrypted by the Keyring in this file.
//
// A GitHub identity grants no CMS permission of its own. Binding one does not
// create an administrator, promote one, or open the admin area: it attaches an
// external account to an administrator who already has a session.
//
// The scheme is the GitHub OAuth App *user authorization* flow (ID-64): the
// binding phase needs exactly what an OAuth App does — prove which GitHub account
// an administrator controls — and asking for anything more (scopes, repository
// permissions, an App installation) would be the next phase's decision, not a
// precondition of this one. Everything this package does is independent of that
// later phase: binding stores identity and credential, and nothing here pushes,
// clones or otherwise talks to a repository (ID-76).
package github

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Credential encryption.
//
// A GitHub token is a bearer credential for someone else's account, so it is never
// stored as plaintext and never leaves the process: not in a response, not in a log
// line, not in an audit event, not in a URL. The database holds ciphertext, and the
// key is read from a file the operator injects — a different secret in a different
// place, so a leaked database or a leaked backup archive is not a leaked token.
//
// The construction is argon2id (key derivation) + AES-256-GCM (encryption), both
// from the same standard library already used for passwords. There is no bespoke
// cryptography here, which is the point.

const (
	// keyLen is the AES-256 key size.
	keyLen = 32
	// saltLen is the per-record argon2 salt.
	saltLen = 16
	// nonceLen is the AES-GCM nonce size.
	nonceLen = 12
	// blobPrefix identifies the sealed format, so a future one does not get
	// decoded by a reader that cannot read it.
	blobPrefix = "v1."
	// minKeyMaterialBytes is the smallest key file this accepts. A one-character
	// key file is a deployment accident, not a key.
	minKeyMaterialBytes = 32
)

// Argon2id parameters for key derivation. They match the password policy in
// internal/auth, so there is one KDF configuration in this repository rather than
// two that drift.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 2
)

// Errors the Keyring can return. They are deliberately coarse.
var (
	ErrKeyTooShort     = errors.New("credential encryption key is too short")
	ErrInvalidBlob     = errors.New("credential blob is malformed")
	ErrDecryptFailed   = errors.New("credential could not be decrypted")
	ErrPlaintextRefuse = errors.New("refusing to store a credential in plaintext")
)

// Keyring seals and opens credential ciphertext.
//
// The derived key is per *record*, not per process: each seal generates a fresh
// argon2 salt, so two bindings encrypted with the same key file do not share a
// derived key and a precomputed table against that file buys nothing.
type Keyring struct {
	material []byte
}

// NewKeyring builds a Keyring from the key file's contents.
//
// An empty or too-short key is refused rather than defaulted: a credential store
// that silently falls back to a hardcoded key is a credential store that lies about
// its security. Material that is nothing but whitespace is refused as well,
// because that is what `echo > key` produces rather than a key.
//
// Otherwise the bytes are used exactly as stored. This used to strip surrounding
// whitespace first, "so a hand-made key file may end in a newline" — and that
// turned a *valid* machine-generated 32-byte key into a refused one whenever
// crypto/rand happened to put a whitespace byte at either end: the raw check
// passed, the trimmed one failed, and the deployment could not start. Roughly one
// fresh install in twenty, with a message about a file that was perfectly fine.
// Whitespace inside a pasted key is entropy like any other byte; the file's length
// is the rule.
func NewKeyring(material []byte) (*Keyring, error) {
	if len(material) < minKeyMaterialBytes {
		return nil, fmt.Errorf("%w: got %d bytes, want at least %d", ErrKeyTooShort, len(material), minKeyMaterialBytes)
	}
	if onlyWhitespace(material) {
		return nil, fmt.Errorf("%w: the file contains nothing but whitespace", ErrKeyTooShort)
	}
	// A copy, so a caller mutating its buffer afterwards cannot reach the key.
	key := make([]byte, len(material))
	copy(key, material)
	return &Keyring{material: key}, nil
}

// Seal encrypts a credential into a self-describing blob.
func (k *Keyring) Seal(plaintext string) (string, error) {
	if k == nil {
		return "", ErrPlaintextRefuse
	}
	if plaintext == "" {
		return "", ErrPlaintextRefuse
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	block, err := k.block(salt)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := block.Seal(nil, nonce, []byte(plaintext), nil)

	buf := make([]byte, 0, len(salt)+len(nonce)+len(sealed))
	buf = append(buf, salt...)
	buf = append(buf, nonce...)
	buf = append(buf, sealed...)
	return blobPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Open decrypts a blob produced by Seal.
func (k *Keyring) Open(blob string) (string, error) {
	if k == nil {
		return "", ErrDecryptFailed
	}
	if !strings.HasPrefix(blob, blobPrefix) {
		return "", ErrInvalidBlob
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(blob, blobPrefix))
	if err != nil {
		return "", ErrInvalidBlob
	}
	if len(raw) < saltLen+nonceLen+16 {
		return "", ErrInvalidBlob
	}
	salt, nonce, sealed := raw[:saltLen], raw[saltLen:saltLen+nonceLen], raw[saltLen+nonceLen:]
	aead, err := k.block(salt)
	if err != nil {
		return "", err
	}
	plaintext, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", ErrDecryptFailed
	}
	return string(plaintext), nil
}

// block derives the AES-GCM for one record's salt.
func (k *Keyring) block(salt []byte) (cipher.AEAD, error) {
	key := argon2.IDKey(k.material, salt, argonTime, argonMemory, argonThreads, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize aead: %w", err)
	}
	return aead, nil
}

// Fingerprint reduces a credential to a short, non-reversible label.
//
// Used where a log line or an admin row needs to say *which* credential without
// saying anything about it: two bindings of the same token share a fingerprint, and
// the fingerprint cannot be turned back into the token.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte("blogcms-github:" + value))
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}

// onlyWhitespace reports whether every byte is ASCII whitespace.
func onlyWhitespace(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\v', '\f', '\r':
		default:
			return false
		}
	}
	return true
}
