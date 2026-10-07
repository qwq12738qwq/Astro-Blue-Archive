// The media usage index: which content files reference which media assets.
//
// ARCHITECTURE.md §50: this index is DERIVED metadata. It is not a source of truth
// and it never becomes one. `content/*.md` is the truth; this is a rescan of it, so
// losing the whole table costs one rebuild and nothing else.
//
// That is the only acceptable relationship in this direction. The alternative —
// asking an admin to maintain `used_by_post` as they write — makes the reference
// list the thing that is trusted, and the trusted thing is the thing that rots.
//
// ARCHITECTURE.md §49: the reference comes from the content itself. There is no
// field to fill in and no button to remember to press.
package media

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Content kinds the index knows about. They match the two directories under
// CONTENT_ROOT, and the singular nouns are what the admin UI shows.
const (
	KindPost = "post"
	KindPage = "page"
)

// UsageRef is one reference from one content file to one media path.
type UsageRef struct {
	MediaPath      string
	Kind           string
	Slug           string
	ReferenceCount int
}

// ScanUsage reads every Markdown file under contentRoot and returns the references
// it finds.
//
// ARCHITECTURE.md §52: Markdown images (`![alt](/media/x.jpg)`) are the primary
// form, but a cover in the frontmatter and a raw `<img src>` are equally real
// references, so all three are recognised. Scanning is textual and read-only: this
// code never parses, renders or rewrites content.
//
// A file that cannot be read is skipped rather than failing the scan. The index is
// a convenience, and one unreadable file must not make the whole library look
// unused — which is exactly how a media library loses data.
func ScanUsage(contentRoot string) ([]UsageRef, error) {
	type key struct{ kind, slug, media string }
	counts := map[key]int{}

	for _, dir := range []struct {
		name string
		kind string
	}{{"posts", KindPost}, {"pages", KindPage}} {
		full := filepath.Join(contentRoot, dir.name)
		entries, err := os.ReadDir(full)
		if err != nil {
			// A missing directory is a valid state: a blog with no pages yet.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			slug := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			raw, err := os.ReadFile(filepath.Join(full, entry.Name()))
			if err != nil {
				continue
			}
			for _, mediaPath := range ExtractMediaPaths(string(raw)) {
				counts[key{kind: dir.kind, slug: slug, media: mediaPath}]++
			}
		}
	}

	out := make([]UsageRef, 0, len(counts))
	for k, n := range counts {
		out = append(out, UsageRef{
			MediaPath:      k.media,
			Kind:           k.kind,
			Slug:           k.slug,
			ReferenceCount: n,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MediaPath != out[j].MediaPath {
			return out[i].MediaPath < out[j].MediaPath
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// mediaRefPattern matches the three ways content points at a media asset.
//
// Deliberately conservative in shape and liberal in form:
//
//   - an optional site origin in front, so a Markdown file written by hand with an
//     absolute URL is still recognised;
//   - the literal `/media/` prefix, which is the public URL contract
//     (ARCHITECTURE.md §13) and is what a Markdown body, a frontmatter `cover`
//     and an `<img src>` all end up containing;
//   - a path made only of characters the server generates: hex ids, digits, `-`,
//     `_`, `.` and `/`.
//
// The character class is what makes this safe to run over arbitrary Markdown. It
// cannot match a quote, a space, a `>` or a scheme delimiter, so a reference can
// never carry a query string, an HTML entity or a path traversal out of the text
// into the index.
var mediaRefPattern = regexp.MustCompile(
	`(?:https?://[^/\s"'<>)\]]+)?/media/([0-9A-Za-z][0-9A-Za-z._/-]*)`,
)

// ExtractMediaPaths returns every media path referenced by a document, in document
// order and *with duplicates*.
//
// Occurrences, not distinct paths: a post that uses the same image in a cover and
// three times in the body depends on it four times, and an admin deciding whether it
// is safe to delete wants that number. De-duplicating here would make the index
// answer a question nobody asked.
func ExtractMediaPaths(markdown string) []string {
	matches := mediaRefPattern.FindAllStringSubmatch(markdown, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if p := NormaliseMediaPath(m[1]); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// NormaliseMediaPath trims a scanned reference down to the storage layout the
// server generates: `YYYY/MM/<id>.<ext>`.
//
// Anything that does not fit is dropped rather than stored. A reference the layout
// cannot produce is not a reference to an asset we own, and the index exists to
// answer "which of MY files is this", so an unrecognisable string is noise.
//
// Note that this also means `../../etc/passwd` can never become a row: the grammar
// admits no `..` segment and the pattern could not have captured one anyway.
var layoutPattern = regexp.MustCompile(`^\d{4}/\d{2}/[0-9a-f]{24}\.(jpg|png|gif|webp)$`)

// NormaliseMediaPath returns the canonical storage path, or "" when the reference
// is not one of ours.
func NormaliseMediaPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/media/")
	p = strings.TrimPrefix(p, "media/")
	if p == "" {
		return ""
	}
	if strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
		return ""
	}
	if !layoutPattern.MatchString(p) {
		return ""
	}
	return p
}

// PublicURL is the single authority for the public URL of a stored asset.
//
// ARCHITECTURE.md §12/§13: nothing outside this package — no theme, no admin
// screen, no Markdown — assembles "/media/" + filename itself. The URL is a
// property of the media system, and a theme that hand-built it would be coupled to
// the storage layout.
//
// The URL always names the ORIGINAL. WebP is chosen by content negotiation at
// request time and never appears in a URL (ARCHITECTURE.md §11/§81), so a Markdown
// file written today keeps rendering correctly when the delivery layer changes.
func PublicURL(rel string) string { return PublicURLPrefix + "/" + rel }

// PublicURLPrefix is the public path segment the delivery layer serves.
const PublicURLPrefix = "/media"

// PublicPath converts a public URL back into a storage path, for the case where a
// caller has a URL rather than a path (the usage scanner, mostly).
func PublicPath(url string) string {
	idx := strings.Index(url, PublicURLPrefix+"/")
	if idx < 0 {
		return ""
	}
	return NormaliseMediaPath(url[idx+len(PublicURLPrefix)+1:])
}

// IsImagePath reports whether a storage path holds a type the delivery layer will
// serve as an image.
func IsImagePath(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	}
	return false
}

// NewestContentModTime returns the most recent modification time of any Markdown file
// under contentRoot.
//
// It exists so the derived usage index can be rebuilt when the source has actually
// changed (ARCHITECTURE.md §50). Rebuilding on a timer, or on every load, would either
// be needlessly expensive or needlessly stale; comparing against the newest file is
// the difference between "the index tracks the files" and "the index is right until
// someone edits a post".
//
// A missing directory is not an error: a blog with no pages yet has no content.
func NewestContentModTime(contentRoot string) (time.Time, error) {
	var newest time.Time

	for _, dir := range []string{"posts", "pages"} {
		full := filepath.Join(contentRoot, dir)
		entries, err := os.ReadDir(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return time.Time{}, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				// An entry that vanished between the read and the stat is not a
				// reason to refuse the answer.
				continue
			}
			if mod := info.ModTime(); mod.After(newest) {
				newest = mod
			}
		}
	}
	return newest, nil
}
