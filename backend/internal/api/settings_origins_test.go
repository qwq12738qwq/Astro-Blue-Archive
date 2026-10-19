package api

// The origin allowlist, validated where it is written and re-decoded where it is
// read.
//
// The property under test is not "does the save succeed" but "can a stale row,
// a wildcard, or a bare hostname widen what the check accepts". A list that
// accepts `*` is a list that disables the CSRF origin check, so every one of these
// cases is a widening attempt.

import (
	"encoding/json"
	"strings"
	"testing"

	"blogcms/internal/auth"
)

func TestValidateAllowedOriginsAcceptsReasonableLists(t *testing.T) {
	ok := [][]string{
		nil,
		{},
		{"https://blog.example.com"},
		{"https://blog.example.com", "http://localhost:9900"},
		{"https://a.example.com", "https://b.example.com", "https://c.example.com"},
	}
	for _, list := range ok {
		if reason := validateAllowedOrigins(list); reason != "" {
			t.Errorf("%v should be accepted, got %q", list, reason)
		}
	}
}

func TestValidateAllowedOriginsRejectsWidening(t *testing.T) {
	cases := []struct {
		name    string
		list    []string
		blocked string
	}{
		{"a wildcard", []string{"*"}, "*"},
		{"a wildcard with a scheme", []string{"https://*"}, "*"},
		{"a wildcard host among good entries", []string{"https://ok.com", "https://*"}, "*"},
		{"a bare hostname", []string{"blog.example.com"}, "must start with"},
		{"a scheme-relative entry", []string{"//blog.example.com"}, "must start with"},
		{"a path", []string{"https://blog.example.com/admin"}, "path"},
		{"a query", []string{"https://blog.example.com?x=1"}, "path"},
		{"a fragment", []string{"https://blog.example.com#x"}, "path"},
		{"a javascript scheme", []string{"javascript://comment/%0aalert(1)"}, "must start with"},
		{"an empty entry", []string{"https://ok.com", ""}, "empty"},
		{"a duplicate", []string{"https://blog.example.com", "https://blog.example.com"}, "duplicate"},
		{"a duplicate by case", []string{"https://blog.example.com", "HTTPS://BLOG.EXAMPLE.COM"}, "duplicate"},
		{"an absurd count", make([]string, auth.MaxAllowlistEntries+1), "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := validateAllowedOrigins(tc.list)
			if reason == "" {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(reason, tc.blocked) {
				t.Errorf("the refusal %q does not explain the problem (%q)", reason, tc.blocked)
			}
		})
	}
}

func TestDecodeAllowedOriginsDropsWhatASaveWouldRefuse(t *testing.T) {
	// A row written by a save is an array; a row written by anything else is not
	// guaranteed to be one, and neither is guaranteed to hold only valid entries.
	raw := `["https://blog.example.com","*","","not-a-url","https://other.com"]`
	got := DecodeAllowedOrigins(raw)
	if len(got) != 2 {
		t.Fatalf("decoded = %v, want the two valid entries", got)
	}
	if got[0] != "https://blog.example.com" || got[1] != "https://other.com" {
		t.Errorf("decoded = %v", got)
	}

	// Anything malformed decodes to nothing, which is the same-origin default.
	for _, bad := range []string{"", "not json", `"a string"`, "null", `{"a":1}`} {
		if got := DecodeAllowedOrigins(bad); len(got) != 0 {
			t.Errorf("%q decoded to %v, want nothing", bad, got)
		}
	}
}

func TestDecodeAllowedOriginsIsBounded(t *testing.T) {
	many := make([]string, 0, auth.MaxAllowlistEntries*3)
	for i := 0; i < auth.MaxAllowlistEntries*3; i++ {
		many = append(many, "https://host"+string(rune('a'+i%26))+"x.example.com")
	}
	raw, err := jsonMarshal(many)
	if err != nil {
		t.Fatal(err)
	}
	if got := DecodeAllowedOrigins(raw); len(got) > auth.MaxAllowlistEntries {
		t.Fatalf("decoded %d entries, want at most %d", len(got), auth.MaxAllowlistEntries)
	}
}

func TestNormalizeAllowedOriginsDeduplicates(t *testing.T) {
	got := auth.NormalizeAllowlist([]string{
		" https://blog.example.com ",
		"HTTPS://BLOG.EXAMPLE.COM",
		"https://web.example.com",
		"   ",
	})
	if len(got) != 2 {
		t.Fatalf("normalized = %v, want two entries", got)
	}
	if got[0] != "https://blog.example.com" {
		t.Errorf("the first entry was not trimmed: %q", got[0])
	}
}

func TestParseAllowedOriginsAcceptsBothSpellings(t *testing.T) {
	// A pasted environment value and a typed textarea are the same list.
	env := ParseAllowedOrigins("https://a.example.com, https://b.example.com")
	typed := ParseAllowedOrigins("https://a.example.com\nhttps://b.example.com\n\n")
	if len(env) != 2 || len(typed) != 2 {
		t.Fatalf("env = %v, typed = %v", env, typed)
	}
	for i := range env {
		if env[i] != typed[i] {
			t.Fatalf("the two spellings disagree: %v vs %v", env, typed)
		}
	}
}

// jsonMarshal encodes the list the way a save writes it.
func jsonMarshal(v any) (string, error) {
	raw, err := json.Marshal(v)
	return string(raw), err
}
