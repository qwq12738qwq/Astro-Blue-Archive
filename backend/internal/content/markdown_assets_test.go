package content_test

// ARCHITECTURE.md §34: the Markdown style-template filesystem
// layer.
//
// These are the properties the HTTP suites cannot cheaply
// observe: what the filename grammar rejects, where a
// disabled file actually lives, that two files which share a
// name in both directories are reported rather than merged,
// and that an oversized or unreadable file is skipped instead
// of failing the whole listing. Asserting them through
// `fetch` would mean a directory tree per assertion.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blogcms/internal/content"
	"blogcms/internal/testutil"
)

func TestValidateMarkdownTemplateNameRejectsTraversal(t *testing.T) {
	// The custom-asset brief's list, adapted: a Markdown
	// template is always CSS, so the extension is fixed and
	// the grammar admits nothing else.
	bad := []string{
		"../../evil.css",
		"..\\evil.css",
		"/etc/test.css",
		"etc/passwd",
		"001-base.css?x",
		"001-base.css#x",
		"001-base.css/",
		"001-base.css\x00",
		"001/evil.css",
		"001-base.css%2F..%2Fevil.css",
		"..",
		".",
		"",
		"1-base.css",                            // two-digit prefix
		"0001-base.css",                         // four-digit prefix
		"0-1-base.css",                          // a two-character prefix
		"01a-base.css",                          // a letter inside the prefix
		"Base-010.css",                          // upper case
		"010-base.css.bak",                      // trailing extension
		"010-base.js.css",                       // two extensions
		"foo.js.css",                            // a dot inside the stem
		"010-.css",                              // empty stem
		"010-base",                              // no extension
		"010-base.CSS",                          // upper-case extension
		"010-base.css ",                         // trailing space
		" 010-base.css",                         // leading space
		"010-base_.css",                         // underscore is not kebab-case
		"010-base.js",                           // a JavaScript extension
		strings.Repeat("010-", 40) + "base.css", // longer than content.AssetNameMaxBytes
	}
	for _, name := range bad {
		if err := content.ValidateMarkdownTemplateName(name); err == nil {
			t.Errorf("content.ValidateMarkdownTemplateName(%q) accepted a name it must refuse", name)
		}
	}

	good := []string{"001-base.css", "010-typography.css", "100-custom.css", "020-card.css", "000-first.css"}
	for _, name := range good {
		if err := content.ValidateMarkdownTemplateName(name); err != nil {
			t.Errorf("content.ValidateMarkdownTemplateName(%q) = %v, want nil", name, err)
		}
	}
}

