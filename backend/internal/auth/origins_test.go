package auth

// The allowlist format.
//
// The check reads what the writer wrote, and refuses anything a writer would have
// refused. These are the cases where a mismatch between the two would be a silent
// widening: a wildcard, a stale row, a hand-edited row, and the spelling of a
// list that arrives as text.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOriginProblemRefusesWidening(t *testing.T) {
	ok := []string{
		"https://blog.example.com",
		"http://localhost:9900",
		"https://192.0.2.10",
		"http://127.0.0.1:8080",
	}
	for _, entry := range ok {
		if reason := OriginProblem(entry); reason != "" {
			t.Errorf("%q should be acceptable, got %q", entry, reason)
		}
	}

	bad := map[string]string{
		"":                              "empty",
		"*":                             "wildcard",
		"https://*":                     "wildcard",
		"https://*example.com":          "wildcard",
		"blog.example.com":              "must start with",
		"//blog.example.com":            "must start with",
		"javascript://comment/%0aalert": "must start with",
		"https://blog.example.com/x":    "must be just the address",
		"https://blog.example.com?x=1":  "must be just the address",
		"https://blog.example.com#f":    "must be just the address",
		"https://user@blog.example.com": "must be just the address",
	}
	for entry, want := range bad {
		if reason := OriginProblem(entry); reason == "" {
			t.Errorf("%q was accepted", entry)
		} else if !strings.Contains(reason, want) {
			t.Errorf("%q: the refusal %q does not name the problem (%q)", entry, reason, want)
		}
	}
}

func TestSplitAllowlistAcceptsBothSpellings(t *testing.T) {
	shell := SplitAllowlist("https://a.example.com, https://b.example.com")
	typed := SplitAllowlist("https://a.example.com\nhttps://b.example.com\n\nhttps://c.example.com")
	if len(shell) != 2 || shell[0] != "https://a.example.com" || shell[1] != "https://b.example.com" {
		t.Errorf("shell = %v", shell)
	}
	if len(typed) != 3 {
		t.Fatalf("typed = %v, want three entries with the blank line dropped", typed)
	}
	if got := SplitAllowlist(" , , "); len(got) != 0 {
		t.Errorf("an all-separator list produced %v", got)
	}
}

func TestNormalizeAllowlistDeduplicatesCaseInsensitively(t *testing.T) {
	got := NormalizeAllowlist([]string{
		"https://blog.example.com",
		"HTTPS://BLOG.EXAMPLE.COM",
		" https://web.example.com ",
		"",
	})
	if len(got) != 2 {
		t.Fatalf("normalized = %v, want two entries", got)
	}
	// The stored form is trimmed, so the row is exactly what the check compiles.
	if got[0] != "https://blog.example.com" || got[1] != "https://web.example.com" {
		t.Errorf("normalized = %v", got)
	}
}

func TestDecodeAllowlistRefusesWhatASaveWouldRefuse(t *testing.T) {
	// The row a save writes, with junk alongside it: the junk is what a restored
	// backup or an older schema could have left there.
	raw, err := json.Marshal([]string{
		"https://blog.example.com", "*", "", "not-a-url", "https://other.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := DecodeAllowlist(string(raw))
	if len(got) != 2 {
		t.Fatalf("decoded = %v, want the two valid entries", got)
	}
	for _, entry := range got {
		if entry == "*" || entry == "" || entry == "not-a-url" {
			t.Errorf("a refused entry survived decoding: %q", entry)
		}
	}
}

func TestDecodeAllowlistFallsBackToNothing(t *testing.T) {
	// Anything that is not a JSON array is nothing, which is the same-origin
	// default rather than an open allowlist.
	for _, raw := range []string{"", "not json", `"a string"`, "null", `{"a":1}`, `[*]`} {
		if got := DecodeAllowlist(raw); len(got) != 0 {
			t.Errorf("%q decoded to %v, want nothing", raw, got)
		}
	}
}

func TestDecodeAllowlistIsBounded(t *testing.T) {
	many := make([]string, 0, MaxAllowlistEntries*3)
	for i := 0; i < MaxAllowlistEntries*3; i++ {
		many = append(many, "https://h"+string(rune('a'+i%26))+"x.example.com")
	}
	raw, err := json.Marshal(many)
	if err != nil {
		t.Fatal(err)
	}
	if got := DecodeAllowlist(string(raw)); len(got) != MaxAllowlistEntries {
		t.Fatalf("decoded %d entries, want %d", len(got), MaxAllowlistEntries)
	}
}

func TestParseAllowlistIsNormalized(t *testing.T) {
	got := ParseAllowlist(" https://a.example.com , HTTPS://A.EXAMPLE.COM \n\nhttps://b.example.com")
	if len(got) != 2 {
		t.Fatalf("parsed = %v, want two entries", got)
	}
}
