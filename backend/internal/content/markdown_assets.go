package content

// Markdown style templates, one file each.
//
// ARCHITECTURE.md §34: `content/system/markdown/` is the presentation
// layer for Markdown content — the styles a post's rendered HTML carries.
// The arrangement is the custom-asset manager's, with one dimension
// removed: every template is CSS, so there is no type, and the grammar
// below admits only `.css`.
//
//	content/system/markdown/001-base.css          enabled
//	content/system/markdown/020-code.css          enabled
//	content/system/parked/markdown/020-code.css   disabled
//
// The three load-bearing decisions are inherited from ID-33:
//
//  1. The filename is the order. The three-digit prefix is the sort key.
//  2. "Enabled" is a location, not a flag. A file in `markdown/` is
//     enabled; the same file in `parked/markdown/` is disabled, which is
//     what lets Astro aggregate the directory with a plain readdir.
//  3. The templates are scoped to `.markdown-body` and are deliberately
//     theme-independent: they style content, never the site around it.
//
// Everything below is filesystem work. The database half lives in
// internal/store, and the reconciliation that joins the two lives in
// internal/api.
import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MarkdownTemplatePattern is the canonical grammar for a Markdown style
// template filename.
//
// Identical in shape to AssetFilenamePattern but closed to `.css`: a
// Markdown style template is always a stylesheet. The grammar admits no
// `/`, no `\`, no `.` outside the extension and no `..`, so a matching
// name cannot escape its directory; containment is re-checked below
// regardless, because defence in depth is cheap.
var MarkdownTemplatePattern = regexp.MustCompile(`^[0-9]{3}-[a-z0-9]+(?:-[a-z0-9]+)*\.css$`)

// ErrInvalidMarkdownTemplateName is returned for a filename outside the
// grammar.
var ErrInvalidMarkdownTemplateName = errors.New("invalid markdown template filename")

// ErrMarkdownTemplateNotFound is returned when no such template file
// exists.
var ErrMarkdownTemplateNotFound = errors.New("markdown template not found")

// MarkdownTemplateFile is one template as it exists on disk.
//
// This is deliberately not the API shape: the API adds `id` and `order`
// and never exposes a path, and nothing here leaves Go.
type MarkdownTemplateFile struct {
	Filename string
	Enabled  bool
	Size     int64
	Checksum string
	// Modified is the file's mtime, RFC 3339 — the only timestamp the
	// filesystem has, and what `updatedAt` reports for a template the
	// database has never seen.
	Modified string
	// Status is one of the AssetStatus* constants.
	Status  string
	Problem string
}

// ValidateMarkdownTemplateName checks one template filename against the
// grammar.
func ValidateMarkdownTemplateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: must not be empty", ErrInvalidMarkdownTemplateName)
	}
	if len(name) > AssetNameMaxBytes {
		return fmt.Errorf("%w: must be at most %d characters", ErrInvalidMarkdownTemplateName, AssetNameMaxBytes)
	}
	if !MarkdownTemplatePattern.MatchString(name) {
		return fmt.Errorf("%w: must look like 010-typography.css", ErrInvalidMarkdownTemplateName)
	}
	return nil
}

// markdownTemplateEnabledDir is the enabled directory, relative to
// content/system.
func markdownTemplateEnabledDir() string { return "markdown" }

// markdownTemplateParkedDir is where a disabled template waits, relative
// to content/system — a child of `system/`, exactly like `parked/css`,
// so one walk of `system/` answers "is anything here enabled" and a
// backup restore cannot quietly re-enable a parked file.
func markdownTemplateParkedDir() string { return filepath.Join("parked", "markdown") }

// markdownTemplatePath resolves a template filename to an absolute path,
// refusing to leave its directory.
//
// Validation happens first and containment second, in that order.
func (s *Store) markdownTemplatePath(name string, enabled bool) (string, error) {
	if err := ValidateMarkdownTemplateName(name); err != nil {
		return "", err
	}
	dir := filepath.Join(s.systemDir(), markdownTemplateEnabledDir())
	if !enabled {
		dir = filepath.Join(s.systemDir(), markdownTemplateParkedDir())
	}
	full := filepath.Clean(filepath.Join(dir, name))

	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("content: refusing to touch %q", full)
	}
	return full, nil
}

// locateMarkdownTemplate reports which directory holds a template, if
// either. One helper rather than a stat in five places, because the
// answer must be identical for a read, a rename and a delete.
func (s *Store) locateMarkdownTemplate(name string) (full string, enabled bool, found bool, err error) {
	for _, on := range []bool{true, false} {
		path, err := s.markdownTemplatePath(name, on)
		if err != nil {
			return "", false, false, err
		}
		if st, statErr := os.Stat(path); statErr == nil && st.Mode().IsRegular() {
			return path, on, true, nil
		}
	}
	return "", false, false, nil
}

// ListMarkdownTemplateFiles walks both directories and reports everything
// it finds, including what it refuses to serve.
//
// Nothing here is deleted and nothing is renamed: a file whose name is
// outside the grammar is reported as invalid so the admin can see it, and
// a missing directory is an empty collection, not a fault.
func (s *Store) ListMarkdownTemplateFiles() ([]MarkdownTemplateFile, error) {
	out := []MarkdownTemplateFile{}

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
			// A dotfile is not a template: that includes the `.tmp-*`
			// file writeFileAtomic leaves behind if the process dies
			// mid-write.
			if strings.HasPrefix(name, ".") {
				continue
			}
			// One flat directory. A subdirectory cannot be reached through
			// this API, and a stray one is reported rather than followed.
			if entry.IsDir() {
				out = append(out, MarkdownTemplateFile{
					Filename: name, Enabled: enabled,
					Status: AssetStatusInvalid, Problem: "subdirectories are not supported",
				})
				continue
			}
			out = append(out, readMarkdownTemplateFile(name, filepath.Join(dir, name), enabled))
		}
		return nil
	}

	if err := appendDir(filepath.Join(s.systemDir(), markdownTemplateEnabledDir()), true); err != nil {
		return nil, err
	}
	if err := appendDir(filepath.Join(s.systemDir(), markdownTemplateParkedDir()), false); err != nil {
		return nil, err
	}

	// The same name in both directories is a contradiction, not a merge.
	// Flag both copies so the admin sees two files to remove.
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

	// ID-35: filename ascending, always. A filesystem's readdir order is
	// an implementation detail.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