func TestMarkdownTemplateOrderComesFromThePrefix(t *testing.T) {
	cases := map[string]int{
		"001-base.css":   1,
		"010-layout.css": 10,
		"100-custom.css": 100,
		"999-last.css":   999,
		"nonsense.css":   0,
		"":               0,
	}
	for name, want := range cases {
		if got := content.AssetOrder(name); got != want {
			t.Errorf("content.AssetOrder(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestListMarkdownTemplateFilesIsOrderedByFilenameNotByDirectory(t *testing.T) {
	s, _ := testutil.ContentStore(t)

	// Written in an order that is deliberately *not* the load
	// order.
	for _, name := range []string{"020-code.css", "001-base.css", "100-custom.css", "010-typography.css"} {
		if err := s.CreateMarkdownTemplate(name, "."+name+" {}\n"); err != nil {
			t.Fatal(err)
		}
	}

	files, err := s.ListMarkdownTemplateFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4", len(files))
	}
	want := []string{"001-base.css", "010-typography.css", "020-code.css", "100-custom.css"}
	for i, f := range files {
		if f.Filename != want[i] {
			t.Errorf("files[%d] = %q, want %q", i, f.Filename, want[i])
		}
		if !f.Enabled || f.Status != content.AssetStatusOK || f.Checksum == "" || f.Size == 0 {
			t.Errorf("files[%d] = %+v, want an enabled, loadable file", i, f)
		}
	}
}

func TestMarkdownTemplateEnabledIsALocation(t *testing.T) {
	s, _ := testutil.ContentStore(t)

	if err := s.CreateMarkdownTemplate("020-code.css", ".a{color:red}\n"); err != nil {
		t.Fatal(err)
	}
	if files, _ := s.ListMarkdownTemplateFiles(); len(files) != 1 || !files[0].Enabled {
		t.Fatalf("a created template must start enabled: %+v", files)
	}

	if err := s.SetMarkdownTemplateEnabled("020-code.css", false); err != nil {
		t.Fatal(err)
	}
	files, err := s.ListMarkdownTemplateFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Enabled || files[0].Status != content.AssetStatusOK {
		t.Fatalf("a parked template is listed, disabled: %+v", files)
	}
	// The body survives the move untouched: a parked file is
	// the same bytes in the other directory.
	body, err := s.ReadMarkdownTemplate("020-code.css")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != ".a{color:red}\n" {
		t.Errorf("parked body = %q, want the original bytes", body)
	}

	// Editing a parked file must not promote it.
	if err := s.WriteMarkdownTemplate("020-code.css", ".a{color:blue}\n"); err != nil {
		t.Fatal(err)
	}
	files, err = s.ListMarkdownTemplateFiles()
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Enabled {
		t.Fatal("saving a parked template promoted it")
	}
	body, err = s.ReadMarkdownTemplate("020-code.css")
	if err != nil || string(body) != ".a{color:blue}\n" {
		t.Errorf("parked edit = %q (%v), want the new bytes", body, err)
	}

	// Unparking is a move back, not a rewrite.
	if err := s.SetMarkdownTemplateEnabled("020-code.css", true); err != nil {
		t.Fatal(err)
	}
	files, _ = s.ListMarkdownTemplateFiles()
	if !files[0].Enabled {
		t.Fatal("unparking did not re-enable the template")
	}

	// Enabling an enabled template is a no-op, not an error:
	// the UI sends the state it believes in.
	if err := s.SetMarkdownTemplateEnabled("020-code.css", true); err != nil {
		t.Errorf("double enable = %v, want nil", err)
	}
}

func TestMarkdownTemplateCreateRefusesOverwriteAndRenameCollision(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateMarkdownTemplate("010-code.css", ".a{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateMarkdownTemplate("010-code.css", ".b{}\n"); !errors.Is(err, content.ErrConflict) {
		t.Errorf("create over existing = %v, want content.ErrConflict", err)
	}
	if err := s.CreateMarkdownTemplate("010-other.css", ".b{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameMarkdownTemplate("010-code.css", "010-other.css"); !errors.Is(err, content.ErrConflict) {
		t.Errorf("rename onto existing = %v, want content.ErrConflict", err)
	}
	if err := s.RenameMarkdownTemplate("100-missing.css", "030-x.css"); !errors.Is(err, content.ErrMarkdownTemplateNotFound) {
		t.Errorf("rename missing = %v, want content.ErrMarkdownTemplateNotFound", err)
	}
	// A rename keeps the enabled state and the bytes.
	if err := s.RenameMarkdownTemplate("010-code.css", "010-code2.css"); err != nil {
		t.Fatal(err)
	}
	files, _ := s.ListMarkdownTemplateFiles()
	if len(files) != 2 || files[0].Filename != "010-code2.css" || !files[0].Enabled {
		t.Fatalf("after rename: %+v", files)
	}
}

func TestMarkdownTemplateDeleteRemovesFromBothDirectories(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateMarkdownTemplate("010-code.css", ".a{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMarkdownTemplateEnabled("010-code.css", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMarkdownTemplate("010-code.css"); err != nil {
		t.Fatal(err)
	}
	files, _ := s.ListMarkdownTemplateFiles()
	if len(files) != 0 {
		t.Fatalf("after delete: %+v", files)
	}
	if err := s.DeleteMarkdownTemplate("010-code.css"); !errors.Is(err, content.ErrMarkdownTemplateNotFound) {
		t.Errorf("delete missing = %v, want content.ErrMarkdownTemplateNotFound", err)
	}
}

func TestMarkdownTemplateListingReportsTheUnusable(t *testing.T) {
	s, root := testutil.ContentStore(t)
	sys := filepath.Join(root, "system")
	if err := os.MkdirAll(filepath.Join(sys, "markdown"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A dotfile is skipped silently (a crash artefact, not an
	// asset), a subdirectory is reported, and a file outside
	// the grammar is reported rather than followed.
	if err := os.WriteFile(filepath.Join(sys, "markdown", ".tmp-xyz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sys, "markdown", "stray-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "markdown", "evil-name.css"), []byte(".a{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file too large to edit is invalid, and the listing
	// survives it.
	if err := os.WriteFile(filepath.Join(sys, "markdown", "999-huge.css"), make([]byte, content.BodyMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := s.ListMarkdownTemplateFiles()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]content.MarkdownTemplateFile{}
	for _, f := range files {
		byName[f.Filename] = f
	}
	if _, ok := byName[".tmp-xyz"]; ok {
		t.Error("a dotfile was listed as a template")
	}
	if f := byName["stray-dir"]; f.Status != content.AssetStatusInvalid || !strings.Contains(f.Problem, "subdirector") {
		t.Errorf("subdirectory = %+v, want invalid with an explanation", f)
	}
	if f := byName["evil-name.css"]; f.Status != content.AssetStatusInvalid {
		t.Errorf("bad grammar = %+v, want invalid", f)
	}
	if f := byName["999-huge.css"]; f.Status != content.AssetStatusInvalid || !strings.Contains(f.Problem, "larger than") {
		t.Errorf("oversize = %+v, want invalid with the limit named", f)
	}

	// The same name in both directories is a contradiction,
	// not a merge.
	if err := s.CreateMarkdownTemplate("010-code.css", ".a{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMarkdownTemplateEnabled("010-code.css", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "markdown", "010-code.css"), []byte(".b{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, _ = s.ListMarkdownTemplateFiles()
	count := 0
	for _, f := range files {
		if f.Filename == "010-code.css" {
			count++
			if f.Status != content.AssetStatusInvalid || !strings.Contains(f.Problem, "share this name") {
				t.Errorf("duplicate = %+v, want both copies flagged", f)
			}
		}
	}
	if count != 2 {
		t.Errorf("got %d rows for a duplicated name, want 2", count)
	}
}

func TestMarkdownTemplateBodyValidation(t *testing.T) {
	s, _ := testutil.ContentStore(t)

	// CSS gets the file:// refusal (ID-37).
	err := s.CreateMarkdownTemplate("010-code.css", ".a{background:url(file:///etc/passwd)}\n")
	var ve *content.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("file:// create = %v, want a content.ValidationError", err)
	}
	if _, ok := ve.Fields["content"]; !ok {
		t.Errorf("validation error fields = %v, want a content field", ve.Fields)
	}

	// And the size ceiling.
	err = s.CreateMarkdownTemplate("010-code.css", strings.Repeat("a", content.BodyMaxBytes+1))
	if !errors.As(err, &ve) {
		t.Fatalf("oversize create = %v, want a content.ValidationError", err)
	}

	// A legal body round-trips.
	if err := s.CreateMarkdownTemplate("010-code.css", ".a{background:url(/media/x.png)}\n"); err != nil {
		t.Errorf("legal create = %v, want nil", err)
	}
}
