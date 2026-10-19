package api

// The origin allowlist, validated where it is written.
//
// ARCHITECTURE.md ID-24: the Origin check is defence in depth for CSRF, and a
// defence that can be widened from the settings screen is only as strong as the
// validation on that screen. The *format* of a list — what an origin is, how a
// stored row is decoded — belongs to the package that checks it (auth); this file
// adds only what a settings form needs: a field name and an error string.
//
// Two rules live here, because a save is the one place they are decided:
//
//   - the count is bounded, because the check compiles the list on every mutation;
//   - an empty list is empty. It is not a wildcard and it is not "allow
//     everything": with nothing configured, the only accepted answer is
//     same-origin, which is the default a fresh deployment runs on.

import (
	"strconv"
	"strings"

	"blogcms/internal/auth"
)

// validateAllowedOrigins returns the field error for a list, or "" when it is
// acceptable.
func validateAllowedOrigins(origins []string) string {
	if len(origins) > auth.MaxAllowlistEntries {
		return "at most " + strconv.Itoa(auth.MaxAllowlistEntries) + " addresses"
	}
	seen := map[string]bool{}
	for _, origin := range origins {
		if reason := auth.OriginProblem(origin); reason != "" {
			return reason
		}
		// Compared case-insensitively on the whole origin, because two entries that
		// differ only by case are one entry as far as the check is concerned: the
		// comparison lower-cases the host, so a duplicate is a rule stated twice.
		key := strings.ToLower(strings.TrimSpace(origin))
		if seen[key] {
			return "duplicate entry: " + strings.TrimSpace(origin)
		}
		seen[key] = true
	}
	return ""
}

// DecodeAllowedOrigins reads a stored allowlist the way the check reads it.
//
// Written by a validated save, but also possibly by an older schema, a restored
// backup or a hand-edited file — and none of those may widen what the check accepts.
func DecodeAllowedOrigins(raw string) []string {
	return auth.DecodeAllowlist(raw)
}

// ParseAllowedOrigins splits a textarea or an environment value into entries, for
// the callers that accept the human spelling (the settings screen, a runbook). It
// applies no validation of its own: the writer validates.
func ParseAllowedOrigins(raw string) []string {
	return auth.ParseAllowlist(raw)
}
