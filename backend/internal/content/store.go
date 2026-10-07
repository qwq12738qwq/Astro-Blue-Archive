package content

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Get reads one entry by slug.
func (s *Store) Get(kind Kind, slug string) (*Entry, error) {
	full, err := s.filePath(kind, slug)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read %s: %w", slug, err)
	}

	entry, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s/%s.md: %w", kind, slug, err)
	}
	st, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", slug, err)
	}

	entry.RelPath = fmt.Sprintf("%s/%s.md", kind, slug)
	entry.ModTime = st.ModTime()

	// ARCHITECTURE.md §9: refuse to serve an entry whose declared slug
	// disagrees with its filename.
	if entry.Slug != slug {
		return nil, fmt.Errorf("%s: frontmatter.slug %q does not match filename %q",
			entry.RelPath, entry.Slug, slug)
	}

	return entry, nil
}

// Summary is a list entry: everything except the body.
type Summary struct {
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Date        *time.Time `json:"date,omitempty"`
	Updated     *time.Time `json:"updated,omitempty"`
	// Tags is always a JSON array, never null.
	//
	// ARCHITECTURE.md ID-20: `Normalize` leaves Tags nil when a file declares none,
	// and a nil slice marshals to `null`. That is a Go-ism leaking into the API
	// contract, and it broke the consumer: the admin post list maps every summary
	// into a view model and called `.map` on this field, so one post written
	// without a tag made the whole screen fail to render. A repeated field is `[]`.
	Tags     []string  `json:"tags"`
	Cover    string    `json:"cover,omitempty"`
	Draft    bool      `json:"draft"`
	NavOrder *int      `json:"navOrder,omitempty"`
	RelPath  string    `json:"path"`
	ModTime  time.Time `json:"modifiedAt"`
}

// tagsOrEmpty turns "no tags" into an empty array rather than nil. See Summary.Tags.
func tagsOrEmpty(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// List returns every entry of a kind, newest first.//
// A malformed file is skipped with a warning rather than failing the list, for
// the same reason the Astro loader isolates per-file failures (ARCHITECTURE.md §7):
// one bad file must not take the admin offline.
func (s *Store) List(kind Kind) ([]Summary, []string, error) {
	dir, err := s.dir(kind)
	if err != nil {
		return nil, nil, err
	}

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A missing directory is an empty collection, not a fault.
			return []Summary{}, nil, nil
		}
		return nil, nil, fmt.Errorf("read %s/: %w", kind, err)
	}

	out := make([]Summary, 0, len(dirEntries))
	skipped := []string{}

	for _, de := range dirEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
			continue
		}
		slug := strings.TrimSuffix(de.Name(), ".md")

		entry, err := s.Get(kind, slug)
		if err != nil {
			rel := fmt.Sprintf("%s/%s", kind, de.Name())
			skipped = append(skipped, rel)
			continue
		}

		out = append(out, Summary{
			Slug:        entry.Slug,
			Title:       entry.Title,
			Description: entry.Description,
			Date:        entry.Date,
			Updated:     entry.Updated,
			Tags:        tagsOrEmpty(entry.Tags),
			Cover:       entry.Cover,
			Draft:       entry.Draft,
			NavOrder:    entry.NavOrder,
			RelPath:     entry.RelPath,
			ModTime:     entry.ModTime,
		})
	}

	// Posts read newest first; pages follow their declared navOrder, then title.
	if kind == KindPages {
		sort.SliceStable(out, func(i, j int) bool {
			oi, oj := 1000, 1000
			if out[i].NavOrder != nil {
				oi = *out[i].NavOrder
			}
			if out[j].NavOrder != nil {
				oj = *out[j].NavOrder
			}
			if oi != oj {
				return oi < oj
			}
			return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
		})
	} else {
		sort.SliceStable(out, func(i, j int) bool {
			di, dj := time.Time{}, time.Time{}
			if out[i].Date != nil {
				di = *out[i].Date
			}
			if out[j].Date != nil {
				dj = *out[j].Date
			}
			if !di.Equal(dj) {
				return di.After(dj)
			}
			return out[i].Slug < out[j].Slug
		})
	}

	return out, skipped, nil
}

