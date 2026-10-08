package content_test

// ARCHITECTURE.md ID-33: the custom-asset filesystem layer.
//
// These are the properties the HTTP suites cannot cheaply observe: what a filename
// grammar rejects, where a disabled file actually lives, that two files which share a
// name in both directories are reported rather than merged, and that an oversized or
// unreadable file is skipped instead of failing the whole listing. Asserting them
// through `fetch` would mean a directory tree per assertion.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blogcms/internal/content"
	"blogcms/internal/testutil"
)

func TestValidateAssetFilenameRejectsTraversal(t *testing.T) {
	// The brief's list, plus the shapes a URL can smuggle through a single path
	// segment. Every one of these must be refused by the grammar itself: a filter that
	// depends on `filepath.Clean` behaving as expected is a filter that breaks the
	// first time someone changes the join.
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
		strings.Repeat("010-", 40) + "base.css", // longer than AssetNameMaxBytes
	}
	for _, name := range bad {
		if err := content.ValidateAssetFilename(content.AssetCSS, name); err == nil {
			t.Errorf("content.ValidateAssetFilename(css, %q) accepted a name it must refuse", name)
		}
	}

	good := []string{"001-base.css", "010-layout.css", "100-custom.css", "020-card.css", "000-first.css"}
	for _, name := range good {
		if err := content.ValidateAssetFilename(content.AssetCSS, name); err != nil {
			t.Errorf("content.ValidateAssetFilename(css, %q) = %v, want nil", name, err)
		}
	}
}

// A rename must not be able to change an asset's type, and the extension is what
// stops it.
func TestValidateAssetFilenameKeepsTheExtensionAndTheKindTogether(t *testing.T) {
	if err := content.ValidateAssetFilename(content.AssetCSS, "010-layout.js"); err == nil {
		t.Fatal("a CSS asset accepted a .js filename; a rename could change type")
	}
	if err := content.ValidateAssetFilename(content.AssetJS, "010-layout.css"); err == nil {
		t.Fatal("a JavaScript asset accepted a .css filename; a rename could change type")
	}
	if err := content.ValidateAssetFilename(content.AssetJS, "010-navigation.js"); err != nil {
		t.Fatalf("content.ValidateAssetFilename(js, 010-navigation.js) = %v, want nil", err)
	}
}

func TestAssetOrderComesFromThePrefix(t *testing.T) {
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
			t.Errorf("AssetOrder(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestListCustomAssetFilesIsOrderedByFilenameNotByDirectory(t *testing.T) {
	s, _ := testutil.ContentStore(t)

	// Written in an order that is deliberately *not* the load order.
	for _, name := range []string{"020-components.css", "001-base.css", "100-custom.css", "010-layout.css"} {
		if err := s.CreateCustomAsset(content.AssetCSS, name, "."+name+" {}\n"); err != nil {
			t.Fatal(err)
		}
	}

	files, err := s.ListCustomAssetFiles(content.AssetCSS)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"001-base.css", "010-layout.css", "020-components.css", "100-custom.css"}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(files), len(want), files)
	}
	for i, name := range want {
		if files[i].Filename != name {
			t.Errorf("position %d = %q, want %q", i, files[i].Filename, name)
		}
		if files[i].Status != content.AssetStatusOK {
			t.Errorf("%s status = %q (%s), want ok", name, files[i].Status, files[i].Problem)
		}
		if files[i].Enabled != true {
			t.Errorf("%s is in css/ but reported disabled", name)
		}
	}
}

