// Package content reads and writes Markdown files under CONTENT_ROOT.
//
// ARCHITECTURE.md §2: this is the single source of truth for posts and pages.
// Go is the only writer (ARCHITECTURE.md §5); the Astro live loader is read-only.
//
// ARCHITECTURE.md §9: the filename stem IS the slug. `hello-world.md` must
// declare `slug: hello-world`. Two authorities for one URL is a data-integrity
// bug, so a mismatch is refused on write and skipped on read.
package content

import (
	"blogcms/internal/media"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Limits mirror astro/src/lib/schema.ts. R4: one schema definition is impossible
// across Go and Zod, so both are kept in step by a cross-checking test.
const (
	SlugMax        = 80
	TitleMax       = 200
	DescriptionMax = 300
	MaxTags        = 20
	TagMax         = 32
	BodyMaxBytes   = 512 * 1024
)

// SlugPattern is the canonical slug grammar (ARCHITECTURE.md §9).
var SlugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// TagPattern is the canonical tag grammar.
var TagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ReservedSlugs must never be used by a post or page, because they would shadow
// a framework route.
var ReservedSlugs = map[string]bool{
	"admin":    true,
	"posts":    true,
	"tags":     true,
	"login":    true,
	"logout":   true,
	"markdown": true,
}

// ErrNotFound is returned when a slug has no file.
var ErrNotFound = errors.New("content not found")

// ErrSlugMismatch is returned when a caller tries to change a slug in place.
var ErrSlugMismatch = errors.New("slug cannot be changed in place; rename the file instead")

// ErrConflict is returned when a slug is already taken.
var ErrConflict = errors.New("content already exists")

// Frontmatter is the on-disk metadata. It is deliberately a plain struct: the
// database must never hold these values (ARCHITECTURE.md §2).
type Frontmatter struct {
	Title       string     `yaml:"title"`
	Slug        string     `yaml:"slug"`
	Description string     `yaml:"description,omitempty"`
	Date        *time.Time `yaml:"date"`
	Updated     *time.Time `yaml:"updated,omitempty"`
	Tags        []string   `yaml:"tags,omitempty"`
	Cover       string     `yaml:"cover,omitempty"`
	Draft       bool       `yaml:"draft"`
	NavOrder    *int       `yaml:"navOrder,omitempty"`
}

// Entry is a parsed Markdown file.
type Entry struct {
	Frontmatter
	Body string `yaml:"-"`
	// RelPath is the path relative to CONTENT_ROOT, for audit logs.
	RelPath string `yaml:"-"`
	// ModTime is the file's modification time, used by the loader cache.
	ModTime time.Time `yaml:"-"`
}

// Kind distinguishes the two content directories.
type Kind string

const (
	KindPosts Kind = "posts"
	KindPages Kind = "pages"
)

// Valid reports whether a kind is one this package handles. Anything else is a
// caller bug and is rejected rather than joined onto a path.
func (k Kind) Valid() bool { return k == KindPosts || k == KindPages }

// Store owns the content root.
type Store struct {
	root string
}

// NewStore builds a Store over an existing content root.
func NewStore(root string) *Store {
	return &Store{root: root}
}

// Root returns the content root.
func (s *Store) Root() string { return s.root }

// dir resolves and containment-checks a content subdirectory.
//
// ARCHITECTURE.md §20: a user-derived path is validated, cleaned, resolved and
// confirmed to stay under the root before it is used. `filepath.Join` alone is
// never trusted with user input.
func (s *Store) dir(kind Kind) (string, error) {
	if !kind.Valid() {
		return "", fmt.Errorf("content: unknown kind %q", kind)
	}
	abs := filepath.Clean(filepath.Join(s.root, string(kind)))
	rel, err := filepath.Rel(filepath.Clean(s.root), abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("content: refusing to use %q", abs)
	}
	return abs, nil
}

// filePath resolves the path of one slug's Markdown file.
func (s *Store) filePath(kind Kind, slug string) (string, error) {
	if err := ValidateSlug(slug); err != nil {
		// Returned as a validation error rather than a bare error so every caller
		// turns bad input into a 422 with a field message. As a bare error it
		// reached the API as a 500 "internal server error" and logged at ERROR,
		// which meant a mistyped slug, a reserved word and a `..` traversal probe
		// all looked like server faults instead of rejected input.
		return "", &ValidationError{Fields: map[string]string{"slug": err.Error()}}
	}
	dir, err := s.dir(kind)
	if err != nil {
		return "", err
	}
	full := filepath.Clean(filepath.Join(dir, slug+".md"))
	// Belt and braces: the slug grammar already forbids separators, but the
	// containment check is what actually guarantees safety.
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("content: refusing to use %q", full)
	}
	return full, nil
}

