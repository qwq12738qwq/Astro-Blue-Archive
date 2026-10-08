package content

// Custom CSS and JavaScript assets, one file each.
//
// ARCHITECTURE.md ID-33: `content/system/custom.css` and `custom.js` stay exactly
// where they have always been and stay exactly as authoritative as they were. What
// this file adds is the ability to have *more than one* of each, without turning
// the admin into an IDE:
//
//	content/system/custom.css                  legacy, still first
//	content/system/custom.js                   legacy, still first
//	content/system/css/001-base.css            managed
//	content/system/css/010-layout.css          managed
//	content/system/parked/css/010-layout.css   managed, disabled
//
// Three decisions live here, and each of them is load-bearing.
//
//  1. The filename is the order. `001-`, `010-`, `100-` is a three-digit prefix, so
//     a lexical sort IS the intended order and there is no second ordering to keep
//     in step with the first (ID-35). A directory listing's order is never used.
//
//  2. "Enabled" is a location, not a flag. A file in `css/` is enabled; the same
//     file in `parked/css/` is disabled. That is what lets Astro aggregate the
//     directory with a plain readdir, so /custom.css needs no database, no build and
//     no second storage system — and an edit is live on the very next request
//     (ID-34). The database records the flag as metadata, reconciled from the tree;
//     the tree is the truth.
//
//  3. One asset is one file. No folders, no imports, no bundler, no package
//     manager, no minifier. Concatenation happens once, at response time, and the
//     concatenated text is never written to disk.
//
// Everything below is filesystem work. The database half lives in
// internal/store, and the reconciliation that joins the two lives in internal/api.
import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// AssetKind is a custom asset's type. There are exactly two.
//
// The set is closed on purpose. A "Custom Asset Manager" that also accepted HTML,
// SVG, JSON or a shell script would be a general file uploader wearing a
// customisation feature's name, and every one of those types is a fresh XSS or RCE
// decision this phase does not need to make.
type AssetKind string

const (
	AssetCSS AssetKind = "css"
	AssetJS  AssetKind = "js"
)

// AssetNameMaxBytes bounds the whole filename, including the extension.
//
// The grammar below leaves the stem unbounded (`[a-z0-9]+(-[a-z0-9]+)*`), which
// would let a request ask for a multi-kilobyte path segment. 80 mirrors SlugMax so
// the two naming grammars in this package have the same shape.
const AssetNameMaxBytes = 80

// AssetFilenamePattern is the canonical grammar for a managed asset filename.
//
// The three-digit prefix is mandatory and it is the sort key; the stem is kebab-case
// so a filename is safe in a URL, a log line and a shell without quoting.
//
// The grammar is the whole traversal defence: it admits no `/`, no `\`, no `.`
// outside the extension, no `..`, no leading dash and no whitespace, so a name that
// matches cannot escape its directory whatever else is wrong with it. Containment is
// still re-checked below, because a grammar is a filter and defence in depth is
// cheap.
var AssetFilenamePattern = regexp.MustCompile(`^[0-9]{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.(css|js)$`)

// ErrInvalidAssetName is returned for a filename outside the grammar.
var ErrInvalidAssetName = errors.New("invalid custom asset filename")

// ErrAssetNotFound is returned when no such asset file exists.
var ErrAssetNotFound = errors.New("custom asset not found")

// Valid reports whether the kind is one this package handles.
func (k AssetKind) Valid() bool { return k == AssetCSS || k == AssetJS }

// Dir is the managed directory for this kind, relative to content/system.
func (k AssetKind) Dir() string { return string(k) }

// Extension is the only extension an asset of this kind may carry.
func (k AssetKind) Extension() string { return "." + string(k) }

// ParseAssetKind maps a URL segment onto a kind. There is no default.
func ParseAssetKind(raw string) (AssetKind, error) {
	kind := AssetKind(raw)
	if !kind.Valid() {
		return "", fmt.Errorf("unknown custom asset type %q", raw)
	}
	return kind, nil
}

// Custom asset states. `ok` is the only one that is served.
const (
	// AssetStatusOK means the file exists and is loadable.
	AssetStatusOK = "ok"
	// AssetStatusMissing means metadata with no file behind it.
	AssetStatusMissing = "missing"
	// AssetStatusInvalid means a file that exists but must never be served:
	// a name outside the grammar, or one too large to edit.
	AssetStatusInvalid = "invalid"
)