// ID-34: enabled is a location, so a disabled asset is in parked/css and nowhere in
// css/. That is what lets the public aggregator answer with a readdir and no database.
func TestEnableAndDisableMoveTheFile(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	const name = "010-layout.css"
	if err := s.CreateCustomAsset(content.AssetCSS, name, ".a{color:red}\n"); err != nil {
		t.Fatal(err)
	}

	cssDir := filepath.Join(s.Root(), "system", "css")
	parkedDir := filepath.Join(s.Root(), "system", "parked", "css")

	if err := s.SetCustomAssetEnabled(content.AssetCSS, name, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cssDir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a disabled asset is still in css/: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(parkedDir, name))
	if err != nil {
		t.Fatalf("the parked file is missing: %v", err)
	}
	if string(body) != ".a{color:red}\n" {
		t.Errorf("parking rewrote the body: %q", body)
	}

	files, err := s.ListCustomAssetFiles(content.AssetCSS)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Enabled {
		t.Fatalf("a parked asset listed as %+v, want one disabled entry", files)
	}

	if err := s.SetCustomAssetEnabled(content.AssetCSS, name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cssDir, name)); err != nil {
		t.Errorf("re-enabling did not move the file back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parkedDir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the parked copy survived: %v", err)
	}

	// Idempotent in both directions: the UI sends the state it believes in.
	if err := s.SetCustomAssetEnabled(content.AssetCSS, name, true); err != nil {
		t.Errorf("enabling an enabled asset: %v", err)
	}
}

// Editing a disabled asset must not switch it back on. A content edit and an enable
// are two decisions, and a save is only the first.
func TestWriteCustomAssetKeepsAParkedAssetParked(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	const name = "010-layout.css"
	if err := s.CreateCustomAsset(content.AssetCSS, name, ".a{color:red}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCustomAssetEnabled(content.AssetCSS, name, false); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteCustomAsset(content.AssetCSS, name, ".a{color:blue}\n"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(s.Root(), "system", "css", name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("saving a disabled asset promoted it into css/: %v", err)
	}
	body, err := s.ReadCustomAsset(content.AssetCSS, name)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != ".a{color:blue}\n" {
		t.Errorf("body = %q, want the saved text", body)
	}
	enabled, found, err := s.CustomAssetLocation(content.AssetCSS, name)
	if err != nil || !found || enabled {
		t.Errorf("location = (%v, %v, %v), want (false, true, nil)", enabled, found, err)
	}
}