// Save writes an entry, creating or replacing the file.
//
// The write is atomic: content is written to a temporary file in the same
// directory and renamed into place, so a crash cannot leave a half-written post
// that the loader would reject.
func (s *Store) Save(kind Kind, entry *Entry) error {
	if !kind.Valid() {
		return fmt.Errorf("content: unknown kind %q", kind)
	}

	entry.Normalize()
	if fields := entry.Validate(); len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	if len(entry.Body) > BodyMaxBytes {
		return &ValidationError{Fields: map[string]string{
			"body": fmt.Sprintf("must be at most %d bytes", BodyMaxBytes),
		}}
	}

	full, err := s.filePath(kind, entry.Slug)
	if err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s/: %w", kind, err)
	}

	data, err := Marshal(entry)
	if err != nil {
		return err
	}

	if err := writeFileAtomic(full, data); err != nil {
		return err
	}
	entry.RelPath = fmt.Sprintf("%s/%s.md", kind, entry.Slug)

	// Report the file's real modification time so the API response is accurate
	// immediately after a write, without a second read.
	if st, err := os.Stat(full); err == nil {
		entry.ModTime = st.ModTime()
	}
	return nil
}

// writeFileAtomic writes via a temp file and rename in the same directory.
func writeFileAtomic(full string, data []byte) error {
	dir := filepath.Dir(full)

	tmp, err := os.CreateTemp(dir, ".tmp-*.md")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	// 0o644: readable by the Astro container, which may run as a different uid.
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// Create writes a new entry and refuses to overwrite an existing one.
func (s *Store) Create(kind Kind, entry *Entry) error {
	full, err := s.filePath(kind, entry.Slug)
	if err != nil {
		return err
	}
	if _, err := os.Stat(full); err == nil {
		return fmt.Errorf("%w: %s/%s.md", ErrConflict, kind, entry.Slug)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", entry.Slug, err)
	}
	return s.Save(kind, entry)
}

// Update replaces an existing entry. The slug must match, because changing it
// would mean renaming the file and moving the URL — a distinct operation.
func (s *Store) Update(kind Kind, slug string, entry *Entry) error {
	if entry.Slug != slug {
		return ErrSlugMismatch
	}
	if _, err := s.Get(kind, slug); err != nil {
		return err
	}
	return s.Save(kind, entry)
}

// Delete removes an entry's file.
func (s *Store) Delete(kind Kind, slug string) error {
	full, err := s.filePath(kind, slug)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("delete %s: %w", slug, err)
	}
	return nil
}

// ValidationError carries per-field messages to the admin UI.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Fields exposes the per-field messages.
func (e *ValidationError) Fields_() map[string]string { return e.Fields }

// ReadSystemFile reads one of the two allowed custom-code files.
func (s *Store) ReadSystemFile(name string) (string, error) {
	if name != "custom.css" && name != "custom.js" {
		return "", fmt.Errorf("content: unknown system file %q", name)
	}
	dir := filepath.Clean(filepath.Join(s.root, "system"))
	full := filepath.Clean(filepath.Join(dir, name))

	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("content: refusing to read %q", full)
	}

	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No custom code yet is a valid state, not an error.
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return string(data), nil
}

// StatSystemFile reports the size and modification time of one of the two legacy
// custom-code files.
//
// The admin list shows a row for `custom.css`/`custom.js` alongside the managed
// assets, and it needs a size and a date for it. `ReadSystemFile` cannot supply
// either — it returns the body only, and a body has no mtime.
func (s *Store) StatSystemFile(name string) (size int64, modified string, exists bool, err error) {
	if name != "custom.css" && name != "custom.js" {
		return 0, "", false, fmt.Errorf("content: unknown system file %q", name)
	}
	full := filepath.Clean(filepath.Join(s.root, "system", name))
	st, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, "", false, nil
		}
		return 0, "", false, fmt.Errorf("stat %s: %w", name, err)
	}
	if !st.Mode().IsRegular() {
		return 0, "", false, nil
	}
	return st.Size(), st.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00"), true, nil
}

// WriteSystemFile replaces one of the two allowed custom-code files.
//
// The file name is a constant chosen by the caller, never user input, but the
// containment check still runs (ARCHITECTURE.md §20).
func (s *Store) WriteSystemFile(name, body string) error {
	if name != "custom.css" && name != "custom.js" {
		return fmt.Errorf("content: unknown system file %q", name)
	}
	if len(body) > BodyMaxBytes {
		return &ValidationError{Fields: map[string]string{
			name: fmt.Sprintf("must be at most %d bytes", BodyMaxBytes),
		}}
	}

	dir := filepath.Clean(filepath.Join(s.root, "system"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create system/: %w", err)
	}
	full := filepath.Clean(filepath.Join(dir, name))

	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("content: refusing to write %q", full)
	}

	return writeFileAtomic(full, []byte(body))
}