// CustomAssetFile is one asset as it exists on disk.
//
// This is deliberately not the API shape. The API adds `id` and `order` and never
// exposes a path; nothing here leaves Go.
type CustomAssetFile struct {
	Filename string
	Type     AssetKind
	Enabled  bool
	Size     int64
	Checksum string
	// Modified is the file's mtime, RFC 3339. It is the only timestamp the
	// filesystem has, and it is what `updatedAt` reports for an asset the database
	// has never seen — which is how a file dropped into `css/` by hand gets a
	// sensible date instead of a blank.
	Modified string
	// Status is one of the AssetStatus* constants.
	Status  string
	Problem string
}

// AssetOrder returns the numeric prefix of a managed filename.
//
// It is 0 for anything that does not match the grammar, and such a name is never in
// the list as a servable asset anyway.
func AssetOrder(filename string) int {
	if !AssetFilenamePattern.MatchString(filename) {
		return 0
	}
	n, err := strconv.Atoi(filename[:3])
	if err != nil {
		return 0
	}
	return n
}

// ValidateAssetFilename checks one managed filename against the grammar and the
// kind.
//
// The extension check is what makes ID-36 possible: a rename cannot turn a CSS
// asset into a JavaScript one, because `010-layout.js` is not a legal name for the
// `css` kind.
func ValidateAssetFilename(kind AssetKind, name string) error {
	if !kind.Valid() {
		return fmt.Errorf("%w: unknown type %q", ErrInvalidAssetName, string(kind))
	}
	if name == "" {
		return fmt.Errorf("%w: must not be empty", ErrInvalidAssetName)
	}
	if len(name) > AssetNameMaxBytes {
		return fmt.Errorf("%w: must be at most %d characters", ErrInvalidAssetName, AssetNameMaxBytes)
	}
	if !AssetFilenamePattern.MatchString(name) {
		return fmt.Errorf("%w: must look like 010-layout%s", ErrInvalidAssetName, kind.Extension())
	}
	if !strings.HasSuffix(name, kind.Extension()) {
		return fmt.Errorf("%w: a %s asset must end in %s", ErrInvalidAssetName, kind, kind.Extension())
	}
	return nil
}

// systemDir is content/system.
func (s *Store) systemDir() string {
	return filepath.Join(s.root, "system")
}

// parkedRel is where a disabled asset waits, relative to content/system.
//
// A child of `system/` rather than a sibling of the managed directory itself, so one
// walk of `system/` still answers "is anything here enabled", and so the backup
// script's single `content/` copy carries the disabled files too — restoring a
// backup must not quietly re-enable code the admin switched off.
func parkedRel(kind AssetKind) string {
	return filepath.Join("parked", string(kind))
}

// assetPath resolves a managed filename to an absolute path, refusing to leave its
// directory.
//
// Validation happens first and containment second, in that order: a name that
// failed validation never reaches the join, and a name that passed it still cannot
// escape if the grammar is ever loosened.
func (s *Store) assetPath(kind AssetKind, name string, enabled bool) (string, error) {
	if err := ValidateAssetFilename(kind, name); err != nil {
		return "", err
	}
	dir := filepath.Join(s.systemDir(), kind.Dir())
	if !enabled {
		dir = filepath.Join(s.systemDir(), parkedRel(kind))
	}
	full := filepath.Clean(filepath.Join(dir, name))

	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("content: refusing to touch %q", full)
	}
	return full, nil
}

// locate reports which directory holds an asset, if either.
//
// One helper rather than a stat in five places, because the answer is "enabled
// directory, else parked directory, else nowhere" and that answer must be identical
// for a read, a rename and a delete.
func (s *Store) locate(kind AssetKind, name string) (full string, enabled bool, found bool, err error) {
	for _, on := range []bool{true, false} {
		path, err := s.assetPath(kind, name, on)
		if err != nil {
			return "", false, false, err
		}
		if st, statErr := os.Stat(path); statErr == nil && st.Mode().IsRegular() {
			return path, on, true, nil
		}
	}
	return "", false, false, nil
}