// ID-38: one unusable file is skipped, and the list around it is unaffected.
func TestListCustomAssetFilesReportsBadFilesInsteadOfFailing(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateCustomAsset(content.AssetCSS, "001-base.css", ".a{}\n"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Root(), "system", "css")

	// A name the grammar refuses, an oversized file, a subdirectory and the temp file
	// a crashed atomic write leaves behind.
	if err := os.WriteFile(filepath.Join(dir, "evil.css"), []byte("*{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "010-huge.css"), make([]byte, content.BodyMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tmp-leftover.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := s.ListCustomAssetFiles(content.AssetCSS)
	if err != nil {
		t.Fatalf("one bad file took down the listing: %v", err)
	}

	byName := map[string]content.CustomAssetFile{}
	for _, f := range files {
		byName[f.Filename] = f
	}
	if got := byName["001-base.css"]; got.Status != content.AssetStatusOK {
		t.Errorf("the good file is %q (%s), want ok", got.Status, got.Problem)
	}
	if got := byName["evil.css"]; got.Status != content.AssetStatusInvalid || got.Problem == "" {
		t.Errorf("evil.css = %q (%q), want invalid with a reason", got.Status, got.Problem)
	}
	if got := byName["010-huge.css"]; got.Status != content.AssetStatusInvalid {
		t.Errorf("010-huge.css = %q, want invalid", got.Status)
	}
	if got := byName["nested"]; got.Status != content.AssetStatusInvalid {
		t.Errorf("a subdirectory = %q, want invalid", got.Status)
	}
	if _, ok := byName[".tmp-leftover.md"]; ok {
		t.Error("a dotfile was listed as an asset; it is a crash artefact, not a file an admin made")
	}
}

// The same name in both directories is a contradiction, and both copies must say so.
func TestListCustomAssetFilesFlagsADuplicateAcrossDirectories(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateCustomAsset(content.AssetCSS, "010-layout.css", ".a{}\n"); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(s.Root(), "system", "parked", "css")
	if err := os.MkdirAll(parked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parked, "010-layout.css"), []byte(".b{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := s.ListCustomAssetFiles(content.AssetCSS)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d entries, want both copies reported: %+v", len(files), files)
	}
	for _, f := range files {
		if f.Status != content.AssetStatusInvalid {
			t.Errorf("%s (enabled=%v) = %q, want invalid", f.Filename, f.Enabled, f.Status)
		}
	}
}

func TestCreateRefusesToOverwrite(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateCustomAsset(content.AssetCSS, "010-layout.css", ".a{}\n"); err != nil {
		t.Fatal(err)
	}
	err := s.CreateCustomAsset(content.AssetCSS, "010-layout.css", ".b{}\n")
	if !errors.Is(err, content.ErrConflict) {
		t.Fatalf("second create = %v, want ErrConflict", err)
	}
	body, err := s.ReadCustomAsset(content.AssetCSS, "010-layout.css")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != ".a{}\n" {
		t.Errorf("the refused create overwrote the file: %q", body)
	}
}

func TestRenameKeepsTheEnabledStateAndRefusesToClobber(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateCustomAsset(content.AssetCSS, "010-layout.css", ".a{color:red}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCustomAsset(content.AssetCSS, "020-card.css", ".b{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCustomAssetEnabled(content.AssetCSS, "010-layout.css", false); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameCustomAsset(content.AssetCSS, "010-layout.css", "011-layout.css"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "system", "parked", "css", "011-layout.css")); err != nil {
		t.Errorf("renaming a parked asset lost it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "system", "css", "010-layout.css")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old name survived the rename: %v", err)
	}

	if err := s.RenameCustomAsset(content.AssetCSS, "011-layout.css", "020-card.css"); !errors.Is(err, content.ErrConflict) {
		t.Errorf("renaming onto an existing name = %v, want ErrConflict", err)
	}
	if err := s.RenameCustomAsset(content.AssetCSS, "020-card.css", "020-card.js"); err == nil {
		t.Error("a rename changed the asset's type; the extension check must stop it")
	}
	if err := s.RenameCustomAsset(content.AssetCSS, "999-gone.css", "001-gone.css"); !errors.Is(err, content.ErrAssetNotFound) {
		t.Errorf("renaming a missing asset = %v, want ErrAssetNotFound", err)
	}
}

func TestDeleteRemovesFromWhicheverDirectory(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateCustomAsset(content.AssetJS, "010-nav.js", "window.x=1\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCustomAssetEnabled(content.AssetJS, "010-nav.js", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCustomAsset(content.AssetJS, "010-nav.js"); err != nil {
		t.Fatal(err)
	}
	files, err := s.ListCustomAssetFiles(content.AssetJS)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("after delete the listing is %+v, want empty", files)
	}
	if err := s.DeleteCustomAsset(content.AssetJS, "010-nav.js"); !errors.Is(err, content.ErrAssetNotFound) {
		t.Errorf("a second delete = %v, want ErrAssetNotFound", err)
	}
	if err := s.DeleteCustomAsset(content.AssetJS, "../../evil.js"); !errors.Is(err, content.ErrInvalidAssetName) {
		t.Errorf("deleting a traversal name = %v, want ErrInvalidAssetName", err)
	}
}

// The two types live in separate directories and cannot see each other.
func TestTheTwoTypesAreIndependent(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateCustomAsset(content.AssetCSS, "010-shared.css", ".a{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCustomAsset(content.AssetJS, "010-shared.js", "// x\n"); err != nil {
		t.Fatal(err)
	}
	cssFiles, err := s.ListCustomAssetFiles(content.AssetCSS)
	if err != nil {
		t.Fatal(err)
	}
	jsFiles, err := s.ListCustomAssetFiles(content.AssetJS)
	if err != nil {
		t.Fatal(err)
	}
	if len(cssFiles) != 1 || len(jsFiles) != 1 {
		t.Fatalf("css=%+v js=%+v, want one each", cssFiles, jsFiles)
	}
	if _, found, _ := s.CustomAssetLocation(content.AssetCSS, "010-shared.js"); found {
		t.Error("a JavaScript asset was found in the CSS collection")
	}
}

func TestBodiesAreLimited(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	oversize := strings.Repeat("a", content.BodyMaxBytes+1)
	err := s.CreateCustomAsset(content.AssetCSS, "010-huge.css", oversize)
	var ve *content.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("an oversized asset = %v, want a ValidationError", err)
	}
	if ve.Fields["content"] == "" {
		t.Errorf("the oversize message names no field: %+v", ve.Fields)
	}
	if _, statErr := os.Stat(filepath.Join(s.Root(), "system", "css", "010-huge.css")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a refused create still wrote a file: %v", statErr)
	}

	if err := s.CreateCustomAsset(content.AssetJS, "010-huge.js", oversize); !errors.As(err, &ve) {
		t.Errorf("JavaScript has the same ceiling, got %v", err)
	}
}

