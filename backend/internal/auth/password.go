// Package auth implements single-admin authentication.
//
// ARCHITECTURE.md §12: Argon2id only, no bespoke cryptography. Sessions store
// only SHA-256(token), so a database disclosure cannot be replayed as a cookie.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are the OWASP-recommended minimums for argon2id.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// Errors are deliberately coarse: callers must not distinguish "no such user"
// from "wrong password" in anything they return to the client (§8 #10/#20).
var (
	ErrInvalidHash     = errors.New("invalid password hash format")
	ErrMismatch        = errors.New("password does not match")
	ErrEmptyPassword   = errors.New("password must not be empty")
	ErrPasswordTooLong = errors.New("password is too long")
)

// MaxPasswordLen bounds the work an attacker can force. 1 KiB of input is far
// beyond any legitimate password and keeps Argon2 cost bounded.
const MaxPasswordLen = 1024

// HashPassword returns a PHC-formatted argon2id string, suitable for storage.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}
	if len(password) > MaxPasswordLen {
		return "", ErrPasswordTooLong
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonTime,
		argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword compares a password against a stored PHC hash in constant
// time with respect to the key material.
//
// A malformed hash is treated as a mismatch rather than an error so that a
// corrupted row cannot become a distinguishable response.
func VerifyPassword(password string, encoded string) error {
	salt, want, err := decodeHash(encoded)
	if err != nil {
		return ErrMismatch
	}
	got := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return nil
	}
	return ErrMismatch
}

// NeedsRehash reports whether a stored hash uses weaker parameters than the
// current policy, so it can be upgraded on the next successful login.
func NeedsRehash(encoded string) bool {
	salt, key, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return argonTime != 3 ||
		argonMemory != 64*1024 ||
		argonThreads != 2 ||
		uint32(len(key)) != argonKeyLen ||
		uint32(len(salt)) != argonSaltLen
}

func decodeHash(encoded string) (salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, key]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return nil, nil, ErrInvalidHash
	}

	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return nil, nil, ErrInvalidHash
	}
	if memory == 0 || timeCost == 0 || threads == 0 {
		return nil, nil, ErrInvalidHash
	}

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, ErrInvalidHash
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, ErrInvalidHash
	}
	return salt, key, nil
}

// dummyHash is a real argon2id hash of an unguessable value.
//
// §8 #10: an unknown username must still perform a password verification, or
// the response time would reveal which usernames exist.
var dummyHash string

func init() {
	// Hashing a random value keeps the dummy cost identical to a real check
	// while remaining useless to an attacker even if the source leaks.
	b, err := HashPassword("dummy-password-never-matches-2f7c1a9e")
	if err != nil {
		panic("auth: cannot build the dummy hash: " + err.Error())
	}
	dummyHash = b
}

// VerifyDummy performs a verification against a throwaway hash so that an
// unknown user costs the same as a known one.
func VerifyDummy(password string) {
	_ = VerifyPassword(password, dummyHash)
}