// ValidateSlug enforces the canonical slug grammar and the reserved list.
func ValidateSlug(slug string) error {
	if slug == "" {
		return errors.New("slug is required")
	}
	if len(slug) > SlugMax {
		return fmt.Errorf("slug must be at most %d characters", SlugMax)
	}
	if !SlugPattern.MatchString(slug) {
		return fmt.Errorf("slug must match %s", SlugPattern)
	}
	if ReservedSlugs[slug] {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	return nil
}

// hasControlChars reports whether s contains a C0 control character or DEL.
//
// None of them is legitimate in a title or a description, and admitting them is
// not cosmetic: the RSS feed is XML, XML 1.0 forbids most control characters, and
// a parser rejects the *whole document*. One post whose title picked up a stray
// form feed therefore took the entire feed down for every reader.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Validate checks a frontmatter block, returning per-field messages.
func (f *Frontmatter) Validate() map[string]string {
	fields := map[string]string{}

	if strings.TrimSpace(f.Title) == "" {
		fields["title"] = "required"
	} else if len([]rune(f.Title)) > TitleMax {
		fields["title"] = fmt.Sprintf("must be at most %d characters", TitleMax)
	} else if hasControlChars(f.Title) {
		fields["title"] = "must not contain control characters"
	}

	if err := ValidateSlug(f.Slug); err != nil {
		fields["slug"] = err.Error()
	}

	if len([]rune(f.Description)) > DescriptionMax {
		fields["description"] = fmt.Sprintf("must be at most %d characters", DescriptionMax)
	} else if hasControlChars(f.Description) {
		fields["description"] = "must not contain control characters"
	}

	if f.Date == nil {
		fields["date"] = "required"
	}

	if len(f.Tags) > MaxTags {
		fields["tags"] = fmt.Sprintf("at most %d tags", MaxTags)
	}
	seen := map[string]bool{}
	for i, t := range f.Tags {
		if len(t) > TagMax || !TagPattern.MatchString(t) {
			fields[fmt.Sprintf("tags.%d", i)] = "must match " + TagPattern.String()
			continue
		}
		if seen[t] {
			fields[fmt.Sprintf("tags.%d", i)] = "duplicate tag"
		}
		seen[t] = true
	}

	if f.Cover != "" {
		if err := validateMediaRef(f.Cover); err != nil {
			fields["cover"] = err.Error()
		}
	}

	return fields
}

// validateMediaRef accepts the two shapes a cover legitimately takes, and nothing
// else.
//
// ARCHITECTURE.md §120: the media picker writes the asset's *public URL*, so
// `/media/2026/10/abc.png` is the normal form a human or a tool produces. The bare
// storage path `2026/10/abc.png` is accepted too, because that is what an older file
// or a hand-written one may contain, and rewriting an author's content on save is
// worse than tolerating two spellings of the same reference (ARCHITECTURE.md §81).
//
// Anything else is refused: a traversal, an absolute path, an external host, or a
// file type the delivery layer would not serve. Both accepted forms are reduced by
// the media package's own normaliser, so the grammar has exactly one definition and
// the admin, the frontend resolver and this validator cannot disagree about it.
func validateMediaRef(ref string) error {
	ref = strings.TrimSpace(ref)

	if normalized := media.NormaliseMediaPath(ref); normalized != "" {
		return nil
	}
	if normalized := media.PublicPath(ref); normalized != "" {
		return nil
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("cover must not contain a path traversal")
	}
	if strings.ContainsAny(ref, "\\:*?\"'<>|") || strings.Contains(ref, "://") {
		return fmt.Errorf("cover must be a media path or a /media/ URL")
	}
	return fmt.Errorf("cover must name an uploaded image (2026/10/abc.png or /media/2026/10/abc.png)")
}

// Normalize lowercases and de-duplicates tags and trims the title.
func (f *Frontmatter) Normalize() {
	f.Title = strings.TrimSpace(f.Title)
	f.Description = strings.TrimSpace(f.Description)
	f.Cover = strings.TrimSpace(f.Cover)

	seen := map[string]bool{}
	out := make([]string, 0, len(f.Tags))
	for _, t := range f.Tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		f.Tags = nil
	} else {
		f.Tags = out
	}
}

// Parse splits a Markdown file into frontmatter and body.
//
// The closing delimiter is a line consisting of exactly "---". Scanning line by
// line (rather than searching for the first "\n---") means a "---" inside a YAML
// block scalar cannot truncate the frontmatter early.
//
// Unknown keys are an error rather than being dropped, so a typo such as
// `descriptoin:` surfaces instead of silently discarding data (ARCHITECTURE.md §9).
func Parse(source string) (*Entry, error) {
	s := strings.TrimPrefix(source, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")

	if !strings.HasPrefix(s, "---\n") && s != "---" {
		return nil, errors.New("file does not start with a YAML frontmatter block")
	}
	rest := strings.TrimPrefix(s, "---\n")

	lines := strings.Split(rest, "\n")
	closing := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == "---" {
			closing = i
			break
		}
	}
	if closing < 0 {
		return nil, errors.New("frontmatter block is not terminated")
	}

	yamlBlock := strings.Join(lines[:closing], "\n")
	body := strings.Join(lines[closing+1:], "\n")

	var fm Frontmatter
	dec := yaml.NewDecoder(strings.NewReader(yamlBlock))
	dec.KnownFields(true) // reject unknown keys
	if err := dec.Decode(&fm); err != nil {
		return nil, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}

	return &Entry{Frontmatter: fm, Body: strings.TrimLeft(body, "\n")}, nil
}

// Marshal renders an entry back to Markdown with YAML frontmatter.
func Marshal(e *Entry) ([]byte, error) {
	var sb strings.Builder
	sb.WriteString("---\n")

	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(e.Frontmatter); err != nil {
		return nil, fmt.Errorf("encode frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close frontmatter encoder: %w", err)
	}

	sb.WriteString("---\n\n")
	body := strings.TrimRight(e.Body, "\n")
	sb.WriteString(body)
	sb.WriteString("\n")

	return []byte(sb.String()), nil
}