// Checksum is the content address used for ETag derivation, cache keys and
// debugging. Metadata only; the file stays the source of truth.
func Checksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ListCustomAssetFiles walks both directories for a kind and reports everything it
// finds, including what it refuses to serve.
//
// Nothing here is deleted and nothing is renamed. A file whose name is outside the
// grammar is reported as invalid so the admin can see it and delete it; a directory
// that does not exist is an empty collection, not a fault.
func (s *Store) ListCustomAssetFiles(kind AssetKind) ([]CustomAssetFile, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("content: unknown custom asset type %q", string(kind))
	}

	out := []CustomAssetFile{}

	appendDir := func(dir string, enabled bool) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("read %s: %w", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			// A dotfile is not an asset. That covers the `.tmp-*` file writeFileAtomic
			// leaves behind if the process dies mid-write, which is a crash artefact
			// rather than something an admin created.
			if strings.HasPrefix(name, ".") {
				continue
			}
			// One flat directory (ID-36). A subdirectory cannot be reached through
			// this API, and a stray one is reported rather than followed.
			if entry.IsDir() {
				out = append(out, CustomAssetFile{
					Filename: name, Type: kind, Enabled: enabled,
					Status: AssetStatusInvalid, Problem: "subdirectories are not supported",
				})
				continue
			}
			out = append(out, readCustomAssetFile(kind, name, filepath.Join(dir, name), enabled))
		}
		return nil
	}

	if err := appendDir(filepath.Join(s.systemDir(), kind.Dir()), true); err != nil {
		return nil, err
	}
	if err := appendDir(filepath.Join(s.systemDir(), parkedRel(kind)), false); err != nil {
		return nil, err
	}

	// The same name in both directories is a contradiction, not a merge. Flag both
	// copies, so the admin sees that there are two files to remove rather than one
	// of them mysteriously losing.
	counts := map[string]int{}
	for _, f := range out {
		counts[f.Filename]++
	}
	for i := range out {
		if counts[out[i].Filename] > 1 {
			out[i].Status = AssetStatusInvalid
			out[i].Problem = fmt.Sprintf("%d files share this name", counts[out[i].Filename])
		}
	}

	// ID-35: filename ascending, always. A filesystem's readdir order is an
	// implementation detail, and a stylesheet whose order changed between two
	// requests of the same code would be unsupportable.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

// readCustomAssetFile stats and hashes one file, classifying anything unusable.
func readCustomAssetFile(kind AssetKind, name, full string, enabled bool) CustomAssetFile {
	file := CustomAssetFile{Filename: name, Type: kind, Enabled: enabled}

	if err := ValidateAssetFilename(kind, name); err != nil {
		file.Status = AssetStatusInvalid
		file.Problem = "the filename is not allowed"
		return file
	}
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() {
		file.Status = AssetStatusMissing
		file.Problem = "the file could not be read"
		return file
	}
	if st.Size() > BodyMaxBytes {
		// Over the editor's ceiling, so the admin could not save it either. Served or
		// not? Not served: an aggregate that cannot round-trip through the editor is
		// a file whose state is a surprise (ID-38).
		file.Size = st.Size()
		file.Status = AssetStatusInvalid
		file.Problem = fmt.Sprintf("larger than %d bytes", BodyMaxBytes)
		return file
	}
	body, err := os.ReadFile(full)
	if err != nil {
		file.Status = AssetStatusMissing
		file.Problem = "the file could not be read"
		return file
	}
	file.Size = int64(len(body))
	file.Checksum = Checksum(body)
	file.Modified = st.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00")
	file.Status = AssetStatusOK
	return file
}

// CustomAssetLocation reports whether an asset exists, and whether it is enabled.
//
// Exported because the admin handlers need to answer "does this id name something"
// before they act, and they must not be reaching for a path. The bool pair is the
// whole answer: a `true` enabled flag with a missing file is a state the tree cannot
// express.
func (s *Store) CustomAssetLocation(kind AssetKind, name string) (enabled bool, found bool, err error) {
	_, enabled, found, err = s.locate(kind, name)
	return enabled, found, err
}

// ReadCustomAsset returns one asset's body.
func (s *Store) ReadCustomAsset(kind AssetKind, name string) ([]byte, error) {
	full, _, found, err := s.locate(kind, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrAssetNotFound, name)
	}
	body, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return body, nil
}

// fileSchemePattern finds a `file:` URL outside an identifier, so a declaration such
// as `--my-file: red` is not mistaken for one.
var fileSchemePattern = regexp.MustCompile(`(^|[^A-Za-z0-9_-])file://`)