// readMarkdownTemplateFile stats and hashes one file, classifying
// anything unusable.
func readMarkdownTemplateFile(name, full string, enabled bool) MarkdownTemplateFile {
	file := MarkdownTemplateFile{Filename: name, Enabled: enabled}

	if err := ValidateMarkdownTemplateName(name); err != nil {
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
		// Over the editor's ceiling, so the admin could not save it
		// either: an aggregate that cannot round-trip through the editor
		// is a file whose state is a surprise (ID-38).
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

// MarkdownTemplateLocation reports whether a template exists, and whether
// it is enabled.
//
// Exported because the admin handlers need to answer "does this id name
// something" before they act, and they must not be reaching for a path.
func (s *Store) MarkdownTemplateLocation(name string) (enabled bool, found bool, err error) {
	_, enabled, found, err = s.locateMarkdownTemplate(name)
	return enabled, found, err
}

// ReadMarkdownTemplate returns one template's body.
func (s *Store) ReadMarkdownTemplate(name string) ([]byte, error) {
	full, _, found, err := s.locateMarkdownTemplate(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrMarkdownTemplateNotFound, name)
	}
	body, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return body, nil
}

// validateMarkdownTemplateBody applies the limits a template body must
// satisfy. A template is CSS, so it carries the CSS rules: the size
// ceiling and the refusal of `file://` URLs (ID-37).
func validateMarkdownTemplateBody(body string) error {
	return validateAssetBody(AssetCSS, body)
}

// CreateMarkdownTemplate writes a new template, refusing to overwrite an
// existing one.
func (s *Store) CreateMarkdownTemplate(name, body string) error {
	if err := ValidateMarkdownTemplateName(name); err != nil {
		return err
	}
	if err := validateMarkdownTemplateBody(body); err != nil {
		return err
	}
	if _, _, found, err := s.locateMarkdownTemplate(name); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: %s", ErrConflict, name)
	}
	full, err := s.markdownTemplatePath(name, true)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create markdown/: %w", err)
	}
	return writeFileAtomic(full, []byte(body))
}

// WriteMarkdownTemplate replaces a template's body, creating it enabled
// if it is new.
//
// A template that is currently disabled is edited where it is parked, not
// promoted: saving the text of a file the admin switched off must not
// switch it back on.
func (s *Store) WriteMarkdownTemplate(name, body string) error {
	if err := ValidateMarkdownTemplateName(name); err != nil {
		return err
	}
	if err := validateMarkdownTemplateBody(body); err != nil {
		return err
	}
	full, _, found, err := s.locateMarkdownTemplate(name)
	if err != nil {
		return err
	}
	if !found {
		if full, err = s.markdownTemplatePath(name, true); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create markdown/: %w", err)
	}
	return writeFileAtomic(full, []byte(body))
}

// SetMarkdownTemplateEnabled parks or unparks a template.
//
// The move is a rename inside one filesystem, so it is atomic, and the
// body is never rewritten. Enabling an already-enabled template is a
// no-op rather than an error, because the UI sends the state it believes
// in and a double click should not be a failure.
func (s *Store) SetMarkdownTemplateEnabled(name string, enabled bool) error {
	_, current, found, err := s.locateMarkdownTemplate(name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrMarkdownTemplateNotFound, name)
	}
	if current == enabled {
		return nil
	}

	from, err := s.markdownTemplatePath(name, current)
	if err != nil {
		return err
	}
	to, err := s.markdownTemplatePath(name, enabled)
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

// RenameMarkdownTemplate renames a template in place, keeping its enabled
// state.
//
// Validate, rename atomically, then let the caller update the metadata
// row. Nothing about the body changes, so this is a single rename rather
// than a read-modify-write.
func (s *Store) RenameMarkdownTemplate(from, to string) error {
	if err := ValidateMarkdownTemplateName(from); err != nil {
		return err
	}
	if err := ValidateMarkdownTemplateName(to); err != nil {
		return err
	}
	if from == to {
		return nil
	}
	_, enabled, found, err := s.locateMarkdownTemplate(from)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrMarkdownTemplateNotFound, from)
	}
	if _, _, taken, err := s.locateMarkdownTemplate(to); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("%w: %s", ErrConflict, to)
	}

	src, err := s.markdownTemplatePath(from, enabled)
	if err != nil {
		return err
	}
	dst, err := s.markdownTemplatePath(to, enabled)
	if err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename %s: %w", from, err)
	}
	return nil
}

// DeleteMarkdownTemplate removes a template file from whichever directory
// holds it.
func (s *Store) DeleteMarkdownTemplate(name string) error {
	// Validate before touching anything, so a bad name is refused rather
	// than silently reported as "no such file" after a pointless removal
	// attempt.
	if err := ValidateMarkdownTemplateName(name); err != nil {
		return err
	}
	for _, on := range []bool{true, false} {
		full, err := s.markdownTemplatePath(name, on)
		if err != nil {
			return err
		}
		if err := os.Remove(full); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete %s: %w", name, err)
		}
	}
	return fmt.Errorf("%w: %s", ErrMarkdownTemplateNotFound, name)
}
