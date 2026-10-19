package auth

// The origin allowlist: one format, two spellings.
//
// An allowlist is stored as a JSON array and accepted as *text* — a comma- or
// newline-separated string, which is how a runbook or an environment variable
// spells one. The stored form is canonical; the text form is parsed on the way in
// and never re-read. The check that consumes the row decodes exactly what the
// writer wrote, and refuses anything that would not have been written.
//
// This file owns the format because the *auth* package is the only reader that
// matters: if the settings API and the CSRF check disagreed about the shape of one
// row, the list would be accepted by the screen and ignored by the check, which is
// a hole that looks like a bug and reads like a bypass.

import (
	"encoding/json"
	"net/url"
	"strings"
)

// MaxAllowlistEntries bounds the list.
//
// The check compiles every entry on every mutation, so the list is a per-request
// cost, and eight is more than any real deployment needs — a www/naked-apex pair,
// a preview host and a mail host.
const MaxAllowlistEntries = 8

// SplitAllowlist parses the text spelling of a list into its entries.
//
// Commas, newlines and semicolons all separate, and surrounding whitespace is not
// part of an entry: a list pasted from a shell and a list typed into a textarea are
// the same list, and neither should be defeated by a trailing space or a blank line
// after the last entry. An empty entry is dropped rather than kept, so an entry can
// never be the empty string — which would be compiled as a rule matching nothing
// and would look, in a log line, like a fault in the matcher.
func SplitAllowlist(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// NormalizeAllowlist returns the canonical form of an accepted list.
//
// Trimming and de-duplicating here means the stored row is exactly what the check
// compiles, so a reader cannot be confused by two spellings of one entry. An empty
// result is nil, which is what "no list" is stored as.
func NormalizeAllowlist(entries []string) []string {
	out := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// OriginProblem names what is wrong with one entry, or "" when it is acceptable.
//
// The three refusals that matter, each because breaking it is invisible until it is
// exploited:
//
//   - not an absolute http(s) origin. A bare hostname (`blog.example.com`) is
//     stored and then matches nothing, so the check looks broken rather than
//     mis-entered; a scheme-relative or `javascript:` value is the same text
//     reaching a comparison it can never satisfy, or one that comparison never
//     happens for;
//   - a path, query, fragment or userinfo. `https://x/admin` is not an origin, and
//     accepting one would admit a rule nobody can reason about;
//   - a wildcard. `*` in any position makes the check accept an attacker's origin,
//     which is the whole check switched off.
func OriginProblem(origin string) string {
	if origin == "" {
		return "an entry is empty"
	}
	// The wildcard check comes first, and it is a substring test rather than a
	// parse: `*` alone does not parse into a scheme, so the scheme refusal would
	// answer "must start with http://" for the one entry whose entire purpose is
	// to switch the check off. Naming the real problem is the difference between an
	// operator fixing their input and concluding the setting is broken.
	if strings.ContainsAny(origin, "*") {
		return origin + " is a wildcard; the address must be named exactly"
	}
	u, err := url.Parse(origin)
	if err != nil {
		return origin + " is not a URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return origin + " must start with http:// or https://"
	}
	if u.Host == "" {
		return origin + " has no host"
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
		return origin + " must be just the address, with no path"
	}
	return ""
}

// DecodeAllowlist reads a stored allowlist defensively.
//
// The row was written by a validated save, but it can also be a row from an older
// schema, a restored backup, or a hand-edited file. Anything that would not have
// been accepted on the way in is dropped here, one entry at a time, because a check
// that a stale row can widen is not a check. A malformed row yields nothing, which
// is the same-origin default rather than an open door.
func DecodeAllowlist(raw string) []string {
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, entry := range list {
		if entry == "" || OriginProblem(entry) != "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(entry))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, strings.TrimSpace(entry))
		if len(out) == MaxAllowlistEntries {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseAllowlist accepts the text spelling of a list. It applies no validation of
// its own: the writer validates, and the reader decodes.
func ParseAllowlist(raw string) []string {
	return NormalizeAllowlist(SplitAllowlist(raw))
}