// validateAssetBody applies the limits an asset body must satisfy.
//
// CSS gets one extra check: a `file://` URL is refused. A stylesheet is not executed,
// but `url(file:///etc/passwd)` is a filesystem reference the author almost certainly
// did not mean to make, and the honest place to refuse it is on the way in. Every
// other resource URL — `url(/media/x.png)`, `url(https://…)` — is left to the CSP's
// img-src and font-src, which is where the browser actually enforces it (ID-37).
//
// JavaScript gets no content check at all. The server never parses, compiles or runs
// it; that is the admin's own code running in the admin's own browser, by design.
func validateAssetBody(kind AssetKind, body string) error {
	if len(body) > BodyMaxBytes {
		return &ValidationError{Fields: map[string]string{
			"content": fmt.Sprintf("must be at most %d bytes", BodyMaxBytes),
		}}
	}
	if kind == AssetCSS && fileSchemePattern.MatchString(body) {
		return &ValidationError{Fields: map[string]string{
			"content": "must not reference a file:// URL",
		}}
	}
	return nil
}

// CreateCustomAsset writes a new asset, refusing to overwrite an existing one.
func (s *Store) CreateCustomAsset(kind AssetKind, name, body string) error {
	if err := ValidateAssetFilename(kind, name); err != nil {
		return err
	}
	if err := validateAssetBody(kind, body); err != nil {
		return err
	}
	if _, _, found, err := s.locate(kind, name); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: %s", ErrConflict, name)
	}
	full, err := s.assetPath(kind, name, true)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create %s/: %w", kind.Dir(), err)
	}
	return writeFileAtomic(full, []byte(body))
}

// WriteCustomAsset replaces an asset's body, creating it enabled if it is new.
//
// An asset that is currently disabled is edited where it is parked, not promoted.
// Saving the text of a file the admin switched off must not switch it back on: a
// content edit and an enable are two decisions, and this is only the first one.
func (s *Store) WriteCustomAsset(kind AssetKind, name, body string) error {
	if err := ValidateAssetFilename(kind, name); err != nil {
		return err
	}
	if err := validateAssetBody(kind, body); err != nil {
		return err
	}
	full, _, found, err := s.locate(kind, name)
	if err != nil {
		return err
	}
	if !found {
		if full, err = s.assetPath(kind, name, true); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create %s/: %w", kind.Dir(), err)
	}
	return writeFileAtomic(full, []byte(body))
}

// SetCustomAssetEnabled parks or unparks an asset.
//
// The move is a rename inside one filesystem, so it is atomic, and the body is never
// rewritten: a disabled asset is the same bytes in the other directory. Enabling an
// already-enabled asset is a no-op rather than an error, because the UI sends the
// state it believes in and a double click should not be a failure.
func (s *Store) SetCustomAssetEnabled(kind AssetKind, name string, enabled bool) error {
	_, current, found, err := s.locate(kind, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrAssetNotFound, name)
	}
	if current == enabled {
		return nil
	}

	from, err := s.assetPath(kind, name, current)
	if err != nil {
		return err
	}
	to, err := s.assetPath(kind, name, enabled)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(filepath.Dir(to)), err)
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// RenameCustomAsset renames an asset in place, keeping its enabled state.
//
// Validate, rename atomically, then let the caller update the metadata row. Nothing
// about the body changes, so this is a single rename rather than a read-modify-write
// — a write would also risk losing the file to a crash between the two steps.
func (s *Store) RenameCustomAsset(kind AssetKind, from, to string) error {
	if err := ValidateAssetFilename(kind, from); err != nil {
		return err
	}
	if err := ValidateAssetFilename(kind, to); err != nil {
		return err
	}
	if from == to {
		return nil
	}
	_, enabled, found, err := s.locate(kind, from)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrAssetNotFound, from)
	}
	if _, _, taken, err := s.locate(kind, to); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("%w: %s", ErrConflict, to)
	}

	src, err := s.assetPath(kind, from, enabled)
	if err != nil {
		return err
	}
	dst, err := s.assetPath(kind, to, enabled)
	if err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename %s: %w", from, err)
	}
	return nil
}

// DeleteCustomAsset removes an asset file from whichever directory holds it.
func (s *Store) DeleteCustomAsset(kind AssetKind, name string) error {
	// Validate before touching anything, so a bad name is refused rather than
	// silently reported as "no such file" after a pointless removal attempt.
	if err := ValidateAssetFilename(kind, name); err != nil {
		return err
	}
	for _, on := range []bool{true, false} {
		full, err := s.assetPath(kind, name, on)
		if err != nil {
			return err
		}
		if err := os.Remove(full); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete %s: %w", name, err)
		}
	}
	return fmt.Errorf("%w: %s", ErrAssetNotFound, name)
}
