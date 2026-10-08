// The backup inclusion policy: the single authority for which files a
// backup contains. ARCHITECTURE.md §2 — the filesystem is the content
// authority, and Git records versions of it. The policy is the boundary
// between source content and runtime state: nothing outside this file
// decides what a backup holds.
package backup

import (
	"path"
	"strings"
)

// contentSourceDirs are the content-root subtrees the Astro live loader
// reads. Only these enter the backup, so a backup records exactly the
// files the site renders and never anything else that happens to sit
// in the content root.
var contentSourceDirs = []string{"posts", "pages", "system"}

// Policy decides whether a file enters the backup. It covers two roots:
// the content root (posts, pages and the system directory) and the
// media root (uploaded originals — the storage contract guarantees the
// media root holds nothing else).
type Policy struct {
	contentRoot string
	mediaRoot   string
	maxFile     int64
	maxTotal    int64
}

// NewPolicy builds the inclusion policy for the two source roots. The
// size limits are a protection against a single huge upload turning a
// backup into a denial of service: an over-limit file blocks the
// backup with an error naming it rather than being silently skipped.
func NewPolicy(contentRoot, mediaRoot string, maxFile, maxTotal int64) *Policy {
	return &Policy{
		contentRoot: contentRoot,
		mediaRoot:   mediaRoot,
		maxFile:     maxFile,
		maxTotal:    maxTotal,
	}
}

// MaxFileBytes and MaxTotalBytes expose the configured ceilings so the
// API can report them to the admin.
func (p *Policy) MaxFileBytes() int64  { return p.maxFile }
func (p *Policy) MaxTotalBytes() int64 { return p.maxTotal }

// source is one filesystem root the policy covers, and the directory
// it is materialized into inside the repository work tree.
type source struct {
	root     string
	snapshot string
}

// sources lists the roots in snapshot order: content first, then media.
func (p *Policy) sources() []source {
	return []source{
		{root: p.contentRoot, snapshot: "content"},
		{root: p.mediaRoot, snapshot: "media"},
	}
}

// included reports whether rel (slash-separated, relative to root)
// enters the backup.
func (p *Policy) included(root, rel string) bool {
	if rel == "" {
		return false
	}
	if base := path.Base(rel); isIgnoredName(base) {
		return false
	}
	if root == p.contentRoot {
		first := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			first = rel[:i]
		}
		for _, dir := range contentSourceDirs {
			if first == dir {
				return true
			}
		}
		// Anything else in the content root is not source content the
		// loader reads, so it is not part of the site and not backed up.
		return false
	}
	// The media root only ever contains uploaded originals.
	return true
}

// isIgnoredName rejects dotfiles and editor leftovers everywhere: they
// are neither source content nor media, and a stray swap file must not
// become part of the site's history.
func isIgnoredName(base string) bool {
	return strings.HasPrefix(base, ".") ||
		strings.HasPrefix(base, "._") ||
		strings.HasSuffix(base, ".tmp") ||
		strings.HasSuffix(base, ".bak") ||
		strings.HasSuffix(base, ".swp") ||
		strings.HasSuffix(base, "~")
}