// §18: a stylesheet is not executed, but `url(file://…)` is a filesystem reference the
// author did not mean to make. CSP would block it too; this refuses it on the way in
// so the failure is a message in the editor rather than a silently missing image.
func TestCSSRefusesAFileScheme(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	bad := []string{
		`.a { background: url(file:///etc/passwd); }`,
		`.a { background: url("file:///etc/passwd"); }`,
		`.a { background: url( file://x ); }`,
	}
	for _, body := range bad {
		if err := s.CreateCustomAsset(content.AssetCSS, "010-bg.css", body); err == nil {
			t.Errorf("CreateCustomAsset accepted %q", body)
		}
	}
	good := []string{
		`.a { background: url(/media/2026/10/x.png); }`,
		`.a { background: url(https://example.com/x.png); }`,
		`.a { --my-file: red; }`,
		`.a::after { content: "no file: here"; }`,
	}
	for _, body := range good {
		if err := s.CreateCustomAsset(content.AssetCSS, "010-bg.css", body); err != nil {
			t.Errorf("CreateCustomAsset refused %q: %v", body, err)
		}
		if err := s.DeleteCustomAsset(content.AssetCSS, "010-bg.css"); err != nil {
			t.Fatal(err)
		}
	}
}

// §61: JavaScript gets no content inspection at all. The server never parses, compiles
// or runs it, so a "smart" check here would only ever reject valid code.
func TestJavaScriptIsNotInspected(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	if err := s.CreateCustomAsset(content.AssetJS, "010-anything.js", "fetch('file:///etc/passwd'); eval('1');"); err != nil {
		t.Fatalf("JavaScript was inspected: %v", err)
	}
}

func TestChecksumTracksContentNotTheName(t *testing.T) {
	first := content.Checksum([]byte(".a{color:red}\n"))
	if first != content.Checksum([]byte(".a{color:red}\n")) {
		t.Error("the same bytes produced different checksums")
	}
	if first == content.Checksum([]byte(".a{color:blue}\n")) {
		t.Error("different bytes produced the same checksum")
	}
}

func TestMissingDirectoryIsAnEmptyCollection(t *testing.T) {
	s, _ := testutil.ContentStore(t)
	files, err := s.ListCustomAssetFiles(content.AssetCSS)
	if err != nil {
		t.Fatalf("no css/ directory is a normal state, got %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %+v, want empty", files)
	}
	if _, err := s.ReadCustomAsset(content.AssetCSS, "001-base.css"); !errors.Is(err, content.ErrAssetNotFound) {
		t.Errorf("reading a missing asset = %v, want ErrAssetNotFound", err)
	}
}
